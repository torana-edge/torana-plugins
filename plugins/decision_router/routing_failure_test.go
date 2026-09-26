package main

import (
	"testing"

	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
	"google.golang.org/protobuf/proto"
)

func TestUnsupportedEffortFallsBackOnlyForModelChange(t *testing.T) {
	for _, sameModel := range []bool{false, true} {
		h := sdktest.New(t)
		var attempts []*pbv1.RouteRequestArgs
		h.StubHostCall("env.route_request", func(raw string) (string, error) {
			args := new(pbv1.RouteRequestArgs)
			if err := proto.Unmarshal([]byte(raw), args); err != nil {
				return "", err
			}
			attempts = append(attempts, args)
			if args.Effort != pbv1.Effort_EFFORT_UNSPECIFIED {
				return sdktest.HostResultError(pbv1.ErrorCode_ERROR_CODE_UNSUPPORTED, "unsupported effort"), nil
			}
			return sdktest.HostResultValue(nil), nil
		})
		model := "fast-model"
		if !sameModel {
			model = "strong-model"
		}
		ladder := shadowLadder{Steps: []shadowStep{{ID: "fast", Model: "fast-model"}, {ID: "strong", Model: model, Effort: "high"}}}
		h.Run(func() { requestRoute("original", ladder, shadowState{Step: "fast"}, "strong", "directive", true) })
		want := 2
		if sameModel {
			want = 1
		}
		if len(attempts) != want {
			t.Fatalf("sameModel=%t attempts=%+v", sameModel, attempts)
		}
		if !sameModel && (attempts[1].Model != "strong-model" || attempts[1].Effort != pbv1.Effort_EFFORT_UNSPECIFIED) {
			t.Fatal("fallback retained unsupported effort")
		}
	}
}

func TestFailoverKeepsRouteWithoutAdvancingLadder(t *testing.T) {
	h := sdktest.New(t)
	ladder := shadowLadder{Steps: []shadowStep{{ID: "fast", Model: "fast-model"}, {ID: "strong", Model: "strong-model"}}}
	state := shadowState{Step: "fast", PolicyHash: "policy", PendingSuggestion: &pendingAdvice{To: "strong", Status: "applying"}}
	h.Run(func() {
		requestRoute("original", ladder, state, "strong", "directive", false)
		reconcileAppliedRoute(&pbv1.ChatResponse{ToranaMetaJson: []byte(`{"_route_applied":{"provider":"original","model":"strong-model","verdict_plugin":"decision_router","served_by":"fallback","served_model":"other-model","failover":true}}`)}, &state)
	})
	if state.Step != "fast" || state.ActiveRoute != "strong" || state.ModelSwitches != 0 || state.PendingSuggestion.Status != "refused" {
		t.Fatalf("failover state = %+v", state)
	}
}

func TestUnreconciledAcceptanceTerminatesOnNextUserTurn(t *testing.T) {
	h := sdktest.New(t)
	state := shadowState{Step: "fast", UserTurns: 3, PendingSuggestion: &pendingAdvice{ID: "sg_1", To: "strong", Status: "applying", AttemptedTurn: 2}}
	h.Run(func() {
		reconcileAdvice(&pbv1.ChatRequest{ToranaMetaJson: []byte(`{"_suggestions":[{"id":"sg_1","status":"accepted"}]}`)}, shadowLadder{}, &state, "fast", false)
	})
	if state.PendingSuggestion.Status != "refused" || state.PendingSuggestion.Reason != "unreconciled" {
		t.Fatalf("state = %+v", state)
	}
	// Host feedback repeating accepted cannot reopen a terminal attempt.
	h.Run(func() { reconcileAdvice(&pbv1.ChatRequest{}, shadowLadder{}, &state, "fast", false) })
	if state.PendingSuggestion.Status != "refused" {
		t.Fatal("terminal acceptance reopened")
	}
}

func TestAcceptedCostGuardAndAlreadySelectedAreTerminal(t *testing.T) {
	policy, _, err := loadShadowPolicy(shadowConfigJSON)
	if err != nil {
		t.Fatal(err)
	}
	ladder := policy.Ladders["original"]
	for _, current := range []string{"fast", "strong"} {
		h := sdktest.New(t)
		state := shadowState{Step: current, UserTurns: 2, PendingSuggestion: &pendingAdvice{To: "strong", Status: "accepted"}}
		h.Run(func() { prepareAcceptance(policy, ladder, &state, 1000000000) })
		want := "blocked"
		if current == "strong" {
			want = "applied"
		}
		if state.PendingSuggestion.Status != want {
			t.Fatalf("current=%s status=%s", current, state.PendingSuggestion.Status)
		}
		if current == "fast" && state.PendingSuggestion.Reason != "cost_guard" {
			t.Fatalf("reason=%s", state.PendingSuggestion.Reason)
		}
	}
}
