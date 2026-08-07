package main

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pbv2 "github.com/torana-edge/torana-plugin-sdk/pb/v2"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
	"google.golang.org/protobuf/proto"
)

// ==========================================================================
// Pure helper contracts
// ==========================================================================

// TestWorthwhileReductionBoundaries pins the gate math: final must be
// STRICTLY fewer than the removed bytes — 2000->1000 is a rounding-ambiguous
// boundary and rejects; 2001->1000 applies.
func TestWorthwhileReductionBoundaries(t *testing.T) {
	for _, c := range []struct {
		original, final int
		want            bool
	}{
		{2000, 1000, false},
		{2001, 1000, true},
		{2000, 999, true},
		{2001, 1001, false},
		{1999, 1000, false},
		{10000, 5000, false},
		{10000, 4999, true},
		{10000, 5001, false},
	} {
		if got := worthwhileReduction(c.original, c.final); got != c.want {
			t.Errorf("worthwhileReduction(%d, %d)=%v, want %v", c.original, c.final, got, c.want)
		}
	}
}

// multibyte builds a string of n bytes made entirely of 3-byte runes, so
// almost every byte index is a mid-rune index.
func multibyte(n int) string {
	var b strings.Builder
	for b.Len() < n {
		b.WriteString("日") // 3 bytes
	}
	return b.String()
}

// TestTruncateHeadTailHonestForEveryBudget — the promised total budget (n,
// notice included) holds for every input, including budgets too small to fit
// the exact notice (a rune-safe raw prefix is returned, no fabricated notice),
// and the result is always valid UTF-8 and never larger than the input.
func TestTruncateHeadTailHonestForEveryBudget(t *testing.T) {
	// Tiny budgets 1..128 sweep the notice-cannot-fit zone; realistic
	// budgets cover the normal path; multibyte forces rune-boundary back-off.
	budgets := make([]int, 0, 128+8)
	for n := 1; n <= 128; n++ {
		budgets = append(budgets, n)
	}
	budgets = append(budgets, 1999, 2000, 2001, 2042, 3000, 4000, 4001, 10000)

	inputs := []string{
		strings.Repeat("a", 10000),
		multibyte(6000),
		strings.Repeat("line\n", 2000),
	}
	for _, n := range budgets {
		for _, content := range inputs {
			got := truncateHeadTail(content, n)
			if !utf8.ValidString(got) {
				t.Fatalf("n=%d: invalid UTF-8", n)
			}
			if len(got) > n {
				t.Fatalf("n=%d: output %d bytes exceeds the total budget", n, len(got))
			}
			if len(got) > len(content) {
				t.Fatalf("n=%d: truncation grew the input (%d -> %d)", n, len(content), len(got))
			}
			if len(content) <= n {
				if got != content {
					t.Fatalf("n=%d: content within budget was modified", n)
				}
				continue
			}
			// When the notice is present it must be the exact one and its
			// count must match what was actually removed.
			if _, rest, ok := strings.Cut(got, "\n\n... ["); ok {
				claimed, _, _ := strings.Cut(rest, " bytes truncated by Torana]")
				head, _, _ := strings.Cut(got, "\n\n... [")
				_, tail, _ := strings.Cut(head, "] ...\n\n")
				_ = tail
				headPart, _, _ := strings.Cut(got, "\n\n... [")
				_, tailPart, _ := strings.Cut(got, "\n\n... [")
				_, tailPart, _ = strings.Cut(tailPart, "] ...\n\n")
				want := len(content) - len(headPart) - len(tailPart)
				if claimed != strconv.Itoa(want) {
					t.Fatalf("n=%d: notice claims %s removed, actually %d", n, claimed, want)
				}
			}
			// Small budgets may not fit the notice: then a raw prefix is the
			// contract (never a partial/fabricated notice).
			if n < len(truncationNotice(len(content))) && strings.Contains(got, "truncated by Torana") {
				t.Fatalf("n=%d: notice rendered where it cannot fit exactly", n)
			}
		}
	}
}

// TestTruncatedContentSurvivesProtoMarshal is the regression test for the
// trap: the truncated string going into a proto3 string field. Sweep budgets
// so the cut lands at every offset modulo the rune width.
func TestTruncatedContentSurvivesProtoMarshal(t *testing.T) {
	for n := 100; n <= 120; n++ {
		got := truncateHeadTail(multibyte(6000), n)
		msg := toolMsg("c", "read", got)
		if _, err := proto.Marshal(msg); err != nil {
			t.Fatalf("n=%d: proto.Marshal rejected the truncated content: %v", n, err)
		}
	}
}

