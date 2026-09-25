package main

import (
	"math"
	"testing"
)

func rate(v float64) *float64 { return &v }

func pricedStep(id, model string, output, read, write float64) shadowStep {
	return shadowStep{ID: id, Model: model, Pricing: &shadowPricing{
		Input: rate(0), Output: rate(output), CacheRead: rate(read), CacheWrite: rate(write),
	}}
}

func TestPolicyEconomicsAndBoundaries(t *testing.T) {
	ladder := shadowLadder{Start: "sonnet", Steps: []shadowStep{
		pricedStep("sonnet", "sonnet", 10, .2, 2.5),
		pricedStep("opus", "opus", 25, .5, 6.25),
	}}
	policy := shadowPolicy{
		Triggers:   shadowTriggers{ToolErrorThreshold: 3, RetryStreak: 3, RequestsPerUserTurn: 25, MaxTokensFinishes: 2, SuggestionCooldown: 3},
		Escalation: shadowEscalation{MaxModelSwitches: 1, MaxSwitchCostUSD: .5, SuggestDeescalation: true, MinUserTurnsBeforeDeescalation: 3},
	}
	base := shadowState{Step: "sonnet", UserTurns: 4}
	signals := policySignals{RecentToolErrors: 3, ContextTokens: 60000, AvgOutputTokens: 1000}
	decision := Evaluate(policy, ladder, base, signals, "")
	if decision.Target != "opus" || decision.BlockedBy != "" || decision.Class != "model_switch" ||
		math.Abs(decision.RebuildUSD-.375) > 1e-9 || math.Abs(decision.PerTurnDelta-.033) > 1e-9 {
		t.Fatalf("upgrade economics = %+v", decision)
	}
	threeRequests := signals
	threeRequests.AvgRequestsPerTurn = 3
	perTurn := Evaluate(policy, ladder, base, threeRequests, "")
	if math.Abs(perTurn.PerTurnDelta-.099) > 1e-9 {
		t.Fatalf("three-request turn economics = %+v", perTurn)
	}

	downgrade := base
	downgrade.Step = "opus"
	decision = Evaluate(policy, ladder, downgrade, policySignals{ContextTokens: 60000, AvgOutputTokens: 1000}, "sonnet")
	if decision.Target != "sonnet" || decision.BlockedBy != "" ||
		math.Abs(decision.RebuildUSD-.150) > 1e-9 || math.Abs(decision.PerTurnDelta+.033) > 1e-9 ||
		math.Abs(decision.PaybackTurns-(.150/.033)) > 1e-9 {
		t.Fatalf("downgrade economics = %+v", decision)
	}

	for _, tc := range []struct {
		name    string
		change  func(*shadowPolicy, *shadowLadder, *shadowState, *policySignals)
		blocked string
	}{
		{"switch cap", func(_ *shadowPolicy, _ *shadowLadder, s *shadowState, _ *policySignals) { s.ModelSwitches = 1 }, "switch_cap"},
		{"cooldown", func(_ *shadowPolicy, _ *shadowLadder, s *shadowState, _ *policySignals) {
			s.LastSuggestion = "opus"
			s.SuggestedAtTurn = 3
		}, "cooldown"},
		{"cost guard", func(p *shadowPolicy, _ *shadowLadder, _ *shadowState, _ *policySignals) {
			p.Escalation.MaxSwitchCostUSD = .1
		}, "cost_guard"},
		{"missing current pricing", func(_ *shadowPolicy, l *shadowLadder, _ *shadowState, _ *policySignals) { l.Steps[0].Pricing = nil }, "pricing_unavailable"},
		{"missing target pricing", func(_ *shadowPolicy, l *shadowLadder, _ *shadowState, _ *policySignals) { l.Steps[1].Pricing = nil }, "pricing_unavailable"},
		{"missing target output", func(_ *shadowPolicy, l *shadowLadder, _ *shadowState, _ *policySignals) {
			l.Steps[1].Pricing.Output = nil
		}, "pricing_unavailable"},
		{"negative context", func(_ *shadowPolicy, _ *shadowLadder, _ *shadowState, s *policySignals) { s.ContextTokens = -1 }, "pricing_unavailable"},
		{"negative output", func(_ *shadowPolicy, _ *shadowLadder, _ *shadowState, s *policySignals) { s.AvgOutputTokens = -1 }, "pricing_unavailable"},
		{"nan output", func(_ *shadowPolicy, _ *shadowLadder, _ *shadowState, s *policySignals) {
			s.AvgOutputTokens = math.NaN()
		}, "pricing_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, l, st, sig := policy, shadowLadder{Start: ladder.Start, Steps: append([]shadowStep(nil), ladder.Steps...)}, base, signals
			for i := range l.Steps {
				price := *l.Steps[i].Pricing
				l.Steps[i].Pricing = &price
			}
			tc.change(&p, &l, &st, &sig)
			got := Evaluate(p, l, st, sig, "")
			if got.Target != "opus" || got.BlockedBy != tc.blocked {
				t.Fatalf("decision = %+v, want blocked by %s", got, tc.blocked)
			}
		})
	}
}

func TestPolicyAlwaysMovesOneStep(t *testing.T) {
	for size := 2; size <= 10; size++ {
		steps := make([]shadowStep, size)
		for i := range steps {
			steps[i] = pricedStep(string(rune('a'+i)), string(rune('a'+i)), 10+float64(i), .2, 2.5)
		}
		ladder := shadowLadder{Start: steps[0].ID, Steps: steps}
		policy := shadowPolicy{Triggers: shadowTriggers{ToolErrorThreshold: 1, RetryStreak: 3, RequestsPerUserTurn: 25, MaxTokensFinishes: 2}, Escalation: shadowEscalation{MaxModelSwitches: 10, MaxSwitchCostUSD: 10}}
		for at := 0; at < size; at++ {
			state := shadowState{Step: steps[at].ID, UserTurns: 10}
			got := Evaluate(policy, ladder, state, policySignals{RecentToolErrors: 1, ContextTokens: 1000}, steps[size-1].ID)
			if at == size-1 && got.Target != "" {
				t.Fatalf("size=%d at=%d moved past end: %+v", size, at, got)
			}
			if at < size-1 && got.Target != steps[at+1].ID {
				t.Fatalf("size=%d at=%d jumped: %+v", size, at, got)
			}
		}
	}
}
