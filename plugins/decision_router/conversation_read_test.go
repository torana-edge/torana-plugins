package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
)

func seedVersionedThread(t *testing.T, h *sdktest.Harness, key, value string) {
	t.Helper()
	h.Run(func() {
		if err := sdk.StateSet(key, value); err != nil {
			t.Fatal(err)
		}
	})
}

func conversationHTTPRequest(session string) *pb.HttpRequest {
	headers, _ := json.Marshal(map[string][]string{"X-Torana-MCP-Binding": {"bound"}, "X-Torana-Conversation-Id": {session}, "X-Torana-Tool-Use-Id": {"call"}})
	return &pb.HttpRequest{Method: "GET", Path: "/agent/conversation", HeadersJson: headers}
}

func TestPublicThreadIDIsSessionScopedAndStable(t *testing.T) {
	leaf := strings.Repeat("a", 64)
	id := publicThreadID(shadowSessionPrefix("one"), leaf)
	if len(id) != 32 || id == leaf || id != publicThreadID(shadowSessionPrefix("one"), leaf) {
		t.Fatal("invalid or unstable public thread ID")
	}
	if id == publicThreadID(shadowSessionPrefix("two"), leaf) {
		t.Fatal("thread ID links separate sessions")
	}
}

func TestConversationReadIsSessionScopedAndContentFree(t *testing.T) {
	h := sdktest.New(t)
	state := shadowState{PolicyHash: "policy", UserTurns: 3, ContextTokens: 42, ModelSwitches: 1, Provider: "private-provider", Step: "private-step", LastClientModel: "private-model", ActiveRoute: "private-route"}
	raw, _ := json.Marshal(state)
	for _, session := range []string{"mine", "other"} {
		for _, root := range []string{"main", "side"} {
			req := request(session, "unused")
			req.Messages[1].Blocks = []*pb.RequestBlock{textBlock(root)}
			seedVersionedThread(t, h, shadowStateKey(session, req), string(raw))
		}
	}
	res := h.HTTPRequest(conversationHTTPRequest("mine"))
	if res.Err != nil || res.Response == nil || res.Response.Status != 200 {
		t.Fatalf("response=%+v", res)
	}
	var body struct {
		Scope   string          `json:"scope"`
		Threads []threadSummary `json:"threads"`
		HasMore bool            `json:"has_more"`
	}
	if err := json.Unmarshal(res.Response.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body.Scope != "session" || len(body.Threads) != 2 || body.HasMore {
		t.Fatalf("body=%+v", body)
	}
	for _, thread := range body.Threads {
		if thread.UserTurns != 3 || thread.ContextTokens != 42 || !thread.ActiveRoute {
			t.Fatal("lost summary")
		}
	}
	for _, secret := range []string{"mine", "other", "private-", "policy", "main", "side"} {
		if strings.Contains(string(res.Response.Body), secret) {
			t.Fatal("private value exposed")
		}
	}
	if got := h.HTTPRequest(conversationHTTPRequest("empty")); got.Err != nil || !strings.Contains(string(got.Response.Body), `"threads":[]`) {
		t.Fatal("empty session did not return empty list")
	}
}

func TestConversationReadDeclinesUnboundBeforeStateAccess(t *testing.T) {
	for _, raw := range []string{"", `{"X-Torana-MCP-Binding":["unbound"]}`} {
		h := sdktest.New(t)
		res := h.HTTPRequest(&pb.HttpRequest{Method: "GET", Path: "/agent/conversation", HeadersJson: []byte(raw)})
		if res.Err != nil || res.Response == nil || res.Response.Status != 409 {
			t.Fatalf("response=%+v", res)
		}
		if len(h.Calls()) != 0 {
			t.Fatal("unbound read touched state")
		}
	}
}

func TestConversationReadBoundsOutputAndRejectsCorruptState(t *testing.T) {
	h := sdktest.New(t)
	for i := 0; i < 40; i++ {
		seedVersionedThread(t, h, shadowSessionPrefix("mine")+fmt.Sprintf("%064x", i), `{"policy_hash":"p","user_turns":1}`)
	}
	res := h.HTTPRequest(conversationHTTPRequest("mine"))
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	var body struct {
		Threads []threadSummary `json:"threads"`
		HasMore bool            `json:"has_more"`
	}
	if json.Unmarshal(res.Response.Body, &body) != nil || len(body.Threads) != 32 || !body.HasMore {
		t.Fatal("unbounded or silently truncated output")
	}
	for _, raw := range []string{`null`, `{}`, `{"policy_hash":"p","user_turns":-1}`, `{"policy_hash":"p","policy_hash":"duplicate"}`} {
		bad := sdktest.New(t)
		seedVersionedThread(t, bad, shadowSessionPrefix("mine")+strings.Repeat("a", 64), raw)
		if got := bad.HTTPRequest(conversationHTTPRequest("mine")); got.Err == nil {
			t.Fatal("corrupt state accepted")
		}
	}
}
