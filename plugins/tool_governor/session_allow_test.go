package main

import (
	"encoding/json"
	"testing"

	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
)

func boundSessionRequest(path, conversation, callID, tool string) *pb.HttpRequest {
	headers, _ := json.Marshal(map[string][]string{
		"X-Torana-MCP-Binding":     {"bound"},
		"X-Torana-Conversation-Id": {conversation},
		"X-Torana-Tool-Use-Id":     {callID},
	})
	body, _ := json.Marshal(map[string]string{"tool": tool})
	return &pb.HttpRequest{Method: "POST", Path: path, HeadersJson: headers, Body: body}
}

func toolNames(req *pb.ChatRequest) []string {
	names := make([]string, 0, len(req.GetTools()))
	for _, item := range req.GetTools() {
		names = append(names, item.GetName())
	}
	return names
}

func filteredNames(t *testing.T, h *sdktest.Harness, req *pb.ChatRequest) []string {
	t.Helper()
	result := h.BeforeRequest(req)
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	if result.PassedThrough {
		return toolNames(req)
	}
	return toolNames(result.Request)
}

func TestConfirmedSessionAllowanceAppliesOnlyToBoundSessionAndUndoes(t *testing.T) {
	h := sdktest.New(t).SetConfig(`{"allow":["read"]}`).SetConversationID("session-a")
	input := requestWithTools(tool("read", "", `{}`, false, ``), tool("shell", "", `{}`, false, ``))
	if got := filteredNames(t, h, input); len(got) != 1 || got[0] != "read" {
		t.Fatalf("before allowance = %v", got)
	}

	apply := h.HTTPRequest(boundSessionRequest("/agent/session/allow-tool", "session-a", "call-1", "shell"))
	if apply.Err != nil || apply.Response == nil || apply.Response.Status != 200 {
		t.Fatalf("apply = %+v", apply)
	}
	if got := filteredNames(t, h, input); len(got) != 2 {
		t.Fatalf("allowed session tools = %v", got)
	}
	h.SetConversationID("session-b")
	if got := filteredNames(t, h, input); len(got) != 1 || got[0] != "read" {
		t.Fatalf("cross-session allowance = %v", got)
	}

	undo := h.HTTPRequest(boundSessionRequest("/agent/session/allow-tool/undo", "session-a", "call-1", "shell"))
	if undo.Err != nil || undo.Response == nil || undo.Response.Status != 200 {
		t.Fatalf("undo = %+v", undo)
	}
	h.SetConversationID("session-a")
	if got := filteredNames(t, h, input); len(got) != 1 || got[0] != "read" {
		t.Fatalf("after undo = %v", got)
	}
	// Host retries are idempotent.
	retry := h.HTTPRequest(boundSessionRequest("/agent/session/allow-tool/undo", "session-a", "call-1", "shell"))
	if retry.Err != nil || retry.Response == nil || retry.Response.Status != 200 {
		t.Fatalf("undo retry = %+v", retry)
	}
	forwardRetry := h.HTTPRequest(boundSessionRequest("/agent/session/allow-tool", "session-a", "call-1", "shell"))
	if forwardRetry.Err != nil || forwardRetry.Response == nil || forwardRetry.Response.Status != 409 {
		t.Fatalf("forward retry after undo = %+v", forwardRetry)
	}
	if got := filteredNames(t, h, input); len(got) != 1 || got[0] != "read" {
		t.Fatalf("forward retry reapplied an undone change: %v", got)
	}
}