// TestSelectionBoundedAndDeterministic — selectKeywordLines hard-caps at
// maxKeepLines unique lines, ties rank score-desc then index-asc, output is
// rebuilt in original order, and repeated calls over identical inputs return
// byte-identical selections (prompt-cache determinism when the cap binds).
func TestSelectionBoundedAndDeterministic(t *testing.T) {
	// Tie-heavy: 300 lines, every 3rd line carries the same keyword -> 100
	// candidate lines, each dragging a 5-line window -> way over 200 without
	// the hard cap.
	var lines []string
	for i := 0; i < 300; i++ {
		if i%3 == 0 {
			lines = append(lines, "MATCH tie keyword line")
		} else {
			lines = append(lines, "noise")
		}
	}
	keywords := []string{"tie", "keyword"}
	keep := selectKeywordLines(lines, keywords)
	if len(keep) > maxKeepLines {
		t.Fatalf("selection kept %d lines, hard cap is %d", len(keep), maxKeepLines)
	}
	if len(keep) == 0 {
		t.Fatal("selection empty")
	}
	// Repeat determinism: identical inputs -> identical selection.
	keep2 := selectKeywordLines(lines, keywords)
	if len(keep) != len(keep2) {
		t.Fatalf("selection differs across identical inputs: %d vs %d", len(keep), len(keep2))
	}
	for i := range keep {
		if !keep2[i] {
			t.Fatal("selection differs across identical inputs")
		}
	}
	// The kept set is exactly maxKeepLines when the cap binds.
	if len(keep) != maxKeepLines {
		t.Fatalf("cap-bounded selection kept %d, want %d", len(keep), maxKeepLines)
	}
	// Deterministic ranking: score-desc, index-asc — the highest-scoring
	// lines are included before lower-scoring ones.
	scored := selectKeywordLines([]string{
		"low one", "HIGH two", "HIGH three", "low four",
	}, []string{"high"})
	if !scored[1] || !scored[2] {
		t.Fatal("score-desc ranking did not select the high-scoring lines")
	}
}

// ==========================================================================
// Hook-level matrix (sdktest; the plugin registers in init())
// ==========================================================================

// newHarness resets the process-global config once-state so every hook-level
// row starts from defaults, then builds a fresh fake host. Tests never run in
// parallel: the plugin's config globals are process-wide.
func newHarness(t *testing.T) *sdktest.Harness {
	t.Helper()
	resetConfigForTest()
	return sdktest.New(t)
}

const keywordCfg = `{"tool_policies":[{"match":"read*","mode":"keyword"}]}`
const deterministicCfg = `{"tool_policies":[{"match":"read*","mode":"deterministic","first_pass":true}]}`

// keywordContent builds a 300-line tool result with one strongly matching
// line in the middle.
func keywordContent() string {
	var b strings.Builder
	for i := 0; i < 300; i++ {
		if i == 150 {
			b.WriteString("MATCH bug server.go: the failure is in the retry loop\n")
			continue
		}
		b.WriteString("noise line with enough padding to clear the eligibility floor for compaction\n")
	}
	return b.String()
}

// toolMsg builds an ordered tool-role message with ONE tool-result block
// carrying a single text arm.
func toolMsg(id, name, content string) *pbv2.Message {
	return &pbv2.Message{Role: "tool", Blocks: []*pbv2.RequestBlock{{Kind: &pbv2.RequestBlock_ToolResult{ToolResult: &pbv2.RequestToolResultBlock{
		ToolCallId: id,
		ToolName:   name,
		Content:    []*pbv2.ToolResultContentBlock{{Kind: &pbv2.ToolResultContentBlock_Text{Text: &pbv2.ToolResultTextBlock{Text: content}}}},
	}}}}}
}

