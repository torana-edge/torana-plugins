package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/strictjson"
)

const conversationReadLimit = 32

type threadSummary struct {
	ID            string `json:"thread_id"`
	UserTurns     int    `json:"user_turns"`
	ContextTokens int64  `json:"context_tokens"`
	ModelSwitches int    `json:"model_switches"`
	OffLadder     bool   `json:"off_ladder"`
	ActiveRoute   bool   `json:"active_route"`
}

func conversationRead(req *pb.HttpRequest) (sdk.HTTPResult, error) {
	binding, present, err := sdk.HTTPConversation(req)
	if err != nil {
		return sdk.PassHTTP(), err
	}
	if !present || !binding.Bound {
		return sdk.ServeHTTP(&pb.HttpResponse{Status: 409, HeadersJson: []byte(`{"Content-Type":["application/json"]}`), Body: []byte(`{"error":"unbound_conversation"}`)}), nil
	}
	prefix := shadowSessionPrefix(binding.ConversationID)
	page, err := sdk.StateScan(prefix, "", conversationReadLimit)
	if err != nil {
		return sdk.PassHTTP(), err
	}
	if len(page.Entries) > conversationReadLimit {
		return sdk.PassHTTP(), fmt.Errorf("router: oversized session state page")
	}
	threads := make([]threadSummary, 0, len(page.Entries))
	for _, entry := range page.Entries {
		if entry == nil || entry.Value == nil || !strings.HasPrefix(entry.Key, prefix) {
			return sdk.PassHTTP(), fmt.Errorf("router: invalid session state page")
		}
		leaf := strings.TrimPrefix(entry.Key, prefix)
		decoded, err := hex.DecodeString(leaf)
		if err != nil || len(decoded) != 32 || len(leaf) != 64 {
			return sdk.PassHTTP(), fmt.Errorf("router: invalid thread key")
		}
		var state shadowState
		object, decodeErr := strictjson.DecodeObject([]byte(entry.Value.Value))
		if decodeErr != nil || object == nil || json.Unmarshal([]byte(entry.Value.Value), &state) != nil || state.PolicyHash == "" || state.UserTurns < 0 || state.ContextTokens < 0 || state.ModelSwitches < 0 {
			return sdk.PassHTTP(), fmt.Errorf("router: invalid thread state")
		}
		threads = append(threads, threadSummary{ID: leaf, UserTurns: state.UserTurns, ContextTokens: state.ContextTokens, ModelSwitches: state.ModelSwitches, OffLadder: state.OffLadder, ActiveRoute: state.ActiveRoute != ""})
	}
	body, err := json.Marshal(struct {
		Scope   string          `json:"scope"`
		Threads []threadSummary `json:"threads"`
		HasMore bool            `json:"has_more"`
	}{"session", threads, page.NextCursor != ""})
	if err != nil {
		return sdk.PassHTTP(), err
	}
	return sdk.ServeHTTP(&pb.HttpResponse{Status: 200, HeadersJson: []byte(`{"Content-Type":["application/json"]}`), Body: body}), nil
}
