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

func TestConfirmDefersAcceptanceUntilNewTurnAndPreservesRequest(t *testing.T) {
	h := sdktest.New(t).SetConfig(strings.Replace(shadowConfigJSON, `"mode":"shadow"`, `"mode":"confirm"`, 1))
	req := request("confirm-session", "Fix tests")
	if res := h.BeforeRequest(req); res.Err != nil {
		t.Fatal(res.Err)
	}
	key := shadowStateKey("confirm-session", req)
	h.Run(func() {
		stored, found, err := sdk.StateGetVersioned(key)
		if err != nil || !found {
			t.Fatalf("state = %t %v", found, err)
		}
		var state shadowState
		if err := json.Unmarshal([]byte(stored.Value), &state); err != nil {
			t.Fatal(err)
		}
		state.PendingSuggestion = &pendingAdvice{ID: "sg_confirm", To: "strong", IssuedTurn: 1, ExpiresUserTurn: 4, Status: "pending"}
		if ok, err := saveShadowState(key, state, &stored.Version); err != nil || !ok {
			t.Fatalf("save = %t %v", ok, err)
		}
	})
	req.ToranaMetaJson = []byte(`{"_conversation_id":"confirm-session","_provider":"original","_suggestions":[{"id":"sg_confirm","status":"accepted","via":"directive"}]}`)
	// Replayed current turn may carry feedback, but cannot trigger a move.
	if res := h.BeforeRequest(req); res.Err != nil {
		t.Fatal(res.Err)
	}
	if len(routes(h)) != 0 {
		t.Fatal("accepted switch applied on retry")
	}
	req.Messages = append(req.Messages, &pbv1.Message{Role: "user", Blocks: []*pbv1.RequestBlock{textBlock("Continue with the fix")}})
	before := proto.Clone(req)
	if res := h.BeforeRequest(req); res.Err != nil {
		t.Fatal(res.Err)
	}
	if got := routes(h); len(got) != 1 || got[0].Model != "strong-model" || got[0].Effort != pbv1.Effort_EFFORT_UNSPECIFIED {
		t.Fatalf("routes = %+v", got)
	}
	if !proto.Equal(before, req) {
		t.Fatal("routing mutated messages or harness effort")
	}
}

func TestAutoRoutesOnlyNewUserTurnAndReconcilesHost(t *testing.T) {
	h := sdktest.New(t).SetConfig(strings.Replace(shadowConfigJSON, `"mode":"shadow"`, `"mode":"auto"`, 1))
	req := request("auto-session", "Fix tests")
	if res := h.BeforeRequest(req); res.Err != nil {
		t.Fatal(res.Err)
	}
	failed := true
	req.Messages = append(req.Messages, &pbv1.Message{Role: "tool", Blocks: []*pbv1.RequestBlock{{Kind: &pbv1.RequestBlock_ToolResult{ToolResult: &pbv1.RequestToolResultBlock{ToolCallId: "new-failure", IsError: &failed, Content: []*pbv1.ToolResultContentBlock{{Kind: &pbv1.ToolResultContentBlock_Text{Text: &pbv1.ToolResultTextBlock{Text: "test failed"}}}}}}}}})
	if res := h.BeforeRequest(req); res.Err != nil {
		t.Fatal(res.Err)
	}
	if len(routes(h)) != 0 {
		t.Fatal("auto moved during continuation")
	}
	req.Messages = append(req.Messages, &pbv1.Message{Role: "user", Blocks: []*pbv1.RequestBlock{textBlock("Try again")}})
	scope := h.NewRequest()
	if res := scope.BeforeRequest(req); res.Err != nil {
		t.Fatal(res.Err)
	}
	if got := routes(h); len(got) != 1 || got[0].Model != "strong-model" {
		t.Fatalf("routes = %+v", got)
	}
	var state shadowState
	key := shadowStateKey("auto-session", req)
	raw, _ := h.State(key)
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		t.Fatal(err)
	}
	if state.Step != "fast" || state.ModelSwitches != 0 {
		t.Fatal("counted route before host application")
	}
	if res := scope.AfterResponse(&pbv1.ChatResponse{ToranaMetaJson: []byte(`{"_route_applied":{"provider":"original","model":"strong-model","verdict_plugin":"decision_router","refused":null,"served_by":"original","served_model":"strong-model","failover":false}}`)}, false); res.Err != nil {
		t.Fatal(res.Err)
	}
	raw, _ = h.State(key)
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		t.Fatal(err)
	}
	if state.Step != "strong" || state.ActiveRoute != "strong" || state.ModelSwitches != 1 || len(state.History) != 1 {
		t.Fatalf("applied state = %+v", state)
	}
	req.Messages = append(req.Messages, &pbv1.Message{Role: "tool", Blocks: []*pbv1.RequestBlock{textBlock("continue working")}})
	if res := h.BeforeRequest(req); res.Err != nil {
		t.Fatal(res.Err)
	}
	if got := routes(h); len(got) != 2 || got[1].Model != "strong-model" {
		t.Fatalf("active route not kept: %+v", got)
	}
}

