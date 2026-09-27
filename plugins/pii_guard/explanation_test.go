package main

import (
	"encoding/json"
	"strings"
	"testing"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
)

func explanationRequest(conversation string) *pb.HttpRequest {
	headers, _ := json.Marshal(map[string][]string{
		"X-Torana-MCP-Binding": {"bound"}, "X-Torana-Conversation-Id": {conversation}, "X-Torana-Tool-Use-Id": {"mcp-call"},
	})
	return &pb.HttpRequest{Method: "GET", Path: "/agent/redaction/explain-last", HeadersJson: headers}
}

func readExplanation(t *testing.T, h *sdktest.Harness, conversation string) explanation {
	t.Helper()
	res := h.HTTPRequest(explanationRequest(conversation))
	if res.Err != nil || res.Response == nil || res.Response.Status != 200 {
		t.Fatalf("response = %+v", res)
	}
	var got explanation
	if err := json.Unmarshal(res.Response.Body, &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestExplanationIsBoundAndContainsOnlyCategoriesAndLines(t *testing.T) {
	h := newHarness(t)
	h.SetConversationID("session-a")
	secret := "sk_test_torana_dummy_key_1234567890"
	res := h.BeforeRequest(&pb.ChatRequest{Messages: []*pb.Message{toolResult("call-a", "private-path-as-tool-name", "safe\n"+secret)}})
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	got := readExplanation(t, h, "session-a")
	if !got.Found || got.Truncated || len(got.Findings) != 1 || got.Findings[0] != (explanationFinding{Category: "api_key", Line: 2}) {
		t.Fatalf("explanation = %+v", got)
	}
	other := readExplanation(t, h, "session-b")
	if other.Found || other.Findings == nil || len(other.Findings) != 0 {
		t.Fatalf("cross-session result = %+v", other)
	}
	for _, call := range h.Calls() {
		if call.Command == "env.state_set" && (strings.Contains(call.Args, secret) || strings.Contains(call.Args, "private-path-as-tool-name") || strings.Contains(call.Args, "safe")) {
			t.Fatal("explanation stored tool content or name")
		}
	}
}

func TestHistoricalReplayDoesNotReplaceLatestExplanation(t *testing.T) {
	h := newHarness(t)
	h.SetConversationID("session-a")
	if res := h.BeforeRequest(&pb.ChatRequest{Messages: []*pb.Message{toolResult("old", "read", "sk_test_torana_dummy_key_1234567890")}}); res.Err != nil {
		t.Fatal(res.Err)
	}
	if res := h.BeforeRequest(&pb.ChatRequest{Messages: []*pb.Message{toolResult("new", "read", "safe\nsafe\n123-45-6789")}}); res.Err != nil {
		t.Fatal(res.Err)
	}
	before := readExplanation(t, h, "session-a")
	history := &pb.ChatRequest{Messages: []*pb.Message{toolResult("old", "read", "sk_test_torana_dummy_key_1234567890"), textMessage("continue")}}
	if res := h.BeforeRequest(history); res.Err != nil {
		t.Fatal(res.Err)
	}
	after := readExplanation(t, h, "session-a")
	if len(after.Findings) != 1 || after.Findings[0] != before.Findings[0] || after.Findings[0].Category != "us_ssn" || after.Findings[0].Line != 3 {
		t.Fatalf("history changed summary: before=%+v after=%+v", before, after)
	}
	first, err := sdk.RequestBlocksFingerprint(history.Messages[0])
	if err != nil {
		t.Fatal(err)
	}
	if res := h.BeforeRequest(history); res.Err != nil {
		t.Fatal(res.Err)
	}
	second, err := sdk.RequestBlocksFingerprint(history.Messages[0])
	if err != nil || first != second {
		t.Fatal("repeated request changed historical bytes")
	}
}

func TestExplanationRejectsUnboundWithoutStateAccess(t *testing.T) {
	for _, headers := range []string{"", `{"X-Torana-MCP-Binding":["unbound"]}`} {
		h := newHarness(t)
		res := h.HTTPRequest(&pb.HttpRequest{Method: "GET", Path: "/agent/redaction/explain-last", HeadersJson: []byte(headers)})
		if res.Err != nil || res.Response == nil || res.Response.Status != 409 || len(h.Calls()) != 0 {
			t.Fatalf("unbound response = %+v calls=%v", res, h.Calls())
		}
	}
}

func TestExplanationCapsOutputAndRejectsCorruptState(t *testing.T) {
	h := newHarness(t)
	h.SetConversationID("session-a")
	if res := h.BeforeRequest(&pb.ChatRequest{Messages: []*pb.Message{toolResult("many", "read", strings.Repeat("123-45-6789\n", 25))}}); res.Err != nil {
		t.Fatal(res.Err)
	}
	got := readExplanation(t, h, "session-a")
	if len(got.Findings) != maxReportedFindings || !got.Truncated {
		t.Fatalf("unbounded result = %+v", got)
	}
	for _, raw := range []string{`null`, `{"found":true,"found":false}`, `{"found":true,"findings":[{"category":"secret-value","line":1}]}`, `{"found":true,"findings":[{"category":"api_key","line":0}]}`} {
		h.SeedState(explanationKey("session-a"), raw)
		res := h.HTTPRequest(explanationRequest("session-a"))
		if res.Err == nil || res.Response != nil {
			t.Fatalf("corrupt state accepted: %+v", res)
		}
		if strings.Contains(res.Err.Error(), raw) {
			t.Fatal("error exposed corrupt record")
		}
	}
}
