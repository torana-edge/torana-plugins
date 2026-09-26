package main

import (
	"errors"
	"strings"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

// Acceptance is not a bypass for current price data, caps, or ownership rules.
func prepareAcceptance(policy shadowPolicy, ladder shadowLadder, state *shadowState, contextTokens int64) policyDecision {
	decision := acceptedDecision(policy, ladder, *state, contextTokens)
	if decision.BlockedBy != "" {
		finishAcceptance(state, "blocked", decision.BlockedBy)
	} else if decision.Target == "" {
		finishAcceptance(state, "applied", "already_selected")
	} else {
		state.PendingSuggestion.Status = "applying"
		state.PendingSuggestion.AttemptedTurn = state.UserTurns
	}
	return decision
}

func acceptedDecision(policy shadowPolicy, ladder shadowLadder, state shadowState, contextTokens int64) policyDecision {
	target := state.PendingSuggestion.To
	current, next := shadowStepIndex(ladder, state.Step), shadowStepIndex(ladder, target)
	if current < 0 || next < 0 || next-current > 1 || current-next > 1 {
		return policyDecision{BlockedBy: "stale_suggestion"}
	}
	state.LastSuggestion = ""
	policy.Escalation.SuggestDeescalation = true
	policy.Escalation.MinUserTurnsBeforeDeescalation = 0
	return Evaluate(policy, ladder, state, policySignals{ContextTokens: contextTokens, AvgOutputTokens: state.AvgOutputTokens, AvgRequestsPerTurn: state.AvgRequestsPerTurn}, target)
}

func requestRoute(provider string, ladder shadowLadder, state shadowState, target, via string, manageEffort bool) {
	if via == "continue" && state.ActiveRouteVia != "" {
		via = state.ActiveRouteVia
	}
	i := shadowStepIndex(ladder, target)
	if i < 0 {
		emit("route_unknown_step", "")
		return
	}
	step := ladder.Steps[i]
	// Carry only the planned step and consent source through request metadata.
	// The after hook must see actual host application before moving the ladder.
	if err := sdk.MetaSet("decision_router_route_target", target); err != nil {
		emit("route_metadata_failed", "")
		return
	}
	if err := sdk.MetaSet("decision_router_route_via", via); err != nil {
		emit("route_metadata_failed", "")
		return
	}
	for key, value := range map[string]string{"decision_router_route_model": step.Model, "decision_router_route_provider": provider, "decision_router_route_policy": state.PolicyHash, "decision_router_route_up": "false", "decision_router_route_turn": state.LastUserTurnKey, "decision_router_route_from": state.Step} {
		if key == "decision_router_route_up" && i > shadowStepIndex(ladder, state.Step) {
			value = "true"
		}
		if err := sdk.MetaSet(key, value); err != nil {
			emit("route_metadata_failed", "")
			return
		}
	}
	var err error
	if manageEffort && step.Effort != "" {
		effort := pbv1.Effort(pbv1.Effort_value["EFFORT_"+strings.ToUpper(step.Effort)])
		err = sdk.RouteRequestWithEffort(provider, step.Model, effort)
		var refusal *sdk.HostCallRefusalError
		if errors.As(err, &refusal) && refusal.Code == pbv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED {
			emitReason("route_refused", "effort_denied")
			return
		}
		current := shadowStepIndex(ladder, state.Step)
		if errors.As(err, &refusal) && refusal.Code == pbv1.ErrorCode_ERROR_CODE_UNSUPPORTED && current >= 0 && ladder.Steps[current].Model != step.Model {
			emit("effort_model_only_fallback", "")
			err = sdk.RouteRequest(provider, step.Model)
		}
	} else {
		err = sdk.RouteRequest(provider, step.Model)
	}
	if err != nil {
		emitReason("route_refused", pricingLookupReason(err))
	}
}

func reconcileAppliedRoute(response *pbv1.ChatResponse, state *shadowState) {
	target, found, err := sdk.MetaGet("decision_router_route_target")
	if err != nil || !found || target == "" {
		return
	}
	model, _, _ := sdk.MetaGet("decision_router_route_model")
	provider, _, _ := sdk.MetaGet("decision_router_route_provider")
	policyHash, _, _ := sdk.MetaGet("decision_router_route_policy")
	via, _, _ := sdk.MetaGet("decision_router_route_via")
	up, _, _ := sdk.MetaGet("decision_router_route_up")
	turn, _, _ := sdk.MetaGet("decision_router_route_turn")
	from, _, _ := sdk.MetaGet("decision_router_route_from")
	if state.PolicyHash != policyHash || state.LastUserTurnKey != turn || (state.Step != from && state.Step != target) {
		return
	}
	actual, present, err := sdk.RouteApplied(response)
	if err != nil || !present || actual.VerdictPlugin != "decision_router" || actual.Refused != nil || actual.Provider != provider || actual.Model != model {
		// Missing/foreign outcomes do not revoke an already established route.
		if present && actual.VerdictPlugin == "decision_router" && actual.Refused != nil {
			state.ActiveRoute = ""
			state.ActiveRouteVia = ""
		}
		finishAcceptance(state, "refused", "not_applied")
		emit("route_not_applied", "")
		return
	}
	if actual.Failover || actual.ServedBy != provider || actual.ServedModel != model {
		// Keep routing to the selected target across the turn. Failover is an
		// upstream transport outcome, not permission to downgrade the ladder.
		state.ActiveRoute = target
		state.ActiveRouteVia = via
		finishAcceptance(state, "refused", "failover")
		emit("route_failover", "")
		return
	}
	if state.Step != target {
		state.History = append(state.History, routeHistory{AtUserTurn: state.UserTurns, From: state.Step, To: target, Via: via})
		if len(state.History) > 32 {
			state.History = state.History[len(state.History)-32:]
		}
		if up == "true" {
			state.ModelSwitches++
		}
	}
	state.Step, state.ActiveRoute, state.OffLadder = target, target, false
	state.ActiveRouteVia = via
	if state.PendingSuggestion != nil && state.PendingSuggestion.To == target {
		state.PendingSuggestion.Status = "applied"
	}
}

func finishAcceptance(state *shadowState, status, reason string) {
	p := state.PendingSuggestion
	if p == nil || (p.Status != "accepted" && p.Status != "applying") {
		return
	}
	p.Status, p.Reason = status, reason
	emit("acceptance_"+status, reason)
}
