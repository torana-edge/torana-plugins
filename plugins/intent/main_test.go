package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pbv2 "github.com/torana-edge/torana-plugin-sdk/pb/v2"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
	"google.golang.org/protobuf/proto"
)

// newHarness resets the process-global config once-state so every hook-level
// row starts from defaults, then builds a fresh fake host. Tests never run in
// parallel: the plugin's config globals are process-wide and a leaked config
// (e.g. fill:off) would otherwise make row outcomes depend on execution order.
func newHarness(t *testing.T) *sdktest.Harness {
	t.Helper()
	resetConfigForTest()
	return sdktest.New(t)
}

// ==========================================================================
// Schema injection helpers, driven through the harness so hadI meta writes
// reach a host.
// ==========================================================================

func inject(t *testing.T, params string) map[string]any {
	t.Helper()
	h := newHarness(t)
	req := &pbv2.ChatRequest{Tools: []*pbv2.ToolDef{{
		Name:           "test_tool",
		ParametersJson: []byte(params),
	}}}
	var got map[string]any
	h.Run(func() {
		if _, err := injectIntentSchema(req); err != nil {
			t.Fatalf("injectIntentSchema: %v", err)
		}
		if err := json.Unmarshal(req.Tools[0].ParametersJson, &got); err != nil {
			t.Fatalf("plugin emitted unparseable schema: %v", err)
		}
	})
	return got
}

func hasIntentField(t *testing.T, schema map[string]any) bool {
	t.Helper()
	props, _ := schema["properties"].(map[string]any)
	_, ok := props[intentField]
	return ok
}

