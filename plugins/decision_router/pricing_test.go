package main

import (
	"fmt"
	"testing"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
)

func TestPartialOverrideMergesDeclaredRatesWithoutMutation(t *testing.T) {
	h := sdktest.New(t)
	h.StubModelCapabilities(func(*pbv1.ModelCapabilitiesArgs) (*pbv1.ModelCapabilities, *pbv1.HostError, error) {
		return &pbv1.ModelCapabilities{Format: "anthropic", Pricing: &pbv1.ModelPricing{
			InputUsdPerMtok: rate(1), OutputUsdPerMtok: rate(2), CacheReadUsdPerMtok: rate(.1), CacheWriteUsdPerMtok: rate(1.25),
		}}, nil, nil
	})
	override := &shadowPricing{Output: rate(0)}
	h.Run(func() {
		got := resolveLadderPricing("original", shadowLadder{Steps: []shadowStep{{ID: "fast", Model: "fast", Pricing: override}}})
		p := got.Steps[0].Pricing
		if !completePrice(p) || *p.Output != 0 || *p.CacheRead != .1 || *p.CacheWrite != 1.25 {
			t.Fatalf("merged prices = %+v", p)
		}
	})
	if override.Input != nil || override.CacheRead != nil || override.CacheWrite != nil {
		t.Fatal("configured override mutated")
	}
}

func TestLookupRefusalsHaveBoundedReasons(t *testing.T) {
	for code, reason := range map[pbv1.ErrorCode]string{
		pbv1.ErrorCode_ERROR_CODE_NOT_FOUND:         "not_declared",
		pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED:    "not_configured",
		pbv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED: "denied",
		pbv1.ErrorCode_ERROR_CODE_INTERNAL:          "error",
	} {
		t.Run(reason+code.String(), func(t *testing.T) {
			err := fmt.Errorf("wrapped: %w", &sdk.HostCallRefusalError{Code: code})
			if got := pricingLookupReason(err); got != reason {
				t.Fatalf("reason = %s", got)
			}
			h := sdktest.New(t)
			h.StubModelCapabilities(func(*pbv1.ModelCapabilitiesArgs) (*pbv1.ModelCapabilities, *pbv1.HostError, error) {
				return nil, &pbv1.HostError{Code: code, Message: "private detail"}, nil
			})
			h.Run(func() {
				resolveLadderPricing("original", shadowLadder{Steps: []shadowStep{{ID: "fast", Model: "fast"}}})
			})
			count := 0
			for _, call := range h.Calls() {
				if call.Command == "env.model_capabilities" {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("lookup count = %d", count)
			}
		})
	}
}

func TestDeclaredPricingPreservesMissingAndZeroRates(t *testing.T) {
	h := sdktest.New(t)
	h.StubModelCapabilities(func(args *pbv1.ModelCapabilitiesArgs) (*pbv1.ModelCapabilities, *pbv1.HostError, error) {
		if args.Provider != "original" || args.Model != "fast" {
			t.Fatalf("lookup = %+v", args)
		}
		return &pbv1.ModelCapabilities{Format: "anthropic", Pricing: &pbv1.ModelPricing{
			OutputUsdPerMtok: rate(2), CacheReadUsdPerMtok: rate(0),
		}}, nil, nil
	})
	input := shadowLadder{Steps: []shadowStep{{ID: "fast", Model: "fast"}}}
	h.Run(func() {
		got := resolveLadderPricing("original", input)
		p := got.Steps[0].Pricing
		if p == nil || p.CacheRead == nil || *p.CacheRead != 0 || p.CacheWrite != nil || completePrice(p) {
			t.Fatalf("missing/zero rates lost: %+v", p)
		}
	})
	if input.Steps[0].Pricing != nil {
		t.Fatal("resolution mutated configured ladder")
	}
}

func TestPricingOverrideSkipsHostLookup(t *testing.T) {
	h := sdktest.New(t)
	input := shadowLadder{Steps: []shadowStep{pricedStep("fast", "fast", 2, 0, 1)}}
	h.Run(func() {
		got := resolveLadderPricing("original", input)
		if got.Steps[0].Pricing != input.Steps[0].Pricing {
			t.Fatal("explicit override was replaced")
		}
	})
	for _, call := range h.Calls() {
		if call.Command == "env.model_capabilities" {
			t.Fatal("override unexpectedly queried host")
		}
	}
}

func TestMissingCapabilitiesKeepPricingUnavailable(t *testing.T) {
	h := sdktest.New(t)
	h.Run(func() {
		got := resolveLadderPricing("original", shadowLadder{Steps: []shadowStep{{ID: "unknown", Model: "unknown"}}})
		if got.Steps[0].Pricing != nil {
			t.Fatal("missing capabilities fabricated pricing")
		}
	})
}
