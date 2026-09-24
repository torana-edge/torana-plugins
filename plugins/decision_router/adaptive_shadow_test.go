package main

import (
	"bytes"
	"encoding/json"
	"testing"

	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
)

const shadowConfigJSON = `{
  "mode":"shadow",
  "ladders":{"original":{"start":"fast","steps":[
    {"id":"fast","description":"Routine work","model":"fast-model"},
    {"id":"strong","description":"Hard reasoning","model":"strong-model"}
  ]}},
  "triggers":{"tool_error_window":3,"tool_error_threshold":1,"reevaluate_every_user_turns":3}
}`

func TestShadowObservesNewFailuresWithoutRouting(t *testing.T) {
	h := sdktest.New(t).SetConfig(shadowConfigJSON)
	req := request("shadow-session", "Please fix the tests")
	if res := h.BeforeRequest(req); res.Err != nil {
		t.Fatal(res.Err)
	}
	if len(routes(h)) != 0 {
		t.Fatal("shadow policy routed the first request")
	}
	var state shadowState
	raw, found := h.State(shadowStateKey("shadow-session"))
	if !found || json.Unmarshal([]byte(raw), &state) != nil || state.Step != "fast" || state.UserTurns != 1 {
		t.Fatalf("initial shadow state: found=%v state=%+v", found, state)
	}

	// The same user turn is a retry, not another conversation turn.
	if res := h.BeforeRequest(req); res.Err != nil {
		t.Fatal(res.Err)
	}
	raw, _ = h.State(shadowStateKey("shadow-session"))
	if err := json.Unmarshal([]byte(raw), &state); err != nil || state.UserTurns != 1 {
		t.Fatalf("retry counted as a new turn: %+v, %v", state, err)
	}

	failed := true
	req.Messages = append(req.Messages, &pbv1.Message{Role: "tool", Blocks: []*pbv1.RequestBlock{{
		Kind: &pbv1.RequestBlock_ToolResult{ToolResult: &pbv1.RequestToolResultBlock{
			ToolCallId: "failed-call", ToolName: "bash", IsError: &failed,
			Content: []*pbv1.ToolResultContentBlock{{Kind: &pbv1.ToolResultContentBlock_Text{Text: &pbv1.ToolResultTextBlock{Text: "test failed"}}}},
		}},
	}}})
	if res := h.BeforeRequest(req); res.Err != nil {
		t.Fatal(res.Err)
	}
	req.Messages = append(req.Messages, &pbv1.Message{Role: "user", Blocks: []*pbv1.RequestBlock{textBlock("Try another way")}})
	if res := h.BeforeRequest(req); res.Err != nil {
		t.Fatal(res.Err)
	}
	if len(routes(h)) != 0 {
		t.Fatal("shadow policy staged a route after an error")
	}
	var suggestions int
	for _, metric := range h.Metrics() {
		if metric.Name == metricDecisionName && metric.Labels["outcome"] == "would_suggest" {
			suggestions++
			if metric.Labels["from"] != "fast" || metric.Labels["to"] != "strong" {
				t.Fatalf("unexpected suggestion metric: %+v", metric)
			}
		}
	}
	if suggestions != 1 {
		t.Fatalf("got %d suggestions, want one", suggestions)
	}
	req.Model = "strong-model" // a harness-side switch, never a Torana route
	if res := h.BeforeRequest(req); res.Err != nil {
		t.Fatal(res.Err)
	}
	raw, _ = h.State(shadowStateKey("shadow-session"))
	if err := json.Unmarshal([]byte(raw), &state); err != nil || state.Step != "strong" {
		t.Fatalf("harness switch not reconciled: %+v, %v", state, err)
	}
	if len(routes(h)) != 0 {
		t.Fatal("shadow routed after a harness switch")
	}
}

func TestShadowRejectsMalformedLadders(t *testing.T) {
	for _, raw := range []string{
		`{"mode":"shadow","mode":"shadow","ladders":{}}`,
		`{"mode":"shadow","ladders":{}}`,
		`{"mode":"shadow","ladders":{"p":{"start":"absent","steps":[{"id":"a","model":"x","description":"a"},{"id":"b","model":"y","description":"b"}]}}}`,
		`{"mode":"shadow","ladders":{"p":{"start":"a","steps":[{"id":"a","model":"x","description":"a"},{"id":"a","model":"y","description":"b"}]}}}`,
	} {
		if _, _, err := loadShadowPolicy(raw); err == nil {
			t.Fatalf("accepted invalid policy: %s", raw)
		}
	}
}

func TestShadowClassifierReceivesOnlyLatestTurnAndSignals(t *testing.T) {
	var policy map[string]any
	if err := json.Unmarshal([]byte(shadowConfigJSON), &policy); err != nil {
		t.Fatal(err)
	}
	policy["classifier"] = map[string]any{
		"enabled": true, "decision_model": "jev-latest", "question": "How hard is this task?",
	}
	raw, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	h := sdktest.New(t).SetConfig(string(raw))
	var sent *pbv1.OutboundHTTPRequestArgs
	stubDecision(h, 200, response("strong", 0.95), func(args *pbv1.OutboundHTTPRequestArgs) { sent = args })
	req := request("shadow-private", "Fix this race")
	if res := h.BeforeRequest(req); res.Err != nil {
		t.Fatal(res.Err)
	}
	if sent == nil || len(routes(h)) != 0 {
		t.Fatalf("shadow call=%v routes=%v", sent, routes(h))
	}
	for _, secret := range []string{"private historical system prompt", "private-tool-result", "private-tool-arguments", "private-tool-schema", "original-model"} {
		if bytes.Contains(sent.Body, []byte(secret)) {
			t.Fatalf("shadow classifier received %q", secret)
		}
	}
	if !bytes.Contains(sent.Body, []byte("Fix this race")) || !bytes.Contains(sent.Body, []byte("recent_tool_errors")) {
		t.Fatalf("missing bounded latest turn/signals: %s", sent.Body)
	}
}