// toolText returns the first text arm of the first tool-result block in
// message mi.
func toolText(t *testing.T, req *pbv2.ChatRequest, mi int) string {
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

func bigToolRequest(content string) *pbv2.ChatRequest {
	return &pbv2.ChatRequest{
		Messages: []*pbv2.Message{
			{Role: "system", Blocks: []*pbv2.RequestBlock{{Kind: &pbv2.RequestBlock_Text{Text: &pbv2.RequestTextBlock{Text: "You are a coding agent."}}}}},
			{Role: "user", Blocks: []*pbv2.RequestBlock{{Kind: &pbv2.RequestBlock_Text{Text: &pbv2.RequestTextBlock{Text: "find the bug"}}}}},
			{Role: "assistant", Blocks: []*pbv2.RequestBlock{{Kind: &pbv2.RequestBlock_ToolUse{ToolUse: &pbv2.RequestToolUseBlock{Id: "call_1", Name: "read", ArgumentsJson: []byte(`{"path":"server.go"}`)}}}}},
			toolMsg("call_1", "read", content),
			{Role: "user", Blocks: []*pbv2.RequestBlock{{Kind: &pbv2.RequestBlock_Text{Text: &pbv2.RequestTextBlock{Text: "now fix it"}}}}},
			{Role: "assistant", Blocks: []*pbv2.RequestBlock{{Kind: &pbv2.RequestBlock_Text{Text: &pbv2.RequestTextBlock{Text: "the fix is in server.go"}}}}},
		},
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
	for _, m := range h.Metrics() {
		if m.Name == name {
			return true
		}
	}
	return false
}

// TestDefaultConfigIsInert — schema default ([] policies) means pass-through
// with no host traffic beyond plugin_config.
func TestDefaultConfigIsInert(t *testing.T) {
	h := newHarness(t)
	res := h.BeforeRequest(bigToolRequest(keywordContent()))
	if res.Err != nil || !res.PassedThrough {
		t.Fatalf("expected pass-through, err=%v", res.Err)
	}
	for _, c := range h.Calls() {
		if c.Command != "env.plugin_config" {
			t.Errorf("default-config dispatch made an unexpected host call: %s", c.Command)
		}
	}
}

// TestEligibleMetricFiresBeforeModeGates — deterministic and keyword
// candidates emit torana_compact_eligible_total BEFORE their consumption
// gates, and source candidates emit it too (matched non-exact) before the
// fail-closed skip.
func TestEligibleMetricFiresBeforeModeGates(t *testing.T) {
	// Deterministic without first_pass, no assistant consumption after the
	// result -> gate skips, but eligible fired.
	h := newHarness(t)
	h.SetConfig(`{"tool_policies":[{"match":"read*","mode":"deterministic"}]}`)
	req := bigToolRequest(keywordContent())
	req.Messages = req.Messages[:5] // drop the trailing assistant
	res := h.BeforeRequest(req)
	if res.Err != nil || !res.PassedThrough {
		t.Fatalf("expected the gate to skip, err=%v", res.Err)
	}
	if !hasMetric(h, "torana_compact_eligible_total") {
		t.Fatal("deterministic candidate must emit the eligible metric before the gate")
	}

	// Source mode: eligible fires, then fail-closed skip.
	h2 := newHarness(t)
	h2.SetConfig(`{"tool_policies":[{"match":"read*","mode":"source"}]}`)
	res2 := h2.BeforeRequest(bigToolRequest(keywordContent()))
	if res2.Err != nil || !res2.PassedThrough {
		t.Fatalf("source mode must fail closed, err=%v", res2.Err)
	}
	if !hasMetric(h2, "torana_compact_eligible_total") {
		t.Fatal("source candidate must emit the eligible metric")
	}

	// Keyword with no consumption -> gate skips after eligible.
	h3 := newHarness(t)
	h3.SetConfig(keywordCfg)
	req3 := bigToolRequest(keywordContent())
	req3.Messages = req3.Messages[:5]
	res3 := h3.BeforeRequest(req3)
	if res3.Err != nil || !res3.PassedThrough {
		t.Fatalf("expected the keyword gate to skip, err=%v", res3.Err)
	}
	if !hasMetric(h3, "torana_compact_eligible_total") {
		t.Fatal("keyword candidate must emit the eligible metric before the gate")
	}
}

// TestExactAndUnsafeNeverEmit — exact mode and ToolResultMustStayExact
// candidates emit nothing.
func TestExactAndUnsafeNeverEmit(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(`{"tool_policies":[{"match":"read*","mode":"exact"}]}`)
	res := h.BeforeRequest(bigToolRequest(keywordContent()))
	if res.Err != nil || !res.PassedThrough {
		t.Fatalf("exact mode must pass through, err=%v", res.Err)
	}
	if hasMetric(h, "torana_compact_eligible_total") {
		t.Fatal("exact mode must not emit the eligible metric")
	}

	h2 := newHarness(t)
	h2.SetConfig(`{"tool_policies":[{"match":"*","mode":"keyword"}]}`)
	req := bigToolRequest(keywordContent())
	req.Messages[3].Blocks[0].GetToolResult().ToolName = "edit_file"
	res2 := h2.BeforeRequest(req)
	if res2.Err != nil || !res2.PassedThrough {
		t.Fatalf("unsafe tool must pass through, err=%v", res2.Err)
	}
	if hasMetric(h2, "torana_compact_eligible_total") {
		t.Fatal("ToolResultMustStayExact must not emit the eligible metric")
	}
}

// TestDeterministicFirstPassAppliesAndCachesThenReuses — first_pass applies,
// caches, and a second dispatch over a fresh clone of the ORIGINAL request
// hits the cache (no CacheSet on turn 2, byte-identical output).
func TestDeterministicFirstPassAppliesAndCachesThenReuses(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(deterministicCfg)
	original := bigToolRequest(keywordContent())
	first := h.BeforeRequest(proto.Clone(original).(*pbv2.ChatRequest))
	if first.Err != nil || first.Request == nil {
		t.Fatalf("expected a replacement on turn 1, err=%v", first.Err)
	}
	if toolText(t, first.Request, 3) == toolText(t, original, 3) {
		t.Fatal("turn 1 did not replace the tool result")
	}
	if countCommand(h, "env.cache_set") != 1 {
		t.Fatalf("turn 1 must cache the replacement, cache_set calls=%d", countCommand(h, "env.cache_set"))
	}

	before := countCommand(h, "env.cache_set")
	second := h.BeforeRequest(proto.Clone(original).(*pbv2.ChatRequest))
	if second.Err != nil || second.Request == nil {
		t.Fatalf("expected a replacement on turn 2, err=%v", second.Err)
	}
	if toolText(t, first.Request, 3) != toolText(t, second.Request, 3) {
		t.Fatal("turn 2 output differs from turn 1")
	}
	if countCommand(h, "env.cache_set") != before {
		t.Fatal("turn 2 wrote the cache — the replacement must have been REUSED")
	}
}

// TestDeterministicConsumptionGate — without first_pass, no replacement until
// an assistant message follows the result.
func TestDeterministicConsumptionGate(t *testing.T) {
	cfg := `{"tool_policies":[{"match":"read*","mode":"deterministic"}]}`
	h := newHarness(t)
	h.SetConfig(cfg)
	req := bigToolRequest(keywordContent())
	req.Messages = req.Messages[:5]
	res := h.BeforeRequest(req)
	if res.Err != nil || !res.PassedThrough {
		t.Fatalf("gate must skip, err=%v", res.Err)
	}

	h2 := newHarness(t)
	h2.SetConfig(cfg)
	res2 := h2.BeforeRequest(bigToolRequest(keywordContent()))
	if res2.Err != nil || res2.Request == nil {
		t.Fatalf("expected replacement after a consumption, err=%v", res2.Err)
	}
	if toolText(t, res2.Request, 3) == keywordContent() {
		t.Fatal("deterministic policy did not replace the consumed result")
	}
}

// TestDeterministicUnusableCachesRecompute — present-empty and non-shorter
// cached values are recomputed locally; the applied output is the computed
// replacement, never the cached value.
func TestDeterministicUnusableCachesRecompute(t *testing.T) {
	content := keywordContent()
	args := `{"path":"server.go"}`
	for name, seed := range map[string]string{
		"present-empty": "",
		"non-shorter":   content + "extra bytes to exceed the original",
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.SetConfig(deterministicCfg)
			key := sdk.ContentAddressedCacheKey(policyCompactionCache, "v2", "read", args, content, "deterministic", "")
			h.SeedCache(key, seed)
			res := h.BeforeRequest(bigToolRequest(content))
			if res.Err != nil || res.Request == nil {
				t.Fatalf("expected a recomputed replacement, err=%v", res.Err)
			}
			if toolText(t, res.Request, 3) == seed {
				t.Fatal("an unusable cached value must never be applied")
			}
			if toolText(t, res.Request, 3) == content {
				t.Fatal("an unusable cached value must be recomputed, not left untouched")
			}
		})
	}
}

