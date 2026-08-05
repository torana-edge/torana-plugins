// The keyword_compactor shrinks large tool results deterministically: it
// extracts the intent captured by the intent plugin, scores the output's
// lines by intent keywords, and keeps the matching lines with a small context
// window — no model call, no offload spend. Compacted results are cached by
// content-address, so later turns replaying the same result reuse the compact
// form for free.
//
// Run it AFTER the intent plugin. It is an alternative to compactor
// (cheap-model offload) — pick ONE per deployment; both consume the same
// intent cache, but their cache namespaces are disjoint
// (keyword_compactor/* vs compactor/*), so no cross-plugin collision.
//
// v2 semantics (typed host calls, same rules as compactor):
//   - cache reads distinguish absent (NOT_FOUND) from present-empty; a
//     present-empty or NON-SHORTER cached value is unusable and recomputed
//     locally (these are local deterministic computations — trusting a
//     corrupt or stale value would cache the corruption forever or expand
//     the request);
//   - any non-NOT_FOUND cache refusal or malformed reply is a
//     contract/configuration defect: the hook errors, so failure_mode
//     preserves the request and the host records the failure;
//   - cache writes and savings reports stay best-effort: the replacement is
//     already applied to the in-memory request and the host logs refusals;
//   - torana_compact_eligible_total fires ONCE per candidate after the
//     general size/safety filters and a matched non-exact policy, before the
//     mode-specific consumption gates — the same definition as compactor.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pbv2 "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

const (
	minContentLength = 2000     // below this, content is already small enough
	contextLines     = 2        // lines of context around keyword matches
	maxKeepLines     = 200      // HARD cap on kept lines (selection is bounded)
	maxResultBytes   = 8000     // total output budget, truncation notice included
	intentCacheKey   = "intent" // cache key for intent (set by the intent plugin)
	// Namespaced by plugin. env.cache_* is a SHARED store — unlike
	// env.state_*, which the host keys by module name — so two plugins using
	// the same namespace string read and write each other's entries. These
	// namespaces are disjoint from compactor's ("compactor/policy_compacted",
	// "compacted"), so the two alternative compactors never cross-apply.
	policyCompactionCache  = "keyword_compactor/policy_compacted"
	keywordCompactionCache = "keyword_compacted"
)

var (
	cfgOnce      sync.Once
	toolPolicies []sdk.ToolPolicyRule
)

// config is the plugin's decoded configuration.
type config struct {
	ToolPolicies []sdk.ToolPolicyRule `json:"tool_policies"`
}

// parseConfig is the pure config decoder; loadConfig installs its result into
// the process-global state exactly once. The host validates config against
// schema.json at write time, so an unmarshal failure here is unreachable in
// practice and falls back to defaults, matching v1.
func parseConfig(raw string) config {
	var c config
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &c)
	}
	return c
}

func loadConfig() {
	cfgOnce.Do(func() {
		c := parseConfig(sdk.PluginConfig())
		toolPolicies = c.ToolPolicies
	})
}

// resetConfigForTest restores every config global so a test row can install a
// fresh config. Production never calls it; the once-only loader is unchanged.
func resetConfigForTest() {
	cfgOnce = sync.Once{}
	toolPolicies = nil
}

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pbv2.ChatRequest) (sdk.RequestResult, error) {
		modified, err := compactToolResults(req)
		if err != nil {
			// failure_mode (pass) preserves the request; the host records the
			// plugin failure.
			return sdk.RequestResult{}, err
		}
		if !modified {
			return sdk.PassRequest(), nil
		}
		return sdk.ReplaceRequest(req), nil
	})
}

// ==========================================================================
// Tool result compaction
// ==========================================================================

