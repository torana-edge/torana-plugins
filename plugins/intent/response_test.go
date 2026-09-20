package main

import (
	"strings"
	"testing"

	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
)

// ==========================================================================
// Response hook (non-streaming capture and strip)
// ==========================================================================
//
// The request side injects "i" whether or not the caller asked for a stream,
// so these rows mirror the streamed ones: capture into the same cache
// protocol, strip what Torana injected, never touch a tool's native "i", and
// leave the bound signature alone unless the arguments actually change.
//
// Deliberately absent: the unparseable/non-object argument rows the stream
// tests carry. A response tool call must hold a non-empty JSON object to be
// dispatchable at all — the SDK's hook-input validation refuses the rest
// before a handler sees it.

func toolCallBlock(id, name, args, sig string) *pbv1.ResponseBlock {
	return &pbv1.ResponseBlock{Kind: &pbv1.ResponseBlock_ToolCall{ToolCall: &pbv1.ToolCall{
		Id: id, Name: name, ArgumentsJson: []byte(args), Signature: sig,
	}}}
}

func textBlock(text string) *pbv1.ResponseBlock {
	return &pbv1.ResponseBlock{Kind: &pbv1.ResponseBlock_Text{Text: &pbv1.ResponseTextBlock{Text: text}}}
}

func responseWith(blocks ...*pbv1.ResponseBlock) *pbv1.ChatResponse {
	return &pbv1.ChatResponse{
		Model:          "claude-sonnet-4",
		Id:             "resp_1",
		FinishReason:   "tool_calls",
		UpstreamStatus: 200,
		Message:        &pbv1.ResponseMessage{Blocks: blocks},
	}
}

// injectedRequest runs the request hook for a tool whose schema does NOT
// declare "i", so the response side treats a present "i" as Torana's.
func injectedRequest(t *testing.T, h *sdktest.Harness, params string) *sdktest.Request {
	t.Helper()
	r := h.NewRequest()
	res := r.BeforeRequest(&pbv1.ChatRequest{
		ToranaMetaJson: []byte(`{"_conversation_id":"conv-1"}`),
		Tools:          []*pbv1.ToolDef{{Name: "read", ParametersJson: []byte(params)}},
	})
	if res.Err != nil {
		t.Fatalf("request hook: %v", res.Err)
	}
	return r
}

func appliedCall(t *testing.T, res sdktest.ResponseResult, index int) *pbv1.ToolCall {
	t.Helper()
	if res.Err != nil {
		t.Fatalf("response dispatch error: %v", res.Err)
	}
	if res.Applied == nil {
		t.Fatal("expected a replacement, got pass-through")
	}
	call := res.Applied.Message.Blocks[index].GetToolCall()
	if call == nil {
		t.Fatalf("block %d is not a tool call", index)
	}
	return call
}

// TestResponseStripsInjectedIntent is the gap this hook closes: without it a
// non-streamed answer hands the harness an argument the tool never declared.
func TestResponseStripsInjectedIntent(t *testing.T) {
	h := newHarness(t)
	r := injectedRequest(t, h, `{"type":"object","properties":{"path":{"type":"string"}}}`)
	res := r.AfterResponse(responseWith(
		toolCallBlock("call_1", "read", `{"path":"server.go","i":"where the currency mapping lives"}`, "sig-abc"),
	), true)

	call := appliedCall(t, res, 0)
	if strings.Contains(string(call.ArgumentsJson), `"i"`) {
		t.Fatalf("the injected field reached the harness: %s", call.ArgumentsJson)
	}
	if string(call.ArgumentsJson) != `{"path":"server.go"}` {
		t.Fatalf("arguments = %s", call.ArgumentsJson)
	}
	if call.Signature != "" {
		t.Fatalf("signature %q survived an argument change", call.Signature)
	}
	if call.Id != "call_1" || call.Name != "read" {
		t.Fatalf("tool identity changed: id=%q name=%q", call.Id, call.Name)
	}
	// The captured intent reaches the compactors through the shared cache,
	// under the same key the stream path publishes.
	if got, ok := h.SharedCache(intentCacheKey + ":call_1"); !ok || got != "where the currency mapping lives" {
		t.Fatalf("shared cache entry = %q (present=%v)", got, ok)
	}
}

