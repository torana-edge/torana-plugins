package main

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	"github.com/torana-edge/torana-plugin-sdk/pb"
	"unicode/utf8"
)

func main() {}

const (
	minContentLength = 2000     // below this, content is already small enough
	contextLines     = 2        // lines of context around keyword matches
	maxKeepLines     = 200      // cap to prevent bloat
	maxResultBytes   = 8000     // cap result size
	intentCacheKey   = "intent" // cache key for intent (set by the intent plugin)
	// Namespaced by plugin. env.cache_get/set is a SHARED store — unlike
	// env.state_*, which the host keys by module name — so two plugins using the
	// same namespace string read and write each other's entries. compactor and
	// keyword_compactor both used "policy_compacted" with the same key format,
	// which is harmless only for as long as their deterministic-policy logic
	// stays byte-identical.
	policyCompactionCache  = "keyword_compactor/policy_compacted"
	keywordCompactionCache = "keyword_compacted"
)

var (
	cfgOnce      sync.Once
	toolPolicies []sdk.ToolPolicyRule
)

func loadConfig() {
	cfgOnce.Do(func() {
		var c struct {
			ToolPolicies []sdk.ToolPolicyRule `json:"tool_policies"`
		}
		if raw := sdk.PluginConfig(); raw != "" {
			_ = json.Unmarshal([]byte(raw), &c)
		}
		toolPolicies = c.ToolPolicies
	})
}

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		loadConfig()
		modified := false
		assistantAfter := assistantMessageCountsAfter(req.Messages)
		toolNames := sdk.ToolNamesByCallID(req.Messages)
		toolCalls := sdk.ToolCallsByID(req.Messages)

		for i, msg := range req.Messages {
			if msg.Role != "tool" || msg.ToolCallId == "" || len(msg.Content) < minContentLength || sdk.IsDeterministicToolReplacement(msg.Content) {
				continue
			}

			toolName := msg.ToolName
			if toolName == "" {
				toolName = toolNames[msg.ToolCallId]
			}
			toolArgs := ""
			if call := toolCalls[msg.ToolCallId]; call != nil {
				toolArgs = string(call.ArgumentsJson)
			}
			if toolName == "" || sdk.ToolResultMustStayExact(toolName, msg.Content) {
				continue
			}
			rule, matched := sdk.MatchToolPolicy(toolPolicies, toolName)
			if !matched || rule.Mode == "" || rule.Mode == "exact" {
				continue
			}

			switch rule.Mode {
			case "deterministic":
				if assistantAfter[i] == 0 && !rule.FirstPass {
					continue
				}
				if applyDeterministicPolicy(msg, toolName, toolArgs, rule, false) {
					modified = true
				}
				continue
			case "source":
				// Live OMP dogfood showed that replacing aged source reads makes
				// autonomous agents reread different ranges of the same file until
				// they hit their request limit. Source mode is therefore fail-closed
				// to exact until the economically gated experiment in #178 ships.
				continue
			case "keyword":
				if assistantAfter[i] == 0 {
					continue
				}
			default:
				continue
			}

			// Phase 0 observability: every result big enough to compact.
			sdk.EmitMetric("torana_compact_eligible_total", sdk.MetricCounter, 1, map[string]string{"tool": toolName})

			// Retrieve cached intent for this tool call (written by the
			// intent plugin).
			cacheKey := intentCacheKey + ":" + msg.ToolCallId
			intent, _ := sdk.HostCall("env.cache_get", cacheKey)
			if intent == "" {
				sdk.EmitMetric("torana_intent_missing_total", sdk.MetricCounter, 1, map[string]string{"tool": toolName})
				continue
			}
			keywordKey := sdk.ContentAddressedCacheKey(keywordCompactionCache,
				"v2", toolName, toolArgs, msg.Content, intent, "keyword")
			if cached, _ := sdk.HostCall("env.cache_get", keywordKey); cached != "" {
				if len(cached) < len(msg.Content) {
					recordSavings(len(msg.Content), len(cached), "cache_reuse")
					msg.Content = cached
					modified = true
				}
				continue
			}

			compacted := compactDeterministic(msg.Content, intent)
			if compacted == msg.Content {
				continue
			}

			// Only apply if we actually reduced the size meaningfully (>50%).
			if len(compacted) >= len(msg.Content)/2 {
				continue
			}

			// Report savings to /stats via the host.
			recordSavings(len(msg.Content), len(compacted), "transformation")
			msg.Content = compacted
			modified = true
			payload, _ := json.Marshal(map[string]string{"key": keywordKey, "value": compacted})
			_, _ = sdk.HostCall("env.cache_set", string(payload))
		}

		if !modified {
			return nil, nil
		}
		return req, nil
	})
}

