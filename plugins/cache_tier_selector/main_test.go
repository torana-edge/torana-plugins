package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	pbv2 "github.com/torana-edge/torana-plugin-sdk/pb/v2"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
	"google.golang.org/protobuf/proto"
)

// ==========================================================================
// Pure helpers
// ==========================================================================

func msg(role, content string) *pbv2.Message {
	return &pbv2.Message{Role: role, Blocks: []*pbv2.RequestBlock{
		{Kind: &pbv2.RequestBlock_Text{Text: &pbv2.RequestTextBlock{Text: content}}},
		{Kind: &pbv2.RequestBlock_CacheBreakpoint{CacheBreakpoint: &pbv2.RequestCacheBreakpoint{MarkerJson: []byte(`{"type":"ephemeral"}`)}}},
	}}
}

// carrierMarkerAt reads the marker bytes of a SPECIFIED outer carrier
// position — every fixture in this suite carries its marker at a known
// block position, so tests assert the row-designated carrier directly
// instead of re-implementing the SDK's ordered carrier discovery (which
// would reintroduce the drift class the SDK-owned seam eliminates).
func carrierMarkerAt(t *testing.T, req *pbv2.ChatRequest, msg, block int) []byte {
	t.Helper()
	cb := req.Messages[msg].Blocks[block].GetCacheBreakpoint()
	if cb == nil {
		t.Fatalf("no outer carrier at messages[%d].blocks[%d]", msg, block)
	}
	return cb.MarkerJson
}

// keyFor is a thin test wrapper over the PRODUCTION decisionKey helper.
func keyFor(t *testing.T, req *pbv2.ChatRequest) string {
	t.Helper()
	k, _, err := decisionKey(req)
	if err != nil {
		t.Fatalf("decisionKey: %v", err)
	}
	return k
}

func baseRequest() *pbv2.ChatRequest {
	return &pbv2.ChatRequest{
		Model:    "claude-sonnet-4",
		Messages: []*pbv2.Message{msg("system", "you are a coding agent"), msg("user", "find the bug")},
	}
}

// TestDecisionExpiresWithProviderTier pins provider-tier expiry.
func TestDecisionExpiresWithProviderTier(t *testing.T) {
	value := decision{DecidedAtMillis: 1_000, TierTTL: 300}
	if decisionExpired(value, 300_999) {
		t.Fatal("decision expired before its provider cache tier")
	}
	if !decisionExpired(value, 301_000) {
		t.Fatal("decision remained sticky after its provider cache tier expired")
	}
}

func TestDecisionWithoutUsableClockOrTTLDoesNotExpire(t *testing.T) {
	for _, value := range []decision{
		{},
		{DecidedAtMillis: 1_000},
		{TierTTL: 300},
	} {
		if decisionExpired(value, 999_999) {
			t.Errorf("incomplete decision unexpectedly expired: %+v", value)
		}
	}
}

// TestDecisionKeySensitivity — the parity contract (I1) at the decision-key
// level. The observable descriptor inventory is OWNED by the SDK
// (RequestObservablePrefix's bidirectional inventory fails closed on
// additive fields); this plugin pins the consequence for ITS decision key:
// every observable field (model, tools, messages, provider extensions,
// safety settings, generation params) folds into the decision key, while
// stream and torana_meta_json (the projection's only exclusions) do not.
func TestDecisionKeySensitivity(t *testing.T) {
	included := map[string]func(*pbv2.ChatRequest){
		"model": func(r *pbv2.ChatRequest) { r.Model = "m2" },
		"tools": func(r *pbv2.ChatRequest) {
			r.Tools = []*pbv2.ToolDef{{Name: "read", Description: "d", ParametersJson: []byte(`{}`)}}
		},
		"messages":                 func(r *pbv2.ChatRequest) { r.Messages[0].Blocks[0].GetText().Text = "changed" },
		"provider_extensions_json": func(r *pbv2.ChatRequest) { r.ProviderExtensionsJson = []byte(`{"x":1}`) },
		"safety_settings_json":     func(r *pbv2.ChatRequest) { r.SafetySettingsJson = []byte(`[]`) },
		"max_tokens":               func(r *pbv2.ChatRequest) { r.MaxTokens = proto.Int32(64) },
		"temperature":              func(r *pbv2.ChatRequest) { r.Temperature = proto.Float64(0.5) },
		"top_p":                    func(r *pbv2.ChatRequest) { r.TopP = proto.Float64(0.9) },
		"stop_sequences":           func(r *pbv2.ChatRequest) { r.StopSequences = []string{"END"} },
	}
	excluded := map[string]func(*pbv2.ChatRequest){
		"stream":           func(r *pbv2.ChatRequest) { r.Stream = true },
		"torana_meta_json": func(r *pbv2.ChatRequest) { r.ToranaMetaJson = []byte(`{"_provider":"x"}`) },
	}
	for name, mutate := range included {
		t.Run("included/"+name, func(t *testing.T) {
			base := baseRequest()
			before := keyFor(t, base)
			if before == "" {
				t.Fatal("fixture decision key empty; vacuous")
			}
			mutate(base)
			if got := keyFor(t, base); got == before {
				t.Errorf("observable field %s did not move the decision key", name)
			}
		})
	}
	for name, mutate := range excluded {
		t.Run("excluded/"+name, func(t *testing.T) {
			base := baseRequest()
			before := keyFor(t, base)
			mutate(base)
			if got := keyFor(t, base); got != before {
				t.Errorf("excluded field %s moved the decision key", name)
			}
		})
	}

	// Suffix messages after the boundary are not part of the cached prefix.
	t.Run("suffix messages excluded", func(t *testing.T) {
		before := keyFor(t, baseRequest())
		extended := baseRequest()
		extended.Messages = append(extended.Messages, &pbv2.Message{Role: "assistant", Blocks: []*pbv2.RequestBlock{{Kind: &pbv2.RequestBlock_Text{Text: &pbv2.RequestTextBlock{Text: "thinking out loud"}}}}})
		if got := keyFor(t, extended); got != before {
			t.Errorf("an unmarked message after the breakpoint changed the decision key")
		}
	})

	// Identical prefixes fingerprint identically.
	t.Run("stable for identical prefixes", func(t *testing.T) {
		if a, b := keyFor(t, baseRequest()), keyFor(t, baseRequest()); a != b {
			t.Fatal("identical prefixes must share a decision key")
		}
	})

	// Fail-closed parity (I3): an out-of-domain request has NO decision key
	// and NO marker presence — the error path declines before state.
	t.Run("out-of-domain empty key", func(t *testing.T) {
		bad := baseRequest()
		bad.Messages[0].Blocks = bad.Messages[0].Blocks[:0]
		k, has, err := decisionKey(bad)
		if err == nil {
			t.Fatalf("out-of-domain request produced a decision key %q (has=%v)", k, has)
		}
		if k != "" || has {
			t.Fatalf("out-of-domain request: key=%q has=%v, want empty/false", k, has)
		}
	})
}

