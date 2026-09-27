package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/strictjson"
)

type sessionTotals struct {
	CompletedResponses int64 `json:"completed_responses"`
	ResponsesWithUsage int64 `json:"responses_with_usage"`
	InputTokens        int64 `json:"input_tokens"`
	OutputTokens       int64 `json:"output_tokens"`
	CacheReadTokens    int64 `json:"cache_read_tokens"`
	CacheWriteTokens   int64 `json:"cache_write_tokens"`
}

func sessionKey(conversation string) string {
	sum := sha256.Sum256([]byte("torana/usage-logger/session/v1\x00" + conversation))
	return "session/v1/" + hex.EncodeToString(sum[:])
}

func decodeSession(raw string) (sessionTotals, error) {
	var totals sessionTotals
	object, err := strictjson.DecodeObject([]byte(raw))
	if err != nil || object == nil || json.Unmarshal([]byte(raw), &totals) != nil || totals.CompletedResponses < 0 || totals.ResponsesWithUsage < 0 || totals.ResponsesWithUsage > totals.CompletedResponses || totals.InputTokens < 0 || totals.OutputTokens < 0 || totals.CacheReadTokens < 0 || totals.CacheWriteTokens < 0 {
		return sessionTotals{}, errors.New("usage_logger: invalid session totals")
	}
	return totals, nil
}

func addCount(total *int64, increment int64) error {
	if increment < 0 || *total > math.MaxInt64-increment {
		return errors.New("usage_logger: invalid or overflowing usage count")
	}
	*total += increment
	return nil
}

// The host calls this observational hook once for each completed response.
// A real retry is another billable response, not a duplicate to suppress.
func accumulateSession(ctx context.Context, response *pb.ChatResponse) error {
	execution := sdk.Execution(ctx)
	if response == nil || execution == nil || execution.GetConversationId() == "" {
		return nil
	}
	key := sessionKey(execution.GetConversationId())
	for attempt := 0; attempt < 3; attempt++ {
		previous, found, err := sdk.StateGetVersioned(key)
		if err != nil {
			return err
		}
		var totals sessionTotals
		var version *string
		if found {
			totals, err = decodeSession(previous.Value)
			if err != nil {
				return err
			}
			version = &previous.Version
		}
		if err := addCount(&totals.CompletedResponses, 1); err != nil {
			return err
		}
		if response.Usage != nil {
			for _, count := range []struct {
				target *int64
				value  int64
			}{
				{&totals.ResponsesWithUsage, 1}, {&totals.InputTokens, int64(response.Usage.InputTokens)},
				{&totals.OutputTokens, int64(response.Usage.OutputTokens)}, {&totals.CacheReadTokens, int64(response.Usage.CacheReadTokens)},
				{&totals.CacheWriteTokens, int64(response.Usage.CacheWriteTokens)},
			} {
				if err := addCount(count.target, count.value); err != nil {
					return err
				}
			}
		}
		raw, err := json.Marshal(totals)
		if err != nil {
			return err
		}
		result, err := sdk.StateCompareAndSet(key, string(raw), version)
		if err != nil {
			return err
		}
		if result.GetApplied() {
			return nil
		}
	}
	return errors.New("usage_logger: session totals changed concurrently; update was not recorded")
}

func sessionUsage(req *pb.HttpRequest) (sdk.HTTPResult, error) {
	binding, present, err := sdk.HTTPConversation(req)
	if err != nil {
		return sdk.PassHTTP(), err
	}
	if !present || !binding.Bound {
		return sdk.ServeHTTP(&pb.HttpResponse{Status: 409, HeadersJson: []byte(`{"Content-Type":["application/json"]}`), Body: []byte(`{"error":"unbound_conversation"}`)}), nil
	}
	raw, found, err := sdk.StateGet(sessionKey(binding.ConversationID))
	if err != nil {
		return sdk.PassHTTP(), err
	}
	var totals sessionTotals
	if found {
		totals, err = decodeSession(raw)
		if err != nil {
			return sdk.PassHTTP(), err
		}
	}
	body, err := json.Marshal(struct {
		sessionTotals
		Found         bool `json:"found"`
		UsageComplete bool `json:"usage_complete"`
	}{totals, found, totals.ResponsesWithUsage == totals.CompletedResponses})
	if err != nil {
		return sdk.PassHTTP(), err
	}
	return sdk.ServeHTTP(&pb.HttpResponse{Status: 200, HeadersJson: []byte(`{"Content-Type":["application/json"]}`), Body: body}), nil
}
