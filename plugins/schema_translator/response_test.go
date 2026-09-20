package main

import (
	"encoding/json"
	"strings"
	"testing"

	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
)

// ==========================================================================
// Response hook (non-streaming reversal)
// ==========================================================================
//
// The request hook translates schemas for streamed and non-streamed requests
// alike, so these rows are the non-streaming half of the matrix the stream
// tests above already cover: same registry, same strictness, same
// preserve-on-no-op rule, plus the host's replacement contract (fixed block
// cardinality/arm/position, host-owned tool id, signature cleared only when
// the arguments it covers change).
//
// Deliberately absent: the malformed-argument rows the stream tests carry
// (empty bytes, null, arrays, scalars, unparseable JSON). A response tool
// call must hold a non-empty JSON object to be dispatchable at all — the
// SDK's hook-input validation refuses the rest before a handler sees it — so
// those states are unreachable here. The reachable strictness rows below are
// the ones a real provider can produce: a recorded path whose value is not a
// KV array, malformed KV items, and duplicate JSON keys.

// mapTool is the schema whose "env" property is translated into a KV array.
const mapTool = `{"type":"object","properties":{"env":{"type":"object","additionalProperties":{"type":"string"}}}}`

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

// translatedRequest runs the request hook so the mutation registry for
// mapTool is published into this request's metadata, exactly as the host
// sequences it before a response arrives.
func translatedRequest(t *testing.T, h *sdktest.Harness) *sdktest.Request {
	t.Helper()
	r := h.NewRequest()
	if res := r.BeforeRequest(reqWithTools(mapTool)); res.Err != nil {
		t.Fatalf("request hook: %v", res.Err)
	}
	return r
}

func replacedCall(t *testing.T, res sdktest.ResponseResult, index int) *pbv1.ToolCall {
	t.Helper()
	if res.Err != nil {
		t.Fatalf("response dispatch error: %v", res.Err)
	}
	if res.Applied == nil {
		t.Fatalf("expected a replacement, got pass-through")
	}
	call := res.Applied.Message.Blocks[index].GetToolCall()
	if call == nil {
		t.Fatalf("block %d is not a tool call: %v", index, res.Applied.Message.Blocks[index])
	}
	return call
}

// TestResponseReversesRecordedCall is the gap this hook closes: a
// non-streaming tool call reaches the harness in the ORIGINAL map shape, not
// the model-facing KV array.
func TestResponseReversesRecordedCall(t *testing.T) {
	h := newHarness(t)
	r := translatedRequest(t, h)
	res := r.AfterResponse(responseWith(
		toolCallBlock("call_1", "read", `{"env":[{"key":"HOME","value":"/root"},{"key":"TERM","value":"xterm"}]}`, "sig-abc"),
	), true)

	call := replacedCall(t, res, 0)
	if got := string(call.ArgumentsJson); got != `{"env":{"HOME":"/root","TERM":"xterm"}}` {
		t.Fatalf("arguments = %s, want the reversed map", got)
	}
	if call.Signature != "" {
		t.Fatalf("signature %q survived an argument change — it binds the arguments the provider signed", call.Signature)
	}
	if call.Id != "call_1" || call.Name != "read" {
		t.Fatalf("tool identity changed: id=%q name=%q", call.Id, call.Name)
	}
}

// TestResponseStreamParity — the two response paths must not drift: the same
// call, reversed through the stream hook and through the response hook,
// produces the same arguments.
func TestResponseStreamParity(t *testing.T) {
	const args = `{"env":[{"key":"HOME","value":"/root"}]}`

	streamed := newHarness(t)
	sr := streamed.NewRequest()
	sr.BeforeRequest(reqWithTools(mapTool))
	streamArgs := emittedArgs(t, streamBlockOn(t, sr, 0, "call_1", "read", "sig-abc", args))

	direct := newHarness(t)
	res := translatedRequest(t, direct).AfterResponse(responseWith(toolCallBlock("call_1", "read", args, "sig-abc")), true)
	responseArgs := string(replacedCall(t, res, 0).ArgumentsJson)

	if streamArgs != responseArgs {
		t.Fatalf("stream reversal %s, response reversal %s", streamArgs, responseArgs)
	}
}