func compactToolResults(req *pbv2.ChatRequest) (bool, error) {
	loadConfig()
	modified := false
	assistantAfter := assistantMessageCountsAfter(req.Messages)
	toolNames := sdk.ToolNamesByCallID(req.Messages)
	toolCalls := sdk.ToolCallsByID(req.Messages)

	// Ordered seam: EVERY message's tool-result blocks are candidates (no
	// role gate); each block is identified by (message index, block index).
	for mi, msg := range req.Messages {
		for _, view := range sdk.ToolResults(msg) {
			// Scalar seam: exactly one text arm, zero unknown arms, any
			// cache-marker arms. An unsupported shape declines the result
			// UNCHANGED before any cache/offload/metric/savings call.
			text, ok := sdk.ToolResultScalarText(view)
			if !ok {
				continue
			}
			if len(text) < minContentLength || sdk.IsDeterministicToolReplacement(text) {
				continue
			}

			toolName := view.ToolName
			if toolName == "" {
				toolName = toolNames[view.ToolCallId]
			}
			toolArgs := ""
			if call, ok := toolCalls[view.ToolCallId]; ok {
				toolArgs = string(call.Arguments)
			}
			if toolName == "" || sdk.ToolResultMustStayExact(toolName, text) {
				continue
			}
			rule, matched := sdk.MatchToolPolicy(toolPolicies, toolName)
			if !matched || rule.Mode == "" || rule.Mode == "exact" {
				continue
			}

			// Eligibility observability: fired ONCE, after the general size and
			// safety filters and a matched non-empty non-exact policy, BEFORE the
			// mode-specific consumption gates — the same definition as compactor,
			// so the metric means the same regardless of which compactor is
			// installed. Deterministic, source, and keyword candidates all emit.
			sdk.EmitMetric("torana_compact_eligible_total", sdk.MetricCounter, 1, map[string]string{"tool": toolName})

			switch rule.Mode {
			case "deterministic":
				if assistantAfter[mi] == 0 && !rule.FirstPass {
					continue
				}
				applied, err := applyDeterministicPolicy(msg, view.Block, text, toolName, toolArgs, rule)
				if err != nil {
					return false, err
				}
				if applied {
					modified = true
				}
				continue
			case "source":
				// Fail closed to exact. Live OMP dogfood showed that replacing
				// aged source reads makes autonomous agents reread different
				// ranges of the same file until they hit their request limit.
				// Source mode stays disabled until the economically gated
				// experiment in #178 ships.
				continue
			case "keyword":
				if assistantAfter[mi] == 0 {
					continue
				}
			default:
				continue
			}

			// Retrieve cached intent for this tool call (written by the intent
			// plugin). NOT_FOUND and present-empty are both unusable (skip +
			// metric); any other refusal or malformed reply is a contract defect
			// — error the hook.
			intent, herr, err := sdk.CacheGet(intentCacheKey + ":" + view.ToolCallId)
			if err != nil {
				return false, fmt.Errorf("keyword_compactor: cache_get %s:%s: %w", intentCacheKey, view.ToolCallId, err)
			}
			if herr != nil && !sdk.IsNotFound(herr) {
				return false, fmt.Errorf("keyword_compactor: cache_get %s:%s refused: %s", intentCacheKey, view.ToolCallId, herr.Message)
			}
			if herr != nil || intent == "" {
				sdk.EmitMetric("torana_intent_missing_total", sdk.MetricCounter, 1, map[string]string{"tool": toolName})
				continue
			}

			keywordKey := sdk.ContentAddressedCacheKey(keywordCompactionCache,
				"v2", toolName, toolArgs, text, intent, "keyword")
			cached, herr, err := sdk.CacheGet(keywordKey)
			if err != nil {
				return false, fmt.Errorf("keyword_compactor: cache_get keyword key: %w", err)
			}
			if herr != nil && !sdk.IsNotFound(herr) {
				return false, fmt.Errorf("keyword_compactor: cache_get keyword key refused: %s", herr.Message)
			}
			// Reuse only a non-empty value that is SHORTER than the original.
			// Missing, present-empty, and non-shorter values are unusable and
			// recomputed locally — the value is a pure function of the inputs, so
			// a stale or corrupt entry must never be applied or cached forever.
			if herr == nil && cached != "" && len(cached) < len(text) {
				recordSavings(len(text), len(cached), "cache_reuse")
				if _, err := sdk.ReplaceToolResultText(msg, view.Block, cached); err != nil {
					return false, fmt.Errorf("keyword_compactor: apply cached replacement: %w", err)
				}
				modified = true
				continue
			}

			compacted := compactDeterministic(text, intent)
			if compacted == text {
				continue
			}
			// Apply only a genuine >50% reduction, without rounding ambiguity:
			// final bytes must be strictly fewer than the removed bytes.
			if !worthwhileReduction(len(text), len(compacted)) {
				continue
			}

			recordSavings(len(text), len(compacted), "transformation")
			if _, err := sdk.ReplaceToolResultText(msg, view.Block, compacted); err != nil {
				return false, fmt.Errorf("keyword_compactor: apply replacement: %w", err)
			}
			modified = true
			// Best-effort: the replacement is already applied in memory; a
			// refused write cannot corrupt it, and the host logs the refusal.
			_, _ = sdk.CacheSet(keywordKey, compacted)
		}
	}
	return modified, nil
}

// worthwhileReduction reports whether final is a reduction of more than 50%:
// final bytes strictly fewer than the removed bytes. No rounding ambiguity:
// 2000 -> 1000 is NOT worthwhile (1000 is not < 1000); 2001 -> 1000 is.
func worthwhileReduction(original, final int) bool {
	return final < original-final
}

