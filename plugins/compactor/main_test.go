package main

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
	"google.golang.org/protobuf/proto"
)

// ==========================================================================
// Shared fixtures
// ==========================================================================

// newHarness resets the process-global config once-state so every row starts
// from defaults, then builds a fresh fake host. Tests never run in parallel:
// the plugin's config globals are process-wide.
func newHarness(t *testing.T) *sdktest.Harness {
	t.Helper()
	resetConfigForTest()
	return sdktest.New(t)
}

// toolMsg builds an ordered tool-role message carrying ONE tool-result block
// with a single text arm (the scalar-compatible shape the plugins act on).
func toolMsg(id, name, content string) *pbv1.Message {
	return &pbv1.Message{Role: "tool", Blocks: []*pbv1.RequestBlock{{Kind: &pbv1.RequestBlock_ToolResult{ToolResult: &pbv1.RequestToolResultBlock{
		ToolCallId: id,
		ToolName:   name,
		Content:    []*pbv1.ToolResultContentBlock{{Kind: &pbv1.ToolResultContentBlock_Text{Text: &pbv1.ToolResultTextBlock{Text: content}}}},
	}}}}}
}

func requestTextBlock(text string) *pbv1.RequestBlock {
	return &pbv1.RequestBlock{Kind: &pbv1.RequestBlock_Text{Text: &pbv1.RequestTextBlock{Text: text}}}
}

func toolText(t *testing.T, req *pbv1.ChatRequest, mi int) string {
	t.Helper()
	for _, b := range req.Messages[mi].Blocks {
		if tr := b.GetToolResult(); tr != nil {
			for _, c := range tr.Content {
				if c.GetText() != nil {
					return c.GetText().Text
				}
			}
		}
	}
	t.Fatalf("no tool-result text in message %d", mi)
	return ""
}

// bigToolRequest builds the cache-compliance shape the real host exercises:
// a large tool result with a prior assistant turn (satisfying the model-mode
// consumption gate) and a replayed tool call for name/args lookup.
func bigToolRequest(content string) *pbv1.ChatRequest {
	return &pbv1.ChatRequest{
		Messages: []*pbv1.Message{
			{Role: "system", Blocks: []*pbv1.RequestBlock{{Kind: &pbv1.RequestBlock_Text{Text: &pbv1.RequestTextBlock{Text: "You are a coding agent."}}}}},
			{Role: "user", Blocks: []*pbv1.RequestBlock{{Kind: &pbv1.RequestBlock_Text{Text: &pbv1.RequestTextBlock{Text: "find the bug"}}}}},
			{Role: "assistant", Blocks: []*pbv1.RequestBlock{{Kind: &pbv1.RequestBlock_ToolUse{ToolUse: &pbv1.RequestToolUseBlock{Id: "call_1", Name: "read", ArgumentsJson: []byte(`{"path":"server.go"}`)}}}}},
			toolMsg("call_1", "read", content),
			{Role: "user", Blocks: []*pbv1.RequestBlock{{Kind: &pbv1.RequestBlock_Text{Text: &pbv1.RequestTextBlock{Text: "now fix it"}}}}},
			// One exact consumption after the result: the model-mode gate
			// requires it (a model summary is never allowed before the result
			// has been consumed once).
			{Role: "assistant", Blocks: []*pbv1.RequestBlock{{Kind: &pbv1.RequestBlock_Text{Text: &pbv1.RequestTextBlock{Text: "the fix is in server.go"}}}}},
		},
	}
}

func bigContent() string {
	return strings.Repeat("line of tool output that is long enough to be compaction-eligible\n", 200)
}

// modelConfig enables the model path with an economic gate that must approve.
const modelConfig = `{"tool_policies":[{"match":"read*","mode":"model"}],"expected_applications":6}`

func modelStub(completion string) func(*pbv1.ModelCompleteArgs) (*pbv1.ModelCompleteResult, *pbv1.HostError, error) {
	return func(*pbv1.ModelCompleteArgs) (*pbv1.ModelCompleteResult, *pbv1.HostError, error) {
		return &pbv1.ModelCompleteResult{Content: completion, ReportedModel: "small-model", Usage: &pbv1.Usage{InputTokens: 100, OutputTokens: 50}}, nil, nil
	}
}

func applyStub(apply bool) func(args string) (string, error) {
	return func(args string) (string, error) {
		return sdktest.HostResultValue([]byte(`{"apply":` + map[bool]string{true: "true", false: "false"}[apply] + `}`)), nil
	}
}

func countCommand(h *sdktest.Harness, cmd string) int {
	n := 0
	for _, c := range h.Calls() {
		if c.Command == cmd {
			n++
		}
	}
	return n
}

func hasMetric(h *sdktest.Harness, name string) bool {
	return metricValue(h, name) != 0
}

func metricValue(h *sdktest.Harness, name string) float64 {
	var value float64
	for _, metric := range h.Metrics() {
		if metric.Name == name {
			value += metric.Value
		}
	}
	return value
}

// ==========================================================================
// Pure helpers (ported; truncation and report math are unchanged behavior)
// ==========================================================================

// TestTruncateForPromptUnboundedByDefault: with no configured limit (maxChars
// <= 0) the complete tool output is sent to the summarizer — no silent
// middle-dropping. Regression guard for the removed hardcoded 14000 cap.
func TestTruncateForPromptUnboundedByDefault(t *testing.T) {
	big := make([]byte, 100_000)
	for i := range big {
		big[i] = 'x'
	}
	content := string(big)

	if got := truncateForPrompt(content, 0); got != content {
		t.Fatalf("maxChars=0 must pass content through unchanged; got %d chars, want %d", len(got), len(content))
	}
	if got := truncateForPrompt(content, -5); got != content {
		t.Fatalf("negative maxChars must be unbounded; got %d chars", len(got))
	}
}

// TestTruncateForPromptBoundedWhenConfigured: a positive cap keeps head+tail
// and drops the middle; the retained SOURCE bytes stay within the budget and
// the marker is additional framing (the emitted string may exceed the budget
// by exactly the marker's length).
func TestTruncateForPromptBoundedWhenConfigured(t *testing.T) {
	content := ""
	for i := 0; i < 1000; i++ {
		content += "abcdefghij" // 10k chars
	}
	const budget = 100
	out := truncateForPrompt(content, budget)
	if len(out) >= len(content) {
		t.Fatalf("expected truncation below %d, got %d", len(content), len(out))
	}
	if !containsMarker(out) {
		t.Fatalf("truncated output missing head/tail marker: %q", out[:min(80, len(out))])
	}
	// The SOURCE bytes retained (everything but the framing separator) must
	// fit the budget; the framing is on top.
	if retained := len(out) - len(framing); retained > budget {
		t.Fatalf("retained source bytes=%d exceed the %d-byte budget", retained, budget)
	}
	// Short content under the cap is returned intact.
	if got := truncateForPrompt("small", 100); got != "small" {
		t.Fatalf("content under cap must be intact, got %q", got)
	}
}

// TestTruncateForPromptMultibyteRuneSafety: byte budgets can land mid-rune;
// the cut must back off to a boundary so the result is valid UTF-8 and still
// within the source-byte budget.
func TestTruncateForPromptMultibyteRuneSafety(t *testing.T) {
	content := strings.Repeat("日本語テキスト", 400) // 6 bytes per rune group
	out := truncateForPrompt(content, 101)
	if !utf8.ValidString(out) {
		t.Fatal("truncation split a rune — output is not valid UTF-8")
	}
	if retained := len(out) - len(framing); retained > 101 {
		t.Fatalf("retained source bytes=%d exceed the 101-byte budget", retained)
	}
	if len(out) >= len(content) {
		t.Fatal("multibyte content was not truncated")
	}
}

// TestModelBatchReportUsesAdjustedTailOnce re-pins the economics math against
// the ordered wire: proto.Size over a pbv1 ChatRequest. Measured 2026-08-04 on the
// ORDERED fixture (tool-role message with a tool-result block):
// rewrite span 5060 bytes -> 1270 estimated tokens.
func TestModelBatchReportUsesAdjustedTailOnce(t *testing.T) {
	original := strings.Repeat("x", 100_000)
	replacement := strings.Repeat("y", 5_000)
	result := toolMsg("large", "read", original)
	req := &pbv1.ChatRequest{Messages: []*pbv1.Message{
		{Role: "user", Blocks: []*pbv1.RequestBlock{{Kind: &pbv1.RequestBlock_Text{Text: &pbv1.RequestTextBlock{Text: "prefix outside the rewrite span"}}}}},
		result,
		{Role: "assistant", Blocks: []*pbv1.RequestBlock{{Kind: &pbv1.RequestBlock_Text{Text: &pbv1.RequestTextBlock{Text: "near-tail response"}}}}},
	}}
	oldExpected := expectedApplications
	expectedApplications = 5
	t.Cleanup(func() { expectedApplications = oldExpected })

	report, ok := modelBatchReport(req, []modelCandidate{{
		message: result, index: 1, originalBytes: len(original), replacement: replacement, source: "cache_reuse",
	}}, false)
	if !ok {
		t.Fatal("modelBatchReport rejected valid candidate")
	}
	if got := report["estimated_tokens_removed"].(int); got != 23_750 {
		t.Fatalf("removed token estimate=%d, want 23750", got)
	}
	// The rewritten tail is roughly the 5k replacement plus protobuf framing
	// and the final assistant message. Double-subtracting the original 100k
	// would collapse this to zero.
	rewrite := report["estimated_rewrite_span_tokens"].(int)
	if rewrite != 1_270 {
		t.Fatalf("rewrite span estimate=%d, want 1270 (measured from the ordered wire)", rewrite)
	}
}

