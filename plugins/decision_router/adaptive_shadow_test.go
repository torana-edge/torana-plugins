package main

import (
	"bytes"
	"encoding/json"
	"math"
	"testing"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
	"google.golang.org/protobuf/proto"
)

const shadowConfigJSON = `{
  "mode":"shadow",
  "ladders":{"original":{"start":"fast","steps":[
    {"id":"fast","description":"Routine work","model":"fast-model","pricing":{"input":1,"output":5,"cache_read":0.1,"cache_write":1.25}},
    {"id":"strong","description":"Hard reasoning","model":"strong-model","pricing":{"input":5,"output":25,"cache_read":0.5,"cache_write":6.25}}
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
	raw, found := h.State(shadowStateKey("shadow-session", req))
	if !found || json.Unmarshal([]byte(raw), &state) != nil || state.Step != "fast" || state.UserTurns != 1 {
		t.Fatalf("initial shadow state: found=%v state=%+v", found, state)
	}

	// The same user turn is a retry, not another conversation turn.
	if res := h.BeforeRequest(req); res.Err != nil {
		t.Fatal(res.Err)
	}
	raw, _ = h.State(shadowStateKey("shadow-session", req))
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
	raw, _ = h.State(shadowStateKey("shadow-session", req))
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

func TestShadowSeparatesSideRequestsWithinOneHarnessSession(t *testing.T) {
	h := sdktest.New(t).SetConfig(shadowConfigJSON)
	main := request("one-session", "Fix the service")
	main.Model = "strong-model"
	side := request("one-session", "Generate a title")
	side.Messages[1].Blocks = []*pbv1.RequestBlock{textBlock("Title-only side request")}
	side.Model = "fast-model"
	if shadowStateKey("one-session", main) == shadowStateKey("one-session", side) {
		t.Fatal("side request shares the main thread's state key")
	}
	for _, req := range []*pbv1.ChatRequest{main, side, main} {
		if result := h.BeforeRequest(req); result.Err != nil {
			t.Fatal(result.Err)
		}
	}
	var mainState, sideState shadowState
	for key, dst := range map[string]*shadowState{
		shadowStateKey("one-session", main): &mainState,
		shadowStateKey("one-session", side): &sideState,
	} {
		raw, found := h.State(key)
		if !found || json.Unmarshal([]byte(raw), dst) != nil {
			t.Fatalf("missing or invalid state for %s", key)
		}
	}
	if mainState.Step != "strong" || sideState.Step != "fast" || mainState.UserTurns != 1 || sideState.UserTurns != 1 {
		t.Fatalf("main=%+v side=%+v", mainState, sideState)
	}
	for _, metric := range h.Metrics() {
		if metric.Labels["outcome"] == "user_switch_unprompted" {
			t.Fatalf("side request produced a false switch: %+v", metric)
		}
	}
	if len(routes(h)) != 0 {
		t.Fatal("shadow mode routed a request")
	}
}

func TestShadowThreadRootIgnoresMovingCacheBreakpoints(t *testing.T) {
	breakpoint := func() *pbv1.RequestBlock {
		return &pbv1.RequestBlock{Kind: &pbv1.RequestBlock_CacheBreakpoint{CacheBreakpoint: &pbv1.RequestCacheBreakpoint{MarkerJson: []byte(`{"type":"ephemeral"}`)}}}
	}
	first := request("marker-session", "Refactor the scheduler")
	first.Messages[0].Blocks = append(first.Messages[0].Blocks, breakpoint())
	first.Messages[1].Blocks = append(first.Messages[1].Blocks, breakpoint())
	second := proto.Clone(first).(*pbv1.ChatRequest)
	second.Messages[0].Blocks = second.Messages[0].Blocks[:1]
	second.Messages[1].Blocks = second.Messages[1].Blocks[:1]
	second.Messages = append(second.Messages,
		&pbv1.Message{Role: "assistant", Blocks: []*pbv1.RequestBlock{textBlock("ok")}},
		&pbv1.Message{Role: "user", Blocks: []*pbv1.RequestBlock{textBlock("next"), breakpoint()}},
	)
	key := shadowStateKey("marker-session", first)
	if key == "" || key != shadowStateKey("marker-session", second) {
		t.Fatal("moving cache breakpoint changed the thread state key")
	}
	h := sdktest.New(t).SetConfig(shadowConfigJSON)
	for _, req := range []*pbv1.ChatRequest{first, second} {
		if result := h.BeforeRequest(req); result.Err != nil {
			t.Fatal(result.Err)
		}
	}
	raw, found := h.State(key)
	var state shadowState
	if !found || json.Unmarshal([]byte(raw), &state) != nil || state.UserTurns != 2 {
		t.Fatalf("lost state across marker move: found=%v state=%+v", found, state)
	}
}

func TestShadowRecoversCorruptState(t *testing.T) {
	req := request("corrupt-session", "Fix the service")
	key := shadowStateKey("corrupt-session", req)
	h := sdktest.New(t).SetConfig(shadowConfigJSON)
	h.Run(func() {
		if err := sdk.StateSet(key, "not-json"); err != nil {
			t.Fatal(err)
		}
	})
	if result := h.BeforeRequest(req); result.Err != nil {
		t.Fatal(result.Err)
	}
	raw, found := h.State(key)
	var state shadowState
	if !found || json.Unmarshal([]byte(raw), &state) != nil || state.UserTurns != 1 {
		t.Fatalf("state was not reset: found=%v value=%q", found, raw)
	}
}

func TestShadowResultIdentityHandlesMissingCallIDsAndUnflaggedFormats(t *testing.T) {
	flagged := true
	req := &pbv1.ChatRequest{Messages: []*pbv1.Message{{Role: "tool", Blocks: []*pbv1.RequestBlock{
		{Kind: &pbv1.RequestBlock_ToolResult{ToolResult: &pbv1.RequestToolResultBlock{ToolName: "search", IsError: &flagged}}},
		{Kind: &pbv1.RequestBlock_ToolResult{ToolResult: &pbv1.RequestToolResultBlock{ToolName: "search"}}},
	}}}}
	results := shadowNewResultCandidates(req)
	if len(results) != 2 || results[0].ID == results[1].ID || !results[0].Error || results[1].Error {
		t.Fatalf("result observations = %+v", results)
	}
}

func TestShadowClassifierRunsOnceAcrossStateConflicts(t *testing.T) {
	var policy map[string]any
	if err := json.Unmarshal([]byte(shadowConfigJSON), &policy); err != nil {
		t.Fatal(err)
	}
	policy["classifier"] = map[string]any{"enabled": true, "decision_model": "jev-latest", "question": "How hard is this task?"}
	raw, _ := json.Marshal(policy)
	h := sdktest.New(t).SetConfig(string(raw))
	classifications := 0
	stubDecision(h, 200, response("strong", 0.95), func(*pbv1.OutboundHTTPRequestArgs) { classifications++ })
	h.StubHostCall("env.state_compare_and_set", func(string) (string, error) {
		result, _ := proto.Marshal(&pbv1.StateMutationResult{Applied: false})
		return sdktest.HostResultValue(result), nil
	})
	if result := h.BeforeRequest(request("conflict-session", "Fix this race")); result.Err != nil {
		t.Fatal(result.Err)
	}
	if classifications != 1 || len(routes(h)) != 0 {
		t.Fatalf("classifications=%d routes=%v", classifications, routes(h))
	}
}

func TestShadowClassifierReceivesOnlyLatestTurnAndSignals(t *testing.T) {
	for _, inputs := range []string{"user_turn+signals", "signals"} {
		t.Run(inputs, func(t *testing.T) {
			var policy map[string]any
			if err := json.Unmarshal([]byte(shadowConfigJSON), &policy); err != nil {
				t.Fatal(err)
			}
			policy["classifier"] = map[string]any{
				"enabled": true, "decision_model": "jev-latest", "question": "How hard is this task?", "inputs": inputs,
			}
			raw, err := json.Marshal(policy)
			if err != nil {
				t.Fatal(err)
			}
			h := sdktest.New(t).SetConfig(string(raw))
			var sent *pbv1.OutboundHTTPRequestArgs
			stubDecision(h, 200, response("strong", 0.95), func(args *pbv1.OutboundHTTPRequestArgs) { sent = args })
			if res := h.BeforeRequest(request("shadow-private", "Fix this race")); res.Err != nil {
				t.Fatal(res.Err)
			}
			if sent == nil || len(routes(h)) != 0 {
				t.Fatalf("shadow call=%v routes=%v", sent, routes(h))
			}
			for _, secret := range []string{"private historical system prompt", "old user turn", "old answer", "private-tool-result", "private-tool-arguments", "private-tool-schema", "fast-model"} {
				if bytes.Contains(sent.Body, []byte(secret)) {
					t.Fatalf("shadow classifier received %q", secret)
				}
			}
			if !bytes.Contains(sent.Body, []byte("recent_tool_errors")) {
				t.Fatalf("missing bounded signals: %s", sent.Body)
			}
			for _, field := range []string{"last_turn_requests", "last_turn_retries", "last_turn_max_tokens", "avg_requests_per_turn", "context_bucket"} {
				if !bytes.Contains(sent.Body, []byte(field)) {
					t.Fatalf("missing %s in bounded signals: %s", field, sent.Body)
				}
			}
			containsTurn := bytes.Contains(sent.Body, []byte("Fix this race"))
			if containsTurn != (inputs == "user_turn+signals") {
				t.Fatalf("latest turn presence=%t with inputs=%s: %s", containsTurn, inputs, sent.Body)
			}
		})
	}
}

func TestBoundedShadowSignalsBucketsAndCaps(t *testing.T) {
	facts := boundedShadowSignals(shadowState{
		UserTurns: 2000, LastTurnRequests: 4000, LastTurnRetries: 3, LastTurnMaxTokens: 2,
		AvgRequestsPerTurn: 7.6, ContextTokens: 60000,
	}, 5)
	if facts.UserTurns != 1000 || facts.LastTurnRequests != 1000 || facts.LastTurnRetries != 3 ||
		facts.LastTurnMaxTokens != 2 || facts.AvgRequestsPerTurn != 8 || facts.ContextBucket != "32k-128k" {
		t.Fatalf("unexpected bounded facts: %+v", facts)
	}
}

func TestShadowDoesNotTreatOffLadderModelAsStartStep(t *testing.T) {
	h := sdktest.New(t).SetConfig(shadowConfigJSON)
	req := request("off-ladder", "Fix this race")
	req.Model = "user-selected-model"
	if result := h.BeforeRequest(req); result.Err != nil {
		t.Fatal(result.Err)
	}
	raw, found := h.State(shadowStateKey("off-ladder", req))
	var state shadowState
	if !found || json.Unmarshal([]byte(raw), &state) != nil || !state.OffLadder {
		t.Fatalf("off-ladder state: found=%t state=%+v", found, state)
	}
	var offLadder, suggestions int
	for _, metric := range h.Metrics() {
		switch metric.Labels["outcome"] {
		case "off_ladder":
			offLadder++
		case "would_suggest":
			suggestions++
		}
	}
	if offLadder != 1 || suggestions != 0 || len(routes(h)) != 0 {
		t.Fatalf("off-ladder=%d suggestions=%d routes=%v", offLadder, suggestions, routes(h))
	}
}

func TestShadowTwentyTurnsSuggestsOnceWithMeasuredCost(t *testing.T) {
	h := sdktest.New(t).SetConfig(shadowConfigJSON)
	req := request("twenty-turns", "Start")
	req.Model = "fast-model"
	var expectedRebuildUSD float64
	for turn := 1; turn <= 20; turn++ {
		if turn == 4 {
			failed := true
			req.Messages = append(req.Messages, &pbv1.Message{Role: "tool", Blocks: []*pbv1.RequestBlock{{
				Kind: &pbv1.RequestBlock_ToolResult{ToolResult: &pbv1.RequestToolResultBlock{
					ToolCallId: "failure-on-turn-four", ToolName: "shell", IsError: &failed,
					Content: []*pbv1.ToolResultContentBlock{{Kind: &pbv1.ToolResultContentBlock_Text{Text: &pbv1.ToolResultTextBlock{Text: "test failed"}}}},
				}},
			}}})
		}
		if turn > 1 {
			req.Messages = append(req.Messages, &pbv1.Message{Role: "user", Blocks: []*pbv1.RequestBlock{textBlock("Turn " + string(rune('A'+turn)))}})
		}
		if turn == 4 {
			expectedRebuildUSD = float64(proto.Size(req)/4) / 1e6 * 6.25
		}
		if result := h.BeforeRequest(req); result.Err != nil {
			t.Fatal(result.Err)
		}
	}
	var suggestions, costs int
	for _, metric := range h.Metrics() {
		if metric.Name == metricDecisionName && metric.Labels["outcome"] == "would_suggest" {
			suggestions++
		}
		if metric.Name == "torana_decision_router_switch_cost_usd" {
			costs++
			if math.Abs(metric.Value-expectedRebuildUSD) > 1e-12 || metric.Labels["to"] != "strong" {
				t.Fatalf("unexpected cost metric: %+v", metric)
			}
		}
	}
	if suggestions != 1 || costs != 1 || len(routes(h)) != 0 {
		t.Fatalf("suggestions=%d costs=%d routes=%v", suggestions, costs, routes(h))
	}
}