// TestKeywordHappyPath — seeded intent, cache miss: the selection is applied
// (a genuine >50% reduction), cached, and reported as a transformation.
func TestKeywordHappyPath(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(keywordCfg)
	h.SeedCache("intent:call_1", "find the bug in server")
	res := h.BeforeRequest(bigToolRequest(keywordContent()))
	if res.Err != nil || res.Request == nil {
		t.Fatalf("expected a replacement, err=%v", res.Err)
	}
	got := toolText(t, res.Request, 3)
	if got == keywordContent() {
		t.Fatal("keyword compaction did not apply")
	}
	if !strings.Contains(got, "MATCH bug server.go") {
		t.Fatalf("the selected evidence is missing: %q", got)
	}
	if !worthwhileReduction(len(keywordContent()), len(got)) {
		t.Fatalf("applied result is not a >50%% reduction: %d -> %d", len(keywordContent()), len(got))
	}
	if countCommand(h, "env.cache_set") != 1 {
		t.Fatalf("transformation must be cached, cache_set calls=%d", countCommand(h, "env.cache_set"))
	}
	if countCommand(h, "torana_record_savings") != 1 {
		t.Fatal("transformation must report savings")
	}
	if !hasMetric(h, "torana_compact_eligible_total") {
		t.Fatal("eligible metric missing")
	}
}

// TestKeywordCacheReuse — a cached value that is non-empty AND shorter is
// reused without recompute; the reuse turn writes no cache.
func TestKeywordCacheReuse(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(keywordCfg)
	h.SeedCache("intent:call_1", "find the bug in server")
	content := keywordContent()
	key := sdk.ContentAddressedCacheKey(keywordCompactionCache, "v2", "read", `{"path":"server.go"}`, content, "find the bug in server", "keyword")
	h.SeedCache(key, "MATCH bug server.go: the failure is in the retry loop")
	res := h.BeforeRequest(bigToolRequest(content))
	if res.Err != nil || res.Request == nil {
		t.Fatalf("expected cache reuse, err=%v", res.Err)
	}
	if toolText(t, res.Request, 3) != "MATCH bug server.go: the failure is in the retry loop" {
		t.Fatalf("cached value not reused: %q", toolText(t, res.Request, 3))
	}
	if countCommand(h, "env.cache_set") != 0 {
		t.Fatal("a reuse turn must not rewrite the cache")
	}
}

// TestKeywordUnusableCachesRecompute — present-empty and non-shorter keyword
// cache values are recomputed; the applied output is the computed selection.
func TestKeywordUnusableCachesRecompute(t *testing.T) {
	content := keywordContent()
	key := sdk.ContentAddressedCacheKey(keywordCompactionCache, "v2", "read", `{"path":"server.go"}`, content, "find the bug in server", "keyword")
	for name, seed := range map[string]string{
		"present-empty": "",
		"non-shorter":   content + "extra",
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.SetConfig(keywordCfg)
			h.SeedCache("intent:call_1", "find the bug in server")
			h.SeedCache(key, seed)
			res := h.BeforeRequest(bigToolRequest(content))
			if res.Err != nil || res.Request == nil {
				t.Fatalf("expected a recomputed replacement, err=%v", res.Err)
			}
			if toolText(t, res.Request, 3) == seed {
				t.Fatal("an unusable cached value must never be applied")
			}
			if !strings.Contains(toolText(t, res.Request, 3), "MATCH bug server.go") {
				t.Fatal("an unusable cached value must be recomputed from the selection")
			}
		})
	}
}

