package main

import (
	"strings"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

// Acceptance is not a bypass for current price data, caps, or ownership rules.
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
	} else {
		err = sdk.RouteRequest(provider, step.Model)
	}
	if err != nil {
		emit("route_refused", pricingLookupReason(err))
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
	if err != nil || !present || actual.VerdictPlugin != "decision_router" || actual.Refused != nil || actual.Failover || actual.Provider != provider || actual.Model != model || actual.ServedBy != provider || actual.ServedModel != model {
		state.ActiveRoute = ""
		emit("route_not_applied", "")
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
	if state.PendingSuggestion != nil && state.PendingSuggestion.To == target {
		state.PendingSuggestion.Status = "applied"
	}
}
