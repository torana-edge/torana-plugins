package main

import (
	"encoding/json"
	"testing"

	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
)

func TestSuggestionFeedbackRequiresMatchingPersistedID(t *testing.T) {
	ladder := shadowLadder{Steps: []shadowStep{{ID: "fast", Model: "fast-model"}, {ID: "strong", Model: "strong-model"}}}
	for _, status := range []string{"accepted", "dismissed", "expired"} {
		t.Run(status, func(t *testing.T) {
			state := shadowState{Step: "fast", UserTurns: 2, PendingSuggestion: &pendingAdvice{ID: "sg_known", To: "strong", ExpiresUserTurn: 5, Status: "pending"}}
			req := &pbv1.ChatRequest{ToranaMetaJson: []byte(`{"_suggestions":[{"id":"other","status":"accepted"}]}`)}
			reconcileAdvice(req, ladder, &state, "fast", false)
			if state.PendingSuggestion.Status != "pending" {
				t.Fatal("unrelated outcome granted consent")
			}
			req.ToranaMetaJson = []byte(`{"_suggestions":[{"id":"sg_known","status":"` + status + `","via":"directive"}]}`)
			reconcileAdvice(req, ladder, &state, "fast", false)
			if state.PendingSuggestion.Status != status || state.Step != "fast" {
				t.Fatalf("state = %+v", state)
			}
		})
	}
}

func TestHarnessSwitchAndExpiry(t *testing.T) {
	ladder := shadowLadder{Steps: []shadowStep{{ID: "fast", Model: "fast-model"}, {ID: "strong", Model: "strong-model"}}}
	state := shadowState{Step: "strong", UserTurns: 3, PendingSuggestion: &pendingAdvice{ID: "sg_1", To: "strong", ExpiresUserTurn: 5, Status: "pending"}}
	reconcileAdvice(&pbv1.ChatRequest{}, ladder, &state, "fast", true)
	if state.PendingSuggestion.Via != "harness_switch" || state.PendingSuggestion.Status != "accepted" || state.ModelSwitches != 1 || len(state.History) != 1 {
		t.Fatalf("state = %+v", state)
	}
	reconcileAdvice(&pbv1.ChatRequest{}, ladder, &state, "strong", false)
	if state.ModelSwitches != 1 || len(state.History) != 1 {
		t.Fatal("replayed request counted another switch")
	}
	state.PendingSuggestion = &pendingAdvice{ID: "sg_old", To: "fast", ExpiresUserTurn: 2, Status: "pending"}
	reconcileAdvice(&pbv1.ChatRequest{}, ladder, &state, "strong", false)
	if state.PendingSuggestion.Status != "expired" {
		t.Fatal("suggestion did not expire")
	}
}

func TestRememberAdviceMergesIntoLatestState(t *testing.T) {
	h := sdktest.New(t)
	state := shadowState{PolicyHash: "policy", UserTurns: 2, LastSuggestion: "strong", ContextTokens: 12345}
	h.Run(func() {
		if ok, err := saveShadowState("session", state, nil); err != nil || !ok {
			t.Fatalf("save = %t %v", ok, err)
		}
		rememberAdvice("session", "policy", &pendingAdvice{ID: "sg_1", To: "strong", IssuedTurn: 2, Status: "pending"})
	})
	raw, _ := h.State("session")
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		t.Fatal(err)
	}
	if state.PendingSuggestion == nil || state.PendingSuggestion.ID != "sg_1" || state.ContextTokens != 12345 {
		t.Fatalf("state = %+v", state)
	}
	h.Run(func() {
		rememberAdvice("session", "policy", &pendingAdvice{ID: "sg_stale", To: "strong", IssuedTurn: 1, Status: "pending"})
	})
	raw, _ = h.State("session")
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		t.Fatal(err)
	}
	if state.PendingSuggestion.ID != "sg_1" {
		t.Fatal("stale request overwrote current suggestion")
	}
}