// TestKeywordNoEvidenceUntouched — no keywords, too-few lines, and no scored
// lines all leave the result untouched.
func TestKeywordNoEvidenceUntouched(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(keywordCfg)
	h.SeedCache("intent:call_1", "the")
	res := h.BeforeRequest(bigToolRequest(keywordContent()))
	if res.Err != nil || !res.PassedThrough {
		t.Fatalf("stopword-only intent must pass through, err=%v", res.Err)
	}

	// Too few lines.
	h2 := newHarness(t)
	h2.SetConfig(keywordCfg)
	h2.SeedCache("intent:call_1", "find the bug in server")
	short := strings.Repeat("x", 2500)
	short += "\nMATCH bug here\n"
	res2 := h2.BeforeRequest(bigToolRequest(short))
	if res2.Err != nil || !res2.PassedThrough {
		t.Fatalf("<=50-line output must pass through, err=%v", res2.Err)
	}
}

// TestKeywordConsumptionGate — no assistant consumption after the result:
// eligible fires, nothing compacts.
func TestKeywordConsumptionGate(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(keywordCfg)
	h.SeedCache("intent:call_1", "find the bug in server")
	req := bigToolRequest(keywordContent())
	req.Messages = req.Messages[:5]
	res := h.BeforeRequest(req)
	if res.Err != nil || !res.PassedThrough {
		t.Fatalf("gate must skip, err=%v", res.Err)
	}
	if !hasMetric(h, "torana_compact_eligible_total") {
		t.Fatal("eligible must fire before the consumption gate")
	}
}

// TestIntentMissSkipsWithMetric — NOT_FOUND and present-empty intents are both
// unusable: skip + torana_intent_missing_total.
func TestIntentMissSkipsWithMetric(t *testing.T) {
	for _, name := range []string{"absent", "present-empty"} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.SetConfig(keywordCfg)
			if name == "present-empty" {
				h.SeedCache("intent:call_1", "")
			}
			res := h.BeforeRequest(bigToolRequest(keywordContent()))
			if res.Err != nil || !res.PassedThrough {
				t.Fatalf("no usable intent, nothing may change, err=%v", res.Err)
			}
			if !hasMetric(h, "torana_intent_missing_total") {
				t.Fatal("missing torana_intent_missing_total metric")
			}
		})
	}
}

// TestCacheRefusalsError — PERMISSION_DENIED on any of the three reads is a
// contract defect: the hook errors.
func TestCacheRefusalsError(t *testing.T) {
	for _, tc := range []struct{ name, cfg, command string }{
		{"intent read", keywordCfg, "env.shared_cache_get"},
		{"keyword read", keywordCfg, "env.cache_get"},
		{"policy read", deterministicCfg, "env.cache_get"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.SetConfig(tc.cfg)
			h.SeedCache("intent:call_1", "find the bug in server")
			h.DenyPermission(tc.command)
			res := h.BeforeRequest(bigToolRequest(keywordContent()))
			if res.Err == nil {
				t.Fatal("a refused cache read must error the hook")
			}
		})
	}
}

// TestMalformedRepliesError — malformed HostCallResult frames on any of the
// three reads error the hook.
func TestMalformedRepliesError(t *testing.T) {
	for _, tc := range []struct{ name, cfg, command string }{
		{"intent read", keywordCfg, "env.shared_cache_get"},
		{"policy read", deterministicCfg, "env.cache_get"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.SetConfig(tc.cfg)
			h.StubHostCall(tc.command, func(string) (string, error) {
				return "not a host-call-result frame", nil
			})
			res := h.BeforeRequest(bigToolRequest(keywordContent()))
			if res.Err == nil {
				t.Fatal("a malformed cache reply must error the hook")
			}
		})
	}
	// Keyword read with a successful intent read.
	h := newHarness(t)
	h.SetConfig(keywordCfg)
	h.SeedCache("intent:call_1", "find the bug in server")
	h.StubHostCall("env.cache_get", func(string) (string, error) {
		return "not a host-call-result frame", nil
	})
	res := h.BeforeRequest(bigToolRequest(keywordContent()))
	if res.Err == nil {
		t.Fatal("a malformed keyword-cache reply must error the hook")
	}
}

// TestBestEffortWritesAndSavings — refused cache writes and refused savings
// reports leave the applied replacement intact.
func TestBestEffortWritesAndSavings(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(keywordCfg)
	h.SeedCache("intent:call_1", "find the bug in server")
	h.DenyPermission("env.cache_set")
	h.StubHostCall("torana_record_savings", func(string) (string, error) {
		return sdktest.HostResultError(pbv2.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "stub"), nil
	})
	res := h.BeforeRequest(bigToolRequest(keywordContent()))
	if res.Err != nil {
		t.Fatalf("best-effort refusals must not fail the hook: %v", res.Err)
	}
	if res.Request == nil {
		t.Fatal("expected the replacement to be applied")
	}
	if !strings.Contains(toolText(t, res.Request, 3), "MATCH bug server.go") {
		t.Fatal("the applied replacement must stand despite refused writes/reports")
	}
}

