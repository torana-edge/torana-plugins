package main

import (
	"encoding/json"
	"testing"

	sdk "github.com/torana-edge/torana-plugin-sdk"
)

func TestHistoricalIntentBelongsToItsConversationAndCall(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(`{"fill":"off"}`)
	meta := func(conversation string) []byte {
		raw, _ := json.Marshal(map[string]string{"_conversation_id": conversation})
		return raw
	}
	capture := func(conversation, id, intent string) {
		t.Helper()
		req := reqWith(`{"path":"server.go"}`)
		req.Messages = req.Messages[:2]
		req.ToranaMetaJson = meta(conversation)
		r := h.NewRequest()
		if res := r.BeforeRequest(req); res.Err != nil {
			t.Fatal(res.Err)
		}
		args, _ := json.Marshal(map[string]string{"path": "server.go", "i": intent})
		streamCallOn(t, r, id, "read", "", string(args))
	}
	replay := func(conversation, id, args string) string {
		t.Helper()
		req := reqWith(args)
		req.ToranaMetaJson = meta(conversation)
		req.Messages[2].Blocks[0].GetToolUse().Id = id
		res := h.BeforeRequest(req)
		if res.Err != nil {
			t.Fatal(res.Err)
		}
		out := req
		if res.Request != nil {
			out = res.Request
		}
		var fields map[string]any
		if err := json.Unmarshal(sdk.ToolCalls(out.Messages[2])[0].Arguments, &fields); err != nil {
			t.Fatal(err)
		}
		intent, _ := fields["i"].(string)
		return intent
	}
	capture("A", "call_1", "inspect authentication")
	if got := replay("A", "call_1", `{"path":"server.go"}`); got != "inspect authentication" {
		t.Fatalf("initial intent = %q", got)
	}
	capture("B", "call_1", "inspect retry logic")
	capture("A", "call_2", "inspect caching")
	for _, row := range []struct{ conversation, id, args, want string }{
		{"A", "call_1", `{"path":"server.go"}`, "inspect authentication"},
		{"A", "call_2", `{"path":"server.go"}`, "inspect caching"},
		{"B", "call_1", `{"path":"server.go"}`, "inspect retry logic"},
		{"A", "remapped", `{"path":"server.go"}`, ""},
		{"A", "call_1", `{"path":"different.go"}`, ""},
		{"", "call_1", `{"path":"server.go"}`, ""},
	} {
		if got := replay(row.conversation, row.id, row.args); got != row.want {
			t.Fatalf("%s/%s: intent = %q, want %q", row.conversation, row.id, got, row.want)
		}
	}
}

func TestArgumentsOnlyLegacyEntryIsNotAnOccurrence(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(`{"fill":"off"}`)
	h.SeedCache(contentKey("read", map[string]any{"path": "server.go"}), "unrelated purpose")
	res := h.BeforeRequest(reqWith(`{"path":"server.go"}`))
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	call := res.Request.Messages[2].Blocks[0].GetToolUse()
	var args map[string]any
	if err := json.Unmarshal(call.ArgumentsJson, &args); err != nil {
		t.Fatal(err)
	}
	if _, ok := args["i"]; ok {
		t.Fatal("legacy arguments-only cache rewrote history")
	}
}
