package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pbv2 "github.com/torana-edge/torana-plugin-sdk/pb/v2"
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

// bigToolRequest builds the cache-compliance shape the real host exercises:
// a large tool result with a prior assistant turn (satisfying the model-mode
// consumption gate) and a replayed tool call for name/args lookup.
func bigToolRequest(content string) *pbv2.ChatRequest {
	return &pbv2.ChatRequest{
		Messages: []*pbv2.Message{
			{Role: "system", Content: "You are a coding agent."},
			{Role: "user", Content: "find the bug"},
			{Role: "assistant", ToolCalls: []*pbv2.ToolCall{{Id: "call_1", Name: "read", ArgumentsJson: []byte(`{"path":"server.go"}`)}}},
			{Role: "tool", ToolCallId: "call_1", ToolName: "read", Content: content},
			{Role: "user", Content: "now fix it"},
			// One exact consumption after the result: the model-mode gate
			// requires it (a model summary is never allowed before the result
			// has been consumed once).
			{Role: "assistant", Content: "the fix is in server.go"},
		},
	}
}

func bigContent() string {
	return strings.Repeat("line of tool output that is long enough to be compaction-eligible\n", 200)
}

// modelConfig enables the model path with an economic gate that must approve.
const modelConfig = `{"tool_policies":[{"match":"read*","mode":"model"}],"expected_applications":6}`

