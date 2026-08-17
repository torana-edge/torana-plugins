// The compactor shrinks large tool results by delegating extraction to a
// cheap model (torana_offload_completion), guided by the intent captured by
// the intent plugin: "given what the agent was trying to find out, keep only
// the relevant parts of this output". Compacted results are cached by
// tool_call_id, so later turns replaying the same result reuse the compact
// form for free. Cache identity includes the original content, tool arguments,
// intent, and policy version so reused call IDs cannot return stale summaries.
//
// Run it AFTER the intent plugin. It is an alternative to keyword_compactor
// (deterministic, local, no model call) — pick ONE per deployment; both
// consume the same intent cache.
//
// v2 semantics (typed host calls):
//   - cache reads distinguish absent (NOT_FOUND) from present-empty; a
//     present-empty value is UNUSABLE, never a usable value, and never a
//     reason to erase a tool result;
//   - any non-NOT_FOUND cache refusal or malformed reply is a
//     contract/configuration defect: the hook errors, so failure_mode
//     preserves the request and the host records the failure;
//   - extension refusals split by actionability: NOT_CONFIGURED/UNAVAILABLE
//     are advisory — skip without retrying in the same request; contract
//     refusals (INVALID_ARGUMENT/PERMISSION_DENIED/INTERNAL) error the hook;
//   - cache writes and savings reports stay best-effort: the replacement is
//     already applied to the in-memory request and the host logs refusals.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pbv2 "github.com/torana-edge/torana-plugin-sdk/pb/v2"
	"google.golang.org/protobuf/proto"
)

func main() {}

const (
	intentCacheKey  = "intent"
	compactionCache = "compacted"
	// Namespaced by plugin. env.cache_* is a SHARED store — unlike
	// env.state_*, which the host keys by module name — so two plugins using
	// the same namespace string read and write each other's entries.
	policyCompactionCache = "compactor/policy_compacted"
	minOffloadChars       = 2000
)

// maxOffloadInputBytes caps how many SOURCE bytes of a tool output are sent
// to the cheap summarizer. 0 (the default) means UNBOUNDED — the complete
// tool output is sent. A positive value is opt-in via
// plugins.config.compactor and retains head+tail within that many bytes (the
// truncation marker is ADDITIONAL framing, not part of the budget). Loaded
// once, lazily, from the plugin config.
var (
	cfgOnce              sync.Once
	maxOffloadInputBytes int
	toolPolicies         []sdk.ToolPolicyRule
	expectedApplications int64
)

// config is the plugin's decoded configuration.
type config struct {
	MaxOffloadInputBytes int                  `json:"max_offload_input_bytes"`
	ToolPolicies         []sdk.ToolPolicyRule `json:"tool_policies"`
	ExpectedApplications int64                `json:"expected_applications"`
}

// parseConfig is the pure config decoder; loadConfig installs its result into
// the process-global state exactly once. The host validates config against
// schema.json at write time, so an unmarshal failure here is unreachable in
// practice and falls back to defaults.
func parseConfig(raw string) config {
	var c config
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &c)
	}
	if c.MaxOffloadInputBytes < 0 {
		c.MaxOffloadInputBytes = 0
	}
	if c.ExpectedApplications < 0 {
		c.ExpectedApplications = 0
	}
	return c
}

func loadConfig() {
	cfgOnce.Do(func() {
		c := parseConfig(sdk.PluginConfig())
		maxOffloadInputBytes = c.MaxOffloadInputBytes
		toolPolicies = c.ToolPolicies
		expectedApplications = c.ExpectedApplications
	})
}