// TestResponseLeavesUntouchedBlocksExactlyAsSent — an unrecorded tool, a text
// block and the block topology all survive a replacement driven by a
// different block.
func TestResponseLeavesUntouchedBlocksExactlyAsSent(t *testing.T) {
	h := newHarness(t)
	r := translatedRequest(t, h)
	res := r.AfterResponse(responseWith(
		textBlock("let me look"),
		toolCallBlock("call_1", "read", `{"env":[{"key":"HOME","value":"/root"}]}`, "sig-read"),
		toolCallBlock("call_2", "search", `{"query":"bug"}`, "sig-search"),
	), true)

	applied := res.Applied
	if res.Err != nil || applied == nil {
		t.Fatalf("expected a replacement, err=%v", res.Err)
	}
	if len(applied.Message.Blocks) != 3 {
		t.Fatalf("block cardinality changed: %d", len(applied.Message.Blocks))
	}
	if text := applied.Message.Blocks[0].GetText(); text == nil || text.Text != "let me look" {
		t.Fatalf("text block was not preserved: %v", applied.Message.Blocks[0])
	}
	untouched := applied.Message.Blocks[2].GetToolCall()
	if untouched == nil || string(untouched.ArgumentsJson) != `{"query":"bug"}` {
		t.Fatalf("unrecorded tool call was rewritten: %v", untouched)
	}
	if untouched.Signature != "sig-search" {
		t.Fatalf("unrecorded call lost its signature (%q) — dropping provenance over unchanged content is forgery", untouched.Signature)
	}
	if applied.Model != "claude-sonnet-4" || applied.Id != "resp_1" || applied.FinishReason != "tool_calls" {
		t.Fatalf("host-owned response facts changed: %+v", applied)
	}
}

// TestResponsePassesSemanticNoOp — a recorded tool whose translated property
// is absent from the arguments is a no-op: the original bytes and the bound
// signature both stay, so no replacement is proposed at all.
func TestResponsePassesSemanticNoOp(t *testing.T) {
	h := newHarness(t)
	r := translatedRequest(t, h)
	res := r.AfterResponse(responseWith(toolCallBlock("call_1", "read", `{"path":"a.go"}`, "sig-abc")), true)
	if res.Err != nil {
		t.Fatalf("dispatch error: %v", res.Err)
	}
	if !res.PassedThrough {
		t.Fatalf("a semantic no-op must pass through, got replacement %v", res.Replacement)
	}
}

// TestResponseWithoutToolCallsNeedsNoRegistry — a plain text answer must not
// fail because no envelope was published (a request with no tools publishes
// none).
func TestResponseWithoutToolCallsNeedsNoRegistry(t *testing.T) {
	h := newHarness(t)
	res := h.AfterResponse(responseWith(textBlock("no tools here")), true)
	if res.Err != nil || !res.PassedThrough {
		t.Fatalf("expected pass-through, err=%v", res.Err)
	}
	for _, c := range h.Calls() {
		if c.Command == "env.meta_get" {
			t.Fatal("read the registry for a response with no tool calls")
		}
	}
}

// TestResponseObservationalDispatchIsUntouched — a streamed or errored
// response is observational: the stream hook already reversed a streamed
// call, a replacement would be discarded, and the registry is not even read.
func TestResponseObservationalDispatchIsUntouched(t *testing.T) {
	h := newHarness(t)
	r := translatedRequest(t, h)
	before := len(h.Calls())
	res := r.AfterResponse(responseWith(
		toolCallBlock("call_1", "read", `{"env":[{"key":"HOME","value":"/root"}]}`, "sig-abc"),
	), false)
	if res.Err != nil || !res.PassedThrough {
		t.Fatalf("an observational dispatch must pass through, err=%v", res.Err)
	}
	for _, c := range h.Calls()[before:] {
		if c.Command == "env.meta_get" {
			t.Fatal("read the registry on an observational dispatch")
		}
	}
}

// TestResponseTerminatesWithoutAValidRegistry — an absent or corrupt envelope
// cannot prove "nothing was translated", so a tool call with no readable
// registry is terminal, exactly as it is on the stream path.
func TestResponseTerminatesWithoutAValidRegistry(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		h := newHarness(t)
		res := h.AfterResponse(responseWith(toolCallBlock("call_1", "read", `{"env":[]}`, "")), true)
		if res.Err == nil {
			t.Fatal("a tool call with no registry must not pass through")
		}
		if !strings.Contains(res.Err.Error(), "registry unavailable") {
			t.Fatalf("error = %v, want the registry-unavailable reason", res.Err)
		}
	})

	t.Run("corrupt", func(t *testing.T) {
		h := newHarness(t)
		h.StubHostCall("env.meta_get", func(string) (string, error) {
			return sdktest.HostResultValue([]byte(`{"version":1,"tools":`)), nil
		})
		res := h.AfterResponse(responseWith(toolCallBlock("call_1", "read", `{"env":[]}`, "")), true)
		if res.Err == nil || !strings.Contains(res.Err.Error(), "registry corrupt") {
			t.Fatalf("error = %v, want the registry-corrupt reason", res.Err)
		}
	})

	t.Run("refused", func(t *testing.T) {
		h := newHarness(t)
		h.StubHostCall("env.meta_get", func(string) (string, error) {
			return sdktest.HostResultError(pbv1.ErrorCode_ERROR_CODE_UNAVAILABLE, "backing store down"), nil
		})
		res := h.AfterResponse(responseWith(toolCallBlock("call_1", "read", `{"env":[]}`, "")), true)
		if res.Err == nil {
			t.Fatal("an advisory refusal is still terminal at response time")
		}
	})
}