func TestOptimisticPreflightChargesUncachedRewrite(t *testing.T) {
	uncached := toolMsg("new", "read", strings.Repeat("x", 10_000))
	cached := toolMsg("cached", "read", strings.Repeat("y", 10_000))
	candidates, hasUncached := optimisticModelCandidates([]modelWork{
		{message: uncached, index: 0},
		{message: cached, index: 1, cached: "summary"},
	})
	if !hasUncached {
		t.Fatal("uncached work was not detected")
	}
	if candidates[0].source != "transformation" {
		t.Fatalf("uncached optimistic candidate source=%q, want transformation", candidates[0].source)
	}
	if candidates[1].source != "cache_reuse" {
		t.Fatalf("cached optimistic candidate source=%q, want cache_reuse", candidates[1].source)
	}
}

// marker is the fixed truncation framing; it is ADDITIONAL to the source-byte
// budget. framing is the full emitted separator (the marker plus its newlines).
const (
	marker  = "... [truncated] ..."
	framing = "\n\n" + marker + "\n\n"
)

func containsMarker(s string) bool {
	for i := 0; i+len(marker) <= len(s); i++ {
		if s[i:i+len(marker)] == marker {
			return true
		}
	}
	return false
}

// ==========================================================================
// Hook-level matrix (sdktest; the plugin registers in init())
// ==========================================================================

// TestDefaultConfigIsInert — schema defaults (0/0/[]) must mean: no policy
// matches, model path disabled, unbounded summarizer input. Result: pass-through
// with no cache/extension traffic.
func TestDefaultConfigIsInert(t *testing.T) {
	h := newHarness(t)
	res := h.BeforeRequest(bigToolRequest(bigContent()))
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if !res.PassedThrough {
		t.Fatal("default config must pass the request through untouched")
	}
	for _, c := range h.Calls() {
		if c.Command != "env.plugin_config" {
			t.Errorf("default-config dispatch made an unexpected host call: %s", c.Command)
		}
	}
}

// TestDeterministicFirstPassAppliesAndCachesThenReuses — first_pass applies
// without a prior assistant turn, caches, and a SECOND dispatch over a fresh
// clone of the ORIGINAL request hits the cache (no CacheSet on turn 2,
// byte-identical output) — proving reuse, not recomputation.
func TestDeterministicFirstPassAppliesAndCachesThenReuses(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(`{"tool_policies":[{"match":"read*","mode":"deterministic","first_pass":true,"rerun":"Repeat the search to recover every result."}]}`)
	// The dispatch mutates the request in place (the plugin replaces the
	// message pointers it was handed), so the comparison needs a pristine
	// clone of the ORIGINAL.
	original := bigToolRequest(bigContent())
	req := proto.Clone(original).(*pbv1.ChatRequest)

	first := h.BeforeRequest(req)
	if first.Err != nil || first.Request == nil {
		t.Fatalf("expected a replacement on turn 1, err=%v", first.Err)
	}
	if toolText(t, first.Request, 3) == toolText(t, original, 3) {
		t.Fatal("turn 1 did not replace the tool result")
	}
	if countCommand(h, "env.cache_set") != 1 {
		t.Fatalf("turn 1 must cache the replacement, cache_set calls=%d", countCommand(h, "env.cache_set"))
	}

	// Turn 2: a FRESH CLONE of the original request on the SAME harness (the
	// cache store is per-harness). Reusing the mutated request would trip
	// IsDeterministicToolReplacement before cache lookup; a fresh harness
	// would lose the cache written by turn 1.
	before := countCommand(h, "env.cache_set")
	second := h.BeforeRequest(proto.Clone(original).(*pbv1.ChatRequest))
	if second.Err != nil || second.Request == nil {
		t.Fatalf("expected a replacement on turn 2, err=%v", second.Err)
	}
	if toolText(t, first.Request, 3) != toolText(t, second.Request, 3) {
		t.Fatal("turn 2 output differs from turn 1 — not a pure function of the input")
	}
	if countCommand(h, "env.cache_set") != before {
		t.Fatal("turn 2 wrote the cache — the replacement must have been REUSED, not recomputed")
	}
}

// TestDeterministicConsumptionGate — without first_pass, no replacement until
// an assistant message follows the tool result.
func TestDeterministicConsumptionGate(t *testing.T) {
	cfg := `{"tool_policies":[{"match":"read*","mode":"deterministic"}]}`
	h := newHarness(t)
	h.SetConfig(cfg)
	req := bigToolRequest(bigContent())
	req.Messages = req.Messages[:5] // drop the trailing assistant consumption
	res := h.BeforeRequest(req)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if !res.PassedThrough {
		t.Fatal("first_pass:false with no prior assistant turn must leave the result untouched")
	}

	// Same request WITH a trailing assistant message -> replaced.
	original := bigToolRequest(bigContent())
	h2 := newHarness(t)
	h2.SetConfig(cfg)
	res2 := h2.BeforeRequest(proto.Clone(original).(*pbv1.ChatRequest))
	if res2.Err != nil || res2.Request == nil {
		t.Fatalf("expected replacement after a consumption, err=%v", res2.Err)
	}
	if toolText(t, res2.Request, 3) == toolText(t, original, 3) {
		t.Fatal("deterministic policy did not replace the consumed result")
	}
}

func TestModelPathAppliesWithNamedService(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(modelConfig)
	h.SeedSharedCache("intent:call_1", "find the bug in server.go")
	var modelArgs *pbv1.ModelCompleteArgs
	h.StubModelComplete(func(args *pbv1.ModelCompleteArgs) (*pbv1.ModelCompleteResult, *pbv1.HostError, error) {
		modelArgs = args
		return modelStub("summary")(args)
	})
	h.StubHostCall("torana_evaluate_compaction", applyStub(true))

	res := h.BeforeRequest(bigToolRequest(bigContent()))
	if res.Err != nil || res.Request == nil {
		t.Fatalf("expected a replacement, err=%v", res.Err)
	}
	got := toolText(t, res.Request, 3)
	if got != "summary" {
		t.Fatalf("tool result not replaced: %q", got[:min(40, len(got))])
	}
	// record_savings was called once with the batch report.
	if countCommand(h, "torana_record_savings") != 1 {
		t.Fatalf("record_savings calls=%d, want 1", countCommand(h, "torana_record_savings"))
	}
	if modelArgs == nil || modelArgs.Service != "summarizer" || len(modelArgs.Messages) != 2 {
		t.Fatalf("named model request = %+v", modelArgs)
	}
	if !strings.Contains(modelArgs.Messages[1].Content, "find the bug in server.go") {
		t.Fatal("model request missing the intent")
	}
	if strings.Contains(modelArgs.Messages[1].Content, "[truncated]") {
		t.Fatal("default max_summarizer_input_bytes=0 must send the FULL output, not a truncated one")
	}
	if hasMetric(h, "torana_intent_missing_total") {
		t.Fatal("a captured intent must take precedence over the derived fallback")
	}
	// The transformation cache entry exists (best-effort write).
	cached := false
	for _, c := range h.Calls() {
		if c.Command == "env.cache_set" {
			cached = true
		}
	}
	if !cached {
		t.Fatal("the model replacement was not cached")
	}
}

// TestModelAdvisoryRefusalSkipsWithoutRetry — NOT_CONFIGURED/UNAVAILABLE are
// advisory: the candidate is skipped, the batch may still apply others, and
// the same call is never retried.
func TestModelAdvisoryRefusalSkipsWithoutRetry(t *testing.T) {
	for _, code := range []pbv1.ErrorCode{
		pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED,
		pbv1.ErrorCode_ERROR_CODE_UNAVAILABLE,
	} {
		t.Run(code.String(), func(t *testing.T) {
			h := newHarness(t)
			h.SetConfig(modelConfig)
			h.SeedSharedCache("intent:call_1", "find the bug")
			h.StubModelComplete(func(*pbv1.ModelCompleteArgs) (*pbv1.ModelCompleteResult, *pbv1.HostError, error) {
				return nil, &pbv1.HostError{Code: code, Message: "stub refusal"}, nil
			})
			h.StubHostCall("torana_evaluate_compaction", applyStub(true))
			res := h.BeforeRequest(bigToolRequest(bigContent()))
			if res.Err != nil {
				t.Fatalf("advisory refusal must not error the hook: %v", res.Err)
			}
			if !res.PassedThrough {
				t.Fatal("no candidate survived the advisory refusal; nothing may change")
			}
			if n := countCommand(h, "env.model_complete"); n != 1 {
				t.Fatalf("advisory refusal was retried: %d summarizer calls", n)
			}
		})
	}
}

// TestModelContractRefusalErrors — INVALID_ARGUMENT/PERMISSION_DENIED are
// contract defects: the hook errors so failure_mode applies.
func TestModelContractRefusalErrors(t *testing.T) {
	for _, code := range []pbv1.ErrorCode{
		pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT,
		pbv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED,
		pbv1.ErrorCode_ERROR_CODE_INTERNAL,
	} {
		t.Run(code.String(), func(t *testing.T) {
			h := newHarness(t)
			h.SetConfig(modelConfig)
			h.SeedSharedCache("intent:call_1", "find the bug")
			h.StubModelComplete(func(*pbv1.ModelCompleteArgs) (*pbv1.ModelCompleteResult, *pbv1.HostError, error) {
				return nil, &pbv1.HostError{Code: code, Message: "stub refusal"}, nil
			})
			h.StubHostCall("torana_evaluate_compaction", applyStub(true))
			res := h.BeforeRequest(bigToolRequest(bigContent()))
			if res.Err == nil {
				t.Fatalf("%s refusal must error the hook", code.String())
			}
		})
	}
}

