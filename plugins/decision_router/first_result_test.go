package main

import (
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
	"strings"
	"testing"
)

func TestFirstResultAfterEmptyBaselineIsFresh(t *testing.T) {
	h := sdktest.New(t).SetConfig(strings.Replace(shadowConfigJSON, `"mode":"shadow"`, `"mode":"auto"`, 1))
	req := request("first-result", "Fix tests")
	req.Messages = []*pbv1.Message{req.Messages[0], req.Messages[4]}
	if res := h.BeforeRequest(req); res.Err != nil {
		t.Fatal(res.Err)
	}
	failed := true
	req.Messages = append(req.Messages, &pbv1.Message{Role: "tool", Blocks: []*pbv1.RequestBlock{{Kind: &pbv1.RequestBlock_ToolResult{ToolResult: &pbv1.RequestToolResultBlock{ToolCallId: "first-failure", ToolName: "shell", IsError: &failed, Content: []*pbv1.ToolResultContentBlock{{Kind: &pbv1.ToolResultContentBlock_Text{Text: &pbv1.ToolResultTextBlock{Text: "test failed"}}}}}}}}})
	if res := h.BeforeRequest(req); res.Err != nil {
		t.Fatal(res.Err)
	}
	if len(routes(h)) != 0 {
		t.Fatal("moved during continuation")
	}
	req.Messages = append(req.Messages, &pbv1.Message{Role: "user", Blocks: []*pbv1.RequestBlock{textBlock("Try again")}})
	if res := h.BeforeRequest(req); res.Err != nil {
		t.Fatal(res.Err)
	}
	if got := routes(h); len(got) != 1 || got[0].Model != "strong-model" {
		t.Fatalf("first fresh failure ignored: %+v", got)
	}
}
