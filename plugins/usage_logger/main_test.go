package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
	"google.golang.org/protobuf/proto"
)

func TestRecordContainsOnlyOperationalFacts(t *testing.T) {
	r := recordFor(context.Background(), &pbv1.ChatResponse{Provider: "anthropic", Model: "claude", UpstreamStatus: 200, DurationMs: 17, CompletedAtUnixMs: 1_700_000_000_000, Usage: &pbv1.Usage{InputTokens: 9, OutputTokens: 3, CacheReadTokens: 7, CacheWriteTokens: 2}})
	if r.Provider != "anthropic" || r.Model != "claude" || r.Status != 200 || r.InputTokens != 9 || !r.UsageReported {
		t.Fatalf("record = %+v", r)
	}
	if r.Timestamp != time.UnixMilli(1_700_000_000_000).UTC().Format(time.RFC3339Nano) {
		t.Fatalf("timestamp = %q", r.Timestamp)
	}
}

func TestAfterResponseAppendsOneContentFreeRecord(t *testing.T) {
	h := sdktest.New(t)
	result := h.AfterResponse(&pbv1.ChatResponse{
		Provider:               "openai",
		Model:                  "gpt-test",
		UpstreamStatus:         200,
		DurationMs:             12,
		CompletedAtUnixMs:      1_700_000_000_000,
		Usage:                  &pbv1.Usage{InputTokens: 10, OutputTokens: 2, CacheReadTokens: 7},
		ProviderExtensionsJson: []byte(`{"must_not":"appear"}`),
	}, true)
	if result.Err != nil || !result.PassedThrough {
		t.Fatalf("AfterResponse = %+v", result)
	}
	calls := h.Calls()
	if len(calls) != 1 || calls[0].Command != "env.file_append" {
		t.Fatalf("calls = %+v", calls)
	}
	var args pbv1.FileAppendArgs
	if err := proto.Unmarshal([]byte(calls[0].Args), &args); err != nil {
		t.Fatal(err)
	}
	if args.Path != usagePath || len(args.Data) == 0 || args.Data[len(args.Data)-1] != '\n' {
		t.Fatalf("append path = %q, data length = %d", args.Path, len(args.Data))
	}
	var record map[string]json.RawMessage
	if err := json.Unmarshal(args.Data, &record); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"messages", "content", "headers", "provider_extensions_json", "must_not"} {
		if _, exists := record[forbidden]; exists {
			t.Fatalf("record exposed %q: %s", forbidden, args.Data)
		}
	}
}

func TestUsagePresenceIsNotInferredFromCounts(t *testing.T) {
	if recordFor(context.Background(), &pbv1.ChatResponse{}).UsageReported {
		t.Fatal("absent usage reported")
	}
	if !recordFor(context.Background(), &pbv1.ChatResponse{Usage: &pbv1.Usage{}}).UsageReported {
		t.Fatal("present zero usage lost")
	}
}