// TestUnusableModelCompletionsSkip — empty or non-shorter completions are
// candidate-local declines, not malformed legacy envelopes.
func TestUnusableModelCompletionsSkip(t *testing.T) {
	for name, completion := range map[string]string{
		"empty completion": "",
		"not shorter":      strings.Repeat("x", 20_000),
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.SetConfig(modelConfig)
			h.SeedSharedCache("intent:call_1", "find the bug")
			h.StubModelComplete(modelStub(completion))
			h.StubHostCall("torana_evaluate_compaction", applyStub(true))
			res := h.BeforeRequest(bigToolRequest(bigContent()))
			if res.Err != nil {
				t.Fatalf("unusable body must skip, not error: %v", res.Err)
			}
			if !res.PassedThrough {
				t.Fatal("unusable body must not apply anything")
			}
		})
	}
}

// TestMissingUsageDeclinesEconomicApplication ensures the plugin never guesses
// model cost when the provider omitted usage.
func TestMissingUsageDeclinesEconomicApplication(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(modelConfig)
	h.SeedSharedCache("intent:call_1", "find the bug")
	h.StubModelComplete(func(*pbv1.ModelCompleteArgs) (*pbv1.ModelCompleteResult, *pbv1.HostError, error) {
		return &pbv1.ModelCompleteResult{Content: "summary"}, nil, nil
	})
	h.StubHostCall("torana_evaluate_compaction", applyStub(true))
	res := h.BeforeRequest(bigToolRequest(bigContent()))
	if res.Err != nil || !res.PassedThrough {
		t.Fatalf("missing usage must decline unchanged, err=%v", res.Err)
	}
}

// TestEconomicGateDeclinesBatch — evaluate {"apply":false} declines; the
// optimistic preflight declines BEFORE any summarizer call.
func TestEconomicGateDeclinesBatch(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(modelConfig)
	h.SeedSharedCache("intent:call_1", "find the bug")
	h.StubModelComplete(modelStub("summary"))
	h.StubHostCall("torana_evaluate_compaction", applyStub(false))
	res := h.BeforeRequest(bigToolRequest(bigContent()))
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if !res.PassedThrough {
		t.Fatal("a declined batch must not apply")
	}
	// Preflight declined -> no summarizer spend at all.
	if n := countCommand(h, "env.model_complete"); n != 0 {
		t.Fatalf("summarizer ran despite a declined preflight: %d calls", n)
	}
}

// TestUncachedBatchEvaluatesTwice — an uncached batch calls
// torana_evaluate_compaction TWICE (optimistic preflight, then the real
// post-summarizer report), both with candidate_count 2 for a two-candidate batch.
func TestUncachedBatchEvaluatesTwice(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(modelConfig)
	h.SeedSharedCache("intent:call_1", "find the bug")
	h.SeedSharedCache("intent:call_2", "second intent")
	h.StubModelComplete(modelStub("summary"))
	var counts []int
	h.StubHostCall("torana_evaluate_compaction", func(args string) (string, error) {
		var report struct {
			CandidateCount int `json:"candidate_count"`
		}
		_ = json.Unmarshal([]byte(args), &report)
		counts = append(counts, report.CandidateCount)
		return sdktest.HostResultValue([]byte(`{"apply":true}`)), nil
	})

	req := bigToolRequest(bigContent())
	req.Messages = append(req.Messages, toolMsg("call_2", "read", strings.Repeat("second big output\n", 200)))
	req.Messages = append(req.Messages, &pbv1.Message{Role: "assistant", Blocks: []*pbv1.RequestBlock{{Kind: &pbv1.RequestBlock_Text{Text: &pbv1.RequestTextBlock{Text: "x"}}}}})
	res := h.BeforeRequest(req)
	if res.Err != nil || res.Request == nil {
		t.Fatalf("expected the batch to apply, err=%v", res.Err)
	}
	if len(counts) != 2 {
		t.Fatalf("evaluate_compaction calls=%d, want 2 (preflight + real)", len(counts))
	}
	if counts[0] != 2 || counts[1] != 2 {
		t.Fatalf("candidate counts=%v, want [2 2]", counts)
	}
}

// TestAllCachedBatchEvaluatesOnce — no uncached work, no preflight: exactly
// one real evaluation.
func TestAllCachedBatchEvaluatesOnce(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(modelConfig)
	h.SeedSharedCache("intent:call_1", "find the bug")
	h.StubModelComplete(modelStub("must-not-run"))
	h.StubHostCall("torana_evaluate_compaction", applyStub(true))
	content := bigContent()
	modelKey := sdk.ContentAddressedCacheKey(compactionCache, "v4", "read", `{"path":"server.go"}`, content, "captured", "find the bug", "model")
	h.SeedCache(modelKey, "cached-summary")

	res := h.BeforeRequest(bigToolRequest(content))
	if res.Err != nil || res.Request == nil {
		t.Fatalf("expected cache reuse, err=%v", res.Err)
	}
	if toolText(t, res.Request, 3) != "cached-summary" {
		t.Fatalf("cached replacement not reused: %q", toolText(t, res.Request, 3))
	}
	if n := countCommand(h, "env.model_complete"); n != 0 {
		t.Fatalf("cached-shorter value must be reused WITHOUT summarizer: %d calls", n)
	}
	if n := countCommand(h, "torana_evaluate_compaction"); n != 1 {
		t.Fatalf("all-cached batch must evaluate exactly once, got %d", n)
	}
}

// TestCachedValueNotShorterLeavesUntouched — a cached value >= the original
// is not applied and not recomputed: the message stays byte-identical, with
// no summarizer and no evaluation (the hit is not even queued as work).
func TestCachedValueNotShorterLeavesUntouched(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(modelConfig)
	h.SeedSharedCache("intent:call_1", "find the bug")
	h.StubModelComplete(modelStub("must-not-run"))
	h.StubHostCall("torana_evaluate_compaction", applyStub(true))
	content := bigContent()
	modelKey := sdk.ContentAddressedCacheKey(compactionCache, "v4", "read", `{"path":"server.go"}`, content, "captured", "find the bug", "model")
	// Value exactly as long as the original: not shorter -> untouched.
	h.SeedCache(modelKey, content)

	res := h.BeforeRequest(bigToolRequest(content))
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if !res.PassedThrough {
		t.Fatal("a cached value >= the original must leave the message untouched")
	}
	if n := countCommand(h, "env.model_complete"); n != 0 {
		t.Fatalf("summarizer ran despite a non-shorter cache hit: %d calls", n)
	}
	if n := countCommand(h, "torana_evaluate_compaction"); n != 0 {
		t.Fatalf("evaluate ran despite a non-shorter cache hit: %d calls", n)
	}
}

// TestTwoCandidatesShareOneBoundService proves model coordinates are not
// guest-selected per candidate; both requests use the same logical slot.
func TestTwoCandidatesShareOneBoundService(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(modelConfig)
	h.SeedSharedCache("intent:call_1", "find the bug")
	h.SeedSharedCache("intent:call_2", "second")
	var calls int
	h.StubModelComplete(func(args *pbv1.ModelCompleteArgs) (*pbv1.ModelCompleteResult, *pbv1.HostError, error) {
		calls++
		if args.Service != "summarizer" {
			t.Fatalf("service = %q", args.Service)
		}
		return modelStub("summary")(args)
	})
	h.StubHostCall("torana_evaluate_compaction", applyStub(true))
	req := bigToolRequest(bigContent())
	req.Messages = append(req.Messages, toolMsg("call_2", "read", strings.Repeat("second big output\n", 200)))
	req.Messages = append(req.Messages, &pbv1.Message{Role: "assistant", Blocks: []*pbv1.RequestBlock{{Kind: &pbv1.RequestBlock_Text{Text: &pbv1.RequestTextBlock{Text: "x"}}}}})
	res := h.BeforeRequest(req)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if res.Request == nil || calls != 2 {
		t.Fatalf("two-candidate batch request=%v model calls=%d", res.Request != nil, calls)
	}
}

// TestIntentMissUsesBoundedFallback — NOT_FOUND and present-empty remain
// distinct host states but have the same policy outcome: emit the miss metric,
// derive the exact bounded local intent, and continue through the real
// economic/summarizer path.
func TestIntentMissUsesBoundedFallback(t *testing.T) {
	for _, name := range []string{"absent", "present-empty"} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.SetConfig(modelConfig)
			h.SeedSharedCache("intent:call_1", "") // empty value, present key
			if name == "absent" {
				// Truly absent: the key is removed again (SeedSharedCache
				// with the empty string stores PRESENCE; a harness with no
				// seed at all is the only way to get a real miss).
				h = newHarness(t)
				h.SetConfig(modelConfig)
			}
			var modelArgs *pbv1.ModelCompleteArgs
			h.StubModelComplete(func(args *pbv1.ModelCompleteArgs) (*pbv1.ModelCompleteResult, *pbv1.HostError, error) {
				modelArgs = args
				return modelStub("summary")(args)
			})
			h.StubHostCall("torana_evaluate_compaction", applyStub(true))
			res := h.BeforeRequest(bigToolRequest(bigContent()))
			if res.Err != nil || res.Request == nil {
				t.Fatalf("derived fallback did not apply: %v", res.Err)
			}
			if got := toolText(t, res.Request, 3); got != "summary" {
				t.Fatalf("replacement = %q, want summary", got)
			}
			if got := metricValue(h, "torana_intent_missing_total"); got != 1 {
				t.Fatalf("intent-missing metric = %v, want 1", got)
			}
			if got := metricValue(h, "torana_compact_eligible_total"); got != 1 {
				t.Fatalf("eligible metric = %v, want 1", got)
			}
			const wantIntent = `torana-derived-intent-v1:{"user_request":"find the bug","tool_name":"read","tool_arguments":"{\"path\":\"server.go\"}"}`
			if modelArgs == nil || len(modelArgs.Messages) != 2 || !strings.HasPrefix(modelArgs.Messages[1].Content, "Intent: "+wantIntent+"\n\n") {
				t.Fatalf("model prompt lacks exact derived intent %q: %+v", wantIntent, modelArgs)
			}
			wantCalls := map[string]int{
				"env.plugin_config":          1,
				"env.shared_cache_get":       1,
				"env.cache_get":              1,
				"env.model_complete":         1,
				"torana_evaluate_compaction": 2,
				"env.cache_set":              1,
				"torana_record_savings":      1,
			}
			gotCalls := map[string]int{}
			for _, call := range h.Calls() {
				gotCalls[call.Command]++
			}
			if !equalCallMultiset(gotCalls, wantCalls) {
				t.Fatalf("host-call multiset = %v, want %v", gotCalls, wantCalls)
			}
		})
	}
}

