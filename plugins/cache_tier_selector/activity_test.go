package main

import (
	"encoding/json"
	"testing"
)

func TestStickyReadsRemainConversationActivity(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(`{"mode":"auto","min_gap_seconds_for_long_tier":1000}`)
	h.StubHostCall("env.cache_policy", pricingStub())
	for _, now := range []int64{1000000, 1200000, 1400000, 1600000, 1800000, 2000000} {
		h.SetNow(now)
		if res := h.BeforeRequest(reqWith(t, h)); res.Err != nil {
			t.Fatal(res.Err)
		}
	}
	raw, found := h.State("activity/conv-1")
	if !found {
		t.Fatal("conversation activity missing")
	}
	var act activity
	if err := json.Unmarshal([]byte(raw), &act); err != nil {
		t.Fatal(err)
	}
	if act.LastSeenMillis != 2000000 || act.LongestGapMillis != 200000 || act.Turns != 6 {
		t.Fatalf("active cached turns treated as an idle gap: %+v", act)
	}
}

func TestExplicitModeActivitySurvivesSwitchToAuto(t *testing.T) {
	for _, mode := range []string{"long", "short"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t)
			h.SetConfig(`{"mode":"` + mode + `"}`)
			h.StubHostCall("env.cache_policy", pricingStub())
			for _, now := range []int64{1000000, 1200000, 1400000, 1600000, 1800000} {
				h.SetNow(now)
				if res := h.BeforeRequest(reqWith(t, h)); res.Err != nil {
					t.Fatal(res.Err)
				}
			}
			h.SetConfig(`{"mode":"auto","min_gap_seconds_for_long_tier":1000}`)
			h.SetNow(2000000)
			if res := h.BeforeRequest(reqWith(t, h)); res.Err != nil {
				t.Fatal(res.Err)
			}
			raw, _ := h.State("activity/conv-1")
			var act activity
			if err := json.Unmarshal([]byte(raw), &act); err != nil {
				t.Fatal(err)
			}
			if act.LastSeenMillis != 2000000 || act.LongestGapMillis != 200000 || act.Turns != 6 {
				t.Fatalf("mode switch invented idle gap: %+v", act)
			}
		})
	}
}
