package main

import (
	"encoding/json"
	"strings"
	"testing"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
	"google.golang.org/protobuf/proto"
)

// sdktest.AfterResponse currently omits execution metadata. Exercise the same
// validated envelope the production host sends, including its session identity.
func recordedResponse(h *sdktest.Harness, response *pb.ChatResponse) sdktest.ResponseResult {
	var raw []byte
	var err error
	h.Run(func() {
		raw, err = sdk.DispatchHook(&pb.HookInput{
			ContractRevision: 1, RequestId: 1,
			Execution: &pb.ExecutionInfo{ConversationId: proto.String("session-a")},
			Payload:   &pb.HookInput_AfterResponse{AfterResponse: &pb.AfterResponse{Response: response, Mutable: true}},
		})
	})
	return sdktest.ResponseResult{Err: err, PassedThrough: err == nil && len(raw) == 0}
}

func sessionRequest(conversation string) *pb.HttpRequest {
	headers, _ := json.Marshal(map[string][]string{
		"X-Torana-MCP-Binding": {"bound"}, "X-Torana-Conversation-Id": {conversation}, "X-Torana-Tool-Use-Id": {"tool-call"},
	})
	return &pb.HttpRequest{Method: "GET", Path: "/agent/session/usage", HeadersJson: headers}
}

func readTotals(t *testing.T, h *sdktest.Harness, conversation string) (sessionTotals, bool, bool) {
	t.Helper()
	res := h.HTTPRequest(sessionRequest(conversation))
	if res.Err != nil || res.Response == nil || res.Response.Status != 200 {
		t.Fatalf("response = %+v", res)
	}
	var got struct {
		sessionTotals
		Found    bool `json:"found"`
		Complete bool `json:"usage_complete"`
	}
	if err := json.Unmarshal(res.Response.Body, &got); err != nil {
		t.Fatal(err)
	}
	return got.sessionTotals, got.Found, got.Complete
}

func TestSessionUsageAggregatesOnlyItsOwnReportedCounts(t *testing.T) {
	h := sdktest.New(t).SetConversationID("session-a")
	for _, usage := range []*pb.Usage{
		{InputTokens: 10, OutputTokens: 3, CacheReadTokens: 7, CacheWriteTokens: 1},
		{InputTokens: 5, OutputTokens: 2, CacheReadTokens: 4, CacheWriteTokens: 2},
	} {
		res := recordedResponse(h, &pb.ChatResponse{Usage: usage, Model: "private-model-name", Provider: "private-provider-name"})
		if res.Err != nil || !res.PassedThrough {
			t.Fatalf("response mutated or failed: %+v", res)
		}
	}
	got, found, complete := readTotals(t, h, "session-a")
	if !found || !complete || got != (sessionTotals{CompletedResponses: 2, ResponsesWithUsage: 2, InputTokens: 15, OutputTokens: 5, CacheReadTokens: 11, CacheWriteTokens: 3}) {
		t.Fatalf("totals=%+v found=%v complete=%v", got, found, complete)
	}
	other, found, _ := readTotals(t, h, "session-b")
	if found || other != (sessionTotals{}) {
		t.Fatalf("cross-session read: %+v", other)
	}
	for _, call := range h.Calls() {
		if call.Command == "env.state_compare_and_set" && (strings.Contains(call.Args, "private-model-name") || strings.Contains(call.Args, "private-provider-name") || strings.Contains(call.Args, "session-a")) {
			t.Fatal("session state stored operational identifiers")
		}
	}
}

func TestSessionUsagePreservesAbsentVersusPresentZeroUsage(t *testing.T) {
	h := sdktest.New(t).SetConversationID("session-a")
	if res := recordedResponse(h, &pb.ChatResponse{Usage: &pb.Usage{}}); res.Err != nil {
		t.Fatal(res.Err)
	}
	got, _, complete := readTotals(t, h, "session-a")
	if !complete || got.ResponsesWithUsage != 1 {
		t.Fatal("present zero usage was lost")
	}
	if res := recordedResponse(h, &pb.ChatResponse{}); res.Err != nil {
		t.Fatal(res.Err)
	}
	got, _, complete = readTotals(t, h, "session-a")
	if complete || got.CompletedResponses != 2 || got.ResponsesWithUsage != 1 {
		t.Fatalf("absent usage treated as reported: %+v", got)
	}
}

func TestSessionUsageUnboundDoesNotReadAnyState(t *testing.T) {
	h := sdktest.New(t)
	res := h.HTTPRequest(&pb.HttpRequest{Method: "GET", Path: "/agent/session/usage"})
	if res.Err != nil || res.Response == nil || res.Response.Status != 409 || len(h.Calls()) != 0 {
		t.Fatalf("unbound response=%+v calls=%v", res, h.Calls())
	}
}

func TestSessionUsageConflictsAreBoundedAndNotSilentlyCounted(t *testing.T) {
	h := sdktest.New(t).SetConversationID("session-a")
	attempts := 0
	h.StubHostCall("env.state_compare_and_set", func(string) (string, error) {
		attempts++
		raw, err := proto.Marshal(&pb.StateMutationResult{Applied: false})
		if err != nil {
			return "", err
		}
		return sdktest.HostResultValue(raw), nil
	})
	res := recordedResponse(h, &pb.ChatResponse{Usage: &pb.Usage{InputTokens: 3}})
	if res.Err == nil || attempts != 3 {
		t.Fatalf("conflicts: attempts=%d result=%+v", attempts, res)
	}
}

func TestSessionUsageRefusesCorruptOrNegativeCounts(t *testing.T) {
	h := sdktest.New(t).SetConversationID("session-a")
	if res := recordedResponse(h, &pb.ChatResponse{Usage: &pb.Usage{InputTokens: -1}}); res.Err == nil {
		t.Fatal("negative provider count accepted")
	}
	for _, raw := range []string{`null`, `{"completed_responses":0,"responses_with_usage":1}`, `{"input_tokens":-1}`, `{"input_tokens":1,"input_tokens":2}`} {
		h.SeedState(sessionKey("session-a"), raw)
		res := h.HTTPRequest(sessionRequest("session-a"))
		if res.Err == nil || res.Response != nil {
			t.Fatalf("corrupt record accepted: %+v", res)
		}
	}
}