// resetConfigForTest restores every config global so a test row can install a
// fresh config. Production never calls it; the once-only loader is unchanged.
func resetConfigForTest() {
	cfgOnce = sync.Once{}
	maxOffloadInputBytes = 0
	toolPolicies = nil
	expectedApplications = 0
}

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pbv2.ChatRequest) (sdk.RequestResult, error) {
		modified, err := compactToolResults(ctx, req)
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

func compactToolResults(ctx context.Context, req *pbv2.ChatRequest) (bool, error) {
	loadConfig()
	modified := false
	var modelWorks []modelWork
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
			if len(text) < minOffloadChars || sdk.IsDeterministicToolReplacement(text) {
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

			// Phase 0 observability: every result big enough to compact.
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
				// Fail closed to exact. Live OMP dogfood showed that aged source
				// markers can trigger unbounded different-range rereads. Re-enable
				// only behind the economic/recovery experiment tracked by #178.
				continue
			case "model":
				// A model summary is never allowed before one exact consumption.
				if assistantAfter[mi] == 0 {
					continue
				}
			default:
				continue
			}

			// Get intent from cache (written by the intent plugin). NOT_FOUND and
			// present-empty are both unusable (skip + metric); any other refusal
			// or malformed reply is a contract defect — error the hook.
			intent, herr, err := sdk.SharedCacheGet(intentCacheKey + ":" + view.ToolCallId)
			if err != nil {
				return false, fmt.Errorf("compactor: cache_get %s:%s: %w", intentCacheKey, view.ToolCallId, err)
			}
			if herr != nil && !sdk.IsNotFound(herr) {
				return false, fmt.Errorf("compactor: cache_get %s:%s refused: %s", intentCacheKey, view.ToolCallId, herr.Message)
			}
			if herr != nil || intent == "" {
				// Eligible but no usable intent — the money left on the table.
				sdk.EmitMetric("torana_intent_missing_total", sdk.MetricCounter, 1, map[string]string{"tool": toolName})
				continue
			}

			// Include every semantic input in the cache identity. A harness may
			// reuse tool_call_ids, and intents can change across rehydrated rounds.
			modelCacheKey := sdk.ContentAddressedCacheKey(compactionCache,
				"v3", toolName, toolArgs, text, intent, "model")
			cached, herr, err := sdk.CacheGet(modelCacheKey)
			if err != nil {
				return false, fmt.Errorf("compactor: cache_get model key: %w", err)
			}
			if herr != nil && !sdk.IsNotFound(herr) {
				return false, fmt.Errorf("compactor: cache_get model key refused: %s", herr.Message)
			}
			// A hit whose value is non-empty AND shorter than the original is
			// reused without offload; a value >= the original leaves the result
			// untouched; a miss or present-empty value is recomputed.
			if (herr != nil || cached == "") || len(cached) < len(text) {
				modelWorks = append(modelWorks, modelWork{
					message: msg, index: mi, block: view.Block, text: text, intent: intent, cacheKey: modelCacheKey, cached: cached,
				})
			}
		}
	}
	applied, err := prepareAndApplyModelBatch(req, modelWorks)
	if err != nil {
		return false, err
	}
	return modified || applied, nil
}

