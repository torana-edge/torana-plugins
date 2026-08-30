package main

import (
	"context"
	"testing"
	"time"

	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
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

func TestUsagePresenceIsNotInferredFromCounts(t *testing.T) {
	if recordFor(context.Background(), &pbv1.ChatResponse{}).UsageReported {
		t.Fatal("absent usage reported")
	}
	if !recordFor(context.Background(), &pbv1.ChatResponse{Usage: &pbv1.Usage{}}).UsageReported {
		t.Fatal("present zero usage lost")
	}
}