// TestOpenSchemasAreNotClosed is the regression test. Injecting "i" is fine in
// every one of these cases; forbidding everything else is not.
func TestOpenSchemasAreNotClosed(t *testing.T) {
	for name, params := range map[string]string{
		"author declared an open map":             `{"type":"object","additionalProperties":{"type":"string"}}`,
		"author declared an open map, bool":       `{"type":"object","additionalProperties":true}`,
		"no properties at all (accepts anything)": `{"type":"object"}`,
		// Identical to the line above in JSON Schema terms: an empty
		// properties map permits any property, exactly as an absent one does.
		// This case used to be asserted as "should be closed", which enshrined
		// the bug — a tool accepting anything became one accepting only "i".
		"explicitly empty properties (same schema, spelled out)": `{"type":"object","properties":{}}`,
		"open map alongside properties":                          `{"type":"object","properties":{"path":{"type":"string"}},"additionalProperties":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			got := inject(t, params)

			if got["additionalProperties"] == false {
				t.Error("schema was closed — stricter than the author wrote it, and a tool that accepted free-form arguments now accepts only \"i\"")
			}
			if !hasIntentField(t, got) {
				t.Error("the intent field should still be injected; only the closing is withheld")
			}
		})
	}
}

// TestNamedArgumentSchemasAreStillClosed pins the behaviour that is correct: a
// tool that listed its arguments by name and did not ask to stay open is closed
// as before. Narrowing the rule must not disable it.
func TestNamedArgumentSchemasAreStillClosed(t *testing.T) {
	for name, params := range map[string]string{
		"named properties":          `{"type":"object","properties":{"path":{"type":"string"}}}`,
		"several named properties":  `{"type":"object","properties":{"path":{"type":"string"},"mode":{"type":"string"}}}`,
		"named plus closed already": `{"type":"object","properties":{"path":{"type":"string"}},"additionalProperties":false}`,
	} {
		t.Run(name, func(t *testing.T) {
			got := inject(t, params)

			if got["additionalProperties"] != false {
				t.Errorf("expected the schema to be closed, got additionalProperties=%v", got["additionalProperties"])
			}
			if !hasIntentField(t, got) {
				t.Error("intent field not injected")
			}
		})
	}
}

// TestRootTypeIsPreserved — this plugin has no reason to change the root type,
// and asserting it keeps the two plugins honest about the same contract.
func TestRootTypeIsPreserved(t *testing.T) {
	for _, params := range []string{
		`{"type":"object"}`,
		`{"type":"object","properties":{"path":{"type":"string"}}}`,
		`{"type":"object","additionalProperties":true}`,
	} {
		got := inject(t, params)
		if got["type"] != "object" {
			t.Errorf("%s: root type became %v", params, got["type"])
		}
	}
}

// TestIntentFieldIsRequiredExactlyOnce guards the required list against
// duplicate entries across repeated injection.
//
// Calling injectIntentSchema twice does NOT exercise it — the second call takes
// the "already declared" branch and never reaches the append. The case that
// matters is a tool that lists "i" as required without declaring it as a
// property, which is what this now uses.
func TestIntentFieldIsRequiredExactlyOnce(t *testing.T) {
	got := inject(t, `{"type":"object","properties":{"path":{"type":"string"}},"required":["path","i"]}`)

	required, _ := got["required"].([]any)
	count := 0
	for _, r := range required {
		if s, ok := r.(string); ok && s == intentField {
			count++
		}
	}
	if count != 1 {
		t.Errorf("intent field appears %d times in required, want 1: %v", count, required)
	}
	if !hasIntentField(t, got) {
		t.Error("the property should still be added even though required already named it")
	}
}

// TestStrictSchemasSurviveInjection settles the OpenAI strict-mode question.
//
// Under strict function calling the caller's own schema must already declare
// additionalProperties:false. This plugin only ever SETS that key, never
// unsets it, so a schema that was strict-compliant on the way in is still
// strict-compliant on the way out — with "i" added to properties and required,
// which is what strict mode demands of every declared property.
//
// The corollary matters too: leaving a bare {"type":"object"} open cannot break
// strict mode, because such a schema was already non-compliant before Torana
// saw it.
func TestStrictSchemasSurviveInjection(t *testing.T) {
	got := inject(t, `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`)

	if got["additionalProperties"] != false {
		t.Errorf("a strict-compliant schema lost additionalProperties:false: %v", got["additionalProperties"])
	}
	if !hasIntentField(t, got) {
		t.Fatal("intent field not injected")
	}
	// Strict mode requires every property to be listed in required.
	required, _ := got["required"].([]any)
	var found bool
	for _, r := range required {
		if s, ok := r.(string); ok && s == intentField {
			found = true
		}
	}
	if !found {
		t.Errorf("under strict mode every property must be required; %q is not in %v", intentField, required)
	}
}

// ==========================================================================
// Hook-level matrix (sdktest; the plugin registers in init(), so every
// dispatch exercises the real hook).
// ==========================================================================

// reqWith builds a request carrying one tool + an assistant history call,
// which exercises schema injection, the system prompt, and rehydration.
func reqWith(toolArgs string) *pbv2.ChatRequest {
	return &pbv2.ChatRequest{
		Tools: []*pbv2.ToolDef{{
			Name:           "read",
			ParametersJson: []byte(`{"type":"object","properties":{"path":{"type":"string"}}}`),
		}},
		Messages: []*pbv2.Message{
			{Role: "system", Blocks: []*pbv2.RequestBlock{{Kind: &pbv2.RequestBlock_Text{Text: &pbv2.RequestTextBlock{Text: "You are a coding agent."}}}}},
			{Role: "user", Blocks: []*pbv2.RequestBlock{{Kind: &pbv2.RequestBlock_Text{Text: &pbv2.RequestTextBlock{Text: "find the bug"}}}}},
			{Role: "assistant", Blocks: []*pbv2.RequestBlock{{Kind: &pbv2.RequestBlock_ToolUse{ToolUse: &pbv2.RequestToolUseBlock{Id: "call_1", Name: "read", ArgumentsJson: []byte(toolArgs)}}}}},
		},
	}
}

// streamCall dispatches one assembled tool-call block (start/delta/stop)
// through the real stream hook and returns the final dispatch's result.
func streamCall(t *testing.T, h *sdktest.Harness, id, name, sig, args string) sdktest.StreamResult {
	t.Helper()
	h.StreamChunk(&pbv2.StreamEvent{Event: &pbv2.StreamEvent_ContentBlockStart{
		ContentBlockStart: &pbv2.ContentBlockStart{Index: 0, Block: &pbv2.ContentBlockStart_ToolCall{
			ToolCall: &pbv2.ToolCallRef{Id: id, Name: name, Signature: sig},
		}},
	}})
	h.StreamChunk(&pbv2.StreamEvent{Event: &pbv2.StreamEvent_ToolCallDelta{
		ToolCallDelta: &pbv2.ToolCallDelta{Index: 0, ArgumentsDelta: args},
	}})
	return h.StreamChunk(&pbv2.StreamEvent{Event: &pbv2.StreamEvent_ContentBlockStop{
		ContentBlockStop: &pbv2.ContentBlockStop{Index: 0},
	}})
}

// emittedArgs pulls the delta from an assembled start+delta+stop result.
func emittedArgs(t *testing.T, res sdktest.StreamResult) string {
	t.Helper()
	if res.Err != nil {
		t.Fatalf("stream dispatch error: %v", res.Err)
	}
	if len(res.Events) != 3 {
		t.Fatalf("expected assembled start+delta+stop, got %d events", len(res.Events))
	}
	delta := res.Events[1].GetToolCallDelta()
	if delta == nil || delta.Index != 0 {
		t.Fatalf("second event is not the tool-call delta: %v", res.Events[1])
	}
	return delta.ArgumentsDelta
}

// emittedSig reads the signature on the re-emitted start's ref.
func emittedSig(t *testing.T, res sdktest.StreamResult) string {
	t.Helper()
	start := res.Events[0].GetContentBlockStart()
	if start == nil {
		t.Fatalf("first event is not a content-block start: %v", res.Events[0])
	}
	if tc := start.GetToolCall(); tc != nil {
		return tc.Signature
	}
	return ""
}

// TestBeforeRequestNoToolsPasses — no tools: nothing to teach, no host calls.
func TestBeforeRequestNoToolsPasses(t *testing.T) {
	h := newHarness(t)
	res := h.BeforeRequest(&pbv2.ChatRequest{Messages: []*pbv2.Message{{Role: "user", Blocks: []*pbv2.RequestBlock{{Kind: &pbv2.RequestBlock_Text{Text: &pbv2.RequestTextBlock{Text: "hi"}}}}}}})
	if !res.PassedThrough || res.Err != nil {
		t.Fatalf("expected pass-through, got err=%v request=%v", res.Err, res.Request != nil)
	}
	for _, c := range h.Calls() {
		t.Errorf("unexpected host call with no tools: %s", c.Command)
	}
}

// TestBeforeRequestInjectsSchemaAndPromptAndIsDeterministic — the request side
// teaches the convention and is a pure function of its input (prompt-cache
// compliance: identical requests must produce identical bytes).
func TestBeforeRequestInjectsSchemaAndPromptAndIsDeterministic(t *testing.T) {
	h := newHarness(t)
	req := reqWith(`{"path":"server.go"}`)
	first := h.BeforeRequest(req)
	if first.Err != nil || first.Request == nil {
		t.Fatalf("expected a replacement, err=%v", first.Err)
	}
	if !hasIntentField(t, mustSchema(t, first.Request.Tools[0])) {
		t.Fatal("tool schema did not gain the intent field")
	}
	sys := first.Request.Messages[0]
	if sys.Role != "system" || !strings.Contains(sdk.Text(sys), "Every tool call has an \"i\" field") {
		t.Fatalf("system prompt did not gain the addendum: %q", sdk.Text(sys))
	}
	// Determinism: a fresh clone of the ORIGINAL request must produce the
	// same bytes.
	second := h.BeforeRequest(reqWith(`{"path":"server.go"}`))
	if second.Err != nil {
		t.Fatal(second.Err)
	}
	if !protoEqual(t, first.Request, second.Request) {
		t.Fatal("request-side output differs between identical inputs — busts the provider prompt cache")
	}
	// Rehydration had nothing to restore (no intent cached): the history call
	// must still carry a heuristic "i" fill.
	hist := sdk.ToolCalls(second.Request.Messages[2])[0]
	if !strings.Contains(string(hist.Arguments), `"i":`) {
		t.Fatalf("history tool call was not filled: %s", hist.Arguments)
	}
}

func mustSchema(t *testing.T, tool *pbv2.ToolDef) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(tool.ParametersJson, &out); err != nil {
		t.Fatalf("unparseable tool schema: %v", err)
	}
	return out
}

func protoEqual(t *testing.T, a, b *pbv2.ChatRequest) bool {
	t.Helper()
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}

// TestRehydrationRestoresAndBridges — a captured intent (by content key) is
// restored onto the history call and bridged to intent:<tool_call_id>, the
// key the compactors consume.
func TestRehydrationRestoresAndBridges(t *testing.T) {
	h := newHarness(t)
	key := contentKey("read", map[string]any{"path": "server.go"})
	h.SeedCache(key, "find the bug in server.go")

	res := h.BeforeRequest(reqWith(`{"path":"server.go"}`))
	if res.Err != nil || res.Request == nil {
		t.Fatalf("expected replacement, err=%v", res.Err)
	}
	hist := sdk.ToolCalls(res.Request.Messages[2])[0]
	var args map[string]any
	if err := json.Unmarshal(hist.Arguments, &args); err != nil {
		t.Fatal(err)
	}
	if got := args[intentField]; got != "find the bug in server.go" {
		t.Fatalf("restored intent=%v, want the captured value", got)
	}
	bridged, ok := h.Cache("intent:call_1")
	if !ok || bridged != "find the bug in server.go" {
		t.Fatalf("intent was not bridged to intent:call_1 (ok=%v value=%q)", ok, bridged)
	}
}

func TestContentKeyIsCanonicalOpaqueAndBounded(t *testing.T) {
	a := contentKey("read", map[string]any{"path": "/secret/customer.go", "line": 12})
	b := contentKey("read", map[string]any{"line": 12, "path": "/secret/customer.go", intentField: "ignored"})
	if a != b {
		t.Fatalf("canonical equivalent calls differ: %q vs %q", a, b)
	}
	if strings.Contains(a, "secret") || strings.Contains(a, "customer.go") || strings.Contains(a, "read") {
		t.Fatalf("cache key exposes tool inputs: %q", a)
	}
	large := contentKey("shell", map[string]any{"command": strings.Repeat("private-command ", 10000)})
	if len(large) != len(a) {
		t.Fatalf("key length depends on argument size: small=%d large=%d", len(a), len(large))
	}
	if large == a {
		t.Fatal("different calls collided")
	}
}

// TestRehydrationFillNeverCached — heuristic fills stay request-local: the
// intent cache keeps real-captured-only values.
func TestRehydrationFillNeverCached(t *testing.T) {
	h := newHarness(t)
	res := h.BeforeRequest(reqWith(`{"path":"server.go"}`))
	if res.Err != nil || res.Request == nil {
		t.Fatalf("expected replacement, err=%v", res.Err)
	}
	if _, ok := h.Cache("intent:call_1"); ok {
		t.Fatal("a heuristic fill was cached — the intent cache must stay real-captured-only")
	}
	filled := false
	for _, m := range h.Metrics() {
		if m.Name == "torana_intent_filled_total" {
			filled = true
		}
	}
	if !filled {
		t.Fatal("missing torana_intent_filled_total metric")
	}
}

// TestRehydrationFillOff — config fill:"off" leaves never-captured history
// calls untouched.
func TestRehydrationFillOff(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(`{"fill":"off"}`)
	res := h.BeforeRequest(reqWith(`{"path":"server.go"}`))
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	hist := sdk.ToolCalls(res.Request.Messages[2])[0]
	if strings.Contains(string(hist.Arguments), `"i"`) {
		t.Fatalf("fill:off must leave history untouched: %s", hist.Arguments)
	}
}

// TestConfigResetPinsIsolation — two sequential rows with contradictory
// configs; without resetConfigForTest the first dispatch would freeze the
// process-global once for every later row.
func TestConfigResetPinsIsolation(t *testing.T) {
	// Row 1: fill:off.
	resetConfigForTest()
	h := newHarness(t)
	h.SetConfig(`{"fill":"off"}`)
	h.BeforeRequest(reqWith(`{"path":"a.go"}`))
	// Row 2: default (heuristic) — must see the default, not row 1's value.
	resetConfigForTest()
	h2 := newHarness(t)
	res := h2.BeforeRequest(reqWith(`{"path":"b.go"}`))
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	hist := sdk.ToolCalls(res.Request.Messages[2])[0]
	if !strings.Contains(string(hist.Arguments), `"i"`) {
		t.Fatalf("row 2 leaked row 1's config (fill:off): %s", hist.Arguments)
	}
}

// TestRehydrationPresentEmptyIsUnusable — a present-empty cached value is not
// a restore; the fill path runs (and nothing is bridged).
func TestRehydrationPresentEmptyIsUnusable(t *testing.T) {
	h := newHarness(t)
	h.SeedCache(contentKey("read", map[string]any{"path": "server.go"}), "")
	res := h.BeforeRequest(reqWith(`{"path":"server.go"}`))
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	hist := sdk.ToolCalls(res.Request.Messages[2])[0]
	var args map[string]any
	if err := json.Unmarshal(hist.Arguments, &args); err != nil {
		t.Fatal(err)
	}
	// The fill is deterministic: "what <first-string-arg, 80 runes> shows".
	got, _ := args[intentField].(string)
	if want := "what server.go shows"; got != want {
		t.Fatalf("present-empty cache entry must take the exact heuristic fill, got %q want %q", got, want)
	}
	if _, ok := h.Cache("intent:call_1"); ok {
		t.Fatal("present-empty value must not be bridged")
	}
}

// TestRehydrationCacheRefusalErrors — a non-NOT_FOUND refusal on the request
// side is a contract defect: the hook errors so failure_mode applies.
func TestRehydrationCacheRefusalErrors(t *testing.T) {
	h := newHarness(t)
	h.DenyPermission("env.cache_get")
	res := h.BeforeRequest(reqWith(`{"path":"server.go"}`))
	if res.Err == nil {
		t.Fatal("expected a hook error on a refused cache read, got none")
	}
}

// TestNativeIFieldRecordsMarkerAndIsNotStripped — a tool that natively
// declares "i" keeps its contract; the response side records hadI and never
// strips the value.
func TestNativeIFieldRecordsMarkerAndIsNotStripped(t *testing.T) {
	h := newHarness(t)
	req := &pbv2.ChatRequest{Tools: []*pbv2.ToolDef{{
		Name:           "read",
		ParametersJson: []byte(`{"type":"object","properties":{"path":{"type":"string"},"i":{"type":"string","description":"concise intent"}},"required":["path","i"]}`),
	}}}
	res := h.BeforeRequest(req)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	// The description was upgraded to the example-carrying form, and the
	// response side can now read hadI:read. The native path is SEMANTIC
	// pass-through: the exact original bytes and the bound signature travel
	// unchanged.
	res2 := streamCall(t, h, "call_1", "read", "sig", `{"path":"server.go","i":"native intent"}`)
	out := emittedArgs(t, res2)
	if out != `{"path":"server.go","i":"native intent"}` {
		t.Fatalf("native \"i\" must pass the EXACT bytes, got %q", out)
	}
	if sig := emittedSig(t, res2); sig != "sig" {
		t.Fatalf("native pass must keep the signature, got %q", sig)
	}
	if got, _ := h.Cache("intent:call_1"); got != "native intent" {
		t.Fatalf("native intent not captured: %q", got)
	}
}

// TestStreamExtractsIntentStripsAndClearsSignature — the capture path: the
// assembled block is re-emitted without "i", and the bound signature is
// cleared because the arguments changed.
func TestStreamExtractsIntentStripsAndClearsSignature(t *testing.T) {
	h := newHarness(t)
	res := streamCall(t, h, "call_1", "read", "sig", `{"path":"server.go","i":"find the bug"}`)
	out := emittedArgs(t, res)
	var args map[string]any
	if err := json.Unmarshal([]byte(out), &args); err != nil {
		t.Fatal(err)
	}
	if _, ok := args[intentField]; ok {
		t.Fatal("\"i\" was not stripped from the emitted call")
	}
	if got, _ := h.Cache("intent:call_1"); got != "find the bug" {
		t.Fatalf("intent not captured under intent:call_1: %q", got)
	}
	if _, ok := h.Cache(contentKey("read", map[string]any{"path": "server.go"})); !ok {
		t.Fatal("intent not captured under the content key")
	}
	if sig := emittedSig(t, res); sig != "" {
		t.Fatalf("a changed call must clear the bound signature, got %q", sig)
	}
	captured := false
	for _, m := range h.Metrics() {
		if m.Name == "torana_intent_captured_total" {
			captured = true
		}
	}
	if !captured {
		t.Fatal("missing torana_intent_captured_total metric")
	}
}

// TestStreamPassPreservesSignature — no intent captured and nothing stripped:
// the original block (and its signature) is re-emitted byte-identical.
func TestStreamPassPreservesSignature(t *testing.T) {
	h := newHarness(t)
	res := streamCall(t, h, "call_1", "read", "sig", `{"path":"server.go"}`)
	out := emittedArgs(t, res)
	if out != `{"path":"server.go"}` {
		t.Fatalf("pass path changed the arguments: %q", out)
	}
	if sig := emittedSig(t, res); sig != "sig" {
		t.Fatalf("an unchanged call must keep its signature, got %q", sig)
	}
	if _, ok := h.Cache("intent:call_1"); ok {
		t.Fatal("nothing to capture, nothing cached")
	}
	absent := false
	for _, m := range h.Metrics() {
		if m.Name == "torana_intent_absent_total" {
			absent = true
		}
	}
	if !absent {
		t.Fatal("missing torana_intent_absent_total metric")
	}
}

// TestStreamFailOpenOnCallbackError — a protocol failure inside the callback
// is consumed by StreamHandler: the ORIGINAL block is re-emitted whole (no
// truncation, no retry, signature preserved) and no trap reaches the hook.
func TestStreamFailOpenOnCallbackError(t *testing.T) {
	h := newHarness(t)
	// hadI meta_get refusal (non-NOT_FOUND) makes the callback error.
	h.DenyPermission("env.meta_get")
	res := streamCall(t, h, "call_1", "read", "sig", `{"path":"server.go","i":"find the bug"}`)
	if res.Err != nil {
		t.Fatalf("callback errors must be consumed for fail-open, got %v", res.Err)
	}
	out := emittedArgs(t, res)
	if out != `{"path":"server.go","i":"find the bug"}` {
		t.Fatalf("fail-open must re-emit the original arguments, got %q", out)
	}
	if sig := emittedSig(t, res); sig != "sig" {
		t.Fatalf("fail-open must preserve the signature, got %q", sig)
	}
	// The capture happens before the hadI read, so a failed
	// strip must not retroactively uncache a valid capture — the block is
	// what matters, and it is untouched.
	if got, _ := h.Cache("intent:call_1"); got != "find the bug" {
		t.Fatalf("a valid capture made before the failure must stand, got %q", got)
	}
	// Exactly one meta_get was attempted — no retry.
	gets := 0
	for _, c := range h.Calls() {
		if c.Command == "env.meta_get" {
			gets++
		}
	}
	if gets != 1 {
		t.Fatalf("expected exactly one meta_get attempt, got %d", gets)
	}
}

// TestStreamCacheSetRefusalBestEffort — a refused cache write is logged and
// the current tool call still completes with "i" stripped.
func TestStreamCacheSetRefusalBestEffort(t *testing.T) {
	h := newHarness(t)
	h.DenyPermission("env.cache_set")
	res := streamCall(t, h, "call_1", "read", "sig", `{"path":"server.go","i":"find the bug"}`)
	if res.Err != nil {
		t.Fatalf("a best-effort cache refusal must not fail the call, got %v", res.Err)
	}
	out := emittedArgs(t, res)
	if strings.Contains(out, `"i"`) {
		t.Fatalf("\"i\" was not stripped despite the refused cache write: %q", out)
	}
	if len(h.Logs()) == 0 {
		t.Fatal("the refused cache write was not logged")
	}
}

// TestStreamNonJSONArgsPass — unparseable arguments pass through untouched.
func TestStreamNonJSONArgsPass(t *testing.T) {
	h := newHarness(t)
	res := streamCall(t, h, "call_1", "read", "sig", `not json`)
	out := emittedArgs(t, res)
	if out != "not json" {
		t.Fatalf("non-JSON arguments were changed: %q", out)
	}
}

// TestStreamNoUnauthorizedCalls — every host call across the capture path is
// within the manifest's declared permission set (meta_append is covered by
// env.meta_set, per the StreamHandler contract).
func TestStreamNoUnauthorizedCalls(t *testing.T) {
	h := newHarness(t)
	streamCall(t, h, "call_1", "read", "sig", `{"path":"server.go","i":"find the bug"}`)
	h.BeforeRequest(reqWith(`{"path":"server.go"}`))
	allowed := map[string]bool{
		"env.plugin_config": true,
		"env.meta_get":      true,
		"env.meta_set":      true,
		// The harness records StreamHandler's buffer storage by its raw
		// command; the HOST gates meta_append under the env.meta_set grant
		// (documented StreamHandler contract), so it is in-permission.
		"env.meta_append":      true,
		"env.cache_get":        true,
		"env.cache_set":        true,
		"env.shared_cache_set": true,
		"env.emit_metric":      true,
		"env.log":              true,
	}
	for _, c := range h.Calls() {
		if !allowed[c.Command] {
			t.Errorf("host call outside the declared permission set: %s", c.Command)
		}
	}
}

// TestRequestSideDeterminismWithoutHarnessLeak — two dispatches from two
// harnesses (as two requests would see) produce identical bytes, including
// the fill (pure function of tool+args).
func TestRequestSideDeterminismWithoutHarnessLeak(t *testing.T) {
	h1 := newHarness(t)
	r1 := h1.BeforeRequest(reqWith(`{"path":"server.go"}`))
	h2 := newHarness(t)
	r2 := h2.BeforeRequest(reqWith(`{"path":"server.go"}`))
	if r1.Err != nil || r2.Err != nil {
		t.Fatalf("dispatch errors: %v %v", r1.Err, r2.Err)
	}
	if !protoEqual(t, r1.Request, r2.Request) {
		t.Fatal("identical requests differ across harnesses — prompt-cache busting input")
	}
}

// compile-time guard: the stream handler is registered once in init().
var _ = context.Background
var _ = sdk.MetricCounter

// ==========================================================================
// Round-1 additions: semantic pass-through table, unrepresentable history
// arguments, malformed rehydration replies, schema-default parity.
// ==========================================================================

// TestStreamSemanticHandlingTable — the plugin rewrites the block ONLY when
// it actually deletes an injected "i". JSON formatting, key order, leading
// whitespace, empty/non-string "i", and unrepresentable arguments must pass
// the EXACT original bytes with the signature intact; only injected-"i"
// replacement canonicalizes and clears the signature.
func TestStreamSemanticHandlingTable(t *testing.T) {
	cases := []struct {
		name    string
		args    string
		want    string
		replace bool // wantReplace: emitted args differ from the input
		wantSig string
		// native marks the tool as natively declaring "i" (hadI:read=true).
		native bool
	}{
		{"whitespace prefix, no i", `  {"path":"server.go"}`, `  {"path":"server.go"}`, false, "sig", false},
		{"reversed key order, no i", `{"z":1,"path":"server.go"}`, `{"z":1,"path":"server.go"}`, false, "sig", false},
		{"no i", `{"path":"server.go"}`, `{"path":"server.go"}`, false, "sig", false},
		{"injected empty i", `{"path":"server.go","i":""}`, `{"path":"server.go"}`, true, "", false},
		{"injected number i", `{"path":"server.go","i":5}`, `{"path":"server.go"}`, true, "", false},
		{"injected object i", `{"path":"server.go","i":{"x":1}}`, `{"path":"server.go"}`, true, "", false},
		{"injected boolean i", `{"path":"server.go","i":true}`, `{"path":"server.go"}`, true, "", false},
		{"injected null i", `{"path":"server.go","i":null}`, `{"path":"server.go"}`, true, "", false},
		{"native i", `{"path":"server.go","i":"native intent"}`, `{"path":"server.go","i":"native intent"}`, false, "sig", true},
		{"native empty i", `{"path":"server.go","i":""}`, `{"path":"server.go","i":""}`, false, "sig", true},
		{"native number i", `{"path":"server.go","i":5}`, `{"path":"server.go","i":5}`, false, "sig", true},
		{"native null i", `{"path":"server.go","i":null}`, `{"path":"server.go","i":null}`, false, "sig", true},
		{"injected i", `{"path":"server.go","i":"find the bug"}`, `{"path":"server.go"}`, true, "", false},
		{"whitespace prefix, injected i", `  {"path":"server.go","i":"find the bug"}`, `{"path":"server.go"}`, true, "", false},
		{"null", `null`, `null`, false, "sig", false},
		{"array", `[1,2]`, `[1,2]`, false, "sig", false},
		{"scalar", `42`, `42`, false, "sig", false},
		{"malformed", `{`, `{`, false, "sig", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			if tc.native {
				// Drive the request side so hadI:read=true is recorded on this
				// harness (the marker lives in the harness's meta store).
				h.BeforeRequest(&pbv2.ChatRequest{Tools: []*pbv2.ToolDef{{
					Name:           "read",
					ParametersJson: []byte(`{"type":"object","properties":{"path":{"type":"string"},"i":{"type":"string"}},"required":["path","i"]}`),
				}}})
			}
			res := streamCall(t, h, "call_1", "read", "sig", tc.args)
			if res.Err != nil {
				t.Fatalf("dispatch error: %v", res.Err)
			}
			if got := emittedArgs(t, res); got != tc.want {
				t.Fatalf("emitted args=%q, want %q", got, tc.want)
			}
			if sig := emittedSig(t, res); sig != tc.wantSig {
				t.Fatalf("signature=%q, want %q", sig, tc.wantSig)
			}
		})
	}
}

// TestRehydrationUnrepresentableArgumentsNoPanic — stored arguments_json
// that is null (which decodes to a nil map), an array, a scalar, or malformed
// JSON must be left byte-identical; only a real JSON object may be filled.
// Regression for the nil-map assignment panic on "null".
func TestRehydrationUnrepresentableArgumentsNoPanic(t *testing.T) {
	cases := []string{
		"null",
		`[1,2]`,
		`"str"`,
		`42`,
		`{`,
	}
	for _, raw := range cases {
		t.Run(raw, func(t *testing.T) {
			newHarness(t) // resets the process-global config for each row
			req := &pbv2.ChatRequest{Tools: []*pbv2.ToolDef{{
				Name:           "read",
				ParametersJson: []byte(`{"type":"object","properties":{"path":{"type":"string"}}}`),
			}}}
			req.Messages = []*pbv2.Message{
				{Role: "user", Blocks: []*pbv2.RequestBlock{{Kind: &pbv2.RequestBlock_Text{Text: &pbv2.RequestTextBlock{Text: "hi"}}}}},
				{Role: "assistant", Blocks: []*pbv2.RequestBlock{{Kind: &pbv2.RequestBlock_ToolUse{ToolUse: &pbv2.RequestToolUseBlock{Id: "call_1", Name: "read", ArgumentsJson: []byte(raw)}}}}},
			}
			before := proto.Clone(req).(*pbv2.ChatRequest)
			modified, err := rehydrateHistoryIntents(req)
			if err != nil {
				t.Fatalf("hook error (must not panic): %v", err)
			}
			if modified {
				t.Fatalf("unrepresentable args must be left untouched (modified=true): %q", raw)
			}
			if !proto.Equal(req, before) {
				t.Fatalf("unrepresentable arguments were changed: %q -> %q", raw, sdk.ToolCalls(req.Messages[1])[0].Arguments)
			}
		})
	}

	// An EMPTY OBJECT is representable and must be filled.
	h := newHarness(t)
	req := &pbv2.ChatRequest{Tools: []*pbv2.ToolDef{{
		Name:           "read",
		ParametersJson: []byte(`{"type":"object","properties":{"path":{"type":"string"}}}`),
	}}}
	req.Messages = []*pbv2.Message{
		{Role: "user", Blocks: []*pbv2.RequestBlock{{Kind: &pbv2.RequestBlock_Text{Text: &pbv2.RequestTextBlock{Text: "hi"}}}}},
		{Role: "assistant", Blocks: []*pbv2.RequestBlock{{Kind: &pbv2.RequestBlock_ToolUse{ToolUse: &pbv2.RequestToolUseBlock{Id: "call_1", Name: "read", ArgumentsJson: []byte(`{}`)}}}}},
	}
	res := h.BeforeRequest(req)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if !strings.Contains(string(sdk.ToolCalls(res.Request.Messages[2])[0].Arguments), `"i"`) {
		t.Fatalf("an empty object must be filled: %s", sdk.ToolCalls(res.Request.Messages[2])[0].Arguments)
	}
}

// TestRehydrationMalformedReplyErrors — a malformed HostCallResult on the
// rehydration cache read is a protocol error: the hook errors.
func TestRehydrationMalformedReplyErrors(t *testing.T) {
	h := newHarness(t)
	h.StubHostCall("env.cache_get", func(string) (string, error) {
		return "not a host-call-result frame", nil
	})
	res := h.BeforeRequest(reqWith(`{"path":"server.go"}`))
	if res.Err == nil {
		t.Fatal("a malformed cache reply must error the hook")
	}
}

// TestSchemaDefaultsMatchRuntimeDefaults — parity against schema.json: the
// schema's fill default must equal the runtime default ("heuristic"), so a
// schema/runtime drift cannot pass unnoticed.
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
	prop, ok := schema.Properties["fill"]
	if !ok {
		t.Fatal("schema.json has no fill property")
	}
	var want string
	if err := json.Unmarshal(prop.Default, &want); err != nil {
		t.Fatal(err)
	}
	if want != "heuristic" {
		t.Fatalf("schema fill default=%q, want heuristic", want)
	}
	if got := parseConfig(""); got != "heuristic" {
		t.Fatalf("runtime fill default=%q does not match the schema", got)
	}
}

// TestRehydrationProvenanceAwareWrites — the rehydration write is
// provenance-aware: with MULTIPLE tool-use blocks (designated + siblings),
// ONLY the designated block changes, a REAL change clears the designated
// call's signature token while the siblings keep theirs, and a
// byte-identical rewrite is a no-op that preserves every token.
func TestRehydrationProvenanceAwareWrites(t *testing.T) {
	use := func(id, name, args, sig string) *pbv2.RequestBlock {
		return &pbv2.RequestBlock{Kind: &pbv2.RequestBlock_ToolUse{ToolUse: &pbv2.RequestToolUseBlock{
			Id: id, Name: name, ArgumentsJson: []byte(args), Signature: sig,
		}}}
	}
	h := newHarness(t)
	req := &pbv2.ChatRequest{Tools: []*pbv2.ToolDef{{
		Name:           "read",
		ParametersJson: []byte(`{"type":"object","properties":{"path":{"type":"string"}}}`),
	}}}
	req.Messages = []*pbv2.Message{
		{Role: "user", Blocks: []*pbv2.RequestBlock{{Kind: &pbv2.RequestBlock_Text{Text: &pbv2.RequestTextBlock{Text: "hi"}}}}},
		{Role: "assistant", Blocks: []*pbv2.RequestBlock{
			use("c1", "read", `{"path":"server.go"}`, "call-sig-1"),
			// A NON-TOOL block interleaved between tool-use blocks: the
			// views' ordinals (0,1,2) differ from the block indices
			// (0,2,3), so a regression from tc.Block to the tool-call loop
			// index would target the wrong blocks (and error on this one).
			{Kind: &pbv2.RequestBlock_Text{Text: &pbv2.RequestTextBlock{Text: "interleaved note"}}},
			use("c2", "read", `{"path":"other.go","i":"existing"}`, "call-sig-2"),
			use("c3", "grep", `{"pattern":"x"}`, "call-sig-3"),
		}},
	}
	h.SeedCache("intent:c1", "find the bug in server")
	var modified bool
	var err error
	h.Run(func() { modified, err = rehydrateHistoryIntents(req) })
	if err != nil {
		t.Fatal(err)
	}
	if !modified {
		t.Fatal("the eligible calls must be filled")
	}
	// The CHANGED blocks (c1, c3) were rewritten and their call-bound tokens
	// cleared; the SKIPPED sibling (c2 — already carrying "i") is
	// byte-identical INCLUDING its signature token.
	got := sdk.ToolCalls(req.Messages[1])
	if string(got[0].Arguments) == `{"path":"server.go"}` {
		t.Fatalf("c1 was not filled: %s", got[0].Arguments)
	}
	if got[0].Signature != "" {
		t.Fatalf("a REAL change must clear the designated call's token: %q", got[0].Signature)
	}
	if string(got[1].Arguments) != `{"path":"other.go","i":"existing"}` || got[1].Signature != "call-sig-2" {
		t.Fatalf("the skipped sibling c2 was disturbed: %s sig=%q", got[1].Arguments, got[1].Signature)
	}
	if string(got[2].Arguments) == `{"pattern":"x"}` {
		t.Fatalf("c3 was not filled: %s", got[2].Arguments)
	}
	if got[2].Signature != "" {
		t.Fatalf("a REAL change must clear c3's token: %q", got[2].Signature)
	}
	// The interleaved non-tool block (block index 1) is byte-identical.
	inter := req.Messages[1].Blocks[1].GetText()
	if inter == nil || inter.Text != "interleaved note" || inter.Signature != "" {
		t.Fatalf("the interleaved block was disturbed: %+v", inter)
	}

	// A request where EVERY call already carries "i" is a no-op preserving
	// every token.
	req2 := &pbv2.ChatRequest{Tools: req.Tools}
	req2.Messages = []*pbv2.Message{
		{Role: "user", Blocks: []*pbv2.RequestBlock{{Kind: &pbv2.RequestBlock_Text{Text: &pbv2.RequestTextBlock{Text: "hi"}}}}},
		{Role: "assistant", Blocks: []*pbv2.RequestBlock{
			use("c1", "read", `{"path":"server.go","i":"find the bug in server"}`, "call-sig-1"),
			use("c2", "read", `{"path":"other.go","i":"existing"}`, "call-sig-2"),
		}},
	}
	before := proto.Clone(req2).(*pbv2.ChatRequest)
	h.Run(func() { modified, err = rehydrateHistoryIntents(req2) })
	if err != nil {
		t.Fatal(err)
	}
	if modified {
		t.Fatal("an already-filled request must not be rewritten")
	}
	if !proto.Equal(req2, before) {
		t.Fatal("the no-op disturbed the message")
	}
}

// TestSystemPromptProvenanceAwareInjection — the injection is
// provenance-aware over the ordered body: with a system
// [text, sibling, text, trailing] topology, the LAST text block receives the
// addendum, the sibling text is untouched, a real change clears the touched
// text block's token AND the trailing carrier (its covered content changed),
// and a no-text system message gets a valid appended text block (the
// trailing carrier removed first).
func TestSystemPromptProvenanceAwareInjection(t *testing.T) {
	text := func(s, sig string) *pbv2.RequestBlock {
		return &pbv2.RequestBlock{Kind: &pbv2.RequestBlock_Text{Text: &pbv2.RequestTextBlock{Text: s, Signature: sig}}}
	}
	trailing := func() *pbv2.RequestBlock {
		return &pbv2.RequestBlock{Kind: &pbv2.RequestBlock_TrailingSignature{TrailingSignature: &pbv2.RequestTrailingSignatureBlock{Signature: "trail-sig"}}}
	}
	h := newHarness(t)
	req := &pbv2.ChatRequest{Tools: []*pbv2.ToolDef{{
		Name:           "read",
		ParametersJson: []byte(`{"type":"object","properties":{"path":{"type":"string"}}}`),
	}}}
	req.Messages = []*pbv2.Message{
		{Role: "system", Blocks: []*pbv2.RequestBlock{
			text("first part", "text-sig-1"),
			text("sibling", "text-sig-2"),
			text("last part", "text-sig-3"),
			trailing(),
		}},
	}
	var changed bool
	var err error
	h.Run(func() { changed, err = injectSystemPrompt(req) })
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("the addendum must be injected")
	}
	blocks := req.Messages[0].Blocks
	if !strings.Contains(blocks[2].GetText().Text, "Every tool call has an \"i\" field") {
		t.Fatalf("the LAST text block must receive the addendum: %q", blocks[2].GetText().Text)
	}
	if blocks[1].GetText().Text != "sibling" || blocks[1].GetText().Signature != "text-sig-2" {
		t.Fatalf("the sibling text was disturbed: %q sig=%q", blocks[1].GetText().Text, blocks[1].GetText().Signature)
	}
	if blocks[0].GetText().Text != "first part" || blocks[0].GetText().Signature != "text-sig-1" {
		t.Fatalf("the first text block was disturbed: %q sig=%q", blocks[0].GetText().Text, blocks[0].GetText().Signature)
	}
	if blocks[2].GetText().Signature != "" {
		t.Fatalf("a real change must clear the touched block's token: %q", blocks[2].GetText().Signature)
	}
	if len(blocks) != 3 {
		t.Fatalf("the trailing carrier must be cleared (covered content changed): %d blocks", len(blocks))
	}

	// A no-text system message: ReplaceAllText appends a valid text block
	// (removing a final trailing carrier first).
	req2 := &pbv2.ChatRequest{Tools: req.Tools}
	req2.Messages = []*pbv2.Message{
		{Role: "system", Blocks: []*pbv2.RequestBlock{trailing()}},
	}
	h.Run(func() { changed, err = injectSystemPrompt(req2) })
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("the no-text system message must be injected")
	}
	blocks2 := req2.Messages[0].Blocks
	if len(blocks2) != 1 || blocks2[0].GetText() == nil {
		t.Fatalf("the no-text path must yield exactly one text block: %+v", blocks2)
	}
	if blocks2[0].GetText().Text != addendum {
		t.Fatalf("the appended text is wrong: %q", blocks2[0].GetText().Text)
	}
}
