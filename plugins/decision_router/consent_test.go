package main

import (
	"encoding/json"
	"testing"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
)

func TestSuggestionFeedbackRequiresMatchingPersistedID(t *testing.T) {
	ladder := shadowLadder{Steps: []shadowStep{{ID: "fast", Model: "fast-model"}, {ID: "strong", Model: "strong-model"}}}
	for _, status := range []string{"accepted", "dismissed", "expired", "superseded"} {
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

func TestUnpromptedAndSupersededSwitchesDoNotConsumeCap(t *testing.T) {
	ladder := shadowLadder{Steps: []shadowStep{{ID: "fast", Model: "fast-model"}, {ID: "strong", Model: "strong-model"}}}
	for _, pending := range []*pendingAdvice{nil, {ID: "sg_old", To: "strong", Status: "superseded", ExpiresUserTurn: 5}} {
		state := shadowState{Step: "strong", UserTurns: 2, PendingSuggestion: pending}
		reconcileAdvice(&pbv1.ChatRequest{}, ladder, &state, "fast", true)
		if state.ModelSwitches != 0 || len(state.History) != 1 || state.History[0].Via != "harness_switch_unprompted" {
			t.Fatalf("unprompted state = %+v", state)
		}
		if pending != nil && pending.Status != "superseded" {
			t.Fatal("terminal advice was accepted")
		}
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
	var stale int
	for _, metric := range h.Metrics() {
		if metric.Labels["outcome"] == "suggestion_state_stale" {
			stale++
		}
	}
	if stale != 1 {
		t.Fatalf("stale metrics = %d", stale)
	}
}

func TestAdviceRefreshExtendsExpiryWithoutReopeningAcceptance(t *testing.T) {
	h := sdktest.New(t)
	state := shadowState{PolicyHash: "policy", UserTurns: 5, LastSuggestion: "strong", PendingSuggestion: &pendingAdvice{ID: "sg_1", To: "strong", Status: "pending", ExpiresUserTurn: 5}}
	h.Run(func() {
		if ok, err := saveShadowState("session", state, nil); err != nil || !ok {
			t.Fatalf("save=%t %v", ok, err)
		}
		rememberAdvice("session", "policy", &pendingAdvice{ID: "sg_1", To: "strong", IssuedTurn: 5, ExpiresUserTurn: 8, Status: "pending"})
	})
	raw, _ := h.State("session")
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		t.Fatal(err)
	}
	if state.PendingSuggestion.ExpiresUserTurn != 8 {
		t.Fatal("refreshed host expiry was not preserved")
	}
	h.Run(func() {
		stored, _, err := sdk.StateGetVersioned("session")
		if err != nil {
			t.Fatal(err)
		}
		state.PendingSuggestion.Status = "accepted"
		if ok, err := saveShadowState("session", state, &stored.Version); err != nil || !ok {
			t.Fatalf("save=%t %v", ok, err)
		}
		rememberAdvice("session", "policy", &pendingAdvice{ID: "sg_1", To: "strong", IssuedTurn: 5, ExpiresUserTurn: 9, Status: "pending"})
	})
	raw, _ = h.State("session")
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		t.Fatal(err)
	}
	if state.PendingSuggestion.Status != "accepted" {
		t.Fatal("refresh reopened accepted advice")
	}
}

func TestHostFeedbackCanRecoverOnlyLocalExpiry(t *testing.T) {
	for _, reason := range []string{"local_expiry", ""} {
		state := shadowState{UserTurns: 9, PendingSuggestion: &pendingAdvice{ID: "sg_1", Status: "expired", Reason: reason}}
		req := &pbv1.ChatRequest{ToranaMetaJson: []byte(`{"_suggestions":[{"id":"sg_1","status":"accepted","via":"directive"}]}`)}
		reconcileAdvice(req, shadowLadder{}, &state, "", false)
		want := "expired"
		if reason == "local_expiry" {
			want = "accepted"
		}
		if state.PendingSuggestion.Status != want {
			t.Fatalf("reason=%q status=%s", reason, state.PendingSuggestion.Status)
		}
	}
}

func TestAdviceRefreshRecoversOnlyLocalExpiry(t *testing.T) {
	for _, status := range []string{"local_expiry", "expired", "dismissed", "superseded"} {
		t.Run(status, func(t *testing.T) {
			h := sdktest.New(t)
			pending := &pendingAdvice{ID: "sg_1", Status: status, ExpiresUserTurn: 2}
			if status == "local_expiry" {
				pending.Status, pending.Reason = "expired", "local_expiry"
			}
			state := shadowState{PolicyHash: "policy", UserTurns: 5, LastSuggestion: "strong", PendingSuggestion: pending}
			h.Run(func() {
				if ok, err := saveShadowState("session", state, nil); !ok || err != nil {
					t.Fatal(err)
				}
				rememberAdvice("session", "policy", &pendingAdvice{ID: "sg_1", To: "strong", IssuedTurn: 5, ExpiresUserTurn: 8, Status: "pending"})
			})
			raw, _ := h.State("session")
			if err := json.Unmarshal([]byte(raw), &state); err != nil {
				t.Fatal(err)
			}
			want := status
			if status == "local_expiry" {
				want = "pending"
			}
			if state.PendingSuggestion.Status != want {
				t.Fatalf("status=%s want=%s", state.PendingSuggestion.Status, want)
			}
		})
	}
}
