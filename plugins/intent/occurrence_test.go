package main

import (
	"encoding/json"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
	"google.golang.org/protobuf/proto"
	"strings"
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

func TestOccurrenceDiagnostics(t *testing.T) {
	for _, tc := range []struct{ meta, id, reason string }{
		{`{`, "call_1", "malformed_torana_meta"},
		{`{}`, "call_1", "missing_conversation"},
		{`{"_conversation_id":"A"}`, "", "missing_call_id=1"},
		{`{"_conversation_id":"A"}`, "remapped", "lookup_miss=1"},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			h := newHarness(t)
			h.SetConfig(`{"fill":"off"}`)
			req := reqWith(`{"path":"server.go"}`)
			req.ToranaMetaJson = []byte(tc.meta)
			req.Messages[2].Blocks[0].GetToolUse().Id = tc.id
			// Exercise the helper directly for malformed host metadata/missing call IDs,
			// which the native hook-input validator correctly rejects before dispatch.
			h.Run(func() {
				if _, err := rehydrateHistoryIntents(req); err != nil {
					t.Fatal(err)
				}
			})
			for _, entry := range h.Logs() {
				if strings.Contains(entry.Message, tc.reason) {
					return
				}
			}
			t.Fatalf("missing diagnostic %s: %v", tc.reason, h.Logs())
		})
	}
}

func TestCaptureContextRefusalKeepsSchemaAndStripping(t *testing.T) {
	h := newHarness(t)
	req := reqWith(`{"path":"server.go"}`)
	req.Messages = req.Messages[:2]
	h.StubHostCall("env.meta_set", func(raw string) (string, error) {
		var args pbv1.MetaSetArgs
		if err := proto.Unmarshal([]byte(raw), &args); err != nil {
			return "", err
		}
		if args.Key == "intent:conversation" {
			return sdktest.HostResultError(pbv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "context denied"), nil
		}
		return sdktest.HostResultValue(nil), nil
	})
	r := h.NewRequest()
	res := r.BeforeRequest(req)
	if res.Err != nil || res.Request == nil {
		t.Fatalf("context bookkeeping blocked schema injection: %v", res.Err)
	}
	if !strings.Contains(string(res.Request.Tools[0].ParametersJson), `"i"`) {
		t.Fatal("schema was not injected")
	}
	// Absent hadI means the injected field is stripped, even with no capture context.
	emitted := streamCallOn(t, r, "c", "read", "", `{"path":"server.go","i":"inspect"}`)
	var stripped map[string]any
	if err := json.Unmarshal([]byte(emittedArgs(t, emitted)), &stripped); err != nil {
		t.Fatal(err)
	}
	if _, present := stripped["i"]; present {
		t.Fatal("capture-context refusal prevented stripping")
	}
	found := false
	for _, entry := range h.Logs() {
		found = found || strings.Contains(entry.Message, "capture context unavailable")
	}
	if !found {
		t.Fatal("context refusal was not logged")
	}
}