// TestResponseRejectsUnreversibleArguments — a recorded tool whose arguments
// cannot be reversed terminates rather than emitting a call with a different
// meaning. These are the malformed shapes a provider can actually deliver
// inside a valid JSON object.
func TestResponseRejectsUnreversibleArguments(t *testing.T) {
	for name, args := range map[string]string{
		"recorded path is not an array": `{"env":"HOME=/root"}`,
		"KV item is not an object":      `{"env":["HOME"]}`,
		"KV item key is not a string":   `{"env":[{"key":1,"value":"x"}]}`,
		"KV item has extra members":     `{"env":[{"key":"A","value":"1","extra":true}]}`,
		"duplicate KV keys":             `{"env":[{"key":"A","value":"1"},{"key":"A","value":"2"}]}`,
		"duplicate JSON keys":           `{"env":[],"env":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			r := translatedRequest(t, h)
			res := r.AfterResponse(responseWith(toolCallBlock("call_1", "read", args, "sig-abc")), true)
			if res.Err == nil {
				t.Fatalf("unreversible arguments were emitted: %v", res.Replacement)
			}
			if res.Replacement != nil {
				t.Fatalf("a failed reversal proposed a replacement: %v", res.Replacement)
			}
		})
	}
}

// TestResponseReversesEveryRecordedBlock — several calls to the same recorded
// tool are each reversed, and one failure does not leave the others
// half-rewritten.
func TestResponseReversesEveryRecordedBlock(t *testing.T) {
	h := newHarness(t)
	r := translatedRequest(t, h)
	res := r.AfterResponse(responseWith(
		toolCallBlock("call_1", "read", `{"env":[{"key":"A","value":"1"}]}`, "sig-1"),
		toolCallBlock("call_2", "read", `{"env":[]}`, "sig-2"),
	), true)

	first := replacedCall(t, res, 0)
	second := replacedCall(t, res, 1)
	if string(first.ArgumentsJson) != `{"env":{"A":"1"}}` {
		t.Fatalf("first call = %s", first.ArgumentsJson)
	}
	// An empty KV array is an empty object, which IS a change.
	if string(second.ArgumentsJson) != `{"env":{}}` {
		t.Fatalf("second call = %s", second.ArgumentsJson)
	}
	if first.Signature != "" || second.Signature != "" {
		t.Fatalf("signatures survived argument changes: %q %q", first.Signature, second.Signature)
	}
}

// TestResponseNestedReversalMatchesTheRecordedPath — a nested recorded path is
// reversed at its exact site and nothing else in the arguments moves.
func TestResponseNestedReversalMatchesTheRecordedPath(t *testing.T) {
	h := newHarness(t)
	r := h.NewRequest()
	if res := r.BeforeRequest(reqWithTools(`{"type":"object","properties":{"cfg":{"type":"object","properties":{"env":{"type":"object","additionalProperties":{"type":"string"}}}},"path":{"type":"string"}}}`)); res.Err != nil {
		t.Fatalf("request hook: %v", res.Err)
	}
	res := r.AfterResponse(responseWith(
		toolCallBlock("call_1", "read", `{"cfg":{"env":[{"key":"A","value":"1"}]},"path":"a.go"}`, ""),
	), true)

	var got map[string]any
	if err := json.Unmarshal(replacedCall(t, res, 0).ArgumentsJson, &got); err != nil {
		t.Fatal(err)
	}
	cfg, _ := got["cfg"].(map[string]any)
	env, ok := cfg["env"].(map[string]any)
	if !ok || env["A"] != "1" {
		t.Fatalf("nested map was not reversed: %v", got)
	}
	if got["path"] != "a.go" {
		t.Fatalf("an unrelated argument changed: %v", got)
	}
}
