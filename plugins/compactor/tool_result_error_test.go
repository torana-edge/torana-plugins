package main

import (
	"google.golang.org/protobuf/proto"
	"strings"
	"testing"
)

func TestExplicitToolFailureMustStayExact(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(`{"tool_policies":[{"match":"read*","mode":"deterministic","first_pass":true}]}`)
	req := bigToolRequest(strings.Repeat("diagnostic evidence\n", 1000))
	req.Messages[3].Blocks[0].GetToolResult().IsError = proto.Bool(true)
	original := proto.Clone(req)
	originalBytes := len(toolText(t, req, 3))
	res := h.BeforeRequest(req)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if !res.PassedThrough {
		t.Fatalf("explicit is_error=true result was compacted from %d to %d bytes", originalBytes, len(toolText(t, res.Request, 3)))
	}
	if !proto.Equal(req, original) {
		t.Fatal("failure request mutated")
	}
	for _, call := range h.Calls() {
		if strings.Contains(call.Command, "cache") || strings.Contains(call.Command, "model_complete") || strings.Contains(call.Command, "saving") || strings.Contains(call.Command, "metric") {
			t.Fatalf("explicit failure caused side effect: %s", call.Command)
		}
	}
}