// TestMinContentLengthBoundary — 1999 bytes is not eligible; 2000 is.
func TestMinContentLengthBoundary(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(keywordCfg)
	res := h.BeforeRequest(bigToolRequest(strings.Repeat("x", 1999)))
	if res.Err != nil || !res.PassedThrough {
		t.Fatalf("1999 bytes must not be eligible, err=%v", res.Err)
	}
	if hasMetric(h, "torana_compact_eligible_total") {
		t.Fatal("1999 bytes must not emit the eligible metric")
	}

	h2 := newHarness(t)
	h2.SetConfig(keywordCfg)
	res2 := h2.BeforeRequest(bigToolRequest(strings.Repeat("x", 2000)))
	if res2.Err != nil {
		t.Fatal(res2.Err)
	}
	if !hasMetric(h2, "torana_compact_eligible_total") {
		t.Fatal("2000 bytes must emit the eligible metric")
	}
}

// TestOversizedSelectionTruncatesSelected — matches only in the middle of a
// large output: the selected result exceeds 8000 bytes, so it is truncated —
// the SELECTED output, never the original. The result retains matched
// evidence, contains no unrelated original-only head/tail, and stays within
// the 8000-byte total budget.
func TestOversizedSelectionTruncatesSelected(t *testing.T) {
	var b strings.Builder
	// 300 lines; lines 100..199 are long MATCH lines (middle-only evidence).
	for i := 0; i < 300; i++ {
		if i >= 100 && i < 200 {
			b.WriteString("MATCH evidence line with lots of detail about the bug and the server internals here " + strings.Repeat("z", 80) + "\n")
			continue
		}
		b.WriteString("noise line " + strconv.Itoa(i) + " with padding to keep the output long enough for eligibility\n")
	}
	content := b.String()
	if len(content) < minContentLength {
		t.Fatalf("fixture too small: %d", len(content))
	}

	h := newHarness(t)
	h.SetConfig(keywordCfg)
	h.SeedCache("intent:call_1", "find the bug in server")
	res := h.BeforeRequest(bigToolRequest(content))
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	got := toolText(t, res.Request, 3)
	if len(got) > maxResultBytes {
		t.Fatalf("oversized selection exceeded the %d-byte cap: %d", maxResultBytes, len(got))
	}
	if !strings.Contains(got, "MATCH evidence") {
		t.Fatal("truncation lost the selected evidence")
	}
	if strings.Contains(got, "noise line 0 ") && !strings.Contains(got, "MATCH evidence") {
		t.Fatal("fallback returned unrelated original head")
	}
	// The original's head/tail (far from any match) must not appear; context
	// noise NEAR a match is legitimately part of the selection.
	if strings.Contains(got, "noise line 0 ") {
		t.Fatal("fallback returned the original's unrelated head")
	}
	if strings.Contains(got, "noise line 299") {
		t.Fatal("fallback returned the original's unrelated tail")
	}
	matchedEvidence := strings.Count(got, "MATCH evidence")
	if matchedEvidence == 0 {
		t.Fatal("no selected evidence survived the cap")
	}
}

// TestNoUnauthorizedCalls — every dispatch's host traffic is within the
// declared permission set.
func TestNoUnauthorizedCalls(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(keywordCfg)
	h.SeedCache("intent:call_1", "find the bug in server")
	h.BeforeRequest(bigToolRequest(keywordContent()))

	allowed := map[string]bool{
		"env.plugin_config":     true,
		"env.cache_get":         true,
		"env.cache_set":         true,
		"env.shared_cache_get":  true,
		"env.emit_metric":       true,
		"torana_record_savings": true,
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
	h := newHarness(t)
	h.SetConfig(keywordCfg)
	h.SeedCache("intent:call_1", "find the bug in server")
	res := h.BeforeRequest(bigToolRequest(keywordContent()))
	if res.Err != nil || res.Request == nil {
		t.Fatalf("row 1 should apply, err=%v", res.Err)
	}

	h2 := newHarness(t) // resets config -> default (no policies)
	res2 := h2.BeforeRequest(bigToolRequest(keywordContent()))
	if res2.Err != nil || !res2.PassedThrough {
		t.Fatalf("row 2 leaked row 1's policies, err=%v", res2.Err)
	}
}

// TestSchemaDefaultsMatchRuntimeDefaults — parity against schema.json: the
// schema's tool_policies default must equal the runtime default ([]).
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
	prop, ok := schema.Properties["tool_policies"]
	if !ok {
		t.Fatal("schema.json has no tool_policies property")
	}
	if string(prop.Default) != "[]" {
		t.Fatalf("schema tool_policies default=%s, want []", prop.Default)
	}
	rt := parseConfig("")
	if len(rt.ToolPolicies) != 0 {
		t.Fatalf("runtime defaults %+v do not match the schema", rt)
	}
}

// TestDeterminismOverIdenticalRequests — two identical dispatches from fresh
// harnesses produce byte-identical output (prompt-cache determinism).
func TestDeterminismOverIdenticalRequests(t *testing.T) {
	h1 := newHarness(t)
	h1.SetConfig(keywordCfg)
	h1.SeedCache("intent:call_1", "find the bug in server")
	r1 := h1.BeforeRequest(bigToolRequest(keywordContent()))
	h2 := newHarness(t)
	h2.SetConfig(keywordCfg)
	h2.SeedCache("intent:call_1", "find the bug in server")
	r2 := h2.BeforeRequest(bigToolRequest(keywordContent()))
	if r1.Err != nil || r2.Err != nil {
		t.Fatalf("dispatch errors: %v %v", r1.Err, r2.Err)
	}
	b1, _ := json.Marshal(r1.Request)
	b2, _ := json.Marshal(r2.Request)
	if string(b1) != string(b2) {
		t.Fatal("identical requests produced different bytes — prompt-cache busting input")
	}
}

