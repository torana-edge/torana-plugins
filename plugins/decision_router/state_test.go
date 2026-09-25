package main

import (
	"encoding/json"
	"testing"

	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
)

func TestResponseUsageUpdatesOnlyItsRequestThread(t *testing.T) {
	h := sdktest.New(t).SetConfig(shadowConfigJSON)
	req := request("usage-session", "Fix this")
	cycle := h.NewRequest()
	if result := cycle.BeforeRequest(req); result.Err != nil {
		t.Fatal(result.Err)
	}
	if result := cycle.AfterResponse(&pbv1.ChatResponse{Usage: &pbv1.Usage{InputTokens: 60000, CacheReadTokens: 50000, OutputTokens: 1000}, FinishReason: "length"}, false); result.Err != nil {
		t.Fatal(result.Err)
	}
	var state shadowState
	raw, found := h.State(shadowStateKey("usage-session", req))
	if !found || json.Unmarshal([]byte(raw), &state) != nil {
		t.Fatalf("state missing: %v", found)
	}
	if state.ContextTokens != 60000 || state.AvgOutputTokens != 1000 || state.MaxTokensFinishes != 1 {
		t.Fatalf("response signals = %+v", state)
	}
	// A response with no paired before-request hook must not alter this thread.
	if result := h.AfterResponse(&pbv1.ChatResponse{Usage: &pbv1.Usage{InputTokens: 999999}}, false); result.Err != nil {
		t.Fatal(result.Err)
	}
	raw, _ = h.State(shadowStateKey("usage-session", req))
	var later shadowState
	if err := json.Unmarshal([]byte(raw), &later); err != nil {
		t.Fatal(err)
	}
	if later.ContextTokens != 60000 {
		t.Fatalf("unpaired response updated state: %+v", later)
	}
}

func TestPreviousTurnSignalsReachPolicyAfterReset(t *testing.T) {
	var policy map[string]any
	if err := json.Unmarshal([]byte(shadowConfigJSON), &policy); err != nil {
		t.Fatal(err)
	}
	triggers := policy["triggers"].(map[string]any)
	triggers["retry_streak"] = 2
	triggers["requests_per_user_turn"] = 3
	triggers["max_tokens_finishes"] = 2
	triggers["reevaluate_every_user_turns"] = 100
	rawPolicy, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	h := sdktest.New(t).SetConfig(string(rawPolicy))
	req := request("turn-signals", "Fix this")
	req.Model = "fast-model"
	for _, finish := range []string{"length", "tool_use", "length"} {
		cycle := h.NewRequest()
		if result := cycle.BeforeRequest(req); result.Err != nil {
			t.Fatal(result.Err)
		}
		if result := cycle.AfterResponse(&pbv1.ChatResponse{Usage: &pbv1.Usage{InputTokens: 60000, OutputTokens: 1000}, FinishReason: finish}, false); result.Err != nil {
			t.Fatal(result.Err)
		}
	}
	req.Messages = append(req.Messages, &pbv1.Message{Role: "user", Blocks: []*pbv1.RequestBlock{textBlock("Please keep trying")}})
	if result := h.BeforeRequest(req); result.Err != nil {
		t.Fatal(result.Err)
	}
	raw, found := h.State(shadowStateKey("turn-signals", req))
	var state shadowState
	if !found || json.Unmarshal([]byte(raw), &state) != nil {
		t.Fatal("missing state after second turn")
	}
	if state.LastTurnRequests != 3 || state.LastTurnRetries != 2 || state.LastTurnMaxTokens != 2 ||
		state.AvgRequestsPerTurn != 3 || state.RequestsPerTurn != 1 || state.MaxTokensFinishes != 0 {
		t.Fatalf("previous-turn signals were lost or reset too early: %+v", state)
	}
	var suggestions int
	for _, metric := range h.Metrics() {
		if metric.Name == metricDecisionName && metric.Labels["outcome"] == "would_suggest" {
			suggestions++
		}
	}
	if suggestions != 1 || len(routes(h)) != 0 {
		t.Fatalf("suggestions=%d routes=%v", suggestions, routes(h))
	}
}