func equalCallMultiset(got, want map[string]int) bool {
	if len(got) != len(want) {
		return false
	}
	for command, count := range want {
		if got[command] != count {
			return false
		}
	}
	return true
}

func TestDerivedIntentBoundaryAndPrecedence(t *testing.T) {
	messages := []*pbv1.Message{
		{Role: "user", Blocks: []*pbv1.RequestBlock{requestTextBlock("old request")}},
		{Role: "assistant", Blocks: []*pbv1.RequestBlock{requestTextBlock("work")}},
		{Role: "user", Blocks: []*pbv1.RequestBlock{
			requestTextBlock(strings.Repeat("日", 300)),
			toolMsg("c", "read", "result").Blocks[0],
			requestTextBlock("same-message text after the result must not leak"),
		}},
		{Role: "user", Blocks: []*pbv1.RequestBlock{requestTextBlock("later request must not leak")}},
	}
	got := deriveCompactionIntent(messages, 2, 1, strings.Repeat("n", 600), strings.Repeat("a", 600))
	if !strings.HasPrefix(got, derivedIntentPrefix) || !utf8.ValidString(got) {
		t.Fatalf("derived intent prefix/UTF-8 = %q", got)
	}
	var payload derivedIntentPayload
	if err := json.Unmarshal([]byte(strings.TrimPrefix(got, derivedIntentPrefix)), &payload); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(payload.UserRequest, "after the result") || strings.Contains(payload.UserRequest, "later") || len(payload.UserRequest) > maxDerivedIntentBytes ||
		len(payload.ToolName) > maxDerivedIntentBytes || len(payload.ToolArguments) > maxDerivedIntentBytes {
		t.Fatalf("derived payload = %#v", payload)
	}
	if payload.UserRequest == "" || !utf8.ValidString(payload.UserRequest) {
		t.Fatalf("rune-safe user request = %q", payload.UserRequest)
	}
}

func TestDerivedIntentCacheIdentity(t *testing.T) {
	const derived = `torana-derived-intent-v1:{"user_request":"find the bug","tool_name":"read","tool_arguments":"{\"path\":\"server.go\"}"}`
	content := bigContent()
	derivedKey := sdk.ContentAddressedCacheKey(compactionCache,
		"v4", "read", `{"path":"server.go"}`, content, "derived", derived, "model")

	t.Run("derived cache hit", func(t *testing.T) {
		h := newHarness(t)
		h.SetConfig(modelConfig)
		h.SeedCache(derivedKey, "cached-derived")
		h.StubModelComplete(modelStub("must-not-run"))
		h.StubHostCall("torana_evaluate_compaction", applyStub(true))
		res := h.BeforeRequest(bigToolRequest(content))
		if res.Err != nil || res.Request == nil {
			t.Fatalf("derived cache hit failed: %v", res.Err)
		}
		if got := toolText(t, res.Request, 3); got != "cached-derived" {
			t.Fatalf("derived cached value = %q", got)
		}
		if calls := countCommand(h, "env.model_complete"); calls != 0 {
			t.Fatalf("derived cache hit made %d summarizer calls", calls)
		}
	})

	t.Run("captured bytes cannot alias derived domain", func(t *testing.T) {
		h := newHarness(t)
		h.SetConfig(modelConfig)
		h.SeedSharedCache("intent:call_1", derived)
		h.SeedCache(derivedKey, "wrong-domain")
		h.StubModelComplete(modelStub("fresh-summary"))
		h.StubHostCall("torana_evaluate_compaction", applyStub(true))
		res := h.BeforeRequest(bigToolRequest(content))
		if res.Err != nil || res.Request == nil {
			t.Fatalf("captured row failed: %v", res.Err)
		}
		if got := toolText(t, res.Request, 3); got != "fresh-summary" {
			t.Fatalf("captured row reused derived-domain value: %q", got)
		}
	})

	mutations := map[string]func(*pbv1.ChatRequest){
		"user request": func(req *pbv1.ChatRequest) {
			req.Messages[1].Blocks[0].GetText().Text = "find another bug"
		},
		"tool name": func(req *pbv1.ChatRequest) {
			req.Messages[2].Blocks[0].GetToolUse().Name = "read_file"
			req.Messages[3].Blocks[0].GetToolResult().ToolName = "read_file"
		},
		"tool arguments": func(req *pbv1.ChatRequest) {
			req.Messages[2].Blocks[0].GetToolUse().ArgumentsJson = []byte(`{"path":"other.go"}`)
		},
	}
	for name, mutate := range mutations {
		t.Run(name+" partitions derived cache", func(t *testing.T) {
			h := newHarness(t)
			h.SetConfig(modelConfig)
			h.SeedCache(derivedKey, "wrong-input")
			h.StubModelComplete(modelStub("fresh-summary"))
			h.StubHostCall("torana_evaluate_compaction", applyStub(true))
			req := bigToolRequest(content)
			mutate(req)
			res := h.BeforeRequest(req)
			if res.Err != nil || res.Request == nil {
				t.Fatalf("mutated row failed: %v", res.Err)
			}
			if calls := countCommand(h, "env.model_complete"); calls != 1 {
				t.Fatalf("mutated row made %d summarizer calls, want 1", calls)
			}
			if got := toolText(t, res.Request, 3); got != "fresh-summary" {
				t.Fatalf("mutated row reused baseline cache: %q", got)
			}
		})
	}
}

// TestIntentCacheRefusalErrors — a non-NOT_FOUND refusal on the intent read is
// a contract defect: the hook errors.
func TestIntentCacheRefusalErrors(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(modelConfig)
	h.DenyPermission("env.shared_cache_get")
	h.StubModelComplete(modelStub("summary"))
	h.StubHostCall("torana_evaluate_compaction", applyStub(true))
	res := h.BeforeRequest(bigToolRequest(bigContent()))
	if res.Err == nil {
		t.Fatal("a refused intent read must error the hook")
	}
}

// TestToolResultMustStayExact — mutation tools and error-looking outputs stay
// verbatim even under a broad policy.
func TestToolResultMustStayExact(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(`{"tool_policies":[{"match":"*","mode":"deterministic","first_pass":true}]}`)
	for name, content := range map[string]string{
		"mutation tool": strings.Repeat("edit result\n", 300),
		"error content": strings.Repeat("line\n", 300) + "Error: something failed",
	} {
		t.Run(name, func(t *testing.T) {
			h2 := newHarness(t)
			h2.SetConfig(`{"tool_policies":[{"match":"*","mode":"deterministic","first_pass":true}]}`)
			req := bigToolRequest(content)
			req.Messages[3].Blocks[0].GetToolResult().ToolName = "edit_file"
			if name == "error content" {
				req.Messages[3].Blocks[0].GetToolResult().ToolName = "read"
			}
			res := h2.BeforeRequest(req)
			if res.Err != nil {
				t.Fatal(res.Err)
			}
			if !res.PassedThrough {
				t.Fatal("ToolResultMustStayExact must keep the result verbatim")
			}
		})
	}
}

// TestMinSummarizerCharsBoundary — 1999 bytes is not eligible; 2000 is.
func TestMinSummarizerCharsBoundary(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(modelConfig)
	h.SeedSharedCache("intent:call_1", "find the bug")
	h.StubModelComplete(modelStub("summary"))
	h.StubHostCall("torana_evaluate_compaction", applyStub(true))
	short := strings.Repeat("x", 1999)
	res := h.BeforeRequest(bigToolRequest(short))
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if !res.PassedThrough {
		t.Fatal("1999 bytes must not be eligible for compaction")
	}

	h2 := newHarness(t)
	h2.SetConfig(modelConfig)
	h2.SeedSharedCache("intent:call_1", "find the bug")
	h2.StubModelComplete(modelStub("summary"))
	h2.StubHostCall("torana_evaluate_compaction", applyStub(true))
	exact := strings.Repeat("x", 2000)
	res2 := h2.BeforeRequest(bigToolRequest(exact))
	if res2.Err != nil || res2.Request == nil {
		t.Fatalf("2000 bytes must be eligible, err=%v", res2.Err)
	}
}