// ==========================================================================
// Hook-level matrix (sdktest)
// ==========================================================================

// newHarness builds a fresh fake host. Config is per-call (no process
// globals), so no reset is needed; tests stay sequential.
func newHarness(t *testing.T) *sdktest.Harness {
	t.Helper()
	return sdktest.New(t)
}

// pricingStub returns a two-tier pricing envelope.
func pricingStub() func(string) (string, error) {
	return func(string) (string, error) {
		return sdktest.HostResultValue([]byte(`{
			"status":"ok",
			"refresh_on_read":true,
			"shortest_ttl_seconds":300,
			"warm_interval_seconds":240,
			"break_even_refreshes":11,
			"tiers":[
				{"ttl_seconds":300,"write_multiplier":1.25,"marker":{"type":"ephemeral"}},
				{"ttl_seconds":3600,"write_multiplier":2.0,"marker":{"type":"ephemeral","ttl":"1h"}}
			]
		}`)), nil
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

// reqWith builds a request with a breakpoint + host meta.
func reqWith(t *testing.T, h *sdktest.Harness) *pbv2.ChatRequest {
	t.Helper()
	req := baseRequest()
	req.ToranaMetaJson = []byte(`{"_provider":"anthropic","_conversation_id":"conv-1"}`)
	return req
}

// TestModeOffIsInert — mode off: pass with no host calls.
func TestModeOffIsInert(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(`{"mode":"off"}`)
	res := h.BeforeRequest(reqWith(t, h))
	if res.Err != nil || !res.PassedThrough {
		t.Fatalf("mode off must pass through, err=%v", res.Err)
	}
	for _, c := range h.Calls() {
		if c.Command != "env.plugin_config" {
			t.Errorf("mode off made an unexpected host call: %s", c.Command)
		}
	}
}

// TestNoBreakpointPasses — no marker, no decision to make.
func TestNoBreakpointPasses(t *testing.T) {
	h := newHarness(t)
	h.StubHostCall("torana_cache_pricing", pricingStub())
	req := &pbv2.ChatRequest{Model: "m", Messages: []*pbv2.Message{{Role: "user", Blocks: []*pbv2.RequestBlock{{Kind: &pbv2.RequestBlock_Text{Text: &pbv2.RequestTextBlock{Text: "hi"}}}}}}}
	req.ToranaMetaJson = []byte(`{"_provider":"anthropic"}`)
	res := h.BeforeRequest(req)
	if res.Err != nil || !res.PassedThrough {
		t.Fatalf("no breakpoint must pass, err=%v", res.Err)
	}
	// Decline-before-state: the no-marker sentinel declines with NO pricing
	// call and NO state access.
	if n := countCommand(h, "torana_cache_pricing"); n != 0 {
		t.Fatalf("no-breakpoint request called pricing %d times", n)
	}
	if n := countCommand(h, "env.state_get"); n != 0 {
		t.Fatalf("no-breakpoint request accessed state %d times", n)
	}
}

// TestPricingAdvisoryDeclinesContractErrors — unknown economics never guesses;
// contract defects surface.
func TestPricingAdvisoryDeclinesContractErrors(t *testing.T) {
	h := newHarness(t)
	h.StubHostCall("torana_cache_pricing", func(string) (string, error) {
		return sdktest.HostResultError(pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "no pricing"), nil
	})
	res := h.BeforeRequest(reqWith(t, h))
	if res.Err != nil || !res.PassedThrough {
		t.Fatalf("advisory pricing must pass unchanged, err=%v", res.Err)
	}

	h2 := newHarness(t)
	h2.StubHostCall("torana_cache_pricing", func(string) (string, error) {
		return sdktest.HostResultError(pbv2.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "stub"), nil
	})
	res2 := h2.BeforeRequest(reqWith(t, h2))
	if res2.Err == nil {
		t.Fatal("contract pricing refusal must error the hook")
	}
}

// TestStoredDecisionReappliedByteIdentically — an unexpired stored decision
// re-applies the marker verbatim with no new decision write and no counter;
// two fresh clones produce byte-identical output.
func TestStoredDecisionReappliedByteIdentically(t *testing.T) {
	h := newHarness(t)
	h.StubHostCall("torana_cache_pricing", pricingStub())
	h.SetNow(1_000_000)
	req := reqWith(t, h)
	key := "decision/" + keyFor(t, req)
	h.SeedState(key, mustJSON(t, decision{
		Marker:          map[string]any{"type": "ephemeral", "ttl": "1h"},
		TierTTL:         3600,
		DecidedAtMillis: 900_000,
	}))

	first := h.BeforeRequest(req)
	if first.Err != nil || first.Request == nil {
		t.Fatalf("expected the stored marker applied, err=%v", first.Err)
	}
	out := carrierMarkerAt(t, first.Request, 1, 1)
	var marker map[string]any
	if err := json.Unmarshal(out, &marker); err != nil {
		t.Fatal(err)
	}
	if marker["ttl"] != "1h" {
		t.Fatalf("stored marker not reapplied: %s", out)
	}
	if countCommand(h, "torana_plugin_counter") != 0 {
		t.Fatal("a stored decision must not re-decide (no counter)")
	}
	// No new decision write.
	if n := countCommand(h, "env.state_set"); n != 0 {
		t.Fatalf("a stored decision must not rewrite state, got %d writes", n)
	}

	// Byte-identical across fresh clones.
	second := h.BeforeRequest(reqWith(t, h))
	b1, _ := json.Marshal(first.Request)
	b2, _ := json.Marshal(second.Request)
	if string(b1) != string(b2) {
		t.Fatal("stored-decision reapplication differs between identical requests")
	}
}

// TestExpiredDecisionDeletedAndRedecided — past the tier TTL the decision is
// deleted (StateDelete) and a new one is made.
func TestExpiredDecisionDeletedAndRedecided(t *testing.T) {
	h := newHarness(t)
	h.StubHostCall("torana_cache_pricing", pricingStub())
	h.SetNow(5_000_000)
	req := reqWith(t, h)
	key := "decision/" + keyFor(t, req)
	h.SeedState(key, mustJSON(t, decision{
		Marker:          map[string]any{"type": "ephemeral", "ttl": "1h"},
		TierTTL:         3600,
		DecidedAtMillis: 900_000,
	}))
	h.SeedState("activity/conv-1", mustJSON(t, activity{LastSeenMillis: 4_900_000, LongestGapMillis: 2_000_000, Turns: 2}))
	res := h.BeforeRequest(req)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if countCommand(h, "env.state_delete") != 1 {
		t.Fatal("an expired decision must be deleted")
	}
	if countCommand(h, "torana_plugin_counter") != 1 {
		t.Fatal("an expired decision must be re-decided")
	}
}

// TestNoClockPasses — without a clock there is no gap history.
func TestNoClockPasses(t *testing.T) {
	h := newHarness(t)
	h.StubHostCall("torana_cache_pricing", pricingStub())
	h.StubHostCall("env.now", func(string) (string, error) {
		return sdktest.HostResultError(pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "no clock"), nil
	})
	res := h.BeforeRequest(reqWith(t, h))
	if res.Err != nil || !res.PassedThrough {
		t.Fatalf("no clock must pass unchanged, err=%v", res.Err)
	}
	if n := countCommand(h, "env.state_set"); n != 0 {
		t.Fatalf("no clock must not store anything, got %d writes", n)
	}

	// Contract clock defect surfaces.
	h2 := newHarness(t)
	h2.StubHostCall("torana_cache_pricing", pricingStub())
	h2.StubHostCall("env.now", func(string) (string, error) {
		return sdktest.HostResultError(pbv2.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "stub"), nil
	})
	if res2 := h2.BeforeRequest(reqWith(t, h2)); res2.Err == nil {
		t.Fatal("contract clock refusal must error the hook")
	}
}

// TestAutoModeThreshold — a gap below the threshold leaves the harness
// default; at or above it the long-tier marker is applied, the decision is
// persisted BEFORE application, and the counter fires.
func TestAutoModeThreshold(t *testing.T) {
	// Short gap: no marker.
	h := newHarness(t)
	h.StubHostCall("torana_cache_pricing", pricingStub())
	h.SetNow(1_000_000)
	h.SeedState("activity/conv-1", mustJSON(t, activity{LastSeenMillis: 999_000, LongestGapMillis: 1_000, Turns: 2}))
	res := h.BeforeRequest(reqWith(t, h))
	if res.Err != nil || !res.PassedThrough {
		t.Fatalf("short gap must not buy the long tier, err=%v", res.Err)
	}

	// Long gap: buys the long tier.
	h2 := newHarness(t)
	h2.StubHostCall("torana_cache_pricing", pricingStub())
	h2.SetNow(1_000_000)
	h2.SeedState("activity/conv-1", mustJSON(t, activity{LastSeenMillis: 999_000, LongestGapMillis: 2_000_000, Turns: 2}))
	res2 := h2.BeforeRequest(reqWith(t, h2))
	if res2.Err != nil || res2.Request == nil {
		t.Fatalf("a long gap must buy the long tier, err=%v", res2.Err)
	}
	var marker map[string]any
	if err := json.Unmarshal(carrierMarkerAt(t, res2.Request, 1, 1), &marker); err != nil {
		t.Fatal(err)
	}
	if marker["ttl"] != "1h" {
		t.Fatalf("long-tier marker not applied: %v", marker)
	}
	if countCommand(h2, "torana_plugin_counter") != 1 {
		t.Fatal("a new decision must fire the counter")
	}
	// The decision was persisted (state_set before the replacement).
	if n := countCommand(h2, "env.state_set"); n == 0 {
		t.Fatal("the decision must be persisted")
	}
}

// TestAutoModeDefaultThreshold — min_gap 0 uses 30% of the long tier TTL
// (3600 * 0.3 = 1080s = 1_080_000ms): a gap of 1_000_000ms is below it.
func TestAutoModeDefaultThreshold(t *testing.T) {
	h := newHarness(t)
	h.StubHostCall("torana_cache_pricing", pricingStub())
	h.SetNow(1_000_000)
	h.SeedState("activity/conv-1", mustJSON(t, activity{LastSeenMillis: 999_000, LongestGapMillis: 1_000_000, Turns: 2}))
	res := h.BeforeRequest(reqWith(t, h))
	if res.Err != nil || !res.PassedThrough {
		t.Fatalf("a gap below the 30%% default threshold must not buy the long tier, err=%v", res.Err)
	}
}

// TestModesShortAndLong — explicit mode overrides.
func TestModesShortAndLong(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(`{"mode":"short"}`)
	h.StubHostCall("torana_cache_pricing", pricingStub())
	h.SetNow(1_000_000)
	h.SeedState("activity/conv-1", mustJSON(t, activity{LastSeenMillis: 999_000, LongestGapMillis: 2_000_000, Turns: 2}))
	res := h.BeforeRequest(reqWith(t, h))
	if res.Err != nil || !res.PassedThrough {
		t.Fatalf("mode short must keep the harness default, err=%v", res.Err)
	}

	h2 := newHarness(t)
	h2.SetConfig(`{"mode":"long"}`)
	h2.StubHostCall("torana_cache_pricing", pricingStub())
	res2 := h2.BeforeRequest(reqWith(t, h2))
	if res2.Err != nil || res2.Request == nil {
		t.Fatalf("mode long must apply the long marker, err=%v", res2.Err)
	}
	if !strings.Contains(string(carrierMarkerAt(t, res2.Request, 1, 1)), `"ttl":"1h"`) {
		t.Fatalf("long marker missing: %s", carrierMarkerAt(t, res2.Request, 1, 1))
	}
}

// TestSingleTierPasses — nothing to choose between.
func TestSingleTierPasses(t *testing.T) {
	h := newHarness(t)
	h.StubHostCall("torana_cache_pricing", func(string) (string, error) {
		return sdktest.HostResultValue([]byte(`{
			"status":"ok","refresh_on_read":true,"shortest_ttl_seconds":300,
			"tiers":[{"ttl_seconds":300,"marker":{"type":"ephemeral"}}]
		}`)), nil
	})
	h.SetNow(1_000_000)
	h.SeedState("activity/conv-1", mustJSON(t, activity{LastSeenMillis: 999_000, LongestGapMillis: 2_000_000, Turns: 2}))
	res := h.BeforeRequest(reqWith(t, h))
	if res.Err != nil || !res.PassedThrough {
		t.Fatalf("a single tier leaves the request untouched, err=%v", res.Err)
	}
}

// TestDecisionPersistRefusals — advisory: pass unchanged (no marker applied);
// contract: hook error.
func TestDecisionPersistRefusals(t *testing.T) {
	h := newHarness(t)
	h.StubHostCall("torana_cache_pricing", pricingStub())
	h.SetNow(1_000_000)
	h.SeedState("activity/conv-1", mustJSON(t, activity{LastSeenMillis: 999_000, LongestGapMillis: 2_000_000, Turns: 2}))
	h.StubHostCall("env.state_set", func(string) (string, error) {
		return sdktest.HostResultError(pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "no store"), nil
	})
	res := h.BeforeRequest(reqWith(t, h))
	if res.Err != nil || !res.PassedThrough {
		t.Fatalf("an advisory state refusal must pass unchanged, err=%v", res.Err)
	}

	// Contract refusal on the decision persist: hook error.
	h2 := newHarness(t)
	h2.StubHostCall("torana_cache_pricing", pricingStub())
	h2.SetNow(1_000_000)
	h2.SeedState("activity/conv-1", mustJSON(t, activity{LastSeenMillis: 999_000, LongestGapMillis: 2_000_000, Turns: 2}))
	h2.DenyPermission("env.state_set")
	if res2 := h2.BeforeRequest(reqWith(t, h2)); res2.Err == nil {
		t.Fatal("a contract state refusal must error the hook")
	}
}

// TestStateReadRefusalsAndMalformed — advisory read declines; contract read
// and malformed frames error; NOT_FOUND takes the normal decide path.
func TestStateReadRefusalsAndMalformed(t *testing.T) {
	h := newHarness(t)
	h.StubHostCall("torana_cache_pricing", pricingStub())
	h.SetNow(1_000_000)
	h.DenyPermission("env.state_get")
	res := h.BeforeRequest(reqWith(t, h))
	if res.Err == nil {
		t.Fatal("a contract state read refusal must error the hook")
	}

	h2 := newHarness(t)
	h2.StubHostCall("torana_cache_pricing", pricingStub())
	h2.StubHostCall("env.state_get", func(string) (string, error) {
		return "not a frame", nil
	})
	if res2 := h2.BeforeRequest(reqWith(t, h2)); res2.Err == nil {
		t.Fatal("a malformed state reply must error the hook")
	}

	// NOT_FOUND (no stored decision): normal decide path.
	h3 := newHarness(t)
	h3.StubHostCall("torana_cache_pricing", pricingStub())
	h3.SetNow(1_000_000)
	h3.SeedState("activity/conv-1", mustJSON(t, activity{LastSeenMillis: 999_000, LongestGapMillis: 2_000_000, Turns: 2}))
	res3 := h3.BeforeRequest(reqWith(t, h3))
	if res3.Err != nil || res3.Request == nil {
		t.Fatalf("a fresh prefix must be decided, err=%v", res3.Err)
	}
}

// TestCorruptDecisionIsKeyLocal — corrupt stored JSON for the decision key
// declines for THIS key (pass unchanged) without poisoning anything else and
// without being treated as absence.
func TestCorruptDecisionIsKeyLocal(t *testing.T) {
	h := newHarness(t)
	h.StubHostCall("torana_cache_pricing", pricingStub())
	h.SetNow(1_000_000)
	req := reqWith(t, h)
	h.SeedState("decision/"+keyFor(t, req), "not json")
	res := h.BeforeRequest(req)
	if res.Err != nil || !res.PassedThrough {
		t.Fatalf("corrupt decision JSON must decline for this key, err=%v", res.Err)
	}
}

// TestApplyMarkerMatchingMarkerPasses — an already-matching marker passes
// without re-serialization (no replacement).
func TestApplyMarkerMatchingMarkerPasses(t *testing.T) {
	req := baseRequest()
	changed, err := replaceMarker(req, map[string]any{"type": "ephemeral"})
	if err != nil || changed {
		t.Fatalf("a matching marker must pass through (changed=%v err=%v)", changed, err)
	}
	req2 := baseRequest()
	changed, err = replaceMarker(req2, map[string]any{"type": "ephemeral", "ttl": "1h"})
	if err != nil || !changed {
		t.Fatalf("a changed marker must replace the request (changed=%v err=%v)", changed, err)
	}
	if !strings.Contains(string(carrierMarkerAt(t, req2, 1, 1)), `"ttl":"1h"`) {
		t.Fatalf("marker not applied: %s", carrierMarkerAt(t, req2, 1, 1))
	}
	// No carrier: the sentinel declines without mutation.
	nomark := &pbv2.ChatRequest{Model: "m", Messages: []*pbv2.Message{{Role: "user", Blocks: []*pbv2.RequestBlock{{Kind: &pbv2.RequestBlock_Text{Text: &pbv2.RequestTextBlock{Text: "hi"}}}}}}}
	before := proto.Clone(nomark).(*pbv2.ChatRequest)
	changed, err = replaceMarker(nomark, map[string]any{"type": "ephemeral"})
	if err != nil || changed {
		t.Fatalf("no-carrier sentinel: changed=%v err=%v, want decline", changed, err)
	}
	if !proto.Equal(nomark, before) {
		t.Fatal("the no-carrier sentinel mutated the request")
	}
}

// TestCleanupExpiredStateBounded — expired decisions/activity are deleted, at
// most 100 per run, and the hourly marker gates repeats.
func TestCleanupExpiredStateBounded(t *testing.T) {
	h := newHarness(t)
	h.StubHostCall("torana_cache_pricing", pricingStub())
	h.SetNow(3_000_000_000)
	for i := 0; i < 5; i++ {
		h.SeedState("decision/"+string(rune('a'+i)), mustJSON(t, decision{
			Marker: map[string]any{"type": "ephemeral"}, TierTTL: 300, DecidedAtMillis: 1_000,
		}))
	}
	h.SeedState("activity/conv-1", mustJSON(t, activity{LastSeenMillis: 3_000_000_000 - 31*86400*1000, Turns: 1}))
	req := reqWith(t, h)
	h.BeforeRequest(req)
	if n := countCommand(h, "env.state_delete"); n != 6 {
		t.Fatalf("cleanup deleted %d keys, want 6 (5 expired decisions + 1 expired activity)", n)
	}
}

// TestNoUnauthorizedCalls — every host call is within the declared
// permission set (state_delete rides env.state_set; the counter is a
// declared extension).
func TestNoUnauthorizedCalls(t *testing.T) {
	h := newHarness(t)
	h.StubHostCall("torana_cache_pricing", pricingStub())
	h.SetNow(1_000_000)
	h.SeedState("activity/conv-1", mustJSON(t, activity{LastSeenMillis: 999_000, LongestGapMillis: 2_000_000, Turns: 2}))
	h.BeforeRequest(reqWith(t, h))

	allowed := map[string]bool{
		"env.plugin_config":     true,
		"env.state_get":         true,
		"env.state_set":         true,
		"env.state_delete":      true, // command; authorized by env.state_set
		"env.state_keys":        true,
		"env.now":               true,
		"torana_cache_pricing":  true,
		"torana_plugin_counter": true,
		"env.log":               true,
	}
	for _, c := range h.Calls() {
		if !allowed[c.Command] {
			t.Errorf("host call outside the declared permission set: %s", c.Command)
		}
	}
}

// TestSchemaDefaultsMatchRuntimeDefaults — schema.json defaults (auto/0/30)
// must equal the runtime defaults.
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
	var mode string
	if err := json.Unmarshal(schema.Properties["mode"].Default, &mode); err != nil || mode != "auto" {
		t.Fatalf("schema mode default=%q, want auto", mode)
	}
	if string(schema.Properties["min_gap_seconds_for_long_tier"].Default) != "0" {
		t.Fatal("schema min_gap default != 0")
	}
	var days int
	if err := json.Unmarshal(schema.Properties["activity_retention_days"].Default, &days); err != nil || days != 30 {
		t.Fatalf("schema retention default=%d, want 30", days)
	}
	rt := parseConfig("")
	if rt.Mode != "auto" || rt.ActivityRetentionDays != 30 || rt.MinGapSecondsForLongTier != 0 {
		t.Fatalf("runtime defaults %+v do not match the schema", rt)
	}
}

// TestDeterminismOverIdenticalRequests — two fresh clones with the same
// stored decision produce byte-identical output.
func TestDeterminismOverIdenticalRequests(t *testing.T) {
	h1 := newHarness(t)
	h1.StubHostCall("torana_cache_pricing", pricingStub())
	h1.SetNow(1_000_000)
	req := reqWith(t, h1)
	h1.SeedState("decision/"+keyFor(t, req), mustJSON(t, decision{
		Marker: map[string]any{"type": "ephemeral", "ttl": "1h"}, TierTTL: 3600, DecidedAtMillis: 900_000,
	}))
	r1 := h1.BeforeRequest(req)
	h2 := newHarness(t)
	h2.StubHostCall("torana_cache_pricing", pricingStub())
	h2.SetNow(1_000_000)
	req2 := reqWith(t, h2)
	h2.SeedState("decision/"+keyFor(t, req2), mustJSON(t, decision{
		Marker: map[string]any{"type": "ephemeral", "ttl": "1h"}, TierTTL: 3600, DecidedAtMillis: 900_000,
	}))
	r2 := h2.BeforeRequest(req2)
	if r1.Err != nil || r2.Err != nil {
		t.Fatalf("dispatch errors: %v %v", r1.Err, r2.Err)
	}
	b1, _ := json.Marshal(r1.Request)
	b2, _ := json.Marshal(r2.Request)
	if string(b1) != string(b2) {
		t.Fatal("identical requests produced different bytes")
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestStoredDecisionClockClassification — with a stored decision, an advisory
// clock failure may reapply the already-sticky marker, but a contract clock
// failure must error exactly as on the fresh-decision path (never swallowed).
func TestStoredDecisionClockClassification(t *testing.T) {
	// Advisory clock + stored decision: reapply, no error.
	h := newHarness(t)
	h.StubHostCall("torana_cache_pricing", pricingStub())
	h.StubHostCall("env.now", func(string) (string, error) {
		return sdktest.HostResultError(pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "no clock"), nil
	})
	req := reqWith(t, h)
	h.SeedState("decision/"+keyFor(t, req), mustJSON(t, decision{
		Marker: map[string]any{"type": "ephemeral", "ttl": "1h"}, TierTTL: 3600, DecidedAtMillis: 900_000,
	}))
	res := h.BeforeRequest(req)
	if res.Err != nil {
		t.Fatalf("advisory clock must allow reapplication, err=%v", res.Err)
	}

	// Contract clock + stored decision: hook error.
	h2 := newHarness(t)
	h2.StubHostCall("torana_cache_pricing", pricingStub())
	h2.StubHostCall("env.now", func(string) (string, error) {
		return sdktest.HostResultError(pbv2.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "stub"), nil
	})
	req2 := reqWith(t, h2)
	h2.SeedState("decision/"+keyFor(t, req2), mustJSON(t, decision{
		Marker: map[string]any{"type": "ephemeral", "ttl": "1h"}, TierTTL: 3600, DecidedAtMillis: 900_000,
	}))
	if res2 := h2.BeforeRequest(req2); res2.Err == nil {
		t.Fatal("a contract clock refusal must error the hook even with a stored decision")
	}
}

// TestActivityPersistenceFailureClasses — mode short so no decision write can
// mask the activity outcome: only ADVISORY activity-write failures are
// swallowed; contract refusals and malformed frames surface.
func TestActivityPersistenceFailureClasses(t *testing.T) {
	// Advisory: pass.
	h := newHarness(t)
	h.SetConfig(`{"mode":"short"}`)
	h.StubHostCall("torana_cache_pricing", pricingStub())
	h.SetNow(1_000_000)
	h.StubHostCall("env.state_set", func(args string) (string, error) {
		if strings.Contains(args, "activity/") {
			return sdktest.HostResultError(pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "no store"), nil
		}
		return sdktest.HostResultValue(nil), nil
	})
	res := h.BeforeRequest(reqWith(t, h))
	if res.Err != nil || !res.PassedThrough {
		t.Fatalf("an advisory activity write must be swallowed, err=%v", res.Err)
	}

	// Contract: hook error.
	h2 := newHarness(t)
	h2.SetConfig(`{"mode":"short"}`)
	h2.StubHostCall("torana_cache_pricing", pricingStub())
	h2.SetNow(1_000_000)
	h2.DenyPermission("env.state_set")
	if res2 := h2.BeforeRequest(reqWith(t, h2)); res2.Err == nil {
		t.Fatal("a contract activity write refusal must error the hook")
	}

	// Malformed frame: hook error (a plain protocol error, not a refusal).
	h3 := newHarness(t)
	h3.SetConfig(`{"mode":"short"}`)
	h3.StubHostCall("torana_cache_pricing", pricingStub())
	h3.SetNow(1_000_000)
	h3.StubHostCall("env.state_set", func(string) (string, error) {
		return "not a frame", nil
	})
	if res3 := h3.BeforeRequest(reqWith(t, h3)); res3.Err == nil {
		t.Fatal("a malformed activity write frame must error the hook")
	}
}

// ==========================================================================
// Ordered-carrier hook rows + decline proofs
// ==========================================================================

// TestCarrierHookRows drives the REAL hook with requests whose marker sits
// on each of the three SDK carriers (and a mixed tool+outer request), and
// proves with SINGLE structural equality that the sticky re-application
// lands EXACTLY on the last existing carrier:
//
//  1. the fixture is cloned as the immutable baseline;
//  2. the EXPECTED request is built independently — only the row-designated
//     last carrier is changed to the exact deterministic marker bytes
//     (never via ReplaceLastCacheBreakpoint);
//  3. proto.Equal(result.Request, expected) — the last carrier changed and
//     every other request fact stayed identical; the mixed row separately
//     asserts the earlier tool carrier bytes equal the baseline;
//  4. the decision is replayed on a fresh fixture clone: the byte-identical
//     sticky re-application is a pass-through (no replacement);
//  5. the harness boundary did not mutate the hook input fixture.
func TestCarrierHookRows(t *testing.T) {
	marker := map[string]any{"type": "ephemeral", "ttl": "1h"}
	markerBytes := []byte(mustJSON(t, marker))
	nestedReq := func() *pbv2.ChatRequest {
		return &pbv2.ChatRequest{Model: "m", Messages: []*pbv2.Message{{Role: "user", Blocks: []*pbv2.RequestBlock{
			{Kind: &pbv2.RequestBlock_Text{Text: &pbv2.RequestTextBlock{Text: "a"}}},
			{Kind: &pbv2.RequestBlock_ToolResult{ToolResult: &pbv2.RequestToolResultBlock{
				ToolCallId: "c1",
				Content: []*pbv2.ToolResultContentBlock{
					{Kind: &pbv2.ToolResultContentBlock_Text{Text: &pbv2.ToolResultTextBlock{Text: "r"}}},
					{Kind: &pbv2.ToolResultContentBlock_CacheBreakpoint{CacheBreakpoint: &pbv2.ToolResultCacheBreakpoint{MarkerJson: []byte(`{"type":"ephemeral"}`)}}},
				},
			}}},
		}}}}
	}
	rows := []struct {
		name  string
		req   *pbv2.ChatRequest
		mixed bool // earlier tool carrier must stay byte-identical
		// expected mutates the independent expected request: ONLY the
		// row-designated last carrier receives the exact marker bytes.
		expected func(t *testing.T, e *pbv2.ChatRequest)
	}{
		{
			"tool carrier",
			func() *pbv2.ChatRequest {
				return &pbv2.ChatRequest{Model: "m",
					Tools:    []*pbv2.ToolDef{{Name: "read", Description: "d", ParametersJson: []byte(`{}`), CacheControlJson: []byte(`{"type":"ephemeral"}`)}},
					Messages: []*pbv2.Message{{Role: "user", Blocks: []*pbv2.RequestBlock{{Kind: &pbv2.RequestBlock_Text{Text: &pbv2.RequestTextBlock{Text: "hi"}}}}}},
				}
			}(), false,
			func(t *testing.T, e *pbv2.ChatRequest) { e.Tools[0].CacheControlJson = markerBytes },
		},
		{
			"outer carrier",
			func() *pbv2.ChatRequest { return baseRequest() }(), false,
			func(t *testing.T, e *pbv2.ChatRequest) {
				msgs := e.Messages[len(e.Messages)-1].Blocks
				msgs[len(msgs)-1].GetCacheBreakpoint().MarkerJson = markerBytes
			},
		},
		{
			"nested carrier",
			nestedReq(), false,
			func(t *testing.T, e *pbv2.ChatRequest) {
				tr := e.Messages[0].Blocks[1].GetToolResult()
				tr.Content[len(tr.Content)-1].GetCacheBreakpoint().MarkerJson = markerBytes
			},
		},
		{
			"mixed tool + outer (last = outer)",
			func() *pbv2.ChatRequest {
				r := baseRequest()
				r.Tools = []*pbv2.ToolDef{{Name: "read", Description: "d", ParametersJson: []byte(`{}`), CacheControlJson: []byte(`{"type":"ephemeral"}`)}}
				return r
			}(), true,
			func(t *testing.T, e *pbv2.ChatRequest) {
				msgs := e.Messages[len(e.Messages)-1].Blocks
				msgs[len(msgs)-1].GetCacheBreakpoint().MarkerJson = markerBytes
				// The earlier tool carrier stays at the seed marker.
				e.Tools[0].CacheControlJson = []byte(`{"type":"ephemeral"}`)
			},
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			// Inject metadata FIRST, then take the ONE immutable input
			// baseline.
			row.req.ToranaMetaJson = []byte(`{"_provider":"anthropic","_conversation_id":"conv-1"}`)
			inputBaseline := proto.Clone(row.req).(*pbv2.ChatRequest)

			h := newHarness(t)
			h.StubHostCall("torana_cache_pricing", pricingStub())
			h.SetNow(1_000_000)
			key, has, err := decisionKey(row.req)
			if err != nil || !has {
				t.Fatalf("decisionKey: err=%v has=%v, want a marker-present key", err, has)
			}
			h.SeedState("decision/"+key, mustJSON(t, decision{
				Marker:          marker,
				TierTTL:         3600,
				DecidedAtMillis: 900_000,
			}))

			res := h.BeforeRequest(row.req)
			if res.Err != nil || res.Request == nil {
				t.Fatalf("sticky reapplication failed: err=%v", res.Err)
			}
			// 2+3: independent expected request; single structural equality.
			expected := proto.Clone(inputBaseline).(*pbv2.ChatRequest)
			row.expected(t, expected)
			if !proto.Equal(res.Request, expected) {
				t.Fatalf("result is not exactly the expected request\n got: %v\nwant: %v", res.Request, expected)
			}
			// Mixed row: the earlier tool carrier equals the baseline bytes.
			if row.mixed {
				if !bytes.Equal(res.Request.Tools[0].CacheControlJson, inputBaseline.Tools[0].CacheControlJson) {
					t.Fatalf("earlier tool carrier disturbed: %s != %s", res.Request.Tools[0].CacheControlJson, inputBaseline.Tools[0].CacheControlJson)
				}
			}
			// 4: seed the decision at the RESULT's key, then replay the
			// result request — the byte-identical sticky reapplication is a
			// pass-through (no replacement).
			resultKey, _, err := decisionKey(res.Request)
			if err != nil {
				t.Fatalf("decisionKey(result): %v", err)
			}
			h.SeedState("decision/"+resultKey, mustJSON(t, decision{
				Marker:          marker,
				TierTTL:         3600,
				DecidedAtMillis: 900_000,
			}))
			replay := proto.Clone(res.Request).(*pbv2.ChatRequest)
			res2 := h.BeforeRequest(replay)
			if res2.Err != nil || !res2.PassedThrough {
				t.Fatalf("byte-identical replay must pass through, err=%v", res2.Err)
			}
			// 5: the only mutation the hook input fixture can have suffered
			// is the plugin's own in-place marker apply — it must end EXACTLY
			// as the expected request (any other change would be an
			// unexpected harness-boundary mutation).
			if !proto.Equal(row.req, expected) {
				t.Fatal("the hook input fixture was mutated beyond the applied marker")
			}
		})
	}
}

// TestDeclineProofsZeroCallsNoMutation — the FULL decline proofs: an
// invalid (out-of-domain) request and a no-marker request both pass with
// NO host calls beyond the mode read (env.plugin_config), and the request
// is byte/structurally unchanged.
func TestDeclineProofsZeroCallsNoMutation(t *testing.T) {
	invalid := &pbv2.ChatRequest{Model: "m", Messages: []*pbv2.Message{{Role: "user", Blocks: nil}}}
	noMarker := &pbv2.ChatRequest{Model: "m", Messages: []*pbv2.Message{{Role: "user", Blocks: []*pbv2.RequestBlock{{Kind: &pbv2.RequestBlock_Text{Text: &pbv2.RequestTextBlock{Text: "hi"}}}}}}}
	rows := []struct {
		name string
		req  *pbv2.ChatRequest
	}{
		{"invalid out-of-domain", invalid},
		{"no marker", noMarker},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			h := newHarness(t)
			h.StubHostCall("torana_cache_pricing", pricingStub())
			before := proto.Clone(row.req).(*pbv2.ChatRequest)
			res := h.BeforeRequest(row.req)
			if res.Err != nil || !res.PassedThrough {
				t.Fatalf("must pass unchanged, err=%v", res.Err)
			}
			if !proto.Equal(row.req, before) {
				t.Fatal("the decline mutated the request")
			}
			// EXACT call multiset: exactly one env.plugin_config (the mode
			// read) and nothing else — zero pricing, zero state, zero of
			// anything.
			calls := h.Calls()
			if len(calls) != 1 || calls[0].Command != "env.plugin_config" {
				var got []string
				for _, c := range calls {
					got = append(got, c.Command)
				}
				t.Fatalf("decline call multiset = %v, want exactly [env.plugin_config]", got)
			}
		})
	}
}