func assistantMessageCountsAfter(messages []*pb.Message) []int {
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

func applyDeterministicPolicy(msg *pb.Message, toolName, toolArgs string, rule sdk.ToolPolicyRule, markerOnly bool) bool {
	cacheKey := sdk.ContentAddressedCacheKey(policyCompactionCache,
		"v2", toolName, toolArgs, msg.Content, rule.Mode, rule.Rerun)
	cached, _ := sdk.HostCall("env.cache_get", cacheKey)
	if cached != "" {
		recordSavings(len(msg.Content), len(cached), "cache_reuse")
		msg.Content = cached
		return true
	}
	replacement := sdk.DeterministicToolReplacement(toolName, toolArgs, msg.Content, rule.Mode, rule.Rerun, markerOnly)
	if len(replacement) >= len(msg.Content) {
		return false
	}
	recordSavings(len(msg.Content), len(replacement), "transformation")
	msg.Content = replacement
	payload, _ := json.Marshal(map[string]string{"key": cacheKey, "value": replacement})
	_, _ = sdk.HostCall("env.cache_set", string(payload))
	return true
}

func recordSavings(originalBytes, finalBytes int, source string) {
	_, _ = sdk.HostCall("torana_record_savings",
		`{"original_bytes":`+itoa(originalBytes)+`,"final_bytes":`+itoa(finalBytes)+`,"source":"`+source+`"}`)
}

// compactDeterministic extracts lines matching intent keywords.
// Falls back to head+tail truncation if no keywords match.
func compactDeterministic(content, intent string) string {
	keywords := extractKeywords(intent)
	if len(keywords) == 0 {
		return content
	}

	lines := strings.Split(content, "\n")
	if len(lines) <= 50 {
		return content
	}

	// Score each line by keyword matches.
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
		return content // no matches — let model offload handle it
	}

	// Sort by score descending.
	sort.Slice(scoredLines, func(a, b int) bool { return scoredLines[a].score > scoredLines[b].score })

	// Collect unique line indices with surrounding context.
	keep := make(map[int]bool)
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
			keep[j] = true
		}
	}

	// Build result in original line order.
	var result []string
	for i, line := range lines {
		if keep[i] {
			result = append(result, line)
		}
	}

	joined := strings.Join(result, "\n")
	if len(joined) > maxResultBytes {
		return truncateHeadTail(content, 2000)
	}
	return joined
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
// allowed is not one.
func truncationNotice(removed int) string {
	return "\n\n... [" + itoa(removed) + " bytes truncated by Torana] ...\n\n"
}

// truncateHeadTail caps content at n bytes TOTAL — notice included — keeping
// roughly the first and last half of what remains.
//
// The threshold used to disagree with itself three ways: the doc comment said
// "first and last N characters" (2n), the guard fired at n*2, and the body kept
// n/2+n/2 (n). Separately, the notice was appended AFTER the halves had already
// consumed the whole budget, so near the boundary the result was longer than
// the input it was shortening.
func truncateHeadTail(content string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(content) <= n {
		return content
	}

	// Reserve using the widest the count can be. The notice states how many
	// bytes were removed, and its own length changes that number — reserving
	// against len(content) breaks the circularity in one pass and can only
	// over-reserve, never under.
	budget := n - len(truncationNotice(len(content)))
	if budget < 0 {
		budget = 0
	}

	head := truncHead(content, budget/2)
	// The remainder rather than budget/2: backing off to a rune boundary can
	// shorten the head, and the tail should use what that frees.
	tail := truncTail(content, budget-len(head))

	return head + truncationNotice(len(content)-len(head)-len(tail)) + tail
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if neg {
		digits = append([]byte{'-'}, digits...)
	}
	return string(digits)
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