// TestSelectionCapEdgeKeepsEvidence — regression: at the cap boundary the
// ranked evidence line has priority over its context. Equal-score matches at
// 2, 7, ..., 192, 196, then 300; the old walker inserted context 298 as line
// 200 and omitted match 300. The two-phase selector keeps every ranked match
// first, never context in lieu of the match that justified it, stays at most
// 200 unique lines, and remains deterministic.
func TestSelectionCapEdgeKeepsEvidence(t *testing.T) {
	var lines []string
	for i := 0; i < 302; i++ {
		if i >= 2 && i <= 192 && (i-2)%5 == 0 {
			lines = append(lines, "MATCH")
			continue
		}
		if i == 196 || i == 300 {
			lines = append(lines, "MATCH")
			continue
		}
		lines = append(lines, "noise")
	}
	keep := selectKeywordLines(lines, []string{"match"})
	if !keep[300] {
		t.Fatalf("ranked match 300 missing; kept=%d", len(keep))
	}
	if keep[298] {
		t.Fatal("context retained in lieu of the match that justified it")
	}
	if len(keep) > maxKeepLines {
		t.Fatalf("cap exceeded: %d", len(keep))
	}
	keep2 := selectKeywordLines(lines, []string{"match"})
	for i := range keep {
		if !keep2[i] {
			t.Fatal("selection differs across identical inputs")
		}
	}
}

// TestTruncationNoticeExactEquality — regression: when n equals the exact
// notice length, the notice-bearing path is used (the notice states the full
// removal), not a raw prefix.
func TestTruncationNoticeExactEquality(t *testing.T) {
	content := strings.Repeat("x", 1000)
	n := len(truncationNotice(len(content)))
	got := truncateHeadTail(content, n)
	want := truncationNotice(len(content))
	if got != want {
		t.Fatalf("equality case: got %q, want the exact notice %q", got, want)
	}
	if len(got) != n {
		t.Fatalf("equality case output %d bytes, want %d", len(got), n)
	}
	// The notice's removed-byte count must match the actual removal.
	if _, rest, ok := strings.Cut(got, "\n\n... ["); ok {
		claimed, _, _ := strings.Cut(rest, " bytes truncated by Torana]")
		if claimed != "1000" {
			t.Fatalf("notice claims %s removed, want 1000", claimed)
		}
	} else {
		t.Fatal("notice missing in the equality case")
	}
	// Strictly below equality, the raw-prefix contract still holds.
	if got := truncateHeadTail(content, n-1); got != strings.Repeat("x", n-1) {
		t.Fatalf("strictly-below case must be a raw prefix, got %q", got)
	}
}

