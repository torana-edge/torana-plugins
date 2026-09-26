package main

import (
	"strings"
	"testing"

	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
	"google.golang.org/protobuf/proto"
)

func TestAdviceIsCostedBoundedAndNeverRoutes(t *testing.T) {
	h := sdktest.New(t)
	var calls []*pbv1.SuggestArgs
	h.StubHostCall("env.suggest", func(raw string) (string, error) {
		args := new(pbv1.SuggestArgs)
		if err := proto.Unmarshal([]byte(raw), args); err != nil {
			return "", err
		}
		if err := args.Validate(); err != nil {
			t.Fatal(err)
		}
		calls = append(calls, args)
		value, err := proto.Marshal(&pbv1.SuggestResult{SuggestionId: "sg_1"})
		return sdktest.HostResultValue(value), err
	})
	ladder := shadowLadder{Steps: []shadowStep{{ID: "strong", Model: "strong-model", Description: strings.Repeat("long\n", 200)}}}
	decision := policyDecision{Target: "strong", Class: "model_switch", RebuildUSD: .25, PerTurnDelta: -.02, PaybackTurns: 12.5}
	h.Run(func() {
		publishAdvice("session", "policy", ladder, shadowState{UserTurns: 2}, decision)
		publishAdvice("session", "policy", ladder, shadowState{UserTurns: 2}, decision)
	})
	if len(calls) != 2 || calls[0].DedupeKey != calls[1].DedupeKey || calls[0].GetHarnessTargetModel() != "strong-model" || calls[0].GetCostUsd() != .25 || !strings.Contains(calls[0].Body, "12.5 turns") {
		t.Fatalf("advice = %+v", calls)
	}
	if len(routes(h)) != 0 {
		t.Fatal("advice changed route")
	}
}

func TestAdviseAcceptsPolicyButNeverSuggestsOnContinuation(t *testing.T) {
	config := strings.Replace(shadowConfigJSON, `"mode":"shadow"`, `"mode":"advise"`, 1)
	if _, _, err := loadShadowPolicy(config); err != nil {
		t.Fatal(err)
	}
	h := sdktest.New(t).SetConfig(config)
	req := request("advice-session", "Fix tests")
	if res := h.BeforeRequest(req); res.Err != nil {
		t.Fatal(res.Err)
	}
	req.Messages = append(req.Messages, &pbv1.Message{Role: "tool", Blocks: []*pbv1.RequestBlock{textBlock("continuation")}})
	if res := h.BeforeRequest(req); res.Err != nil {
		t.Fatal(res.Err)
	}
	for _, call := range h.Calls() {
		if call.Command == "env.suggest" || call.Command == "env.route_request" {
			t.Fatalf("unexpected continuation action: %s", call.Command)
		}
	}
}
