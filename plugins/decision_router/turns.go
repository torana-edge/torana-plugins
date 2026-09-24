package main

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func conversationID(req *pbv1.ChatRequest) string {
	if req == nil || len(req.ToranaMetaJson) == 0 {
		return ""
	}
	var meta struct {
		ConversationID string `json:"_conversation_id"`
	}
	if json.Unmarshal(req.ToranaMetaJson, &meta) != nil {
		return ""
	}
	return meta.ConversationID
}

func latestUserText(req *pbv1.ChatRequest) string {
	if req == nil {
		return ""
	}
	for i := len(req.Messages) - 1; i >= 0; i-- {
		message := req.Messages[i]
		if message == nil || message.Role != "user" {
			continue
		}
		var parts []string
		for _, block := range message.Blocks {
			if block != nil && block.GetText() != nil {
				parts = append(parts, block.GetText().Text)
			}
		}
		if joined := strings.Join(parts, "\n"); strings.TrimSpace(joined) != "" {
			return joined
		}
	}
	return ""
}

func truncateUTF8(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}