// TestKeywordOrderedSeamRows — the ordered-body seam rows for keyword
// compaction: EVERY message's tool-result blocks are candidates (no role
// gate); each block is identified by (message, block); unsupported shapes
// decline unchanged with EXACTLY ONE env.plugin_config call and zero spend
// calls; two results in one message are independent candidates with exact
// per-candidate call cardinalities.
func TestKeywordOrderedSeamRows(t *testing.T) {
	content := keywordContent()
	result := func(id, text string) *pbv2.RequestBlock {
		return &pbv2.RequestBlock{Kind: &pbv2.RequestBlock_ToolResult{ToolResult: &pbv2.RequestToolResultBlock{
			ToolCallId: id, ToolName: "read",
			Content: []*pbv2.ToolResultContentBlock{{Kind: &pbv2.ToolResultContentBlock_Text{Text: &pbv2.ToolResultTextBlock{Text: text}}}},
		}}}
	}
	assistantAfter := func() *pbv2.Message {
		return &pbv2.Message{Role: "assistant", Blocks: []*pbv2.RequestBlock{{Kind: &pbv2.RequestBlock_Text{Text: &pbv2.RequestTextBlock{Text: "the fix is in server.go"}}}}}
	}

	// User-role result: the gate must not require role "tool".
	t.Run("user-role result is a candidate", func(t *testing.T) {
		h := newHarness(t)
		h.SetConfig(keywordCfg)
		h.SeedCache("intent:c1", "find the bug in server")
		req := &pbv2.ChatRequest{Messages: []*pbv2.Message{
			{Role: "user", Blocks: []*pbv2.RequestBlock{{Kind: &pbv2.RequestBlock_Text{Text: &pbv2.RequestTextBlock{Text: "u"}}}, result("c1", content)}},
			assistantAfter(),
		}}
		res := h.BeforeRequest(req)
		if res.Err != nil || res.Request == nil {
			t.Fatalf("err=%v", res.Err)
		}
		got := res.Request.Messages[0].Blocks[1].GetToolResult().Content[0].GetText().Text
		if !worthwhileReduction(len(content), len(got)) {
			t.Fatalf("user-role result not compacted: %d -> %d", len(content), len(got))
		}
		if res.Request.Messages[0].Blocks[0].GetText().Text != "u" {
			t.Fatal("surrounding text disturbed")
		}
	})

	// Two results in one message: independent candidates, both applied,
	// with exact cardinalities: intent + keyword cache_get per candidate
	// (4 total), savings per candidate (2: cache_reuse + transformation),
	// eligible metric per candidate (2), one plugin_config. cache_set
	// fires ONCE: the candidates share the content-addressed key (same
	// tool, args, text, intent), so the second candidate reuses the value
	// the first just stored — determinism across identical inputs.
	t.Run("two results in one message", func(t *testing.T) {
		h := newHarness(t)
		h.SetConfig(keywordCfg)
		h.SeedCache("intent:c1", "find the bug in server")
		h.SeedCache("intent:c2", "find the bug in server")
		req := &pbv2.ChatRequest{Messages: []*pbv2.Message{
			{Role: "user", Blocks: []*pbv2.RequestBlock{result("c1", content), result("c2", content)}},
			assistantAfter(),
		}}
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
			if !worthwhileReduction(len(content), len(got)) {
				t.Fatalf("result %s not compacted: %d -> %d", id, len(content), len(got))
			}
		}
		// The COMPLETE host-call multiset (EmitMetric is a metric, not a
		// host call — its exact cardinality is asserted below).
		wantMultiset := map[string]int{
			"env.plugin_config":     1,
			"env.cache_get":         2, // keyword key per candidate
			"env.shared_cache_get":  2, // intent key per candidate
			"env.cache_set":         1, // the second candidate reuses the first's stored value
			"torana_record_savings": 2, // cache_reuse + transformation
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
		// The two savings payloads decoded: exactly one transformation and
		// one cache_reuse — the count alone would not prove the shared-key
		// reuse path.
		type savingsPayload struct {
			OriginalBytes int    `json:"original_bytes"`
			FinalBytes    int    `json:"final_bytes"`
			Source        string `json:"source"`
		}
		sources := map[string]int{}
		var sawOriginal bool
		for _, c := range h.Calls() {
			if c.Command != "torana_record_savings" {
				continue
			}
			var p savingsPayload
			if err := json.Unmarshal([]byte(c.Args), &p); err != nil {
				t.Fatalf("savings args not JSON: %v (%s)", err, c.Args)
			}
			if p.OriginalBytes == len(content) {
				sawOriginal = true
			}
			sources[p.Source]++
		}
		if sources["transformation"] != 1 || sources["cache_reuse"] != 1 {
			t.Fatalf("savings sources = %v, want exactly one transformation + one cache_reuse", sources)
		}
		if !sawOriginal {
			t.Fatal("neither savings payload reported the original content size")
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
	})

	// Unsupported shapes decline unchanged with EXACTLY ONE env.plugin_config
	// and zero cache/metric/savings calls: the marker-only row is IN-DOMAIN
	// (a real cache-breakpoint arm), the explicit-empty row is a scalar below
	// the minimum threshold — inert with the same zero-spend multiset.
	t.Run("unsupported shapes exact multiset", func(t *testing.T) {
		unknown := &pbv2.ToolResultContentBlock{Kind: &pbv2.ToolResultContentBlock_Unknown{Unknown: &pbv2.ToolResultUnknownBlock{Kind: "provider_blob", PayloadJson: []byte(`{"x":1}`)}}}
		marker := &pbv2.ToolResultContentBlock{Kind: &pbv2.ToolResultContentBlock_CacheBreakpoint{CacheBreakpoint: &pbv2.ToolResultCacheBreakpoint{MarkerJson: []byte(`{"type":"ephemeral"}`)}}}
		rows := map[string]*pbv2.RequestToolResultBlock{
			"marker-only":    {ToolCallId: "c1", ToolName: "read", Content: []*pbv2.ToolResultContentBlock{marker}},
			"multiple text":  {ToolCallId: "c1", ToolName: "read", Content: []*pbv2.ToolResultContentBlock{{Kind: &pbv2.ToolResultContentBlock_Text{Text: &pbv2.ToolResultTextBlock{Text: "a"}}}, {Kind: &pbv2.ToolResultContentBlock_Text{Text: &pbv2.ToolResultTextBlock{Text: "b"}}}}},
			"unknown arm":    {ToolCallId: "c1", ToolName: "read", Content: []*pbv2.ToolResultContentBlock{{Kind: &pbv2.ToolResultContentBlock_Text{Text: &pbv2.ToolResultTextBlock{Text: content}}}, unknown}},
			"explicit empty": {ToolCallId: "c1", ToolName: "read", Content: []*pbv2.ToolResultContentBlock{{Kind: &pbv2.ToolResultContentBlock_Text{Text: &pbv2.ToolResultTextBlock{Text: ""}}}}},
		}
		for name, tr := range rows {
			t.Run(name, func(t *testing.T) {
				h := newHarness(t)
				h.SetConfig(keywordCfg)
				req := &pbv2.ChatRequest{Messages: []*pbv2.Message{
					{Role: "user", Blocks: []*pbv2.RequestBlock{{Kind: &pbv2.RequestBlock_ToolResult{ToolResult: tr}}}},
					assistantAfter(),
				}}
				before := proto.Clone(req).(*pbv2.ChatRequest)
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