func TestOlderAllowanceCannotUndoOverNewerState(t *testing.T) {
	h := sdktest.New(t).SetConfig(`{"allow":["read"]}`).SetConversationID("session-a")
	for _, change := range []struct{ call, tool string }{{"call-1", "shell"}, {"call-2", "deploy"}} {
		got := h.HTTPRequest(boundSessionRequest("/agent/session/allow-tool", "session-a", change.call, change.tool))
		if got.Err != nil || got.Response == nil || got.Response.Status != 200 {
			t.Fatalf("apply %s = %+v", change.call, got)
		}
	}
	stale := h.HTTPRequest(boundSessionRequest("/agent/session/allow-tool/undo", "session-a", "call-1", "shell"))
	if stale.Err != nil || stale.Response == nil || stale.Response.Status != 409 {
		t.Fatalf("stale undo = %+v", stale)
	}
	input := requestWithTools(tool("shell", "", `{}`, false, ``), tool("deploy", "", `{}`, false, ``))
	if got := filteredNames(t, h, input); len(got) != 2 {
		t.Fatalf("stale undo changed state: %v", got)
	}
	latest := h.HTTPRequest(boundSessionRequest("/agent/session/allow-tool/undo", "session-a", "call-2", "deploy"))
	if latest.Err != nil || latest.Response == nil || latest.Response.Status != 200 {
		t.Fatalf("latest undo = %+v", latest)
	}
	if got := filteredNames(t, h, input); len(got) != 1 || got[0] != "shell" {
		t.Fatalf("latest undo did not restore prior state: %v", got)
	}
}

func TestPolicyChangeInvalidatesOlderSessionAllowance(t *testing.T) {
	h := sdktest.New(t).SetConfig(`{"allow":["read"]}`).SetConversationID("session-a")
	input := requestWithTools(tool("shell", "", `{}`, false, ``))
	apply := h.HTTPRequest(boundSessionRequest("/agent/session/allow-tool", "session-a", "call-1", "shell"))
	if apply.Err != nil || apply.Response == nil || apply.Response.Status != 200 {
		t.Fatalf("apply = %+v", apply)
	}
	if got := filteredNames(t, h, input); len(got) != 1 {
		t.Fatalf("allowance did not apply: %v", got)
	}

	// An operator policy edit is authoritative. A stale session exception must
	// not silently survive a newly reviewed configuration.
	h.SetConfig(`{"allow":[]}`)
	if got := filteredNames(t, h, input); len(got) != 0 {
		t.Fatalf("stale allowance survived policy change: %v", got)
	}
}

func TestSessionAllowanceCannotOverrideOperatorDeny(t *testing.T) {
	h := sdktest.New(t).SetConfig(`{"deny":["shell"]}`).SetConversationID("session-a")
	apply := h.HTTPRequest(boundSessionRequest("/agent/session/allow-tool", "session-a", "call-1", "shell"))
	if apply.Err != nil || apply.Response == nil || apply.Response.Status != 409 || string(apply.Response.Body) != `{"error":"denied_by_policy"}` {
		t.Fatalf("deny override = %+v", apply)
	}
	input := requestWithTools(tool("shell", "", `{}`, false, ``))
	if got := filteredNames(t, h, input); len(got) != 0 {
		t.Fatalf("denied tool became visible: %v", got)
	}
}

func TestSessionAllowanceOnlyAppliesToAllowlistOmissions(t *testing.T) {
	for name, config := range map[string]string{
		"no allowlist":    `{}`,
		"already allowed": `{"allow":["shell"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			h := sdktest.New(t).SetConfig(config).SetConversationID("session-a")
			apply := h.HTTPRequest(boundSessionRequest("/agent/session/allow-tool", "session-a", "call-1", "shell"))
			if apply.Err != nil || apply.Response == nil || apply.Response.Status != 409 || string(apply.Response.Body) != `{"error":"already_allowed_by_policy"}` {
				t.Fatalf("unnecessary allowance = %+v", apply)
			}
		})
	}
}

func TestSessionAllowanceRejectsUnboundAndMalformedInputWithoutState(t *testing.T) {
	h := sdktest.New(t)
	for _, req := range []*pb.HttpRequest{
		{Method: "POST", Path: "/agent/session/allow-tool", Body: []byte(`{"tool":"shell"}`)},
		boundSessionRequest("/agent/session/allow-tool", "session-a", "call-1", ""),
		func() *pb.HttpRequest {
			r := boundSessionRequest("/agent/session/allow-tool", "session-a", "call-1", "shell")
			r.Body = []byte(`{"tool":"shell","extra":true}`)
			return r
		}(),
	} {
		got := h.HTTPRequest(req)
		if got.Err != nil || got.Response == nil || (got.Response.Status != 400 && got.Response.Status != 409) {
			t.Fatalf("rejection = %+v", got)
		}
	}
	if len(h.Calls()) != 0 {
		t.Fatalf("rejected calls touched state: %+v", h.Calls())
	}
}
