package main

import (
	"errors"
	"strings"
	"testing"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
)

func newHarness(t *testing.T) *sdktest.Harness {
	t.Helper()
	resetConfigForTest()
	return sdktest.New(t)
}

func toolResult(id, name, text string) *pbv1.Message {
	return &pbv1.Message{Role: "tool", Blocks: []*pbv1.RequestBlock{{
		Kind: &pbv1.RequestBlock_ToolResult{ToolResult: &pbv1.RequestToolResultBlock{
			ToolCallId: id,
			ToolName:   name,
			Content: []*pbv1.ToolResultContentBlock{{
				Kind: &pbv1.ToolResultContentBlock_Text{Text: &pbv1.ToolResultTextBlock{Text: text}},
			}},
		}},
	}}}
}

func TestRecognizedPatternsBecomeRecoverableErrorsWithoutModelOrNetworkCalls(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		category string
	}{
		{"US SSN", "ssn=123-45-6789", "us_ssn"},
		{"AWS key", "AKIA1234567890ABCDEF", "aws_access_key"},
		{"private key", "-----BEGIN PRIVATE KEY-----", "private_key"},
		{"dash API key", "sk-proj-abcdefghijklmnopqrstuvwxyz123456", "api_key"},
		{"underscore API key", "sk_test_torana_demo_not_a_real_key_123", "api_key"},
		{"restricted API key", "rk_live_torana_demo_not_a_real_key_123", "api_key"},
		{"GitHub token", "ghp_abcdefghijklmnopqrstuvwxyz0123456789", "access_token"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t)
			req := &pbv1.ChatRequest{Messages: []*pbv1.Message{toolResult("call-1", "read", test.value)}}
			result := h.BeforeRequest(req)
			if result.Err != nil {
				t.Fatalf("hook error: %v", result.Err)
			}
			resultView := sdk.ToolResults(req.Messages[0])[0]
			text, ok := sdk.ToolResultScalarText(resultView)
			if !ok || resultView.IsError == nil || !*resultView.IsError {
				t.Fatalf("tool result was not converted to an error: %+v", resultView)
			}
			if !strings.Contains(text, test.category) || strings.Contains(text, test.value) {
				t.Fatalf("unsafe or unhelpful replacement: %q", text)
			}
			for _, call := range h.Calls() {
				if call.Command == "env.model_complete" || call.Command == "env.http_request" || call.Command == "env.cache_get" || call.Command == "env.cache_set" {
					t.Fatalf("deterministic guard made forbidden call %q", call.Command)
				}
			}
		})
	}
}

func TestUnmatchedAndPlaceholderValuesPass(t *testing.T) {
	for _, value := range []string{
		"ordinary tool output",
		"commit author someone@example.com",
		"API_KEY=replace-me",
		"sk_test_short",
		"github_pat_placeholder",
	} {
		h := newHarness(t)
		result := h.BeforeRequest(&pbv1.ChatRequest{Messages: []*pbv1.Message{toolResult("call-1", "read", value)}})
		if result.Err != nil || !result.PassedThrough || len(h.BlockCalls()) != 0 {
			t.Fatalf("value %q result=%+v blocks=%d", value, result, len(h.BlockCalls()))
		}
	}
}

func TestToolAllowlistAndUnknownName(t *testing.T) {
	secret := "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	h := newHarness(t)
	h.StubHostCall("env.plugin_config", func(string) (string, error) {
		return sdktest.HostResultValue([]byte(`{"tools":["shell"]}`)), nil
	})
	if result := h.BeforeRequest(&pbv1.ChatRequest{Messages: []*pbv1.Message{toolResult("call-1", "read", secret)}}); result.Err != nil || !result.PassedThrough {
		t.Fatalf("excluded tool result=%+v", result)
	}
	if len(h.BlockCalls()) != 0 {
		t.Fatal("excluded named tool was scanned")
	}

	h = newHarness(t)
	h.StubHostCall("env.plugin_config", func(string) (string, error) {
		return sdktest.HostResultValue([]byte(`{"tools":["shell"]}`)), nil
	})
	req := &pbv1.ChatRequest{Messages: []*pbv1.Message{toolResult("call-2", "", secret)}}
	if result := h.BeforeRequest(req); result.Err != nil {
		t.Fatalf("unknown tool result=%+v", result)
	}
	view := sdk.ToolResults(req.Messages[0])[0]
	if view.IsError == nil || !*view.IsError {
		t.Fatal("unknown tool name should err toward scanning")
	}
}

func TestConfigRefusalIsRetryable(t *testing.T) {
	h := newHarness(t)
	calls := 0
	h.StubHostCall("env.plugin_config", func(string) (string, error) {
		calls++
		if calls == 1 {
			return sdktest.HostResultError(pbv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "denied"), nil
		}
		return sdktest.HostResultValue([]byte(`{"tools":["*"]}`)), nil
	})

	first := h.BeforeRequest(&pbv1.ChatRequest{})
	var refusal *sdk.HostCallRefusalError
	if !errors.As(first.Err, &refusal) || configLoaded {
		t.Fatalf("first result=%+v loaded=%v", first, configLoaded)
	}
	second := h.BeforeRequest(&pbv1.ChatRequest{})
	if second.Err != nil || !second.PassedThrough || calls != 2 || !configLoaded {
		t.Fatalf("second result=%+v calls=%d loaded=%v", second, calls, configLoaded)
	}
}

func TestMultipleTextArmsKeepStableLineNumbers(t *testing.T) {
	result := sdk.ToolResultView{Content: []sdk.ToolResultContentView{
		{Text: "safe"},
		{Text: "also safe\nssn=123-45-6789"},
	}}
	findings := scanToolResult(result)
	if len(findings) != 1 || findings[0].line != 3 {
		t.Fatalf("findings=%+v, want US SSN on line 3", findings)
	}
}
