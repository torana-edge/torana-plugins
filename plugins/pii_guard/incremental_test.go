package main

import (
	"strings"
	"testing"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
)

func textMessage(text string) *pbv1.Message {
	return &pbv1.Message{Role: "user", Blocks: []*pbv1.RequestBlock{{Kind: &pbv1.RequestBlock_Text{Text: &pbv1.RequestTextBlock{Text: text}}}}}
}

func resultText(t *testing.T, message *pbv1.Message) (string, bool) {
	t.Helper()
	results := sdk.ToolResults(message)
	if len(results) != 1 {
		t.Fatalf("tool results = %d, want 1", len(results))
	}
	text, ok := sdk.ToolResultScalarText(results[0])
	if !ok {
		t.Fatal("tool result is not scalar")
	}
	return text, results[0].IsError != nil && *results[0].IsError
}

func TestOnlyTrailingToolResultBatchIsNewWork(t *testing.T) {
	oldSecret := "sk_test_old_torana_demo_not_a_real_key_123"
	newSecret := "sk_test_new_torana_demo_not_a_real_key_123"
	old := toolResult("old", "read", oldSecret)
	latest := toolResult("new", "read", newSecret)
	req := &pbv1.ChatRequest{Messages: []*pbv1.Message{old, textMessage("continue"), latest}}

	h := newHarness(t)
	res := h.BeforeRequest(req)
	if res.Err != nil || res.Request == nil {
		t.Fatalf("result = %+v", res)
	}
	if text, isError := resultText(t, old); text != oldSecret || isError {
		t.Fatalf("historical unscanned result changed: text=%q error=%v", text, isError)
	}
	if text, isError := resultText(t, latest); strings.Contains(text, newSecret) || !isError {
		t.Fatalf("latest result was not safely replaced: text=%q error=%v", text, isError)
	}
}

func TestStableReplayToolCallIDKeepsGeminiSemanticIdentity(t *testing.T) {
	if got := stableReplayToolCallID("torana_gemini_abcdef_2"); got != "torana_gemini_abcdef" {
		t.Fatalf("stable id = %q", got)
	}
	if got := stableReplayToolCallID("caller_id_2"); got != "caller_id_2" {
		t.Fatalf("caller id changed = %q", got)
	}
}

func TestNewestToolResultPrecedingInjectedDeveloperMessageIsScanned(t *testing.T) {
	latest := toolResult("new", "exec", "key: sk_test_newest_torana_demo_not_a_real_key_123")
	developer := &pbv1.Message{Role: "developer", Blocks: []*pbv1.RequestBlock{{Kind: &pbv1.RequestBlock_Text{Text: &pbv1.RequestTextBlock{Text: "injected harness metadata"}}}}}
	h := newHarness(t)
	if res := h.BeforeRequest(&pbv1.ChatRequest{Messages: []*pbv1.Message{latest, developer}}); res.Err != nil || res.Request == nil {
		t.Fatalf("result = %+v", res)
	}
	if text, isError := resultText(t, latest); strings.Contains(text, "sk_test_newest_torana_demo_not_a_real_key_123") || !isError {
		t.Fatalf("tool output before developer metadata was not scanned: text=%q error=%v", text, isError)
	}
}

func TestReplaySurvivesMarkerMovement(t *testing.T) {
	secret := "sk_test_replay_torana_demo_not_a_real_key_123"
	withMarker := func(id, name string, markerFirst bool) *pbv1.Message {
		content := []*pbv1.ToolResultContentBlock{
			{Kind: &pbv1.ToolResultContentBlock_Text{Text: &pbv1.ToolResultTextBlock{Text: secret}}},
			{Kind: &pbv1.ToolResultContentBlock_CacheBreakpoint{CacheBreakpoint: &pbv1.ToolResultCacheBreakpoint{MarkerJson: []byte(`{"type":"ephemeral"}`)}}},
		}
		if markerFirst {
			content[0], content[1] = content[1], content[0]
		}
		return &pbv1.Message{Role: "tool", Blocks: []*pbv1.RequestBlock{{Kind: &pbv1.RequestBlock_ToolResult{ToolResult: &pbv1.RequestToolResultBlock{ToolCallId: id, ToolName: name, Content: content}}}}}
	}

	h := newHarness(t)
	if result := h.BeforeRequest(&pbv1.ChatRequest{Messages: []*pbv1.Message{withMarker("stable-call", "read", false)}}); result.Err != nil || result.Request == nil {
		t.Fatalf("first replacement = %+v", result)
	}
	historical := withMarker("stable-call", "", true)
	if result := h.BeforeRequest(&pbv1.ChatRequest{Messages: []*pbv1.Message{historical, textMessage("continue")}}); result.Err != nil || result.Request == nil {
		t.Fatalf("historical replay = %+v", result)
	}
	text, isError := resultText(t, historical)
	if strings.Contains(text, secret) || !isError {
		t.Fatalf("history was not replayed: text=%q error=%v", text, isError)
	}
}

func TestReplayIsIsolatedByConversation(t *testing.T) {
	secret := "sk_test_same_torana_demo_not_a_real_key_123"
	h := newHarness(t)
	h.SetConversationID("conversation-b")
	if res := h.BeforeRequest(&pbv1.ChatRequest{Messages: []*pbv1.Message{toolResult("call", "read", secret)}}); res.Err != nil || res.Request == nil {
		t.Fatalf("protect conversation B = %+v", res)
	}

	h.SetConversationID("conversation-a")
	historical := toolResult("call", "", secret)
	if res := h.BeforeRequest(&pbv1.ChatRequest{Messages: []*pbv1.Message{historical, textMessage("continue")}}); res.Err != nil {
		t.Fatalf("conversation A = %+v", res)
	}
	if text, isError := resultText(t, historical); text != secret || isError {
		t.Fatalf("conversation B decision crossed into A: text=%q error=%v", text, isError)
	}
}

func TestReplayReadFailureFailsClosed(t *testing.T) {
	h := newHarness(t)
	h.StubHostCall("env.state_get", func(string) (string, error) {
		return sdktest.HostResultError(pbv1.ErrorCode_ERROR_CODE_UNAVAILABLE, "state database unavailable"), nil
	})
	result := h.BeforeRequest(&pbv1.ChatRequest{Messages: []*pbv1.Message{toolResult("call", "read", "ordinary output")}})
	if result.Err == nil || result.PassedThrough || result.Request != nil {
		t.Fatalf("unavailable replay state did not fail closed: %+v", result)
	}
}

func TestPriorReplacementReplaysOutsideLatestBatch(t *testing.T) {
	secret := "sk_test_replay_torana_demo_not_a_real_key_123"
	h := newHarness(t)
	first := &pbv1.ChatRequest{Messages: []*pbv1.Message{toolResult("call", "read", secret)}}
	if res := h.BeforeRequest(first); res.Err != nil {
		t.Fatal(res.Err)
	}

	// Simulate a compacted/resumed history where the original tool-use block
	// (and therefore the resolved name) is no longer present.
	historical := toolResult("call", "", secret)
	second := &pbv1.ChatRequest{Messages: []*pbv1.Message{historical, textMessage("next turn")}}
	if res := h.BeforeRequest(second); res.Err != nil {
		t.Fatal(res.Err)
	}
	text, isError := resultText(t, historical)
	if strings.Contains(text, secret) || !isError {
		t.Fatalf("historical replacement not replayed: text=%q error=%v", text, isError)
	}
	for _, call := range h.Calls() {
		if call.Command == "env.state_compare_and_set" && strings.Contains(call.Args, secret) {
			t.Fatal("durable state contained the sensitive value")
		}
	}
}
