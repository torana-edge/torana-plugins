package main

import (
	"encoding/json"
	"testing"

	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
)

func TestResponseUsageUpdatesOnlyItsRequestThread(t *testing.T) {
	h := sdktest.New(t).SetConfig(shadowConfigJSON)
	req := request("usage-session", "Fix this")
	cycle := h.NewRequest()
	if result := cycle.BeforeRequest(req); result.Err != nil {
		t.Fatal(result.Err)
	}
	if result := cycle.AfterResponse(&pbv1.ChatResponse{Usage: &pbv1.Usage{InputTokens: 60000, CacheReadTokens: 50000, OutputTokens: 1000}, FinishReason: "length"}, false); result.Err != nil {
		t.Fatal(result.Err)
	}
	var state shadowState
	raw, found := h.State(shadowStateKey("usage-session", req))
	if !found || json.Unmarshal([]byte(raw), &state) != nil {
		t.Fatalf("state missing: %v", found)
	}
	if state.ContextTokens != 60000 || state.AvgOutputTokens != 1000 || state.MaxTokensFinishes != 1 {
		t.Fatalf("response signals = %+v", state)
	}
	// A response with no paired before-request hook must not alter this thread.
	if result := h.AfterResponse(&pbv1.ChatResponse{Usage: &pbv1.Usage{InputTokens: 999999}}, false); result.Err != nil {
		t.Fatal(result.Err)
	}
	raw, _ = h.State(shadowStateKey("usage-session", req))
	var later shadowState
	if err := json.Unmarshal([]byte(raw), &later); err != nil {
		t.Fatal(err)
	}
	if later.ContextTokens != 60000 {
		t.Fatalf("unpaired response updated state: %+v", later)
	}
}
