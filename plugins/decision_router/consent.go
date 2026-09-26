package main

import (
	"encoding/json"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

type pendingAdvice struct {
	ID              string `json:"id"`
	To              string `json:"to"`
	IssuedTurn      int    `json:"issued_user_turn"`
	ExpiresUserTurn int    `json:"expires_user_turn"`
	Status          string `json:"status"`
	Via             string `json:"via,omitempty"`
	Reason          string `json:"reason,omitempty"`
	AttemptedTurn   int    `json:"attempted_user_turn,omitempty"`
}

type routeHistory struct {
	AtUserTurn int    `json:"at_user_turn"`
	From       string `json:"from"`
	To         string `json:"to"`
	Via        string `json:"via"`
}

// The host owns acceptance. We only consume feedback matching our persisted
// suggestion; no prompt text or guessed confirmation code grants consent.
func reconcileAdvice(req *pbv1.ChatRequest, ladder shadowLadder, state *shadowState, previousStep string, switched bool) {
	p := state.PendingSuggestion
	if p != nil {
		if p.Status == "applying" && state.UserTurns > p.AttemptedTurn {
			finishAcceptance(state, "refused", "unreconciled")
		}
		outcomes, err := sdk.Suggestions(req)
		if err != nil {
			emit("suggestion_feedback_invalid", "")
		} else {
			for _, outcome := range outcomes {
				if outcome.ID != p.ID || p.Status != "pending" {
					continue
				}
				switch outcome.Status {
				case "accepted", "dismissed", "expired", "superseded":
					p.Status, p.Via = outcome.Status, outcome.Via
				}
			}
		}
		// Host expiry is authoritative; this is a fallback for missing feedback.
		if p.Status == "pending" && state.UserTurns > p.ExpiresUserTurn {
			p.Status = "expired"
		}
	}
	if !switched {
		return
	}
	via := "harness_switch_unprompted"
	if p != nil && (p.Status == "pending" || p.Status == "accepted") && state.Step == p.To {
		p.Status, p.Via = "accepted", "harness_switch"
		via = "harness_switch"
	}
	if state.Step == previousStep || state.OffLadder {
		return
	}
	if via == "harness_switch" && shadowStepIndex(ladder, state.Step) > shadowStepIndex(ladder, previousStep) {
		state.ModelSwitches++
	}
	state.History = append(state.History, routeHistory{AtUserTurn: state.UserTurns, From: previousStep, To: state.Step, Via: via})
	if len(state.History) > 32 {
		state.History = state.History[len(state.History)-32:]
	}
}

// Merge the returned host ID into the latest state, not the pre-Suggest
// snapshot: another request may have updated usage while the host call ran.
func rememberAdvice(key, policyHash string, pending *pendingAdvice) {
	for attempt := 0; attempt < 3; attempt++ {
		stored, found, err := sdk.StateGetVersioned(key)
		if err != nil || !found {
			emit("suggestion_state_failed", "")
			return
		}
		var state shadowState
		if json.Unmarshal([]byte(stored.Value), &state) != nil {
			emit("suggestion_state_failed", "")
			return
		}
		if state.PolicyHash != policyHash || state.UserTurns != pending.IssuedTurn || state.LastSuggestion != pending.To {
			emit("suggestion_state_stale", "")
			return
		}
		if state.PendingSuggestion != nil && state.PendingSuggestion.ID == pending.ID {
			return
		}
		state.PendingSuggestion = pending
		applied, err := saveShadowState(key, state, &stored.Version)
		if err != nil {
			emit("suggestion_state_failed", "")
			return
		}
		if applied {
			return
		}
	}
	emit("suggestion_state_conflict", "")
}
