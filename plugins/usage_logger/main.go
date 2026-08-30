// usage_logger writes one content-free JSON line for every completed request.
// The file is plugin-private and Torana owns its size limit and rotation.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

const usagePath = "usage.jsonl"

func main() {}

type usageRecord struct {
	Timestamp        string `json:"timestamp"`
	RequestID        string `json:"request_id"`
	Provider         string `json:"provider"`
	Model            string `json:"model"`
	Status           int32  `json:"status"`
	DurationMS       int64  `json:"duration_ms"`
	InputTokens      int32  `json:"input_tokens"`
	OutputTokens     int32  `json:"output_tokens"`
	CacheReadTokens  int32  `json:"cache_read_tokens"`
	CacheWriteTokens int32  `json:"cache_write_tokens"`
	UsageReported    bool   `json:"usage_reported"`
}

func recordFor(ctx context.Context, response *pbv1.ChatResponse) usageRecord {
	record := usageRecord{RequestID: strconv.FormatUint(sdk.RequestID(ctx), 10)}
	if response == nil {
		return record
	}
	record.Timestamp = time.UnixMilli(response.CompletedAtUnixMs).UTC().Format(time.RFC3339Nano)
	record.Provider = response.Provider
	record.Model = response.Model
	record.Status = response.UpstreamStatus
	record.DurationMS = response.DurationMs
	if response.Usage != nil {
		record.UsageReported = true
		record.InputTokens = response.Usage.InputTokens
		record.OutputTokens = response.Usage.OutputTokens
		record.CacheReadTokens = response.Usage.CacheReadTokens
		record.CacheWriteTokens = response.Usage.CacheWriteTokens
	}
	return record
}

func init() {
	sdk.OnAfterResponse(func(ctx context.Context, response *pbv1.ChatResponse, mutable bool) (sdk.ResponseResult, error) {
		line, err := json.Marshal(recordFor(ctx, response))
		if err != nil {
			return sdk.PassResponse(), err
		}
		line = append(line, '\n')
		if refusal, err := sdk.AppendFile(usagePath, line); err != nil {
			return sdk.PassResponse(), err
		} else if refusal != nil {
			return sdk.PassResponse(), fmt.Errorf("usage log append refused: %s", refusal.Code)
		}
		return sdk.PassResponse(), nil
	})
}
