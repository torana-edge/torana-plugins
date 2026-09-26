package main

import sdk "github.com/torana-edge/torana-plugin-sdk"

// Resolve only for policy evaluations, never for tool continuations. Missing
// rates stay missing: partial declarations do not mean free caching.
func resolveLadderPricing(provider string, ladder shadowLadder) shadowLadder {
	ladder.Steps = append([]shadowStep(nil), ladder.Steps...)
	for i := range ladder.Steps {
		step := &ladder.Steps[i]
		if step.Pricing != nil {
			emit("deprecated_pricing_override", step.ID)
			continue
		}
		caps, err := sdk.GetModelCapabilities(provider, step.Model)
		if err != nil || caps == nil || caps.Pricing == nil {
			continue
		}
		p := caps.Pricing
		step.Pricing = &shadowPricing{Input: p.InputUsdPerMtok, Output: p.OutputUsdPerMtok,
			CacheRead: p.CacheReadUsdPerMtok, CacheWrite: p.CacheWriteUsdPerMtok}
	}
	return ladder
}