// TestTruncationMarkerInSummarizerPayload — a positive max_summarizer_input_bytes
// truncates head+tail in the summarizer payload; the tool result itself is never
// truncated.
func TestTruncationMarkerInSummarizerPayload(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(`{"tool_policies":[{"match":"read*","mode":"model"}],"expected_applications":6,"max_summarizer_input_bytes":100}`)
	h.SeedSharedCache("intent:call_1", "find the bug")
	var modelArgs *pbv1.ModelCompleteArgs
	h.StubModelComplete(func(args *pbv1.ModelCompleteArgs) (*pbv1.ModelCompleteResult, *pbv1.HostError, error) {
		modelArgs = args
		return modelStub("summary")(args)
	})
	h.StubHostCall("torana_evaluate_compaction", applyStub(true))
	res := h.BeforeRequest(bigToolRequest(bigContent()))
	if res.Err != nil || res.Request == nil {
		t.Fatalf("expected replacement, err=%v", res.Err)
	}
	if modelArgs == nil || len(modelArgs.Messages) != 2 || !strings.Contains(modelArgs.Messages[1].Content, "... [truncated] ...") {
		t.Fatal("configured cap must truncate the summarizer payload head+tail")
	}
	if len(toolText(t, res.Request, 3)) >= len(bigContent()) {
		t.Fatal("the tool result itself must not be truncated by the input cap")
	}
}

// TestExactModeSkips — mode "exact" never compacts.
func TestExactModeSkips(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(`{"tool_policies":[{"match":"read*","mode":"exact"}],"expected_applications":6}`)
	h.SeedSharedCache("intent:call_1", "find the bug")
	h.StubModelComplete(modelStub("summary"))
	h.StubHostCall("torana_evaluate_compaction", applyStub(true))
	res := h.BeforeRequest(bigToolRequest(bigContent()))
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if !res.PassedThrough {
		t.Fatal("exact mode must pass everything through")
	}
}

// TestCacheSetRefusalIsBestEffort — a refused cache write leaves the applied
// replacement intact; the request is not corrupted and no empty value is ever
// applied.
func TestCacheSetRefusalIsBestEffort(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(modelConfig)
	h.SeedSharedCache("intent:call_1", "find the bug")
	h.DenyPermission("env.cache_set")
	h.StubModelComplete(modelStub("summary"))
	h.StubHostCall("torana_evaluate_compaction", applyStub(true))
	res := h.BeforeRequest(bigToolRequest(bigContent()))
	if res.Err != nil {
		t.Fatalf("a refused cache write must not fail the hook: %v", res.Err)
	}
	if res.Request == nil {
		t.Fatal("expected a replacement despite the refused cache write")
	}
	if toolText(t, res.Request, 3) != "summary" {
		t.Fatalf("the replacement must still be applied despite the refusal: %q", toolText(t, res.Request, 3))
	}
}

// TestSavingsReportRefusalDoesNotChangeReplacement — record_savings is
// best-effort: a refusal leaves the applied replacement untouched.
func TestSavingsReportRefusalDoesNotChangeReplacement(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(modelConfig)
	h.SeedSharedCache("intent:call_1", "find the bug")
	h.StubModelComplete(modelStub("summary"))
	h.StubHostCall("torana_evaluate_compaction", applyStub(true))
	h.StubHostCall("torana_record_savings", func(string) (string, error) {
		return sdktest.HostResultError(pbv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "stub refusal"), nil
	})
	res := h.BeforeRequest(bigToolRequest(bigContent()))
	if res.Err != nil {
		t.Fatalf("a refused savings report must not fail the hook: %v", res.Err)
	}
	if res.Request == nil || toolText(t, res.Request, 3) != "summary" {
		t.Fatal("the applied replacement must stand after a refused savings report")
	}
}

// TestNoUnauthorizedCalls — every dispatch's host traffic is within the
// manifest's declared permission set.
func TestNoUnauthorizedCalls(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(modelConfig)
	h.SeedSharedCache("intent:call_1", "find the bug")
	h.StubModelComplete(modelStub("summary"))
	h.StubHostCall("torana_evaluate_compaction", applyStub(true))
	h.BeforeRequest(bigToolRequest(bigContent()))

	// The harness records RAW command tokens; the HOST gates each of them on
	// the env.host_call.<command> grant, which is what the manifest declares.
	allowed := map[string]bool{
		"env.plugin_config":          true,
		"env.cache_get":              true,
		"env.cache_set":              true,
		"env.shared_cache_get":       true,
		"env.emit_metric":            true,
		"env.model_complete":         true,
		"env.model_pricing":          true,
		"torana_evaluate_compaction": true,
		"torana_record_savings":      true,
	}
	for _, c := range h.Calls() {
		if !allowed[c.Command] {
			t.Errorf("host call outside the declared permission set: %s", c.Command)
		}
	}
}

// TestConfigResetPinsIsolation — sequential rows with contradictory configs
// must not leak through the process-global once.
func TestConfigResetPinsIsolation(t *testing.T) {
	// Row 1: model path with expected_applications=6.
	h := newHarness(t)
	h.SetConfig(modelConfig)
	h.SeedSharedCache("intent:call_1", "find the bug")
	h.StubModelComplete(modelStub("summary"))
	h.StubHostCall("torana_evaluate_compaction", applyStub(true))
	res := h.BeforeRequest(bigToolRequest(bigContent()))
	if res.Err != nil || res.Request == nil {
		t.Fatalf("row 1 should apply, err=%v", res.Err)
	}
	// Row 2: expected_applications=0 disables the model path entirely.
	h2 := newHarness(t)
	h2.SetConfig(`{"tool_policies":[{"match":"read*","mode":"model"}],"expected_applications":0}`)
	h2.SeedSharedCache("intent:call_1", "find the bug")
	h2.StubModelComplete(modelStub("summary"))
	h2.StubHostCall("torana_evaluate_compaction", applyStub(true))
	res2 := h2.BeforeRequest(bigToolRequest(bigContent()))
	if res2.Err != nil {
		t.Fatal(res2.Err)
	}
	if !res2.PassedThrough {
		t.Fatal("row 2 leaked row 1's expected_applications — the once was not reset")
	}
}

// TestModelPathDisabledByDefault — expected_applications defaults to 0: no
// model work even with a model policy.
func TestModelPathDisabledByDefault(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(`{"tool_policies":[{"match":"read*","mode":"model"}]}`)
	h.SeedSharedCache("intent:call_1", "find the bug")
	h.StubModelComplete(modelStub("summary"))
	h.StubHostCall("torana_evaluate_compaction", applyStub(true))
	res := h.BeforeRequest(bigToolRequest(bigContent()))
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if !res.PassedThrough {
		t.Fatal("expected_applications=0 must disable the model path")
	}
	if n := countCommand(h, "env.model_complete"); n != 0 {
		t.Fatalf("summarizer ran with the model path disabled: %d calls", n)
	}
}

// TestModelConsumptionGate — model summaries never precede one exact
// consumption of the result.
func TestModelConsumptionGate(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(modelConfig)
	h.SeedSharedCache("intent:call_1", "find the bug")
	h.StubModelComplete(modelStub("summary"))
	h.StubHostCall("torana_evaluate_compaction", applyStub(true))
	// No assistant message after the tool result.
	req := bigToolRequest(bigContent())
	req.Messages = req.Messages[:4]
	res := h.BeforeRequest(req)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if !res.PassedThrough {
		t.Fatal("model compaction must wait for one exact consumption")
	}
}

// TestDeterminismOverIdenticalRequests — two identical dispatches (fresh
// harnesses, fresh clones) produce byte-identical output.
func TestDeterminismOverIdenticalRequests(t *testing.T) {
	cfg := `{"tool_policies":[{"match":"read*","mode":"deterministic","first_pass":true}]}`
	h1 := newHarness(t)
	h1.SetConfig(cfg)
	r1 := h1.BeforeRequest(bigToolRequest(bigContent()))
	h2 := newHarness(t)
	h2.SetConfig(cfg)
	r2 := h2.BeforeRequest(bigToolRequest(bigContent()))
	if r1.Err != nil || r2.Err != nil {
		t.Fatalf("dispatch errors: %v %v", r1.Err, r2.Err)
	}
	if string(mustJSON(t, r1.Request)) != string(mustJSON(t, r2.Request)) {
		t.Fatal("identical requests produced different bytes — prompt-cache busting input")
	}
}

