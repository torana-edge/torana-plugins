package main

import "math"

// policyDecision is an observation in shadow mode. The host is not asked to
// route; later modes can use the same pure evaluation with explicit consent.
type policyDecision struct {
	Target       string
	Class        string
	Reason       string
	BlockedBy    string
	RebuildUSD   float64
	PerTurnDelta float64
	PaybackTurns float64
}

type policySignals struct {
	RecentToolErrors    int
	RetryStreak         int
	RequestsPerUserTurn int
	MaxTokensFinishes   int
	ContextTokens       int64
	AvgOutputTokens     float64
}

// Evaluate advances at most one ladder step. Missing price data never becomes
// zero cost; it keeps the candidate visible to shadow metrics but blocks an
// applied or user-facing switch until the operator supplies real rates.
func Evaluate(policy shadowPolicy, ladder shadowLadder, state shadowState, signals policySignals, classifierAnswer string) policyDecision {
	current := shadowStepIndex(ladder, state.Step)
	if current < 0 {
		return policyDecision{BlockedBy: "unknown_step"}
	}
	target := current
	reason := ""
	localEscalation := signals.RecentToolErrors >= policy.Triggers.ToolErrorThreshold ||
		signals.RetryStreak >= policy.Triggers.RetryStreak ||
		signals.RequestsPerUserTurn >= policy.Triggers.RequestsPerUserTurn ||
		signals.MaxTokensFinishes >= policy.Triggers.MaxTokensFinishes
	if localEscalation {
		target, reason = current+1, "local_signal"
	}
	if wanted := shadowStepIndex(ladder, classifierAnswer); wanted > current {
		target, reason = current+1, "classifier"
	} else if !localEscalation && wanted >= 0 && wanted < current && policy.Escalation.SuggestDeescalation &&
		state.UserTurns >= policy.Escalation.MinUserTurnsBeforeDeescalation {
		target, reason = current-1, "deescalation"
	}
	if target < 0 || target >= len(ladder.Steps) || target == current {
		return policyDecision{}
	}
	from, to := ladder.Steps[current], ladder.Steps[target]
	decision := policyDecision{Target: to.ID, Reason: reason, Class: "model_switch"}
	if from.Model == to.Model {
		decision.Class = "effort_change"
		if !policy.ManageEffort {
			decision.BlockedBy = "harness_owns_effort"
			return decision
		}
	}
	if target > current && decision.Class == "model_switch" && state.ModelSwitches >= policy.Escalation.MaxModelSwitches {
		decision.BlockedBy = "switch_cap"
		return decision
	}
	if to.ID == state.LastSuggestion && state.UserTurns-state.SuggestedAtTurn < policy.Triggers.SuggestionCooldown {
		decision.BlockedBy = "cooldown"
		return decision
	}
	if !completePrice(from.Pricing) || !completePrice(to.Pricing) || signals.ContextTokens < 0 ||
		math.IsNaN(signals.AvgOutputTokens) || math.IsInf(signals.AvgOutputTokens, 0) || signals.AvgOutputTokens < 0 {
		decision.BlockedBy = "pricing_unavailable"
		return decision
	}
	context := float64(signals.ContextTokens) / 1e6
	output := signals.AvgOutputTokens / 1e6
	decision.RebuildUSD = context * *to.Pricing.CacheWrite
	decision.PerTurnDelta = context*(*to.Pricing.CacheRead-*from.Pricing.CacheRead) + output*(*to.Pricing.Output-*from.Pricing.Output)
	if target < current && decision.PerTurnDelta < 0 {
		decision.PaybackTurns = decision.RebuildUSD / -decision.PerTurnDelta
	}
	if decision.RebuildUSD > policy.Escalation.MaxSwitchCostUSD {
		decision.BlockedBy = "cost_guard"
	}
	return decision
}

func completePrice(p *shadowPricing) bool {
	return p != nil && p.Output != nil && p.CacheRead != nil && p.CacheWrite != nil
}