type tokenUsage struct {
	Reported               bool  `json:"reported"`
	InputTokens            int64 `json:"input_tokens,omitempty"`
	OutputTokens           int64 `json:"output_tokens,omitempty"`
	CacheReadTokens        int64 `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens       int64 `json:"cache_write_tokens,omitempty"`
	InputIncludesCacheRead bool  `json:"input_includes_cache_read,omitempty"`
}

type modelWork struct {
	message  *pbv2.Message
	index    int    // message index (candidate identity + report tail)
	block    int    // tool-result block index inside message
	text     string // the scalar-compatible text
	intent   string
	cacheKey string
	cached   string
}

type modelCandidate struct {
	message       *pbv2.Message
	index         int
	block         int
	originalBytes int
	replacement   string
	source        string
	provider      string
	model         string
	usage         tokenUsage
	cacheKey      string
}

// offloadResponse is the domain body of a successful offload. Additive JSON
// fields (e.g. a legacy "status") are tolerated by the decoder and never
// consulted: refusals arrive ONLY in the framed error arm.
type offloadResponse struct {
	Completion string     `json:"completion"`
	Provider   string     `json:"provider"`
	Model      string     `json:"model"`
	Usage      tokenUsage `json:"usage"`
}

// prepareAndApplyModelBatch runs the economic gate (optimistic preflight for
// any uncached candidate, then the real post-offload report) and applies the
// approved batch. Returns (applied, error); a contract-defect refusal errors
// the hook, an advisory refusal declines the batch.
func prepareAndApplyModelBatch(req *pbv2.ChatRequest, works []modelWork) (bool, error) {
	if len(works) == 0 || expectedApplications <= 0 {
		return false, nil
	}

	// Do not incur offload cost unless even a zero-cost, best-case replacement
	// would be economical. Cached candidates use their known final size; an
	// uncached candidate optimistically assumes zero bytes.
	optimistic, hasUncached := optimisticModelCandidates(works)
	if hasUncached {
		preflight, ok := modelBatchReport(req, optimistic, false)
		if !ok {
			return false, nil
		}
		approve, err := evaluateModelReport(preflight)
		if err != nil {
			return false, err
		}
		if !approve {
			return false, nil
		}
	}

	var candidates []modelCandidate
	for _, work := range works {
		if work.cached != "" {
			candidates = append(candidates, modelCandidate{
				message: work.message, index: work.index, block: work.block, originalBytes: len(work.text), replacement: work.cached,
				source: "cache_reuse", cacheKey: work.cacheKey,
			})
			continue
		}
		ctxStr := extractConversationContext(req.Messages)
		payload, _ := json.Marshal(map[string]any{
			"system_prompt": "You are a tool output summarizer. Given a tool output and an extraction intent, return ONLY the relevant parts. Be concise. Do not add commentary.",
			"user_prompt": fmt.Sprintf("Intent: %s\n\nConversation context:\n%s\n\nTool output:\n%s\n\nExtract only the parts relevant to the intent.",
				work.intent, ctxStr, truncateForPrompt(work.text, maxOffloadInputBytes)),
		})
		result, herr, err := sdk.HostCallExtension("torana_offload_completion", payload)
		if err != nil {
			return false, fmt.Errorf("compactor: offload: %w", err)
		}
		if herr != nil {
			switch herr.Code {
			case pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED, pbv2.ErrorCode_ERROR_CODE_UNAVAILABLE:
				// Advisory: operator/transient. Skip this candidate; do NOT
				// retry in the same request (duplicate spend).
				continue
			default:
				return false, fmt.Errorf("compactor: offload refused: %s", herr.Message)
			}
		}
		var response offloadResponse
		if json.Unmarshal(result, &response) != nil || response.Completion == "" || len(response.Completion) >= len(work.text) {
			// Malformed or unusable domain body: skip, like a transient
			// failure — the host framed every refusal already.
			continue
		}
		candidates = append(candidates, modelCandidate{
			message: work.message, index: work.index, block: work.block, originalBytes: len(work.text), replacement: response.Completion,
			source: "transformation", provider: response.Provider, model: response.Model, usage: response.Usage, cacheKey: work.cacheKey,
		})
	}
	if len(candidates) == 0 {
		return false, nil
	}
	report, ok := modelBatchReport(req, candidates, true)
	if !ok {
		return false, nil
	}
	approve, err := evaluateModelReport(report)
	if err != nil {
		return false, err
	}
	if !approve {
		return false, nil
	}
	for _, candidate := range candidates {
		// Exact in-place replacement of the designated tool-result block's
		// single text arm (the scalar seam); a failure here is a contract
		// defect — surface it.
		if _, err := sdk.ReplaceToolResultText(candidate.message, candidate.block, candidate.replacement); err != nil {
			return false, fmt.Errorf("compactor: apply replacement: %w", err)
		}
		if candidate.source == "transformation" {
			// Best-effort: the replacement is already applied in memory; a
			// refused write cannot corrupt it, and the host logs the refusal.
			_, _ = sdk.CacheSet(candidate.cacheKey, candidate.replacement)
		}
	}
	payload, _ := json.Marshal(report)
	// Best-effort observability: the batch is applied; rolling back after
	// cache/report side effects would be worse. The host logs refusals.
	_, _, _ = sdk.HostCallExtension("torana_record_savings", payload)
	return true, nil
}

func optimisticModelCandidates(works []modelWork) ([]modelCandidate, bool) {
	candidates := make([]modelCandidate, 0, len(works))
	hasUncached := false
	for _, work := range works {
		replacement := work.cached
		source := "cache_reuse"
		if replacement == "" {
			hasUncached = true
			// This candidate would create a new compact prefix. Even the
			// zero-byte optimistic preflight must charge that cache rewrite;
			// labeling it cache_reuse lets uneconomic summaries reach offload
			// before the exact post-offload gate rejects them.
			source = "transformation"
		}
		candidates = append(candidates, modelCandidate{
			message: work.message, index: work.index, block: work.block, originalBytes: len(work.text), replacement: replacement,
			source: source, cacheKey: work.cacheKey,
		})
	}
	return candidates, hasUncached
}

func modelBatchReport(req *pbv2.ChatRequest, candidates []modelCandidate, includeOffload bool) (map[string]any, bool) {
	if len(candidates) == 0 {
		return nil, false
	}
	earliest := len(req.Messages)
	originalBytes, finalBytes := 0, 0
	source := "cache_reuse"
	var provider, model string
	usage := tokenUsage{Reported: true}
	hasTransformation := false
	for _, candidate := range candidates {
		if candidate.index < earliest {
			earliest = candidate.index
		}
		originalBytes += candidate.originalBytes
		finalBytes += len(candidate.replacement)
		if candidate.source != "transformation" {
			continue
		}
		hasTransformation = true
		source = "transformation"
		if provider == "" {
			provider, model = candidate.provider, candidate.model
		}
		if provider != candidate.provider || model != candidate.model {
			return nil, false
		}
		usage.Reported = usage.Reported && candidate.usage.Reported
		usage.InputTokens += candidate.usage.InputTokens
		usage.OutputTokens += candidate.usage.OutputTokens
		usage.CacheReadTokens += candidate.usage.CacheReadTokens
		usage.CacheWriteTokens += candidate.usage.CacheWriteTokens
		usage.InputIncludesCacheRead = usage.InputIncludesCacheRead || candidate.usage.InputIncludesCacheRead
	}

	tail := proto.Clone(req).(*pbv2.ChatRequest)
	tail.Messages = tail.Messages[earliest:]
	rewriteBytes := proto.Size(tail) - originalBytes + finalBytes
	report := map[string]any{
		"original_bytes":                originalBytes,
		"final_bytes":                   finalBytes,
		"estimated_tokens_removed":      estimateTokens(originalBytes - finalBytes),
		"estimated_rewrite_span_tokens": estimateTokens(rewriteBytes),
		"estimator":                     "protobuf_tail_bytes_adjusted_div_4_ceil",
		"candidate_count":               len(candidates),
		"expected_applications":         expectedApplications,
		"source":                        source,
	}
	if includeOffload && hasTransformation {
		if provider == "" || model == "" || !usage.Reported {
			return nil, false
		}
		report["offload"] = map[string]any{"provider": provider, "model": model, "usage": usage}
	}
	return report, true
}

// evaluateModelReport asks the host's economic gate. Advisory refusals
// (NOT_CONFIGURED/UNAVAILABLE) decline the batch without error; contract
// refusals error the hook.
func evaluateModelReport(report map[string]any) (bool, error) {
	payload, _ := json.Marshal(report)
	result, herr, err := sdk.HostCallExtension("torana_evaluate_compaction", payload)
	if err != nil {
		return false, fmt.Errorf("compactor: evaluate_compaction: %w", err)
	}
	if herr != nil {
		switch herr.Code {
		case pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED, pbv2.ErrorCode_ERROR_CODE_UNAVAILABLE:
			return false, nil
		default:
			return false, fmt.Errorf("compactor: evaluate_compaction refused: %s", herr.Message)
		}
	}
	var decision struct {
		Apply bool `json:"apply"`
	}
	if json.Unmarshal(result, &decision) != nil || !decision.Apply {
		return false, nil
	}
	return true, nil
}

func estimateTokens(bytes int) int {
	if bytes <= 0 {
		return 0
	}
	return (bytes + 3) / 4
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

func applyDeterministicPolicy(msg *pbv2.Message, block int, text, toolName, toolArgs string, rule sdk.ToolPolicyRule) (bool, error) {
	cacheKey := sdk.ContentAddressedCacheKey(policyCompactionCache,
		"v2", toolName, toolArgs, text, rule.Mode, rule.Rerun)
	cached, herr, err := sdk.CacheGet(cacheKey)
	if err != nil {
		return false, fmt.Errorf("compactor: policy cache_get: %w", err)
	}
	if herr != nil && !sdk.IsNotFound(herr) {
		return false, fmt.Errorf("compactor: policy cache_get refused: %s", herr.Message)
	}
	// Trust the cached value only when it is non-empty AND shorter than the
	// original. Missing, present-empty, and NON-SHORTER values are unusable
	// and recomputed locally — the replacement is a pure function of the
	// inputs, so a corrupt or stale entry must never be applied (applying a
	// non-shorter value would expand the request) or cached forever.
	if herr == nil && cached != "" && len(cached) < len(text) {
		recordSavings(len(text), len(cached), "cache_reuse")
		if _, err := sdk.ReplaceToolResultText(msg, block, cached); err != nil {
			return false, fmt.Errorf("compactor: apply policy replacement: %w", err)
		}
		return true, nil
	}
	replacement := sdk.DeterministicToolReplacement(toolName, toolArgs, text, rule.Mode, rule.Rerun, false)
	if len(replacement) >= len(text) {
		return false, nil
	}
	recordSavings(len(text), len(replacement), "transformation")
	if _, err := sdk.ReplaceToolResultText(msg, block, replacement); err != nil {
		return false, fmt.Errorf("compactor: apply policy replacement: %w", err)
	}
	// Best-effort: the replacement is already applied; a refused write cannot
	// corrupt it, and the host logs the refusal.
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

func extractConversationContext(msgs []*pbv2.Message) string {
	// Ordered port: the context is the last FIVE NON-EMPTY qualifying
	// user/assistant messages, each capped at 500 SOURCE BYTES with the
	// existing rune-safe boundary (truncHead). Text comes from the SDK's
	// plain concatenation of the top-level text arms (sdk.Text); nested
	// tool-result content is excluded by construction, so surrounding
	// top-level text and sibling results in a mixed message survive — no
	// block-level exclusion is needed.
	var parts []string
	collected := 0
	for i := len(msgs) - 1; i >= 0 && collected < 5; i-- {
		msg := msgs[i]
		content := ""
		switch msg.Role {
		case "user":
			content = sdk.Text(msg)
		case "assistant":
			content = sdk.Text(msg)
		default:
			continue
		}
		if content != "" {
			if len(content) > 500 {
				content = truncHead(content, 500) + "..."
			}
			parts = append([]string{msg.Role + ": " + content}, parts...)
			collected++
		}
	}
	if len(parts) == 0 {
		return "no prior conversation context available"
	}
	return strings.Join(parts, "\n")
}

// truncateForPrompt bounds the tool output sent to the summarizer.
// maxBytes <= 0 means unbounded: the complete output is sent (the default).
// A positive cap is the maximum number of SOURCE bytes retained — the head
// and tail (first + last maxBytes/2, rune-safe) where signal tends to
// cluster; the fixed truncation marker is ADDITIONAL framing and does not
// count against the budget.
func truncateForPrompt(content string, maxBytes int) string {
	if maxBytes <= 0 || len(content) <= maxBytes {
		return content
	}
	half := maxBytes / 2
	return truncHead(content, half) + "\n\n... [truncated] ...\n\n" + truncTail(content, half)
}

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