func TestRouteEffortRequiresExplicitOwnership(t *testing.T) {
	for _, manage := range []bool{false, true} {
		h := sdktest.New(t)
		ladder := shadowLadder{Steps: []shadowStep{{ID: "strong", Model: "strong-model", Effort: "high"}}}
		h.Run(func() { requestRoute("original", ladder, shadowState{Step: "strong"}, "strong", "continue", manage) })
		got := routes(h)
		want := pbv1.Effort_EFFORT_UNSPECIFIED
		if manage {
			want = pbv1.Effort_EFFORT_HIGH
		}
		if len(got) != 1 || got[0].Effort != want {
			t.Fatalf("manage=%t routes=%+v", manage, got)
		}
	}
}

func TestRouteRefusalDoesNotAdvanceState(t *testing.T) {
	h := sdktest.New(t)
	ladder := shadowLadder{Steps: []shadowStep{{ID: "fast", Model: "fast-model"}, {ID: "strong", Model: "strong-model"}}}
	for _, metadata := range []string{
		`{}`,
		`{"_route_applied":{"provider":"original","model":"strong-model","verdict_plugin":"decision_router","refused":"unsupported_model","served_by":"original","served_model":"fast-model"}}`,
		`{"_route_applied":{"provider":"original","model":"strong-model","verdict_plugin":"other","served_by":"original","served_model":"strong-model"}}`,
	} {
		state := shadowState{Step: "fast", PolicyHash: "policy"}
		h.Run(func() {
			requestRoute("original", ladder, state, "strong", "auto", false)
			reconcileAppliedRoute(&pbv1.ChatResponse{ToranaMetaJson: []byte(metadata)}, &state)
		})
		if state.Step != "fast" || state.ActiveRoute != "" || state.ModelSwitches != 0 {
			t.Fatalf("refused state = %+v", state)
		}
	}
}

func TestAcceptedDecisionKeepsGuardsAndAllowsOnlyOneStep(t *testing.T) {
	policy, _, err := loadShadowPolicy(shadowConfigJSON)
	if err != nil {
		t.Fatal(err)
	}
	ladder := policy.Ladders["original"]
	state := shadowState{Step: "fast", UserTurns: 3, LastSuggestion: "strong", PendingSuggestion: &pendingAdvice{To: "strong", Status: "accepted"}}
	if d := acceptedDecision(policy, ladder, state, 1000); d.Target != "strong" || d.BlockedBy != "" {
		t.Fatalf("accepted = %+v", d)
	}
	state.ModelSwitches = policy.Escalation.MaxModelSwitches
	if d := acceptedDecision(policy, ladder, state, 1000); d.BlockedBy != "switch_cap" {
		t.Fatalf("cap = %+v", d)
	}
	state.ModelSwitches = 0
	if d := acceptedDecision(policy, ladder, state, 1000000000); d.BlockedBy != "cost_guard" {
		t.Fatalf("cost = %+v", d)
	}
	ladder.Steps[1].Pricing = nil
	if d := acceptedDecision(policy, ladder, state, 1000); d.BlockedBy != "pricing_unavailable" {
		t.Fatalf("prices = %+v", d)
	}
}
