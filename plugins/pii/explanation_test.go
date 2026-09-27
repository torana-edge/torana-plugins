package main

import (
	"encoding/json"
	"strings"
	"testing"

	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
)

func explanationRequest(conversation string) *pbv1.HttpRequest {
	headers, _ := json.Marshal(map[string][]string{
		"X-Torana-MCP-Binding": {"bound"}, "X-Torana-Conversation-Id": {conversation}, "X-Torana-Tool-Use-Id": {"mcp-call"},
	})
	return &pbv1.HttpRequest{Method: "GET", Path: "/agent/redaction/explain-last", HeadersJson: headers}
}

func readExplanation(t *testing.T, h *sdktest.Harness, conversation string) explanation {
	t.Helper()
	res := h.HTTPRequest(explanationRequest(conversation))
	if res.Err != nil || res.Response == nil || res.Response.Status != 200 {
		t.Fatalf("response=%+v", res)
	}
	var got explanation
	if err := json.Unmarshal(res.Response.Body, &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestDeterministicExplanationIsSessionBoundAndValueFree(t *testing.T) {
	h := newHarness(t).SetConversationID("session-a").SetConfig(`{}`)
	secret := "sk_test_torana_dummy_key_1234567890"
	res := h.BeforeRequest(reqWith(toolMsg("call", "private-tool", textArm("safe\n"+secret))))
	if !requestCompleted(res) {
		t.Fatalf("result=%+v", res)
	}
	got := readExplanation(t, h, "session-a")
	if !got.Found || got.Truncated || len(got.Findings) != 1 || got.Findings[0] != (explanationFinding{Category: "api_key", Line: 2}) {
		t.Fatalf("explanation=%+v", got)
	}
	other := readExplanation(t, h, "session-b")
	if other.Found || other.Findings == nil || len(other.Findings) != 0 {
		t.Fatalf("cross-session explanation=%+v", other)
	}
	for _, call := range h.Calls() {
		if call.Command == "env.state_set" && (strings.Contains(call.Args, secret) || strings.Contains(call.Args, "private-tool") || strings.Contains(call.Args, "safe")) {
			t.Fatal("explanation state exposed tool data")
		}
	}
}

func TestModelExplanationNormalizesCategoryAndUnreliableLine(t *testing.T) {
	h := newHarness(t).SetConversationID("session-a").SetConfig(`{}`)
	h.StubModelComplete(modelStub(`{"pii":true,"findings":[{"type":"email address","line":99},{"type":"model controlled value","line":1}]}`))
	res := h.BeforeRequest(reqWith(toolMsg("call", "read", textArm("ordinary line"))))
	if !requestCompleted(res) {
		t.Fatalf("result=%+v", res)
	}
	got := readExplanation(t, h, "session-a")
	want := []explanationFinding{{Category: "email", Line: 0}, {Category: "unspecified", Line: 1}}
	if len(got.Findings) != len(want) || got.Findings[0] != want[0] || got.Findings[1] != want[1] {
		t.Fatalf("normalized explanation=%+v", got)
	}
}

func TestTransientFailureAndHistoricalReplayDoNotReplaceExplanation(t *testing.T) {
	h := newHarness(t).SetConversationID("session-a").SetConfig(`{}`)
	if res := h.BeforeRequest(reqWith(toolMsg("sensitive", "read", textArm("123-45-6789")))); !requestCompleted(res) {
		t.Fatal(res.Err)
	}
	before := readExplanation(t, h, "session-a")
	h.StubModelComplete(modelStub(`not-json`))
	if res := h.BeforeRequest(reqWith(toolMsg("transient", "read", textArm("ordinary output")))); !requestCompleted(res) {
		t.Fatal(res.Err)
	}
	afterFailure := readExplanation(t, h, "session-a")
	if len(afterFailure.Findings) != 1 || afterFailure.Findings[0] != before.Findings[0] {
		t.Fatalf("transient failure replaced explanation: before=%+v after=%+v", before, afterFailure)
	}
	historical := reqWith(toolMsg("sensitive", "read", textArm("123-45-6789")), &pbv1.Message{Role: "user", Blocks: []*pbv1.RequestBlock{{Kind: &pbv1.RequestBlock_Text{Text: &pbv1.RequestTextBlock{Text: "continue"}}}}})
	if res := h.BeforeRequest(historical); !requestCompleted(res) {
		t.Fatal(res.Err)
	}
	afterReplay := readExplanation(t, h, "session-a")
	if len(afterReplay.Findings) != 1 || afterReplay.Findings[0] != before.Findings[0] {
		t.Fatal("historical replay replaced explanation")
	}
}

func TestExplanationUnboundDoesNotReadStateAndCorruptStateFailsClosed(t *testing.T) {
	h := newHarness(t)
	res := h.HTTPRequest(&pbv1.HttpRequest{Method: "GET", Path: "/agent/redaction/explain-last"})
	if res.Err != nil || res.Response == nil || res.Response.Status != 409 || len(h.Calls()) != 0 {
		t.Fatalf("unbound=%+v calls=%v", res, h.Calls())
	}
	for _, raw := range []string{`null`, `{"found":true,"found":false}`, `{"found":true,"findings":[]}`, `{"found":true,"findings":[{"category":"model text","line":1}]}`} {
		h.SeedState(explanationKey("session-a"), raw)
		res := h.HTTPRequest(explanationRequest("session-a"))
		if res.Err == nil || res.Response != nil {
			t.Fatalf("corrupt state accepted: %+v", res)
		}
	}
}
