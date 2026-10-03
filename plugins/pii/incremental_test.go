package main

import (
	"encoding/json"
	"strings"
	"testing"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
	"google.golang.org/protobuf/proto"
)

func incrementalTextMessage(text string) *pbv1.Message {
	return &pbv1.Message{Role: "user", Blocks: []*pbv1.RequestBlock{{Kind: &pbv1.RequestBlock_Text{Text: &pbv1.RequestTextBlock{Text: text}}}}}
}

func incrementalResultText(t *testing.T, message *pbv1.Message) (string, bool) {
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

func TestModelScansOnlyTrailingToolResultBatch(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(`{}`)
	h.StubModelComplete(modelStub(`{"pii":true,"findings":[{"type":"unspecified","line":1}]}`))
	old := toolMsg("old", "read", textArm("historical ordinary output"))
	latest := toolMsg("new", "read", textArm("new contextual value"))
	req := reqWith(old, incrementalTextMessage("continue"), latest)

	if res := h.BeforeRequest(req); res.Err != nil || res.Request == nil {
		t.Fatalf("result = %+v", res)
	}
	if calls := countCommand(h, "env.model_complete"); calls != 1 {
		t.Fatalf("model calls = %d, want 1 for only the latest batch", calls)
	}
	if text, isError := incrementalResultText(t, old); text != "historical ordinary output" || isError {
		t.Fatalf("historical unscanned result changed: text=%q error=%v", text, isError)
	}
	if text, isError := incrementalResultText(t, latest); strings.Contains(text, "new contextual value") || !isError {
		t.Fatalf("latest result was not safely replaced: text=%q error=%v", text, isError)
	}
}

func TestCleanOccurrenceSurvivesRestartAndMergedUserPrompt(t *testing.T) {
	first := newHarness(t)
	first.StubModelComplete(modelStub(`{"pii":false,"findings":[]}`))
	if result := first.BeforeRequest(reqWith(toolMsg("read-1", "read", textArm("ordinary text")))); result.Err != nil || !result.PassedThrough {
		t.Fatalf("initial scan: %+v", result)
	}
	var saved pbv1.StateCompareAndSetArgs
	for _, call := range first.Calls() {
		if call.Command == "env.state_compare_and_set" {
			if err := proto.Unmarshal([]byte(call.Args), &saved); err != nil {
				t.Fatal(err)
			}
		}
	}
	if saved.Key == "" {
		t.Fatal("clean occurrence was not persisted")
	}
	for _, row := range []struct {
		name, id, content string
		scans             int
	}{
		{"same occurrence", "read-1", "ordinary text", 0},
		{"new call", "read-2", "ordinary text", 1},
		{"changed content", "read-1", "different text", 1},
	} {
		t.Run(row.name, func(t *testing.T) {
			// Fresh process: durable state retained, no in-memory clean cache.
			h := newHarness(t).SeedState(saved.Key, saved.Value)
			h.StubModelComplete(modelStub(`{"pii":false,"findings":[]}`))
			msg := toolMsg(row.id, "read", textArm(row.content), markerArm())
			msg.Role = "user"
			// Claude can merge a new prompt into the old tool-result message.
			msg.Blocks = append(msg.Blocks, incrementalTextMessage("continue without reading again").Blocks...)
			before := proto.Clone(msg)
			result := h.BeforeRequest(reqWith(msg))
			if result.Err != nil || !result.PassedThrough || !proto.Equal(msg, before) {
				t.Fatalf("clean history must pass byte-equivalent with no replacement: %+v", result)
			}
			if calls := countCommand(h, "env.model_complete"); calls != row.scans {
				t.Fatalf("model calls = %d, want %d", calls, row.scans)
			}
		})
	}
}

func TestOversizedModelInputIsWithheldAndReplayed(t *testing.T) {
	for _, policy := range []string{"block", "allow"} {
		t.Run(policy, func(t *testing.T) {
			h := newHarness(t)
			h.SetConfig(`{"on_error":"` + policy + `"}`)
			h.StubModelComplete(func(*pbv1.ModelCompleteArgs) (*pbv1.ModelCompleteResult, *pbv1.HostError, error) {
				return nil, &pbv1.HostError{Code: pbv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, Message: "model request exceeds the approved input limit"}, nil
			})
			original := strings.Repeat("x\n", 100)
			latest := toolMsg("oversized", "read", textArm(original))
			if res := h.BeforeRequest(reqWith(latest)); !requestCompleted(res) {
				t.Fatalf("limit refusal failed the hook: %+v", res)
			}
			text, isError := incrementalResultText(t, latest)
			if policy == "block" {
				if !isError || !strings.Contains(text, "input limit") || strings.Contains(text, original) {
					t.Fatalf("missing safe replacement: %q, error=%v", text, isError)
				}
			} else if isError || text != original {
				t.Fatalf("explicit allow did not forward: %q, error=%v", text, isError)
			}
			if countCommand(h, "env.cache_set") != 0 {
				t.Fatal("oversized input was cached clean")
			}
			previousCalls := countCommand(h, "env.model_complete")
			history := toolMsg("oversized", "read", textArm(original))
			if res := h.BeforeRequest(reqWith(history, incrementalTextMessage("continue"))); !requestCompleted(res) {
				t.Fatalf("historical replay failed: %+v", res)
			}
			replayed, replayError := incrementalResultText(t, history)
			if replayed != text || replayError != isError {
				t.Fatalf("historical result drifted: %q, error=%v", replayed, replayError)
			}
			if countCommand(h, "env.model_complete") != previousCalls {
				t.Fatal("historical result was rescanned")
			}
		})
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
	h := newHarness(t)
	h.SetConfig(`{}`)
	h.StubModelComplete(modelStub(`{"pii":true,"findings":[{"type":"unspecified","line":1}]}`))
	latest := toolMsg("new", "exec", textArm("new contextual value"))
	developer := &pbv1.Message{Role: "developer", Blocks: []*pbv1.RequestBlock{{Kind: &pbv1.RequestBlock_Text{Text: &pbv1.RequestTextBlock{Text: "injected harness metadata"}}}}}
	if res := h.BeforeRequest(reqWith(latest, developer)); res.Err != nil || res.Request == nil {
		t.Fatalf("result = %+v", res)
	}
	if text, isError := incrementalResultText(t, latest); strings.Contains(text, "new contextual value") || !isError {
		t.Fatalf("tool output before developer metadata was not scanned: text=%q error=%v", text, isError)
	}
}

func TestReplaySurvivesMarkerMovement(t *testing.T) {
	secret := "key sk_test_replay_torana_demo_not_a_real_key_123"
	h := newHarness(t)
	first := toolMsg("stable-call", "read", textArm(secret), markerArm())
	if result := h.BeforeRequest(reqWith(first)); result.Err != nil || result.Request == nil {
		t.Fatalf("first replacement = %+v", result)
	}

	// Cache marker placement is not provider-visible result content and does
	// not change the occurrence decision.
	historical := toolMsg("stable-call", "", markerArm(), textArm(secret))
	request := reqWith(historical, incrementalTextMessage("continue"))
	if result := h.BeforeRequest(request); result.Err != nil || result.Request == nil {
		t.Fatalf("historical replay = %+v", result)
	}
	text, isError := incrementalResultText(t, historical)
	if strings.Contains(text, secret) || !isError {
		t.Fatalf("history was not replayed: text=%q error=%v", text, isError)
	}
}

func TestSensitiveReplayDecisionSupersedesTransientFailure(t *testing.T) {
	h := newHarness(t)
	h.Run(func() {
		first := replayRecord{Version: 2, Outcome: outcomeTransient, Replacement: "temporary"}
		second := replayRecord{Version: 2, Outcome: outcomeSensitive, Replacement: "sensitive"}
		if got, err := writeReplayDecision("replay/occurrence/test", first); err != nil || got != first {
			t.Fatalf("first write = %+v, %v", got, err)
		}
		if got, err := writeReplayDecision("replay/occurrence/test", second); err != nil || got != second {
			t.Fatalf("sensitive decision did not replace transient failure = %+v, %v", got, err)
		}
	})
}

func TestTransientFailureDoesNotPoisonNewOccurrence(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(`{"on_error":"block"}`)
	failing := true
	h.StubModelComplete(func(*pbv1.ModelCompleteArgs) (*pbv1.ModelCompleteResult, *pbv1.HostError, error) {
		if failing {
			return nil, &pbv1.HostError{Code: pbv1.ErrorCode_ERROR_CODE_UNAVAILABLE, Message: "temporary"}, nil
		}
		return modelResult(`{"pii":false,"findings":[]}`), nil, nil
	})
	old := toolMsg("call-1", "read", textArm("ordinary identical output"))
	if res := h.BeforeRequest(reqWith(old)); res.Err != nil || res.Request == nil {
		t.Fatalf("temporary failure = %+v", res)
	}

	failing = false
	historical := toolMsg("call-1", "", textArm("ordinary identical output"))
	latest := toolMsg("call-2", "read", textArm("ordinary identical output"))
	if res := h.BeforeRequest(reqWith(historical, incrementalTextMessage("continue"), latest)); res.Err != nil || res.Request == nil {
		t.Fatalf("recovery = %+v", res)
	}
	if text, isError := incrementalResultText(t, historical); strings.Contains(text, "ordinary identical output") || !isError {
		t.Fatalf("historical transient failure was not replayed: text=%q error=%v", text, isError)
	}
	if text, isError := incrementalResultText(t, latest); text != "ordinary identical output" || isError {
		t.Fatalf("new occurrence inherited old failure: text=%q error=%v", text, isError)
	}
}

func TestCleanScanAppliesConcurrentSensitiveWinner(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(`{}`)
	h.StubModelComplete(func(*pbv1.ModelCompleteArgs) (*pbv1.ModelCompleteResult, *pbv1.HostError, error) {
		var key string
		for _, call := range h.Calls() {
			if call.Command != "env.state_get" {
				continue
			}
			var args pbv1.StateGetArgs
			if err := proto.Unmarshal([]byte(call.Args), &args); err != nil {
				t.Fatal(err)
			}
			key = args.Key
		}
		if key == "" {
			t.Fatal("replay lookup did not run before model scan")
		}
		raw, err := json.Marshal(replayRecord{Version: 2, Outcome: outcomeSensitive, Replacement: "concurrent sensitive winner"})
		if err != nil {
			t.Fatal(err)
		}
		if err := sdk.StateSet(key, string(raw)); err != nil {
			t.Fatal(err)
		}
		return modelResult(`{"pii":false,"findings":[]}`), nil, nil
	})
	result := toolMsg("call", "read", textArm("contextual clean-looking value"))
	res := h.BeforeRequest(reqWith(result))
	if res.Err != nil || res.Request == nil {
		t.Fatalf("result = %+v", res)
	}
	text, isError := incrementalResultText(t, result)
	if text != "concurrent sensitive winner" || !isError {
		t.Fatalf("concurrent sensitive verdict was ignored: text=%q error=%v", text, isError)
	}
}

func TestReplayReadFailureFailsClosed(t *testing.T) {
	h := newHarness(t)
	h.StubHostCall("env.state_get", func(string) (string, error) {
		return sdktest.HostResultError(pbv1.ErrorCode_ERROR_CODE_UNAVAILABLE, "state database unavailable"), nil
	})
	result := h.BeforeRequest(reqWith(toolMsg("call", "read", textArm("ordinary output"))))
	if result.Err == nil || result.PassedThrough || result.Request != nil {
		t.Fatalf("unavailable replay state did not fail closed: %+v", result)
	}
}

func TestModelReplacementReplaysWithoutAnotherScan(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(`{}`)
	h.StubModelComplete(modelStub(`{"pii":true,"findings":[{"type":"unspecified","line":1}]}`))
	first := reqWith(toolMsg("call", "read", textArm("contextual value")))
	if res := h.BeforeRequest(first); res.Err != nil {
		t.Fatal(res.Err)
	}

	// Simulate compaction removing the tool-use block that supplied the name.
	historical := toolMsg("call", "", textArm("contextual value"))
	second := reqWith(historical, incrementalTextMessage("next turn"))
	if res := h.BeforeRequest(second); res.Err != nil {
		t.Fatal(res.Err)
	}
	if calls := countCommand(h, "env.model_complete"); calls != 1 {
		t.Fatalf("model calls = %d, replay should not rescan", calls)
	}
	text, isError := incrementalResultText(t, historical)
	if strings.Contains(text, "contextual value") || !isError {
		t.Fatalf("historical replacement not replayed: text=%q error=%v", text, isError)
	}
}