func mustJSON(t *testing.T, req *pbv1.ChatRequest) []byte {
	t.Helper()
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ==========================================================================
// Round-1 additions: present-empty recompute, malformed replies, economic
// gate refusal classes, preflight-then-decline, schema-default parity.
// ==========================================================================

// TestModelPresentEmptyReplacementRecomputes — a present-empty model-cache
// value is unusable: it must never erase the tool result; the work is
// recomputed through summarizer.
func TestModelPresentEmptyReplacementRecomputes(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(modelConfig)
	h.SeedSharedCache("intent:call_1", "find the bug")
	content := bigContent()
	modelKey := sdk.ContentAddressedCacheKey(compactionCache, "v4", "read", `{"path":"server.go"}`, content, "captured", "find the bug", "model")
	h.SeedCache(modelKey, "") // present, empty
	h.StubModelComplete(modelStub("summary"))
	h.StubHostCall("torana_evaluate_compaction", applyStub(true))
	res := h.BeforeRequest(bigToolRequest(content))
	if res.Err != nil || res.Request == nil {
		t.Fatalf("expected a recomputed replacement, err=%v", res.Err)
	}
	if toolText(t, res.Request, 3) != "summary" {
		t.Fatalf("present-empty cache value must recompute, got %q", toolText(t, res.Request, 3))
	}
	if n := countCommand(h, "env.model_complete"); n != 1 {
		t.Fatalf("summarizer must run for a present-empty cache value, got %d", n)
	}
}

// TestDeterministicPresentEmptyReplacementRecomputes — same rule on the
// deterministic path: an empty cached value must not be applied (it would
// erase the result); the replacement is recomputed.
func TestDeterministicPresentEmptyReplacementRecomputes(t *testing.T) {
	cfg := `{"tool_policies":[{"match":"read*","mode":"deterministic","first_pass":true}]}`
	h := newHarness(t)
	h.SetConfig(cfg)
	content := bigContent()
	policyKey := sdk.ContentAddressedCacheKey(policyCompactionCache, "policy-v1", "read", `{"path":"server.go"}`, content, "deterministic", "")
	h.SeedCache(policyKey, "") // present, empty
	res := h.BeforeRequest(bigToolRequest(content))
	if res.Err != nil || res.Request == nil {
		t.Fatalf("expected a recomputed replacement, err=%v", res.Err)
	}
	if toolText(t, res.Request, 3) == content {
		t.Fatal("present-empty policy cache value must recompute, not pass through")
	}
	if toolText(t, res.Request, 3) == "" {
		t.Fatal("present-empty cache value must never be applied (would erase the result)")
	}
}

// TestIntentCacheMalformedReplyErrors — a malformed HostCallResult on the
// intent read is a protocol error: the hook errors.
func TestIntentCacheMalformedReplyErrors(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(modelConfig)
	h.StubHostCall("env.shared_cache_get", func(string) (string, error) {
		return "not a host-call-result frame", nil
	})
	res := h.BeforeRequest(bigToolRequest(bigContent()))
	if res.Err == nil {
		t.Fatal("a malformed cache reply must error the hook")
	}
}

// TestModelCacheMalformedReplyErrors — malformed reply on the MODEL cache
// read (the intent read succeeds): hook error.
func TestModelCacheMalformedReplyErrors(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(modelConfig)
	h.SeedSharedCache("intent:call_1", "find the bug")
	h.StubHostCall("env.cache_get", func(string) (string, error) {
		return "not a host-call-result frame", nil
	})
	res := h.BeforeRequest(bigToolRequest(bigContent()))
	if res.Err == nil {
		t.Fatal("a malformed model-cache reply must error the hook")
	}
}

// TestDeterministicCacheMalformedReplyErrors — malformed reply on the
// deterministic-policy cache read: hook error.
func TestDeterministicCacheMalformedReplyErrors(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(`{"tool_policies":[{"match":"read*","mode":"deterministic","first_pass":true}]}`)
	h.StubHostCall("env.cache_get", func(string) (string, error) {
		return "not a host-call-result frame", nil
	})
	res := h.BeforeRequest(bigToolRequest(bigContent()))
	if res.Err == nil {
		t.Fatal("a malformed policy-cache reply must error the hook")
	}
}

// TestEvaluateAdvisoryRefusalDeclinesWithoutRetry — NOT_CONFIGURED on the
// economic gate declines the batch; the preflight fails so no summarizer spend
// happens, and evaluate is called exactly once.
func TestEvaluateAdvisoryRefusalDeclinesWithoutRetry(t *testing.T) {
	for _, code := range []pbv1.ErrorCode{
		pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED,
		pbv1.ErrorCode_ERROR_CODE_UNAVAILABLE,
	} {
		t.Run(code.String(), func(t *testing.T) {
			h := newHarness(t)
			h.SetConfig(modelConfig)
			h.SeedSharedCache("intent:call_1", "find the bug")
			h.StubModelComplete(modelStub("summary"))
			h.StubHostCall("torana_evaluate_compaction", func(string) (string, error) {
				return sdktest.HostResultError(code, "stub"), nil
			})
			res := h.BeforeRequest(bigToolRequest(bigContent()))
			if res.Err != nil {
				t.Fatalf("advisory refusal must not error the hook: %v", res.Err)
			}
			if !res.PassedThrough {
				t.Fatal("a declined batch must not apply")
			}
			if n := countCommand(h, "torana_evaluate_compaction"); n != 1 {
				t.Fatalf("advisory refusal was retried: %d evaluate calls", n)
			}
			if n := countCommand(h, "env.model_complete"); n != 0 {
				t.Fatalf("summarizer ran despite a declined preflight: %d calls", n)
			}
		})
	}
}

// TestEvaluateContractRefusalErrors — INVALID_ARGUMENT on the economic gate
// is a contract defect: the hook errors.
func TestEvaluateContractRefusalErrors(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(modelConfig)
	h.SeedSharedCache("intent:call_1", "find the bug")
	h.StubModelComplete(modelStub("summary"))
	h.StubHostCall("torana_evaluate_compaction", func(string) (string, error) {
		return sdktest.HostResultError(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "stub"), nil
	})
	res := h.BeforeRequest(bigToolRequest(bigContent()))
	if res.Err == nil {
		t.Fatal("a contract refusal on the economic gate must error the hook")
	}
}

// TestRealEvaluationDeclinesAfterPreflight — the preflight approves, the
// real evaluation declines: summarizer spent at most once and NO mutation is
// applied (a declined batch never half-applies).
func TestRealEvaluationDeclinesAfterPreflight(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(modelConfig)
	h.SeedSharedCache("intent:call_1", "find the bug")
	h.StubModelComplete(modelStub("summary"))
	seq := 0
	h.StubHostCall("torana_evaluate_compaction", func(string) (string, error) {
		seq++
		if seq == 1 {
			return sdktest.HostResultValue([]byte(`{"apply":true}`)), nil // preflight approves
		}
		return sdktest.HostResultValue([]byte(`{"apply":false}`)), nil // real evaluation declines
	})
	res := h.BeforeRequest(bigToolRequest(bigContent()))
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if !res.PassedThrough {
		t.Fatal("a declined real evaluation must not apply any mutation")
	}
	if n := countCommand(h, "env.model_complete"); n != 1 {
		t.Fatalf("summarizer spend=%d, want exactly 1 (preflight approved once)", n)
	}
}

// TestRealEvaluationRefusalAfterPreflight — preflight approves, the real
// evaluation contract-refuses: hook error, no mutation, summarizer spent once.
func TestRealEvaluationRefusalAfterPreflight(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(modelConfig)
	h.SeedSharedCache("intent:call_1", "find the bug")
	h.StubModelComplete(modelStub("summary"))
	seq := 0
	h.StubHostCall("torana_evaluate_compaction", func(string) (string, error) {
		seq++
		if seq == 1 {
			return sdktest.HostResultValue([]byte(`{"apply":true}`)), nil
		}
		return sdktest.HostResultError(pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "stub"), nil
	})
	res := h.BeforeRequest(bigToolRequest(bigContent()))
	if res.Err == nil {
		t.Fatal("a contract refusal on the real evaluation must error the hook")
	}
	if n := countCommand(h, "env.model_complete"); n != 1 {
		t.Fatalf("summarizer spend=%d, want exactly 1", n)
	}
}

// TestSchemaDefaultsMatchRuntimeDefaults — parity against schema.json itself,
// so a schema/default drift cannot pass: the schema's defaults must equal the
// runtime defaults (0 unbounded / 0 disables the model path / empty policies),
// and the budget field must be named max_summarizer_input_bytes.
func TestSchemaDefaultsMatchRuntimeDefaults(t *testing.T) {
	raw, err := os.ReadFile("schema.json")
	if err != nil {
		t.Fatalf("read schema.json: %v", err)
	}
	var schema struct {
		Properties map[string]struct {
			Default json.RawMessage `json:"default"`
		} `json:"properties"`
		Defs map[string]struct {
			Properties map[string]struct {
				Enum []string `json:"enum"`
			} `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("parse schema.json: %v", err)
	}
	prop, ok := schema.Properties["max_summarizer_input_bytes"]
	if !ok {
		t.Fatal("schema.json has no max_summarizer_input_bytes property (legacy name would drift)")
	}
	if string(prop.Default) != "0" {
		t.Fatalf("schema max_summarizer_input_bytes default=%s, want 0", prop.Default)
	}
	if string(schema.Properties["expected_applications"].Default) != "0" {
		t.Fatalf("schema expected_applications default=%s, want 0", schema.Properties["expected_applications"].Default)
	}
	if string(schema.Properties["tool_policies"].Default) != "[]" {
		t.Fatalf("schema tool_policies default=%s, want []", schema.Properties["tool_policies"].Default)
	}
	if got, want := schema.Defs["policy"].Properties["mode"].Enum, []string{"exact", "deterministic", "model"}; !slices.Equal(got, want) {
		t.Fatalf("schema policy modes=%v, want %v", got, want)
	}

	// Runtime defaults must match: no config -> inert (0/0/nil).
	rt := parseConfig("")
	if rt.MaxSummarizerInputBytes != 0 || rt.ExpectedApplications != 0 || len(rt.ToolPolicies) != 0 {
		t.Fatalf("runtime defaults %+v do not match the schema defaults", rt)
	}
}

// TestDeterministicNonShorterCacheRecomputes — the batch-2 consistency fix:
// a NON-SHORTER deterministic-policy cache value is unusable (applying it
// would expand the request) and is recomputed locally; the applied output is
// the computed replacement, never the cached value.
func TestDeterministicNonShorterCacheRecomputes(t *testing.T) {
	cfg := `{"tool_policies":[{"match":"read*","mode":"deterministic","first_pass":true}]}`
	content := bigContent()
	args := `{"path":"server.go"}`
	key := sdk.ContentAddressedCacheKey(policyCompactionCache, "policy-v1", "read", args, content, "deterministic", "")
	h := newHarness(t)
	h.SetConfig(cfg)
	h.SeedCache(key, content+"extra bytes making the cached value non-shorter")
	res := h.BeforeRequest(bigToolRequest(content))
	if res.Err != nil || res.Request == nil {
		t.Fatalf("expected a recomputed replacement, err=%v", res.Err)
	}
	if toolText(t, res.Request, 3) == content+"extra bytes making the cached value non-shorter" {
		t.Fatal("a non-shorter cached value must never be applied")
	}
	if toolText(t, res.Request, 3) == content {
		t.Fatal("a non-shorter cached value must be recomputed, not left untouched")
	}
}

// ==========================================================================
// Ordered-seam rows (checkpoint REV 2)
// ==========================================================================

// TestOrderedSeamCarrierRows — the ordered-ABI behavior through the REAL
// hook:
//   - a USER-ROLE tool result is a candidate (no role gate);
//   - TWO results in ONE message are independent candidates, both applied;
//   - a mixed [text, tool_result, text] message: the surrounding text
//     survives byte-exact while the result is compacted.
func TestOrderedSeamCarrierRows(t *testing.T) {
	text := func(s string) *pbv1.RequestBlock {
		return &pbv1.RequestBlock{Kind: &pbv1.RequestBlock_Text{Text: &pbv1.RequestTextBlock{Text: s}}}
	}
	result := func(id string, content string) *pbv1.RequestBlock {
		return &pbv1.RequestBlock{Kind: &pbv1.RequestBlock_ToolResult{ToolResult: &pbv1.RequestToolResultBlock{
			ToolCallId: id, ToolName: "read",
			Content: []*pbv1.ToolResultContentBlock{{Kind: &pbv1.ToolResultContentBlock_Text{Text: &pbv1.ToolResultTextBlock{Text: content}}}},
		}}}
	}
	content := strings.Repeat("line of tool output that is long enough to be compaction-eligible\n", 200)
	summary := "short summary"
	assistant := func() *pbv1.Message {
		return &pbv1.Message{Role: "assistant", Blocks: []*pbv1.RequestBlock{text("consumed")}}
	}

	// User-role result: the gate must not require role "tool".
	t.Run("user-role result is a candidate", func(t *testing.T) {
		h := newHarness(t)
		h.SetConfig(modelConfig)
		h.StubModelComplete(modelStub(summary))
		h.StubHostCall("torana_evaluate_compaction", applyStub(true))
		h.SeedSharedCache("intent:c1", "find the bug")
		req := &pbv1.ChatRequest{Messages: []*pbv1.Message{
			{Role: "user", Blocks: []*pbv1.RequestBlock{text("u"), result("c1", content)}},
			assistant(),
		}}
		req.ToranaMetaJson = []byte(`{"_provider":"p","_conversation_id":"conv-1","_path":"/x"}`)
		res := h.BeforeRequest(req)
		if res.Err != nil || res.Request == nil {
			t.Fatalf("err=%v", res.Err)
		}
		if got := res.Request.Messages[0].Blocks[1].GetToolResult().Content[0].GetText().Text; got != summary {
			t.Fatalf("user-role result not compacted: %q", got)
		}
		if got := res.Request.Messages[0].Blocks[0].GetText().Text; got != "u" {
			t.Fatalf("surrounding text disturbed: %q", got)
		}
	})

	// Free-form result payloads use the same ordered text carrier. Compaction
	// changes only that text and must preserve the invocation family.
	t.Run("free-form result is a candidate", func(t *testing.T) {
		h := newHarness(t)
		h.SetConfig(modelConfig)
		h.StubModelComplete(modelStub(summary))
		h.StubHostCall("torana_evaluate_compaction", applyStub(true))
		h.SeedSharedCache("intent:c1", "find the bug")
		block := result("c1", content)
		block.GetToolResult().InvocationKind = pbv1.ToolInvocationKind_TOOL_INVOCATION_KIND_FREEFORM
		req := &pbv1.ChatRequest{Messages: []*pbv1.Message{
			{Role: "user", Blocks: []*pbv1.RequestBlock{block}},
			assistant(),
		}}
		req.ToranaMetaJson = []byte(`{"_provider":"p","_conversation_id":"conv-1","_path":"/x"}`)
		res := h.BeforeRequest(req)
		if res.Err != nil || res.Request == nil {
			t.Fatalf("err=%v", res.Err)
		}
		got := res.Request.Messages[0].Blocks[0].GetToolResult()
		if got.GetInvocationKind() != pbv1.ToolInvocationKind_TOOL_INVOCATION_KIND_FREEFORM || got.Content[0].GetText().Text != summary {
			t.Fatalf("free-form result not preserved and compacted: %+v", got)
		}
	})

	// Two results in one message: independent candidates, both applied. The
	// accounting is pinned exactly: the real evaluate report carries
	// candidate_count=2, and every per-candidate call fires exactly once per
	// candidate (intent cache_get x2, model-key cache_get x2, summarizer x2,
	// eligible metric x2, cache_set x2) while the batch-level calls fire once
	// (preflight + real evaluate x2, record_savings x1, plugin_config x1).
	t.Run("two results in one message", func(t *testing.T) {
		h := newHarness(t)
		h.SetConfig(modelConfig)
		h.StubModelComplete(modelStub(summary))
		h.StubHostCall("torana_evaluate_compaction", applyStub(true))
		h.SeedSharedCache("intent:c1", "find the bug")
		h.SeedSharedCache("intent:c2", "find the bug")
		req := &pbv1.ChatRequest{Messages: []*pbv1.Message{
			{Role: "user", Blocks: []*pbv1.RequestBlock{result("c1", content), result("c2", content)}},
			assistant(),
		}}
		req.ToranaMetaJson = []byte(`{"_provider":"p","_conversation_id":"conv-1","_path":"/x"}`)
		res := h.BeforeRequest(req)
		if res.Err != nil || res.Request == nil {
			t.Fatalf("err=%v", res.Err)
		}
		for _, id := range []string{"c1", "c2"} {
			got := ""
			for _, b := range res.Request.Messages[0].Blocks {
				if tr := b.GetToolResult(); tr != nil && tr.ToolCallId == id {
					got = tr.Content[0].GetText().Text
				}
			}
			if got != summary {
				t.Fatalf("result %s not compacted: %q", id, got)
			}
		}
		// The COMPLETE host-call multiset (EmitMetric is a metric, not a
		// host call — its exact cardinality is asserted below).
		wantMultiset := map[string]int{
			"env.plugin_config":          1,
			"env.cache_get":              2, // model key per candidate
			"env.shared_cache_get":       2, // intent key per candidate
			"env.cache_set":              2, // best-effort per transformation
			"env.model_complete":         2, // one per uncached candidate
			"torana_evaluate_compaction": 2, // optimistic preflight + real report
			"torana_record_savings":      1, // the batch
		}
		gotMultiset := map[string]int{}
		for _, c := range h.Calls() {
			gotMultiset[c.Command]++
		}
		if len(gotMultiset) != len(wantMultiset) {
			t.Fatalf("call multiset = %v, want %v", gotMultiset, wantMultiset)
		}
		for cmd, want := range wantMultiset {
			if gotMultiset[cmd] != want {
				t.Fatalf("call %s count = %d, want %d (multiset %v)", cmd, gotMultiset[cmd], want, gotMultiset)
			}
		}
		// The eligible metric fires exactly once per candidate.
		eligible := 0
		for _, m := range h.Metrics() {
			if m.Name == "torana_compact_eligible_total" {
				eligible += int(m.Value)
			}
		}
		if eligible != 2 {
			t.Fatalf("eligible metric = %d, want 2 (one per candidate)", eligible)
		}
		// The evaluate payloads IN ORDER: BOTH carry candidate_count=2, but
		// only the REAL report (the second) carries the summarizer facts — the
		// optimistic preflight is summarizer-free by construction.
		type evalPayload struct {
			CandidateCount  int    `json:"candidate_count"`
			Source          string `json:"source"`
			PricingResource string `json:"pricing_resource"`
			Summarizer      *struct {
				PricingResource string `json:"pricing_resource"`
			} `json:"summarizer"`
		}
		var evals []evalPayload
		for _, c := range h.Calls() {
			if c.Command != "torana_evaluate_compaction" {
				continue
			}
			var p evalPayload
			if err := json.Unmarshal([]byte(c.Args), &p); err != nil {
				t.Fatalf("evaluate args not JSON: %v (%s)", err, c.Args)
			}
			evals = append(evals, p)
		}
		if len(evals) != 2 {
			t.Fatalf("evaluate payloads = %d, want 2", len(evals))
		}
		if evals[0].CandidateCount != 2 || evals[1].CandidateCount != 2 {
			t.Fatalf("candidate_count = %d/%d, want 2/2", evals[0].CandidateCount, evals[1].CandidateCount)
		}
		if evals[0].Summarizer != nil {
			t.Fatalf("the OPTIMISTIC preflight must not carry summarizer facts: %+v", evals[0].Summarizer)
		}
		if evals[0].PricingResource != "target" || evals[1].PricingResource != "target" {
			t.Fatalf("target pricing resource = %q/%q", evals[0].PricingResource, evals[1].PricingResource)
		}
		if evals[1].Summarizer == nil || evals[1].Summarizer.PricingResource != "summarizer" {
			t.Fatalf("the REAL report must carry the summarizer facts: %+v", evals[1].Summarizer)
		}
	})

	// Unsupported shapes decline unchanged with EXACTLY ONE env.plugin_config
	// call and ZERO cache/summarizer/metrics/savings calls. The marker-only row
	// is IN-DOMAIN (a real cache-breakpoint arm, not the out-of-domain empty
	// content list); the explicit-empty row is a scalar candidate whose
	// content is below the minimum threshold — inert with the same zero-spend
	// multiset.
	t.Run("unsupported shapes exact multiset", func(t *testing.T) {
		unknown := &pbv1.ToolResultContentBlock{Kind: &pbv1.ToolResultContentBlock_Unknown{Unknown: &pbv1.ToolResultUnknownBlock{Kind: "provider_blob", PayloadJson: []byte(`{"x":1}`)}}}
		marker := &pbv1.ToolResultContentBlock{Kind: &pbv1.ToolResultContentBlock_CacheBreakpoint{CacheBreakpoint: &pbv1.ToolResultCacheBreakpoint{MarkerJson: []byte(`{"type":"ephemeral"}`)}}}
		rows := map[string]*pbv1.RequestToolResultBlock{
			"marker-only":    {ToolCallId: "c1", ToolName: "read", Content: []*pbv1.ToolResultContentBlock{marker}},
			"multiple text":  {ToolCallId: "c1", ToolName: "read", Content: []*pbv1.ToolResultContentBlock{{Kind: &pbv1.ToolResultContentBlock_Text{Text: &pbv1.ToolResultTextBlock{Text: "a"}}}, {Kind: &pbv1.ToolResultContentBlock_Text{Text: &pbv1.ToolResultTextBlock{Text: "b"}}}}},
			"unknown arm":    {ToolCallId: "c1", ToolName: "read", Content: []*pbv1.ToolResultContentBlock{{Kind: &pbv1.ToolResultContentBlock_Text{Text: &pbv1.ToolResultTextBlock{Text: content}}}, unknown}},
			"explicit empty": {ToolCallId: "c1", ToolName: "read", Content: []*pbv1.ToolResultContentBlock{{Kind: &pbv1.ToolResultContentBlock_Text{Text: &pbv1.ToolResultTextBlock{Text: ""}}}}},
		}
		for name, tr := range rows {
			t.Run(name, func(t *testing.T) {
				h := newHarness(t)
				h.SetConfig(modelConfig)
				h.StubModelComplete(modelStub(summary))
				req := &pbv1.ChatRequest{Messages: []*pbv1.Message{
					{Role: "user", Blocks: []*pbv1.RequestBlock{{Kind: &pbv1.RequestBlock_ToolResult{ToolResult: tr}}}},
					assistant(),
				}}
				req.ToranaMetaJson = []byte(`{"_provider":"p","_conversation_id":"conv-1","_path":"/x"}`)
				before := proto.Clone(req).(*pbv1.ChatRequest)
				res := h.BeforeRequest(req)
				if res.Err != nil || !res.PassedThrough {
					t.Fatalf("must pass unchanged, err=%v", res.Err)
				}
				if !proto.Equal(req, before) {
					t.Fatal("an unsupported shape was mutated")
				}
				calls := h.Calls()
				if len(calls) != 1 || calls[0].Command != "env.plugin_config" {
					got := make([]string, 0, len(calls))
					for _, c := range calls {
						got = append(got, c.Command)
					}
					t.Fatalf("the complete call multiset = %v, want exactly [env.plugin_config]", got)
				}
			})
		}
	})
}

func TestToolInvocationInputIdentitySeparatesFreeformInputs(t *testing.T) {
	empty := ""
	one := "echo one"
	two := "echo two"
	identities := []string{
		toolInvocationInputIdentity(pbv1.ToolInvocationKind_TOOL_INVOCATION_KIND_FUNCTION, sdk.ToolCallView{
			InvocationKind: pbv1.ToolInvocationKind_TOOL_INVOCATION_KIND_FUNCTION, Arguments: []byte(`{"path":"x"}`),
		}, true),
		toolInvocationInputIdentity(pbv1.ToolInvocationKind_TOOL_INVOCATION_KIND_FREEFORM, sdk.ToolCallView{
			InvocationKind: pbv1.ToolInvocationKind_TOOL_INVOCATION_KIND_FREEFORM, InputText: &empty,
		}, true),
		toolInvocationInputIdentity(pbv1.ToolInvocationKind_TOOL_INVOCATION_KIND_FREEFORM, sdk.ToolCallView{
			InvocationKind: pbv1.ToolInvocationKind_TOOL_INVOCATION_KIND_FREEFORM, InputText: &one,
		}, true),
		toolInvocationInputIdentity(pbv1.ToolInvocationKind_TOOL_INVOCATION_KIND_FREEFORM, sdk.ToolCallView{
			InvocationKind: pbv1.ToolInvocationKind_TOOL_INVOCATION_KIND_FREEFORM, InputText: &two,
		}, true),
		toolInvocationInputIdentity(pbv1.ToolInvocationKind_TOOL_INVOCATION_KIND_FREEFORM, sdk.ToolCallView{}, false),
	}
	seen := make(map[string]struct{}, len(identities))
	for _, identity := range identities {
		if _, exists := seen[identity]; exists {
			t.Fatalf("invocation identity collision: %q in %v", identity, identities)
		}
		seen[identity] = struct{}{}
	}
	if identities[0] != `{"path":"x"}` {
		t.Fatalf("function identity changed: %q", identities[0])
	}
	if identities[1] != "freeform:" || identities[4] != "freeform-absent:" {
		t.Fatalf("free-form presence collapsed: %v", identities)
	}
	if modelResultCacheKey("shell", identities[2], "same output", "same intent", false) ==
		modelResultCacheKey("shell", identities[3], "same output", "same intent", false) {
		t.Fatal("distinct free-form inputs share a model compaction cache key")
	}
}

// TestOrderedSeamContextExtraction — the ported context algorithm pinned
// EXACTLY: the last FIVE NON-EMPTY qualifying user/assistant messages via
// sdk.Text, in wire order, each prefixed "role: ", joined with "\n", with
// the 500-source-byte rune-safe cap and the "..." suffix. A mixed
// [text, tool_result, text] message keeps its surrounding text (the
// candidate result's content never enters the context); empty and
// non-qualifying messages are skipped WITHOUT consuming a slot.
func TestOrderedSeamContextExtraction(t *testing.T) {
	text := func(s string) *pbv1.RequestBlock {
		return &pbv1.RequestBlock{Kind: &pbv1.RequestBlock_Text{Text: &pbv1.RequestTextBlock{Text: s}}}
	}
	resultBlock := func(id, content string) *pbv1.RequestBlock {
		return &pbv1.RequestBlock{Kind: &pbv1.RequestBlock_ToolResult{ToolResult: &pbv1.RequestToolResultBlock{
			ToolCallId: id, ToolName: "read",
			Content: []*pbv1.ToolResultContentBlock{{Kind: &pbv1.ToolResultContentBlock_Text{Text: &pbv1.ToolResultTextBlock{Text: content}}}},
		}}}
	}

	// Six qualifying messages plus an empty user and a system message: the
	// window keeps the LAST FIVE non-empty in order, skipping the empties
	// and system rows, and the mixed message contributes its surrounding
	// text only.
	msgs := []*pbv1.Message{
		{Role: "system", Blocks: []*pbv1.RequestBlock{text("sys")}},
		{Role: "user", Blocks: []*pbv1.RequestBlock{text("first user"), resultBlock("c1", "RESULT-CONTENT-NOT-IN-CONTEXT"), text("second user")}},
		{Role: "assistant", Blocks: []*pbv1.RequestBlock{text("assistant reply")}},
		{Role: "user", Blocks: []*pbv1.RequestBlock{text("")}},
		{Role: "user", Blocks: []*pbv1.RequestBlock{text("third user")}},
		{Role: "assistant", Blocks: []*pbv1.RequestBlock{text("fourth assistant")}},
		{Role: "user", Blocks: []*pbv1.RequestBlock{text("fifth user")}},
	}
	want := "user: first usersecond user\nassistant: assistant reply\nuser: third user\nassistant: fourth assistant\nuser: fifth user"
	if got := extractConversationContext(msgs); got != want {
		t.Fatalf("context = %q, want %q", got, want)
	}

	// A seventh qualifying message pushes the OLDEST out of the window.
	msgs2 := append(msgs, &pbv1.Message{Role: "assistant", Blocks: []*pbv1.RequestBlock{text("sixth assistant")}})
	want2 := "assistant: assistant reply\nuser: third user\nassistant: fourth assistant\nuser: fifth user\nassistant: sixth assistant"
	if got := extractConversationContext(msgs2); got != want2 {
		t.Fatalf("window slide: context = %q, want %q", got, want2)
	}

	// The empty context sentinel.
	if got := extractConversationContext([]*pbv1.Message{{Role: "system", Blocks: []*pbv1.RequestBlock{text("sys")}}}); got != "no prior conversation context available" {
		t.Fatalf("empty context = %q", got)
	}

	// The 500-byte rune-safe cap with the "..." suffix, EXACT: 3-byte runes
	// force the boundary back to the last whole rune (498 bytes = 166 界),
	// plus "user: " and "...".
	long := strings.Repeat("界", 200) // 600 source bytes, 3-byte runes
	capped := extractConversationContext([]*pbv1.Message{{Role: "user", Blocks: []*pbv1.RequestBlock{text(long)}}})
	wantCapped := "user: " + strings.Repeat("界", 166) + "..."
	if capped != wantCapped {
		t.Fatalf("capped = %q (len %d), want %q (len %d)", capped, len(capped), wantCapped, len(wantCapped))
	}
}
