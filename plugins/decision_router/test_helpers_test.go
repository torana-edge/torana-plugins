package main

import (
	"encoding/json"

	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
	"google.golang.org/protobuf/proto"
)

func textBlock(text string) *pbv1.RequestBlock {
	return &pbv1.RequestBlock{Kind: &pbv1.RequestBlock_Text{Text: &pbv1.RequestTextBlock{Text: text}}}
}

func request(conversation, text string) *pbv1.ChatRequest {
	return &pbv1.ChatRequest{
		Model: "original-model",
		Messages: []*pbv1.Message{
			{Role: "system", Blocks: []*pbv1.RequestBlock{textBlock("private historical system prompt")}},
			{Role: "user", Blocks: []*pbv1.RequestBlock{textBlock("old user turn must not be sent")}},
			{Role: "assistant", Blocks: []*pbv1.RequestBlock{
				textBlock("old answer must not be sent"),
				{Kind: &pbv1.RequestBlock_ToolUse{ToolUse: &pbv1.RequestToolUseBlock{Id: "call-1", Name: "read", ArgumentsJson: []byte(`{"canary":"private-tool-arguments-731"}`)}}},
			}},
			{Role: "tool", Blocks: []*pbv1.RequestBlock{{Kind: &pbv1.RequestBlock_ToolResult{ToolResult: &pbv1.RequestToolResultBlock{
				ToolCallId: "call-1", ToolName: "read", Content: []*pbv1.ToolResultContentBlock{{Kind: &pbv1.ToolResultContentBlock_Text{Text: &pbv1.ToolResultTextBlock{Text: "private-tool-result-839"}}}},
			}}}}},
			{Role: "user", Blocks: []*pbv1.RequestBlock{textBlock(text)}},
		},
		Tools:          []*pbv1.ToolDef{{Name: "read", ParametersJson: []byte(`{"type":"object","description":"private-tool-schema-427"}`)}, {Name: "shell", ParametersJson: []byte(`{"type":"object"}`)}},
		ToranaMetaJson: []byte(`{"_conversation_id":"` + conversation + `","_provider":"original"}`),
	}
}

func response(choice string, confidence float64) []byte {
	raw, _ := json.Marshal(map[string]any{"model": "jev-1.13", "answers": map[string]any{"route": map[string]any{"type": "choice", "choice": choice, "confidence": confidence}}})
	return raw
}

func stubDecision(h *sdktest.Harness, status int32, body []byte, capture func(*pbv1.OutboundHTTPRequestArgs)) {
	h.StubHostCall("env.http_request", func(args string) (string, error) {
		var request pbv1.OutboundHTTPRequestArgs
		if err := proto.Unmarshal([]byte(args), &request); err != nil {
			return "", err
		}
		if capture != nil {
			capture(&request)
		}
		raw, err := proto.Marshal(&pbv1.OutboundHTTPResponse{Status: status, Body: body})
		if err != nil {
			return "", err
		}
		return sdktest.HostResultValue(raw), nil
	})
}

func routes(h *sdktest.Harness) []*pbv1.RouteRequestArgs {
	var out []*pbv1.RouteRequestArgs
	for _, call := range h.Calls() {
		if call.Command != "env.route_request" {
			continue
		}
		var route pbv1.RouteRequestArgs
		if proto.Unmarshal([]byte(call.Args), &route) == nil {
			out = append(out, &route)
		}
	}
	return out
}