func assistantMessageCountsAfter(messages []*pbv2.Message) []int {
	counts := make([]int, len(messages))
	count := 0
	for i := len(messages) - 1; i >= 0; i-- {
		counts[i] = count
		if messages[i].Role == "assistant" {
			count++
		}
	}
	return counts
}

// applyDeterministicPolicy applies the shared deterministic replacement
// contract. The cached value is trusted only when it is non-empty AND shorter
// than the original; missing, present-empty, or non-shorter values are
// recomputed locally (the replacement is a pure function of the inputs).
func applyDeterministicPolicy(msg *pbv2.Message, block int, text, toolName, toolArgs string, rule sdk.ToolPolicyRule) (bool, error) {
	cacheKey := sdk.ContentAddressedCacheKey(policyCompactionCache,
		"v2", toolName, toolArgs, text, rule.Mode, rule.Rerun)
	cached, herr, err := sdk.CacheGet(cacheKey)
	if err != nil {
		return false, fmt.Errorf("keyword_compactor: policy cache_get: %w", err)
	}
	if herr != nil && !sdk.IsNotFound(herr) {
		return false, fmt.Errorf("keyword_compactor: policy cache_get refused: %s", herr.Message)
	}
	if herr == nil && cached != "" && len(cached) < len(text) {
		recordSavings(len(text), len(cached), "cache_reuse")
		if _, err := sdk.ReplaceToolResultText(msg, block, cached); err != nil {
			return false, fmt.Errorf("keyword_compactor: apply policy replacement: %w", err)
		}
		return true, nil
	}
	replacement := sdk.DeterministicToolReplacement(toolName, toolArgs, text, rule.Mode, rule.Rerun, false)
	if len(replacement) >= len(text) {
		return false, nil
	}
	recordSavings(len(text), len(replacement), "transformation")
	if _, err := sdk.ReplaceToolResultText(msg, block, replacement); err != nil {
		return false, fmt.Errorf("keyword_compactor: apply policy replacement: %w", err)
	}
	_, _ = sdk.CacheSet(cacheKey, replacement)
	return true, nil
}

// recordSavings reports compaction byte savings to /stats via the host.
// Best-effort by contract: it runs after the mutation and must never change
// the applied replacement.
func recordSavings(originalBytes, finalBytes int, source string) {
	payload, _ := json.Marshal(map[string]any{
		"original_bytes": originalBytes,
		"final_bytes":    finalBytes,
		"source":         source,
	})
	_, _, _ = sdk.HostCallExtension("torana_record_savings", payload)
}

// compactDeterministic extracts lines matching intent keywords, keeping the
// matched lines with a small context window in ORIGINAL order. Falls back to
// head+tail truncation when the selected output exceeds maxResultBytes —
// truncating the SELECTED output, never the original, so keyword evidence
// survives the cap.
func compactDeterministic(content, intent string) string {
	keywords := extractKeywords(intent)
	if len(keywords) == 0 {
		return content
	}

	lines := strings.Split(content, "\n")
	if len(lines) <= 50 {
		return content
	}

	keep := selectKeywordLines(lines, keywords)
	if len(keep) == 0 {
		return content
	}

	var result []string
	for i, line := range lines {
		if keep[i] {
			result = append(result, line)
		}
	}

	joined := strings.Join(result, "\n")
	if len(joined) > maxResultBytes {
		return truncateHeadTail(joined, maxResultBytes)
	}
	return joined
}

// selectKeywordLines ranks lines by keyword score (descending) then source
// index (ascending — ties are explicit, no reliance on sort stability) and
// returns at most maxKeepLines UNIQUE kept indices, each with its context
// window. Pure: identical inputs produce identical output, which is the
// prompt-cache guarantee when the cap binds.
func selectKeywordLines(lines []string, keywords []string) map[int]bool {
	type scored struct {
		idx   int
		score int
	}
	var scoredLines []scored
	for i, line := range lines {
		s := 0
		lower := strings.ToLower(line)
		for _, kw := range keywords {
			if strings.Contains(lower, kw) {
				s++
			}
		}
		if s > 0 {
			scoredLines = append(scoredLines, scored{i, s})
		}
	}
	if len(scoredLines) == 0 {
		return nil
	}

	sort.Slice(scoredLines, func(a, b int) bool {
		if scoredLines[a].score != scoredLines[b].score {
			return scoredLines[a].score > scoredLines[b].score
		}
		return scoredLines[a].idx < scoredLines[b].idx
	})

	keep := make(map[int]bool)
	// Phase 1: the ranked evidence lines themselves have priority over their
	// context — the cap can never spend a slot on a noise line while the
	// match that justified it is absent.
	for _, sl := range scoredLines {
		if len(keep) >= maxKeepLines {
			break
		}
		keep[sl.idx] = true
	}
	// Phase 2: surrounding context, deterministically in rank order, while
	// capacity remains.
	for _, sl := range scoredLines {
		if len(keep) >= maxKeepLines {
			break
		}
		start := sl.idx - contextLines
		if start < 0 {
			start = 0
		}
		end := sl.idx + contextLines + 1
		if end > len(lines) {
			end = len(lines)
		}
		for j := start; j < end; j++ {
			if len(keep) >= maxKeepLines {
				break
			}
			keep[j] = true
		}
	}
	if len(keep) == 0 {
		return nil
	}
	return keep
}