// TestResponseStreamParity — the two paths must not drift.
func TestResponseStreamParity(t *testing.T) {
	const args = `{"path":"server.go","i":"why the EU price is wrong"}`

	streamed := newHarness(t)
	streamArgs := emittedArgs(t, streamCall(t, streamed, "call_1", "read", "sig-abc", args))

	direct := newHarness(t)
	r := injectedRequest(t, direct, `{"type":"object","properties":{"path":{"type":"string"}}}`)
	responseArgs := string(appliedCall(t, r.AfterResponse(responseWith(toolCallBlock("call_1", "read", args, "sig-abc")), true), 0).ArgumentsJson)

	if streamArgs != responseArgs {
		t.Fatalf("stream strip %s, response strip %s", streamArgs, responseArgs)
	}
	streamIntent, _ := streamed.SharedCache(intentCacheKey + ":call_1")
	responseIntent, _ := direct.SharedCache(intentCacheKey + ":call_1")
	if streamIntent != responseIntent {
		t.Fatalf("stream captured %q, response captured %q", streamIntent, responseIntent)
	}
}

// TestResponseKeepsNativeIntentField — a tool that declares "i" itself keeps
// its value AND its signature; the marker written on the request side is what
// decides, not a guess about the value.
func TestResponseKeepsNativeIntentField(t *testing.T) {
	h := newHarness(t)
	r := injectedRequest(t, h, `{"type":"object","properties":{"path":{"type":"string"},"i":{"type":"string"}},"required":["i"]}`)
	res := r.AfterResponse(responseWith(
		toolCallBlock("call_1", "read", `{"path":"server.go","i":"native reason"}`, "sig-abc"),
	), true)

	if res.Err != nil {
		t.Fatalf("dispatch error: %v", res.Err)
	}
	if !res.PassedThrough {
		t.Fatalf("a native \"i\" must pass byte-identical, got %v", res.Replacement)
	}
	if got, ok := h.SharedCache(intentCacheKey + ":call_1"); !ok || got != "native reason" {
		t.Fatalf("native intent was not forwarded to compactors (ok=%v value=%q)", ok, got)
	}
}

// TestResponseWithoutIntentPassesByteIdentical — no "i" key at all means no
// marker lookup, no marshal, and no signature change.
func TestResponseWithoutIntentPassesByteIdentical(t *testing.T) {
	h := newHarness(t)
	r := injectedRequest(t, h, `{"type":"object","properties":{"path":{"type":"string"}}}`)
	before := len(h.Calls())
	res := r.AfterResponse(responseWith(toolCallBlock("call_1", "read", `{"path":"server.go"}`, "sig-abc")), true)
	if res.Err != nil || !res.PassedThrough {
		t.Fatalf("expected pass-through, err=%v", res.Err)
	}
	for _, c := range h.Calls()[before:] {
		if c.Command == "env.meta_get" {
			t.Fatal("looked up the hadI marker for a call with no \"i\"")
		}
	}
	absent := false
	for _, m := range h.Metrics() {
		absent = absent || m.Name == "torana_intent_absent_total"
	}
	if !absent {
		t.Fatal("the absent-intent counter was not emitted")
	}
}

// TestResponseObservationalDispatchIsUntouched — a streamed response reaches
// this hook observationally after the stream hook already handled it, and an
// upstream error has no body to rewrite. Neither may capture or strip.
func TestResponseObservationalDispatchIsUntouched(t *testing.T) {
	h := newHarness(t)
	r := injectedRequest(t, h, `{"type":"object","properties":{"path":{"type":"string"}}}`)
	res := r.AfterResponse(responseWith(
		toolCallBlock("call_1", "read", `{"path":"server.go","i":"already captured by the stream hook"}`, "sig-abc"),
	), false)
	if res.Err != nil || !res.PassedThrough {
		t.Fatalf("an observational dispatch must pass through, err=%v", res.Err)
	}
	if _, ok := h.SharedCache(intentCacheKey + ":call_1"); ok {
		t.Fatal("an observational dispatch captured an intent a second time")
	}
}

// TestResponseMarkerRefusalDoesNotGuess — a refused hadI read is a protocol
// failure. The hook errors, so the host applies failure_mode and the original
// response stands, rather than stripping a field the tool may own.
func TestResponseMarkerRefusalDoesNotGuess(t *testing.T) {
	h := newHarness(t)
	r := injectedRequest(t, h, `{"type":"object","properties":{"path":{"type":"string"}}}`)
	h.StubHostCall("env.meta_get", func(string) (string, error) {
		return sdktest.HostResultError(pbv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "denied"), nil
	})
	res := r.AfterResponse(responseWith(
		toolCallBlock("call_1", "read", `{"path":"server.go","i":"reason"}`, "sig-abc"),
	), true)
	if res.Err == nil {
		t.Fatalf("a refused marker read must not strip, got %v", res.Replacement)
	}
	if res.Replacement != nil {
		t.Fatalf("a failed strip proposed a replacement: %v", res.Replacement)
	}
}

