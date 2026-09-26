package main

import (
	"errors"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

// Resolve only for policy evaluations, never for tool continuations. Missing
// rates stay missing: partial declarations do not mean free caching.
func resolveLadderPricing(provider string, ladder shadowLadder) shadowLadder {
	ladder.Steps = append([]shadowStep(nil), ladder.Steps...)
	for i := range ladder.Steps {
		step := &ladder.Steps[i]
		if step.Pricing != nil {
			emit("deprecated_pricing_override", step.ID)
			if completePrice(step.Pricing) && step.Pricing.Input != nil {
				continue
			}
		}
		caps, err := sdk.GetModelCapabilities(provider, step.Model)
		if err != nil {
			emit("pricing_lookup", pricingLookupReason(err))
			continue
		}
		if caps == nil || caps.Pricing == nil {
			continue
		}
		p := caps.Pricing
		declared := shadowPricing{Input: p.InputUsdPerMtok, Output: p.OutputUsdPerMtok,
			CacheRead: p.CacheReadUsdPerMtok, CacheWrite: p.CacheWriteUsdPerMtok}
		if override := step.Pricing; override != nil {
			if override.Input != nil {
				declared.Input = override.Input
			}
			if override.Output != nil {
				declared.Output = override.Output
			}
			if override.CacheRead != nil {
				declared.CacheRead = override.CacheRead
			}
			if override.CacheWrite != nil {
				declared.CacheWrite = override.CacheWrite
			}
		}
		step.Pricing = &declared
	}
	return ladder
}

func pricingLookupReason(err error) string {
	var refusal *sdk.HostCallRefusalError
	if errors.As(err, &refusal) {
		switch refusal.Code {
		case pbv1.ErrorCode_ERROR_CODE_NOT_FOUND:
			return "not_declared"
		case pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED:
			return "not_configured"
		case pbv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED:
			return "denied"
		}
	}
	return "error"
}
