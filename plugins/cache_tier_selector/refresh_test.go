package main

import (
	"strings"
	"testing"
)

func TestLivePrefixDoesNotExpireWhileRefreshed(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(`{"mode":"long"}`)
	h.StubHostCall("env.cache_policy", pricingStub())
	h.SetNow(1000000)
	initial := h.BeforeRequest(reqWith(t, h))
	if initial.Err != nil || initial.Request == nil {
		t.Fatalf("initial long decision failed: %v", initial.Err)
	}
	// Configuration reloads preserve durable state; the current implementation
	// deliberately honors the prior decision until it considers it expired.
	h.SetConfig(`{"mode":"short"}`)
	// Real provider policy is RefreshOnRead=true: each use renews the
	// same prefix for another hour. It is live at the final invocation.
	for _, now := range []int64{2000000, 3000000, 4000000, 4600001} {
		h.SetNow(now)
		res := h.BeforeRequest(reqWith(t, h))
		if res.Err != nil {
			t.Fatal(res.Err)
		}
		if res.Request == nil || !strings.Contains(string(carrierMarkerAt(t, res.Request, 1, 1)), `"ttl":"1h"`) {
			t.Fatalf("live, repeatedly refreshed prefix lost its sticky long tier at %d", now)
		}
	}
	// A genuinely idle prefix can be reconsidered after its refreshed TTL.
	h.SetNow(8200001)
	expired := h.BeforeRequest(reqWith(t, h))
	if expired.Err != nil {
		t.Fatal(expired.Err)
	}
	if expired.Request != nil && strings.Contains(string(carrierMarkerAt(t, expired.Request, 1, 1)), `"ttl":"1h"`) {
		t.Fatal("idle prefix retained an expired long-tier decision")
	}
}

func TestAbsoluteTierLifetimeDoesNotRefresh(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(`{"mode":"long"}`)
	policy := testPolicy()
	policy.RefreshOnRead = false
	h.StubHostCall("env.cache_policy", policyStub(policy))
	h.SetNow(1000000)
	if res := h.BeforeRequest(reqWith(t, h)); res.Err != nil || res.Request == nil {
		t.Fatalf("initial decision: %v", res.Err)
	}
	h.SetConfig(`{"mode":"short"}`)
	writes := countCommand(h, "env.state_set")
	h.SetNow(4000000)
	if res := h.BeforeRequest(reqWith(t, h)); res.Err != nil || res.Request == nil {
		t.Fatalf("unexpired decision: %v", res.Err)
	}
	if got := countCommand(h, "env.state_set"); got != writes {
		t.Fatal("absolute TTL use rewrote state")
	}
	h.SetNow(4600001)
	res := h.BeforeRequest(reqWith(t, h))
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if res.Request != nil && strings.Contains(string(carrierMarkerAt(t, res.Request, 1, 1)), `"ttl":"1h"`) {
		t.Fatal("absolute TTL was extended by a read")
	}
}