// TestResponseLeavesOtherBlocksExactlyAsSent — text blocks, block topology,
// and host-owned response facts survive a replacement driven by one call.
func TestResponseLeavesOtherBlocksExactlyAsSent(t *testing.T) {
	h := newHarness(t)
	r := injectedRequest(t, h, `{"type":"object","properties":{"path":{"type":"string"}}}`)
	res := r.AfterResponse(responseWith(
		textBlock("looking now"),
		toolCallBlock("call_1", "read", `{"path":"a.go","i":"find the bug"}`, "sig-1"),
		toolCallBlock("call_2", "search", `{"query":"bug"}`, "sig-2"),
	), true)

	applied := res.Applied
	if res.Err != nil || applied == nil {
		t.Fatalf("expected a replacement, err=%v", res.Err)
	}
	if len(applied.Message.Blocks) != 3 {
		t.Fatalf("block cardinality changed: %d", len(applied.Message.Blocks))
	}
	if text := applied.Message.Blocks[0].GetText(); text == nil || text.Text != "looking now" {
		t.Fatalf("text block was not preserved: %v", applied.Message.Blocks[0])
	}
	untouched := applied.Message.Blocks[2].GetToolCall()
	if untouched == nil || string(untouched.ArgumentsJson) != `{"query":"bug"}` || untouched.Signature != "sig-2" {
		t.Fatalf("a call with no \"i\" was rewritten: %v", untouched)
	}
	if applied.Model != "claude-sonnet-4" || applied.Id != "resp_1" || applied.FinishReason != "tool_calls" {
		t.Fatalf("host-owned response facts changed: %+v", applied)
	}
}

// TestResponseCacheRefusalIsBestEffort — a refused cache write affects future
// compaction, not this response: the field is still stripped.
func TestResponseCacheRefusalIsBestEffort(t *testing.T) {
	h := newHarness(t)
	r := injectedRequest(t, h, `{"type":"object","properties":{"path":{"type":"string"}}}`)
	h.DenyPermission("env.shared_cache_set")
	res := r.AfterResponse(responseWith(
		toolCallBlock("call_1", "read", `{"path":"a.go","i":"find the bug"}`, ""),
	), true)
	call := appliedCall(t, res, 0)
	if strings.Contains(string(call.ArgumentsJson), `"i"`) {
		t.Fatalf("a refused cache write stopped the strip: %s", call.ArgumentsJson)
	}
}

// TestCapturedIntentIsNeverLogged — the captured value is the user's task in
// the model's words. Diagnostics may say that something was captured and how
// long it was; they may not reproduce it. Both response paths are checked,
// because a leak on either one ships the same text to the host log.
func TestCapturedIntentIsNeverLogged(t *testing.T) {
	const secret = "why the Contoso invoice totals are wrong"

	t.Run("stream", func(t *testing.T) {
		h := newHarness(t)
		streamCall(t, h, "call_1", "read", "", `{"path":"a.go","i":"`+secret+`"}`)
		assertNoIntentInLogs(t, h, secret)
	})

	t.Run("response", func(t *testing.T) {
		h := newHarness(t)
		r := injectedRequest(t, h, `{"type":"object","properties":{"path":{"type":"string"}}}`)
		r.AfterResponse(responseWith(toolCallBlock("call_1", "read", `{"path":"a.go","i":"`+secret+`"}`, "")), true)
		assertNoIntentInLogs(t, h, secret)
	})

	// History fill derives its value from the call's own arguments, so
	// logging the fill leaks the same class of content.
	t.Run("history fill", func(t *testing.T) {
		h := newHarness(t)
		req := reqWith(`{"path":"` + secret + `"}`)
		if res := h.BeforeRequest(req); res.Err != nil {
			t.Fatalf("request hook: %v", res.Err)
		}
		assertNoIntentInLogs(t, h, secret)
	})
}

func assertNoIntentInLogs(t *testing.T, h *sdktest.Harness, secret string) {
	t.Helper()
	for _, entry := range h.Logs() {
		if strings.Contains(entry.Message, secret) {
			t.Fatalf("a log line carried the captured content: %q", entry.Message)
		}
	}
}