// extractKeywords pulls meaningful words from an intent string,
// filtering out stop words and short tokens.
func extractKeywords(intent string) []string {
	stopWords := map[string]bool{
		"the": true, "a": true, "an": true, "is": true, "are": true,
		"in": true, "of": true, "to": true, "for": true, "and": true,
		"or": true, "that": true, "this": true, "be": true, "it": true,
		"what": true, "find": true, "extract": true, "look": true,
		"from": true, "with": true, "specify": true, "explicitly": true,
		"critical": true, "specifically": true, "information": true,
		"output": true, "tool": true, "result": true, "need": true,
	}

	words := strings.Fields(strings.ToLower(intent))
	var kw []string
	for _, w := range words {
		w = strings.Trim(w, ".,;:!?\"'()[]{}")
		if len(w) < 3 {
			continue
		}
		if stopWords[w] {
			continue
		}
		kw = append(kw, w)
	}
	return kw
}

// truncationNotice is the marker inserted between the kept halves. It counts
// against the budget: a "truncation" that returns more bytes than it was
// allowed is not one. The removed-byte count must be exact for the FINAL
// output.
func truncationNotice(removed int) string {
	return "\n\n... [" + strconv.Itoa(removed) + " bytes truncated by Torana] ...\n\n"
}

// truncateHeadTail caps content at n bytes TOTAL — notice included — keeping
// roughly the first and last half of what remains.
//
// The cap is honest for every input:
//   - n <= 0 -> empty;
//   - content within n -> unchanged;
//   - the exact notice cannot fit (0 < n < notice length) -> a rune-safe raw
//     prefix within n, with no fabricated or partial notice;
//   - otherwise head + exact notice + tail, always valid UTF-8, never larger
//     than n or the input, notice's removed-byte count exact.
//
// The notice length is reserved against len(content) — the widest the removed
// count can be — so the reservation can only over-reserve, never under, and
// the rendered notice (fewer digits) always fits the reserved space.
func truncateHeadTail(content string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(content) <= n {
		return content
	}

	noticeLen := len(truncationNotice(len(content)))

	// The exact notice cannot fit strictly (0 < n < noticeLen): a rune-safe
	// raw prefix within n, with no fabricated or partial notice. At EXACT
	// equality the notice fits perfectly and the notice-bearing path is used
	// (head and tail halves are empty, the notice states the full removal).
	if noticeLen > n {
		return truncHead(content, n)
	}

	budget := n - noticeLen

	head := truncHead(content, budget/2)
	// The remainder rather than budget/2: backing off to a rune boundary can
	// shorten the head, and the tail should use what that frees.
	tail := truncTail(content, budget-len(head))

	return head + truncationNotice(len(content)-len(head)-len(tail)) + tail
}

// Torana truncates tool output by byte budget, but a byte index can land in
// the middle of a UTF-8 rune. The resulting string is invalid UTF-8, and the
// SDK marshals it into a proto3 string field — which enforces UTF-8 and
// panics, trapping the plugin for the entire request. Any non-ASCII tool
// output was enough to trigger it.
//
// These back the cut off to the nearest rune boundary, so the result is always
// within budget and always valid UTF-8.

// truncHead returns the longest prefix of s that is at most n bytes and does
// not split a rune.
func truncHead(s string, n int) string {
	// A negative budget would reach s[:n] and panic, and a panic inside a
	// plugin traps the whole request. No caller passes one today; the helper
	// is copied into three modules and states no precondition.
	if n <= 0 {
		return ""
	}
	if n >= len(s) {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// truncTail returns the longest suffix of s that is at most n bytes and does
// not split a rune.
func truncTail(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if n >= len(s) {
		return s
	}
	i := len(s) - n
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return s[i:]
}
