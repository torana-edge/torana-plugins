package main

import (
	"testing"

	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
)

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