// offloadStub returns a v2-shaped success (NO status field) for the offload
// host call.
func offloadStub(completion string) func(args string) (string, error) {
	return func(args string) (string, error) {
		return sdktest.HostResultValue([]byte(`{"completion":"` + completion + `","provider":"deepseek","model":"deepseek-v4-flash","usage":{"reported":true,"input_tokens":100,"output_tokens":50}}`)), nil
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
// the v2 wire: proto.Size over a pbv2 ChatRequest. Measured 2026-08-02:
// tail size 100054 bytes, rewrite span 5054 bytes -> 1264 estimated tokens.
// The v1 numbers (v1 tags) were 1250-1400; the v2 wire is now pinned exactly.
func TestModelBatchReportUsesAdjustedTailOnce(t *testing.T) {
	original := strings.Repeat("x", 100_000)
	replacement := strings.Repeat("y", 5_000)
	result := &pbv2.Message{Role: "tool", ToolCallId: "large", Content: original}
	req := &pbv2.ChatRequest{Messages: []*pbv2.Message{
		{Role: "user", Content: "prefix outside the rewrite span"},
		result,
		{Role: "assistant", Content: "near-tail response"},
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
	if rewrite != 1_264 {
		t.Fatalf("rewrite span estimate=%d, want 1264 (measured from the v2 wire)", rewrite)
	}
}

func TestOptimisticPreflightChargesUncachedRewrite(t *testing.T) {
	uncached := &pbv2.Message{Role: "tool", ToolCallId: "new", Content: strings.Repeat("x", 10_000)}
	cached := &pbv2.Message{Role: "tool", ToolCallId: "cached", Content: strings.Repeat("y", 10_000)}
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
// matches, model path disabled, unbounded offload input. Result: pass-through
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
	req := proto.Clone(original).(*pbv2.ChatRequest)

	first := h.BeforeRequest(req)
	if first.Err != nil || first.Request == nil {
		t.Fatalf("expected a replacement on turn 1, err=%v", first.Err)
	}
	if first.Request.Messages[3].Content == original.Messages[3].Content {
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
	second := h.BeforeRequest(proto.Clone(original).(*pbv2.ChatRequest))
	if second.Err != nil || second.Request == nil {
		t.Fatalf("expected a replacement on turn 2, err=%v", second.Err)
	}
	if string(first.Request.Messages[3].Content) != string(second.Request.Messages[3].Content) {
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
	res2 := h2.BeforeRequest(proto.Clone(original).(*pbv2.ChatRequest))
	if res2.Err != nil || res2.Request == nil {
		t.Fatalf("expected replacement after a consumption, err=%v", res2.Err)
	}
	if res2.Request.Messages[3].Content == original.Messages[3].Content {
		t.Fatal("deterministic policy did not replace the consumed result")
	}
}

// TestModelPathAppliesWithV2OffloadShape — the full model row: stubbed offload
// with the v2 body (no status), economic gate approves; asserts the
// replacement, the cache write, the savings report, and the carried
// provider/model/usage.
func TestModelPathAppliesWithV2OffloadShape(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(modelConfig)
	h.SeedCache("intent:call_1", "find the bug in server.go")
	h.StubHostCall("torana_offload_completion", offloadStub("summary"))
	h.StubHostCall("torana_evaluate_compaction", applyStub(true))

	res := h.BeforeRequest(bigToolRequest(bigContent()))
	if res.Err != nil || res.Request == nil {
		t.Fatalf("expected a replacement, err=%v", res.Err)
	}
	got := res.Request.Messages[3].Content
	if got != "summary" {
		t.Fatalf("tool result not replaced: %q", got[:min(40, len(got))])
	}
	// record_savings was called once with the batch report.
	if countCommand(h, "torana_record_savings") != 1 {
		t.Fatalf("record_savings calls=%d, want 1", countCommand(h, "torana_record_savings"))
	}
	// The offload payload carried the intent and the (unbounded) full output.
	offloadArgs := ""
	for _, c := range h.Calls() {
		if c.Command == "torana_offload_completion" {
			offloadArgs = c.Args
		}
	}
	if !strings.Contains(offloadArgs, "find the bug in server.go") {
		t.Fatal("offload payload missing the intent")
	}
	if strings.Contains(offloadArgs, "[truncated]") {
		t.Fatal("default max_offload_input_chars=0 must send the FULL output, not a truncated one")
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

// TestOffloadAdvisoryRefusalSkipsWithoutRetry — NOT_CONFIGURED/UNAVAILABLE are
// advisory: the candidate is skipped, the batch may still apply others, and
// the same call is never retried.
func TestOffloadAdvisoryRefusalSkipsWithoutRetry(t *testing.T) {
	for _, code := range []pbv2.ErrorCode{
		pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED,
		pbv2.ErrorCode_ERROR_CODE_UNAVAILABLE,
	} {
		t.Run(code.String(), func(t *testing.T) {
			h := newHarness(t)
			h.SetConfig(modelConfig)
			h.SeedCache("intent:call_1", "find the bug")
			h.StubHostCall("torana_offload_completion", func(string) (string, error) {
				return sdktest.HostResultError(code, "stub refusal"), nil
			})
			h.StubHostCall("torana_evaluate_compaction", applyStub(true))
			res := h.BeforeRequest(bigToolRequest(bigContent()))
			if res.Err != nil {
				t.Fatalf("advisory refusal must not error the hook: %v", res.Err)
			}
			if !res.PassedThrough {
				t.Fatal("no candidate survived the advisory refusal; nothing may change")
			}
			if n := countCommand(h, "torana_offload_completion"); n != 1 {
				t.Fatalf("advisory refusal was retried: %d offload calls", n)
			}
		})
	}
}

// TestOffloadContractRefusalErrors — INVALID_ARGUMENT/PERMISSION_DENIED are
// contract defects: the hook errors so failure_mode applies.
func TestOffloadContractRefusalErrors(t *testing.T) {
	for _, code := range []pbv2.ErrorCode{
		pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT,
		pbv2.ErrorCode_ERROR_CODE_PERMISSION_DENIED,
		pbv2.ErrorCode_ERROR_CODE_INTERNAL,
	} {
		t.Run(code.String(), func(t *testing.T) {
			h := newHarness(t)
			h.SetConfig(modelConfig)
			h.SeedCache("intent:call_1", "find the bug")
			h.StubHostCall("torana_offload_completion", func(string) (string, error) {
				return sdktest.HostResultError(code, "stub refusal"), nil
			})
			h.StubHostCall("torana_evaluate_compaction", applyStub(true))
			res := h.BeforeRequest(bigToolRequest(bigContent()))
			if res.Err == nil {
				t.Fatalf("%s refusal must error the hook", code.String())
			}
		})
	}
}

// TestUnusableOffloadBodiesSkip — empty completion or a completion no shorter
// than the original is unusable: skip, no error, nothing applied.
func TestUnusableOffloadBodiesSkip(t *testing.T) {
	for name, body := range map[string]string{
		"empty completion":   `{"completion":"","provider":"p","model":"m"}`,
		"not shorter":        `{"completion":"` + strings.Repeat("x", 20_000) + `","provider":"p","model":"m"}`,
		"no completion":      `{"provider":"p","model":"m"}`,
		"legacy error shape": `{"status":"error","message":"nope"}`,
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.SetConfig(modelConfig)
			h.SeedCache("intent:call_1", "find the bug")
			h.StubHostCall("torana_offload_completion", func(string) (string, error) {
				return sdktest.HostResultValue([]byte(body)), nil
			})
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

// TestOffloadAdditiveFieldsAreTolerated — a body with an extra legacy
// "status":"ok" field plus a valid completion APPLIES: the decoder never
// consults status (OffloadResult is additively evolvable).
func TestOffloadAdditiveFieldsAreTolerated(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(modelConfig)
	h.SeedCache("intent:call_1", "find the bug")
	h.StubHostCall("torana_offload_completion", func(string) (string, error) {
		return sdktest.HostResultValue([]byte(`{"status":"ok","completion":"summary","provider":"p","model":"m","usage":{"reported":true}}`)), nil
	})
	h.StubHostCall("torana_evaluate_compaction", applyStub(true))
	res := h.BeforeRequest(bigToolRequest(bigContent()))
	if res.Err != nil || res.Request == nil {
		t.Fatalf("expected the additive-tolerant decode to apply, err=%v", res.Err)
	}
	if res.Request.Messages[3].Content != "summary" {
		t.Fatal("completion with an additive status field must apply")
	}
}

// TestEconomicGateDeclinesBatch — evaluate {"apply":false} declines; the
// optimistic preflight declines BEFORE any offload call.
func TestEconomicGateDeclinesBatch(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(modelConfig)
	h.SeedCache("intent:call_1", "find the bug")
	h.StubHostCall("torana_offload_completion", offloadStub("summary"))
	h.StubHostCall("torana_evaluate_compaction", applyStub(false))
	res := h.BeforeRequest(bigToolRequest(bigContent()))
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if !res.PassedThrough {
		t.Fatal("a declined batch must not apply")
	}
	// Preflight declined -> no offload spend at all.
	if n := countCommand(h, "torana_offload_completion"); n != 0 {
		t.Fatalf("offload ran despite a declined preflight: %d calls", n)
	}
}

// TestUncachedBatchEvaluatesTwice — an uncached batch calls
// torana_evaluate_compaction TWICE (optimistic preflight, then the real
// post-offload report), both with candidate_count 2 for a two-candidate batch.
func TestUncachedBatchEvaluatesTwice(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(modelConfig)
	h.SeedCache("intent:call_1", "find the bug")
	h.SeedCache("intent:call_2", "second intent")
	h.StubHostCall("torana_offload_completion", offloadStub("summary"))
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
	req.Messages = append(req.Messages, &pbv2.Message{
		Role: "tool", ToolCallId: "call_2", ToolName: "read", Content: strings.Repeat("second big output\n", 200),
	})
	req.Messages = append(req.Messages, &pbv2.Message{Role: "assistant", Content: "x"})
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
	h.SeedCache("intent:call_1", "find the bug")
	h.StubHostCall("torana_offload_completion", offloadStub("must-not-run"))
	h.StubHostCall("torana_evaluate_compaction", applyStub(true))
	content := bigContent()
	modelKey := sdk.ContentAddressedCacheKey(compactionCache, "v3", "read", `{"path":"server.go"}`, content, "find the bug", "model")
	h.SeedCache(modelKey, "cached-summary")

	res := h.BeforeRequest(bigToolRequest(content))
	if res.Err != nil || res.Request == nil {
		t.Fatalf("expected cache reuse, err=%v", res.Err)
	}
	if res.Request.Messages[3].Content != "cached-summary" {
		t.Fatalf("cached replacement not reused: %q", res.Request.Messages[3].Content)
	}
	if n := countCommand(h, "torana_offload_completion"); n != 0 {
		t.Fatalf("cached-shorter value must be reused WITHOUT offload: %d calls", n)
	}
	if n := countCommand(h, "torana_evaluate_compaction"); n != 1 {
		t.Fatalf("all-cached batch must evaluate exactly once, got %d", n)
	}
}

// TestCachedValueNotShorterLeavesUntouched — a cached value >= the original
// is not applied and not recomputed: the message stays byte-identical, with
// no offload and no evaluation (the hit is not even queued as work).
func TestCachedValueNotShorterLeavesUntouched(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(modelConfig)
	h.SeedCache("intent:call_1", "find the bug")
	h.StubHostCall("torana_offload_completion", offloadStub("must-not-run"))
	h.StubHostCall("torana_evaluate_compaction", applyStub(true))
	content := bigContent()
	modelKey := sdk.ContentAddressedCacheKey(compactionCache, "v3", "read", `{"path":"server.go"}`, content, "find the bug", "model")
	// Value exactly as long as the original: not shorter -> untouched.
	h.SeedCache(modelKey, content)

	res := h.BeforeRequest(bigToolRequest(content))
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if !res.PassedThrough {
		t.Fatal("a cached value >= the original must leave the message untouched")
	}
	if n := countCommand(h, "torana_offload_completion"); n != 0 {
		t.Fatalf("offload ran despite a non-shorter cache hit: %d calls", n)
	}
	if n := countCommand(h, "torana_evaluate_compaction"); n != 0 {
		t.Fatalf("evaluate ran despite a non-shorter cache hit: %d calls", n)
	}
}

// TestProviderModelInconsistencyRejectsBatch — two transformation candidates
// from different providers/models cannot share one report: nothing applies.
func TestProviderModelInconsistencyRejectsBatch(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(modelConfig)
	h.SeedCache("intent:call_1", "find the bug")
	h.SeedCache("intent:call_2", "second")
	seq := 0
	h.StubHostCall("torana_offload_completion", func(string) (string, error) {
		seq++
		prov := "deepseek"
		model := "deepseek-v4-flash"
		if seq == 2 {
			prov, model = "openai", "gpt-4o-mini"
		}
		return sdktest.HostResultValue([]byte(`{"completion":"summary","provider":"` + prov + `","model":"` + model + `"}`)), nil
	})
	h.StubHostCall("torana_evaluate_compaction", applyStub(true))
	req := bigToolRequest(bigContent())
	req.Messages = append(req.Messages, &pbv2.Message{
		Role: "tool", ToolCallId: "call_2", ToolName: "read", Content: strings.Repeat("second big output\n", 200),
	})
	req.Messages = append(req.Messages, &pbv2.Message{Role: "assistant", Content: "x"})
	res := h.BeforeRequest(req)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if !res.PassedThrough {
		t.Fatal("a provider-inconsistent batch must not apply")
	}
}

// TestIntentMissSkipsWithMetric — NOT_FOUND intent: eligible, no intent;
// skip + metric. Present-empty intent is the same OUTCOME (unusable) but the
// plugin read the key as present (a present-empty entry is never treated as
// absence at the ABI layer).
func TestIntentMissSkipsWithMetric(t *testing.T) {
	for _, name := range []string{"absent", "present-empty"} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.SetConfig(modelConfig)
			h.SeedCache("intent:call_1", "") // empty value, present key
			if name == "absent" {
				// Truly absent: the key is removed again (SeedCache with the
				// empty string stores presence; use a harness with no seed).
				h = newHarness(t)
				h.SetConfig(modelConfig)
			}
			h.StubHostCall("torana_offload_completion", offloadStub("summary"))
			h.StubHostCall("torana_evaluate_compaction", applyStub(true))
			res := h.BeforeRequest(bigToolRequest(bigContent()))
			if res.Err != nil {
				t.Fatal(res.Err)
			}
			if !res.PassedThrough {
				t.Fatal("no usable intent, nothing may change")
			}
			missed := false
			for _, m := range h.Metrics() {
				if m.Name == "torana_intent_missing_total" {
					missed = true
				}
			}
			if !missed {
				t.Fatal("missing torana_intent_missing_total metric")
			}
		})
	}
}

// TestIntentCacheRefusalErrors — a non-NOT_FOUND refusal on the intent read is
// a contract defect: the hook errors.
func TestIntentCacheRefusalErrors(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(modelConfig)
	h.DenyPermission("env.cache_get")
	h.StubHostCall("torana_offload_completion", offloadStub("summary"))
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
			req.Messages[3].ToolName = "edit_file"
			if name == "error content" {
				req.Messages[3].ToolName = "read"
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

// TestMinOffloadCharsBoundary — 1999 bytes is not eligible; 2000 is.
func TestMinOffloadCharsBoundary(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(modelConfig)
	h.SeedCache("intent:call_1", "find the bug")
	h.StubHostCall("torana_offload_completion", offloadStub("summary"))
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
	h2.SeedCache("intent:call_1", "find the bug")
	h2.StubHostCall("torana_offload_completion", offloadStub("summary"))
	h2.StubHostCall("torana_evaluate_compaction", applyStub(true))
	exact := strings.Repeat("x", 2000)
	res2 := h2.BeforeRequest(bigToolRequest(exact))
	if res2.Err != nil || res2.Request == nil {
		t.Fatalf("2000 bytes must be eligible, err=%v", res2.Err)
	}
}

// TestTruncationMarkerInOffloadPayload — a positive max_offload_input_chars
// truncates head+tail in the offload payload; the tool result itself is never
// truncated.
func TestTruncationMarkerInOffloadPayload(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(`{"tool_policies":[{"match":"read*","mode":"model"}],"expected_applications":6,"max_offload_input_bytes":100}`)
	h.SeedCache("intent:call_1", "find the bug")
	var offloadArgs string
	h.StubHostCall("torana_offload_completion", func(args string) (string, error) {
		offloadArgs = args
		return sdktest.HostResultValue([]byte(`{"completion":"summary","provider":"p","model":"m","usage":{"reported":true}}`)), nil
	})
	h.StubHostCall("torana_evaluate_compaction", applyStub(true))
	res := h.BeforeRequest(bigToolRequest(bigContent()))
	if res.Err != nil || res.Request == nil {
		t.Fatalf("expected replacement, err=%v", res.Err)
	}
	if !strings.Contains(offloadArgs, "... [truncated] ...") {
		t.Fatal("configured cap must truncate the offload payload head+tail")
	}
	if len(res.Request.Messages[3].Content) >= len(bigContent()) {
		t.Fatal("the tool result itself must not be truncated by the input cap")
	}
}

// TestExactModeSkips — mode "exact" never compacts.
func TestExactModeSkips(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(`{"tool_policies":[{"match":"read*","mode":"exact"}],"expected_applications":6}`)
	h.SeedCache("intent:call_1", "find the bug")
	h.StubHostCall("torana_offload_completion", offloadStub("summary"))
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
	h.SeedCache("intent:call_1", "find the bug")
	h.DenyPermission("env.cache_set")
	h.StubHostCall("torana_offload_completion", offloadStub("summary"))
	h.StubHostCall("torana_evaluate_compaction", applyStub(true))
	res := h.BeforeRequest(bigToolRequest(bigContent()))
	if res.Err != nil {
		t.Fatalf("a refused cache write must not fail the hook: %v", res.Err)
	}
	if res.Request == nil {
		t.Fatal("expected a replacement despite the refused cache write")
	}
	if res.Request.Messages[3].Content != "summary" {
		t.Fatalf("the replacement must still be applied despite the refusal: %q", res.Request.Messages[3].Content)
	}
}

// TestSavingsReportRefusalDoesNotChangeReplacement — record_savings is
// best-effort: a refusal leaves the applied replacement untouched.
func TestSavingsReportRefusalDoesNotChangeReplacement(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(modelConfig)
	h.SeedCache("intent:call_1", "find the bug")
	h.StubHostCall("torana_offload_completion", offloadStub("summary"))
	h.StubHostCall("torana_evaluate_compaction", applyStub(true))
	h.StubHostCall("torana_record_savings", func(string) (string, error) {
		return sdktest.HostResultError(pbv2.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "stub refusal"), nil
	})
	res := h.BeforeRequest(bigToolRequest(bigContent()))
	if res.Err != nil {
		t.Fatalf("a refused savings report must not fail the hook: %v", res.Err)
	}
	if res.Request == nil || res.Request.Messages[3].Content != "summary" {
		t.Fatal("the applied replacement must stand after a refused savings report")
	}
}

// TestNoUnauthorizedCalls — every dispatch's host traffic is within the
// manifest's declared permission set.
func TestNoUnauthorizedCalls(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(modelConfig)
	h.SeedCache("intent:call_1", "find the bug")
	h.StubHostCall("torana_offload_completion", offloadStub("summary"))
	h.StubHostCall("torana_evaluate_compaction", applyStub(true))
	h.BeforeRequest(bigToolRequest(bigContent()))

	// The harness records RAW command tokens; the HOST gates each of them on
	// the env.host_call.<command> grant, which is what the manifest declares.
	allowed := map[string]bool{
		"env.plugin_config":          true,
		"env.cache_get":              true,
		"env.cache_set":              true,
		"env.emit_metric":            true,
		"torana_offload_completion":  true,
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
	h.SeedCache("intent:call_1", "find the bug")
	h.StubHostCall("torana_offload_completion", offloadStub("summary"))
	h.StubHostCall("torana_evaluate_compaction", applyStub(true))
	res := h.BeforeRequest(bigToolRequest(bigContent()))
	if res.Err != nil || res.Request == nil {
		t.Fatalf("row 1 should apply, err=%v", res.Err)
	}
	// Row 2: expected_applications=0 disables the model path entirely.
	h2 := newHarness(t)
	h2.SetConfig(`{"tool_policies":[{"match":"read*","mode":"model"}],"expected_applications":0}`)
	h2.SeedCache("intent:call_1", "find the bug")
	h2.StubHostCall("torana_offload_completion", offloadStub("summary"))
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
	h.SeedCache("intent:call_1", "find the bug")
	h.StubHostCall("torana_offload_completion", offloadStub("summary"))
	h.StubHostCall("torana_evaluate_compaction", applyStub(true))
	res := h.BeforeRequest(bigToolRequest(bigContent()))
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if !res.PassedThrough {
		t.Fatal("expected_applications=0 must disable the model path")
	}
	if n := countCommand(h, "torana_offload_completion"); n != 0 {
		t.Fatalf("offload ran with the model path disabled: %d calls", n)
	}
}

// TestModelConsumptionGate — model summaries never precede one exact
// consumption of the result.
func TestModelConsumptionGate(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(modelConfig)
	h.SeedCache("intent:call_1", "find the bug")
	h.StubHostCall("torana_offload_completion", offloadStub("summary"))
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

func mustJSON(t *testing.T, req *pbv2.ChatRequest) []byte {
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
// recomputed through offload.
func TestModelPresentEmptyReplacementRecomputes(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(modelConfig)
	h.SeedCache("intent:call_1", "find the bug")
	content := bigContent()
	modelKey := sdk.ContentAddressedCacheKey(compactionCache, "v3", "read", `{"path":"server.go"}`, content, "find the bug", "model")
	h.SeedCache(modelKey, "") // present, empty
	h.StubHostCall("torana_offload_completion", offloadStub("summary"))
	h.StubHostCall("torana_evaluate_compaction", applyStub(true))
	res := h.BeforeRequest(bigToolRequest(content))
	if res.Err != nil || res.Request == nil {
		t.Fatalf("expected a recomputed replacement, err=%v", res.Err)
	}
	if res.Request.Messages[3].Content != "summary" {
		t.Fatalf("present-empty cache value must recompute, got %q", res.Request.Messages[3].Content)
	}
	if n := countCommand(h, "torana_offload_completion"); n != 1 {
		t.Fatalf("offload must run for a present-empty cache value, got %d", n)
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
	policyKey := sdk.ContentAddressedCacheKey(policyCompactionCache, "v2", "read", `{"path":"server.go"}`, content, "deterministic", "")
	h.SeedCache(policyKey, "") // present, empty
	res := h.BeforeRequest(bigToolRequest(content))
	if res.Err != nil || res.Request == nil {
		t.Fatalf("expected a recomputed replacement, err=%v", res.Err)
	}
	if res.Request.Messages[3].Content == content {
		t.Fatal("present-empty policy cache value must recompute, not pass through")
	}
	if res.Request.Messages[3].Content == "" {
		t.Fatal("present-empty cache value must never be applied (would erase the result)")
	}
}

// TestIntentCacheMalformedReplyErrors — a malformed HostCallResult on the
// intent read is a protocol error: the hook errors.
func TestIntentCacheMalformedReplyErrors(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(modelConfig)
	h.StubHostCall("env.cache_get", func(string) (string, error) {
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
	h.SeedCache("intent:call_1", "find the bug")
	h.StubHostCall("env.cache_get", func(args string) (string, error) {
		if strings.Contains(args, "intent:call_1") {
			return sdktest.HostResultValue([]byte("find the bug")), nil
		}
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
// economic gate declines the batch; the preflight fails so no offload spend
// happens, and evaluate is called exactly once.
func TestEvaluateAdvisoryRefusalDeclinesWithoutRetry(t *testing.T) {
	for _, code := range []pbv2.ErrorCode{
		pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED,
		pbv2.ErrorCode_ERROR_CODE_UNAVAILABLE,
	} {
		t.Run(code.String(), func(t *testing.T) {
			h := newHarness(t)
			h.SetConfig(modelConfig)
			h.SeedCache("intent:call_1", "find the bug")
			h.StubHostCall("torana_offload_completion", offloadStub("summary"))
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
			if n := countCommand(h, "torana_offload_completion"); n != 0 {
				t.Fatalf("offload ran despite a declined preflight: %d calls", n)
			}
		})
	}
}

// TestEvaluateContractRefusalErrors — INVALID_ARGUMENT on the economic gate
// is a contract defect: the hook errors.
func TestEvaluateContractRefusalErrors(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(modelConfig)
	h.SeedCache("intent:call_1", "find the bug")
	h.StubHostCall("torana_offload_completion", offloadStub("summary"))
	h.StubHostCall("torana_evaluate_compaction", func(string) (string, error) {
		return sdktest.HostResultError(pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "stub"), nil
	})
	res := h.BeforeRequest(bigToolRequest(bigContent()))
	if res.Err == nil {
		t.Fatal("a contract refusal on the economic gate must error the hook")
	}
}

// TestRealEvaluationDeclinesAfterPreflight — the preflight approves, the
// real evaluation declines: offload spent at most once and NO mutation is
// applied (a declined batch never half-applies).
func TestRealEvaluationDeclinesAfterPreflight(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(modelConfig)
	h.SeedCache("intent:call_1", "find the bug")
	h.StubHostCall("torana_offload_completion", offloadStub("summary"))
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
	if n := countCommand(h, "torana_offload_completion"); n != 1 {
		t.Fatalf("offload spend=%d, want exactly 1 (preflight approved once)", n)
	}
}

// TestRealEvaluationRefusalAfterPreflight — preflight approves, the real
// evaluation contract-refuses: hook error, no mutation, offload spent once.
func TestRealEvaluationRefusalAfterPreflight(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(modelConfig)
	h.SeedCache("intent:call_1", "find the bug")
	h.StubHostCall("torana_offload_completion", offloadStub("summary"))
	seq := 0
	h.StubHostCall("torana_evaluate_compaction", func(string) (string, error) {
		seq++
		if seq == 1 {
			return sdktest.HostResultValue([]byte(`{"apply":true}`)), nil
		}
		return sdktest.HostResultError(pbv2.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "stub"), nil
	})
	res := h.BeforeRequest(bigToolRequest(bigContent()))
	if res.Err == nil {
		t.Fatal("a contract refusal on the real evaluation must error the hook")
	}
	if n := countCommand(h, "torana_offload_completion"); n != 1 {
		t.Fatalf("offload spend=%d, want exactly 1", n)
	}
}

// TestSchemaDefaultsMatchRuntimeDefaults — parity against schema.json itself,
// so a schema/default drift cannot pass: the schema's defaults must equal the
// runtime defaults (0 unbounded / 0 disables the model path / empty policies),
// and the budget field must be named max_offload_input_bytes.
func TestSchemaDefaultsMatchRuntimeDefaults(t *testing.T) {
	raw, err := os.ReadFile("schema.json")
	if err != nil {
		t.Fatalf("read schema.json: %v", err)
	}
	var schema struct {
		Properties map[string]struct {
			Default json.RawMessage `json:"default"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("parse schema.json: %v", err)
	}
	prop, ok := schema.Properties["max_offload_input_bytes"]
	if !ok {
		t.Fatal("schema.json has no max_offload_input_bytes property (legacy name would drift)")
	}
	if string(prop.Default) != "0" {
		t.Fatalf("schema max_offload_input_bytes default=%s, want 0", prop.Default)
	}
	if string(schema.Properties["expected_applications"].Default) != "0" {
		t.Fatalf("schema expected_applications default=%s, want 0", schema.Properties["expected_applications"].Default)
	}
	if string(schema.Properties["tool_policies"].Default) != "[]" {
		t.Fatalf("schema tool_policies default=%s, want []", schema.Properties["tool_policies"].Default)
	}

	// Runtime defaults must match: no config -> inert (0/0/nil).
	rt := parseConfig("")
	if rt.MaxOffloadInputBytes != 0 || rt.ExpectedApplications != 0 || len(rt.ToolPolicies) != 0 {
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
	key := sdk.ContentAddressedCacheKey(policyCompactionCache, "v2", "read", args, content, "deterministic", "")
	h := newHarness(t)
	h.SetConfig(cfg)
	h.SeedCache(key, content+"extra bytes making the cached value non-shorter")
	res := h.BeforeRequest(bigToolRequest(content))
	if res.Err != nil || res.Request == nil {
		t.Fatalf("expected a recomputed replacement, err=%v", res.Err)
	}
	if res.Request.Messages[3].Content == content+"extra bytes making the cached value non-shorter" {
		t.Fatal("a non-shorter cached value must never be applied")
	}
	if res.Request.Messages[3].Content == content {
		t.Fatal("a non-shorter cached value must be recomputed, not left untouched")
	}
}
