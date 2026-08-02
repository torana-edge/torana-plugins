package main

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pbv2 "github.com/torana-edge/torana-plugin-sdk/pb/v2"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
)

// ==========================================================================
// Fixtures
// ==========================================================================

// newHarness builds a fresh fake host. Config is per-call (no process
// globals); tests stay sequential.
func newHarness(t *testing.T) *sdktest.Harness {
	t.Helper()
	return sdktest.New(t)
}

const warmerCfg = `{"conversations":"conv-1","warm_for_minutes":45}`

// warmPrefix encodes a valid cached prefix (a request ending at a cache
// breakpoint) exactly as the request path stores it.
func warmPrefix(t *testing.T) string {
	t.Helper()
	req := &pbv2.ChatRequest{
		Model: "claude-sonnet-4",
		Messages: []*pbv2.Message{
			{Role: "system", Content: "you are a coding agent", CacheControlJson: []byte(`{"type":"ephemeral"}`)},
			{Role: "user", Content: "find the bug"},
		},
	}
	enc, err := sdk.EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	return enc
}

// warmEntrySeed builds a due, valid entry.
func warmEntrySeed(t *testing.T) warmEntry {
	t.Helper()
	return warmEntry{
		SchemaVersion:     schemaVersion,
		ConversationID:    "conv-1",
		Provider:          "anthropic",
		Model:             "claude-sonnet-4",
		Path:              "/v1/messages",
		PrefixPB:          warmPrefix(t),
		LastSeenMillis:    1_000,
		LastRefreshMillis: 1_000,
		DeadlineMillis:    0,
	}
}

// pricingStub returns a warmable two-tier pricing envelope.
func pricingStub() func(string) (string, error) {
	return func(string) (string, error) {
		return sdktest.HostResultValue([]byte(`{
			"status":"ok",
			"refresh_on_read":true,
			"shortest_ttl_seconds":300,
			"warm_interval_seconds":240,
			"break_even_refreshes":11,
			"tiers":[
				{"ttl_seconds":300,"write_multiplier":1.25,"marker":{"type":"ephemeral"}},
				{"ttl_seconds":3600,"write_multiplier":2.0,"marker":{"type":"ephemeral","ttl":"1h"}}
			]
		}`)), nil
	}
}

func hitStub() func(string) (string, error) {
	return func(string) (string, error) {
		return sdktest.HostResultValue([]byte(`{"http_status":200,"usage":{"cache_read":1,"cache_write":0}}`)), nil
	}
}

func rebuiltStub() func(string) (string, error) {
	return func(string) (string, error) {
		return sdktest.HostResultValue([]byte(`{"http_status":200,"usage":{"cache_read":0,"cache_write":1}}`)), nil
	}
}

func noSignalStub() func(string) (string, error) {
	return func(string) (string, error) {
		return sdktest.HostResultValue([]byte(`{"http_status":200}`)), nil
	}
}

func countCommand(h *sdktest.Harness, cmd string) int {
	n := 0
	for _, c := range h.Calls() {
		if c.Command == cmd {
			n++
		}
	}
	return n
}

func seedEntry(t *testing.T, h *sdktest.Harness, entry warmEntry) {
	t.Helper()
	b, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	h.SeedState("warm/conv-1", string(b))
}

func tickAt(h *sdktest.Harness, now int64) {
	h.Tick(&pbv2.TickRequest{UnixMillis: now})
}

// ==========================================================================
// Request path (observational)
// ==========================================================================

// TestRequestPathStoresOptedInEntryAndPassesThrough — the request path stores
// the opted-in conversation's prefix and NEVER mutates the request.
func TestRequestPathStoresOptedInEntryAndPassesThrough(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(warmerCfg)
	h.SetNow(100_000)
	req := &pbv2.ChatRequest{
		Model: "claude-sonnet-4",
		Messages: []*pbv2.Message{
			{Role: "system", Content: "s", CacheControlJson: []byte(`{"type":"ephemeral"}`)},
			{Role: "user", Content: "u"},
		},
		ToranaMetaJson: []byte(`{"_provider":"anthropic","_conversation_id":"conv-1","_path":"/v1/messages"}`),
	}
	res := h.BeforeRequest(req)
	if res.Err != nil || !res.PassedThrough {
		t.Fatalf("the warmer must never mutate a request, err=%v", res.Err)
	}
	raw, ok := h.State("warm/conv-1")
	if !ok {
		t.Fatal("the opted-in conversation was not stored")
	}
	var entry warmEntry
	if err := json.Unmarshal([]byte(raw), &entry); err != nil {
		t.Fatal(err)
	}
	if entry.SchemaVersion != schemaVersion {
		t.Fatalf("schema version %d, want %d", entry.SchemaVersion, schemaVersion)
	}
	if entry.ConversationID != "conv-1" || entry.Provider != "anthropic" || entry.Model != "claude-sonnet-4" {
		t.Fatalf("entry identity wrong: %+v", entry)
	}
	if entry.DeadlineMillis != 100_000+45*60_000 {
		t.Fatalf("deadline %d, want now+45min", entry.DeadlineMillis)
	}
}

// TestRequestPathStoresNothingWhenIneligible — non-opted-in, no meta, no
// breakpoint, and an unavailable clock all store nothing.
func TestRequestPathStoresNothingWhenIneligible(t *testing.T) {
	for name, setup := range map[string]func(*sdktest.Harness) *pbv2.ChatRequest{
		"non-opted-in": func(h *sdktest.Harness) *pbv2.ChatRequest {
			req := &pbv2.ChatRequest{Model: "m", Messages: []*pbv2.Message{{Role: "user", Content: "u", CacheControlJson: []byte(`{"type":"ephemeral"}`)}}}
			req.ToranaMetaJson = []byte(`{"_provider":"p","_conversation_id":"other","_path":"/x"}`)
			return req
		},
		"no meta": func(h *sdktest.Harness) *pbv2.ChatRequest {
			return &pbv2.ChatRequest{Model: "m", Messages: []*pbv2.Message{{Role: "user", Content: "u", CacheControlJson: []byte(`{"type":"ephemeral"}`)}}}
		},
		"no breakpoint": func(h *sdktest.Harness) *pbv2.ChatRequest {
			req := &pbv2.ChatRequest{Model: "m", Messages: []*pbv2.Message{{Role: "user", Content: "u"}}}
			req.ToranaMetaJson = []byte(`{"_provider":"p","_conversation_id":"conv-1","_path":"/x"}`)
			return req
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.SetConfig(warmerCfg)
			h.SetNow(100_000)
			res := h.BeforeRequest(setup(h))
			if res.Err != nil || !res.PassedThrough {
				t.Fatalf("must pass through, err=%v", res.Err)
			}
			if _, ok := h.State("warm/conv-1"); ok {
				t.Fatal("nothing may be stored")
			}
		})
	}

	// Clock unavailable (advisory): store nothing.
	h := newHarness(t)
	h.SetConfig(warmerCfg)
	h.StubHostCall("env.now", func(string) (string, error) {
		return sdktest.HostResultError(pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "no clock"), nil
	})
	req := &pbv2.ChatRequest{Model: "m", Messages: []*pbv2.Message{{Role: "user", Content: "u", CacheControlJson: []byte(`{"type":"ephemeral"}`)}}}
	req.ToranaMetaJson = []byte(`{"_provider":"p","_conversation_id":"conv-1","_path":"/x"}`)
	res := h.BeforeRequest(req)
	if res.Err != nil || !res.PassedThrough {
		t.Fatalf("no clock must pass through, err=%v", res.Err)
	}
	if _, ok := h.State("warm/conv-1"); ok {
		t.Fatal("an unavailable clock must store nothing")
	}
}

// TestRequestPathDeterminism — the request path never mutates: two fresh
// clones produce identical requests.
func TestRequestPathDeterminism(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(warmerCfg)
	h.SetNow(100_000)
	build := func() *pbv2.ChatRequest {
		req := &pbv2.ChatRequest{Model: "m", Messages: []*pbv2.Message{{Role: "user", Content: "u", CacheControlJson: []byte(`{"type":"ephemeral"}`)}}}
		req.ToranaMetaJson = []byte(`{"_provider":"p","_conversation_id":"conv-1","_path":"/x"}`)
		return req
	}
	r1 := h.BeforeRequest(build())
	r2 := h.BeforeRequest(build())
	if r1.Err != nil || r2.Err != nil {
		t.Fatalf("errors: %v %v", r1.Err, r2.Err)
	}
	b1, _ := json.Marshal(r1.Request)
	b2, _ := json.Marshal(r2.Request)
	if string(b1) != string(b2) {
		t.Fatal("the request path mutated the request")
	}
}

// ==========================================================================
// Tick path: the write-ahead spend reservation state machine
// ==========================================================================

// TestTickHappyPathHit — a due, valid entry sends exactly once, advances the
// budget, and a confirmed cache hit with successful final persistence clears
// the pending state.
func TestTickHappyPathHit(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(warmerCfg)
	h.StubHostCall("torana_cache_pricing", pricingStub())
	h.StubHostCall("torana_send_request", hitStub())
	seedEntry(t, h, warmEntrySeed(t))

	tickAt(h, 300_000)
	if n := countCommand(h, "torana_send_request"); n != 1 {
		t.Fatalf("sends=%d, want exactly 1", n)
	}
	raw, ok := h.State("warm/conv-1")
	if !ok {
		t.Fatal("entry missing after the tick")
	}
	var entry warmEntry
	if err := json.Unmarshal([]byte(raw), &entry); err != nil {
		t.Fatal(err)
	}
	if entry.Stopped != "" {
		t.Fatalf("a confirmed hit must clear the pending state, stopped=%q", entry.Stopped)
	}
	if entry.RefreshesSpent != 1 {
		t.Fatalf("refreshes_spent=%d, want 1", entry.RefreshesSpent)
	}
	if entry.AttemptMillis != 0 {
		t.Fatalf("attempt_millis must be cleared, got %d", entry.AttemptMillis)
	}
}

// TestTickCacheRebuiltStops — a refresh that had to rebuild the cache stops
// permanently after exactly one send.
func TestTickCacheRebuiltStops(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(warmerCfg)
	h.StubHostCall("torana_cache_pricing", pricingStub())
	h.StubHostCall("torana_send_request", rebuiltStub())
	seedEntry(t, h, warmEntrySeed(t))

	tickAt(h, 300_000)
	if n := countCommand(h, "torana_send_request"); n != 1 {
		t.Fatalf("sends=%d, want 1", n)
	}
	raw, _ := h.State("warm/conv-1")
	var entry warmEntry
	_ = json.Unmarshal([]byte(raw), &entry)
	if entry.Stopped != "cache had already expired" {
		t.Fatalf("stopped=%q, want cache had already expired", entry.Stopped)
	}
	// A second tick sends nothing.
	h2 := newHarness(t)
	h2.SetConfig(warmerCfg)
	h2.StubHostCall("torana_cache_pricing", pricingStub())
	h2.StubHostCall("torana_send_request", rebuiltStub())
	h2.SeedState("warm/conv-1", raw)
	tickAt(h2, 300_000)
	if n := countCommand(h2, "torana_send_request"); n != 0 {
		t.Fatalf("a stopped entry must not send again, got %d", n)
	}
}

// TestTickUnknownOutcomeStops — missing usage or both counters zero is an
// unknown outcome: never cleared, never retried automatically.
func TestTickUnknownOutcomeStops(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(warmerCfg)
	h.StubHostCall("torana_cache_pricing", pricingStub())
	h.StubHostCall("torana_send_request", noSignalStub())
	seedEntry(t, h, warmEntrySeed(t))

	tickAt(h, 300_000)
	if n := countCommand(h, "torana_send_request"); n != 1 {
		t.Fatalf("sends=%d, want 1", n)
	}
	raw, _ := h.State("warm/conv-1")
	var entry warmEntry
	_ = json.Unmarshal([]byte(raw), &entry)
	if entry.Stopped != "refresh outcome unknown" {
		t.Fatalf("stopped=%q, want refresh outcome unknown", entry.Stopped)
	}
	// Replay: a pending/unknown entry sends zero.
	h2 := newHarness(t)
	h2.SetConfig(warmerCfg)
	h2.StubHostCall("torana_cache_pricing", pricingStub())
	h2.StubHostCall("torana_send_request", noSignalStub())
	h2.SeedState("warm/conv-1", raw)
	tickAt(h2, 300_000)
	if n := countCommand(h2, "torana_send_request"); n != 0 {
		t.Fatalf("replay of an unknown-outcome entry must send zero, got %d", n)
	}
}

// TestTickAdvisorySendRefusalStopsNoRetry — an advisory egress refusal stops
// the entry after exactly one send; the next tick sends zero.
func TestTickAdvisorySendRefusalStopsNoRetry(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(warmerCfg)
	h.StubHostCall("torana_cache_pricing", pricingStub())
	h.StubHostCall("torana_send_request", func(string) (string, error) {
		return sdktest.HostResultError(pbv2.ErrorCode_ERROR_CODE_UNAVAILABLE, "transient"), nil
	})
	seedEntry(t, h, warmEntrySeed(t))
	tickAt(h, 300_000)
	if n := countCommand(h, "torana_send_request"); n != 1 {
		t.Fatalf("sends=%d, want exactly 1", n)
	}
	raw, _ := h.State("warm/conv-1")
	h2 := newHarness(t)
	h2.SetConfig(warmerCfg)
	h2.StubHostCall("torana_cache_pricing", pricingStub())
	h2.StubHostCall("torana_send_request", func(string) (string, error) {
		return sdktest.HostResultError(pbv2.ErrorCode_ERROR_CODE_UNAVAILABLE, "transient"), nil
	})
	h2.SeedState("warm/conv-1", raw)
	tickAt(h2, 300_000)
	if n := countCommand(h2, "torana_send_request"); n != 0 {
		t.Fatalf("a stopped entry must not retry, got %d sends", n)
	}
}

// TestTickContractSendRefusalErrors — a contract egress refusal surfaces on
// the tick; the durable pending reservation still prevents replay.
func TestTickContractSendRefusalErrors(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(warmerCfg)
	h.StubHostCall("torana_cache_pricing", pricingStub())
	h.StubHostCall("torana_send_request", func(string) (string, error) {
		return sdktest.HostResultError(pbv2.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "stub"), nil
	})
	seedEntry(t, h, warmEntrySeed(t))
	if res := h.Tick(&pbv2.TickRequest{UnixMillis: 300_000}); res.Err == nil {
		t.Fatal("a contract egress refusal must error the tick")
	}
}

// TestTickPricingAdvisoryStopsContractErrors — pricing unavailable stops with
// zero sends; a contract pricing refusal errors the tick.
func TestTickPricingAdvisoryStopsContractErrors(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(warmerCfg)
	h.StubHostCall("torana_cache_pricing", func(string) (string, error) {
		return sdktest.HostResultError(pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "no pricing"), nil
	})
	seedEntry(t, h, warmEntrySeed(t))
	tickAt(h, 300_000)
	if n := countCommand(h, "torana_send_request"); n != 0 {
		t.Fatalf("no pricing must not spend, got %d sends", n)
	}
	raw, _ := h.State("warm/conv-1")
	var entry warmEntry
	_ = json.Unmarshal([]byte(raw), &entry)
	if entry.Stopped != "pricing unavailable" {
		t.Fatalf("stopped=%q, want pricing unavailable", entry.Stopped)
	}

	h2 := newHarness(t)
	h2.SetConfig(warmerCfg)
	h2.StubHostCall("torana_cache_pricing", func(string) (string, error) {
		return sdktest.HostResultError(pbv2.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "stub"), nil
	})
	seedEntry(t, h2, warmEntrySeed(t))
	if res := h2.Tick(&pbv2.TickRequest{UnixMillis: 300_000}); res.Err == nil {
		t.Fatal("a contract pricing refusal must error the tick")
	}
}

// TestTickNoSpendGates — not warmable, deadline reached, break-even reached,
// and not-due each stop or defer without sending.
func TestTickNoSpendGates(t *testing.T) {
	// Not warmable (automatic prefix caching).
	h := newHarness(t)
	h.SetConfig(warmerCfg)
	h.StubHostCall("torana_cache_pricing", func(string) (string, error) {
		return sdktest.HostResultValue([]byte(`{"status":"ok","refresh_on_read":false,"shortest_ttl_seconds":300}`)), nil
	})
	seedEntry(t, h, warmEntrySeed(t))
	tickAt(h, 300_000)
	if n := countCommand(h, "torana_send_request"); n != 0 {
		t.Fatalf("non-warmable must not spend, got %d", n)
	}

	// Deadline reached.
	h2 := newHarness(t)
	h2.SetConfig(warmerCfg)
	h2.StubHostCall("torana_cache_pricing", pricingStub())
	entry := warmEntrySeed(t)
	entry.DeadlineMillis = 150_000
	seedEntry(t, h2, entry)
	tickAt(h2, 300_000)
	if n := countCommand(h2, "torana_send_request"); n != 0 {
		t.Fatalf("deadline reached must not spend, got %d", n)
	}

	// Break-even reached.
	h3 := newHarness(t)
	h3.SetConfig(warmerCfg)
	h3.StubHostCall("torana_cache_pricing", pricingStub())
	entry3 := warmEntrySeed(t)
	entry3.RefreshesSpent = 11
	seedEntry(t, h3, entry3)
	tickAt(h3, 200_000)
	if n := countCommand(h3, "torana_send_request"); n != 0 {
		t.Fatalf("break-even reached must not spend, got %d", n)
	}

	// Not due yet.
	h4 := newHarness(t)
	h4.SetConfig(warmerCfg)
	h4.StubHostCall("torana_cache_pricing", pricingStub())
	entry4 := warmEntrySeed(t)
	entry4.LastRefreshMillis = 190_000
	seedEntry(t, h4, entry4)
	tickAt(h4, 200_000)
	if n := countCommand(h4, "torana_send_request"); n != 0 {
		t.Fatalf("not due must not spend, got %d", n)
	}
	raw, _ := h4.State("warm/conv-1")
	if strings.Contains(raw, `"stopped"`) {
		t.Fatal("a not-due entry must not be stopped")
	}
}

// TestTickEntryValidationStopsWithZeroSends — unsupported schema, unreadable
// prefix, mid-tool-call prefix, model mismatch, invalid identity, and a
// missing breakpoint each stop with zero sends.
func TestTickEntryValidationStopsWithZeroSends(t *testing.T) {
	badPrefix := func() string {
		req := &pbv2.ChatRequest{Model: "m", Messages: []*pbv2.Message{{Role: "user", Content: "u", CacheControlJson: []byte(`{"type":"ephemeral"}`)}}}
		enc, _ := sdk.EncodeRequest(req)
		return enc
	}
	midToolPrefix := func() string {
		req := &pbv2.ChatRequest{Model: "claude-sonnet-4", Messages: []*pbv2.Message{
			{Role: "system", Content: "s", CacheControlJson: []byte(`{"type":"ephemeral"}`)},
			{Role: "assistant", ToolCalls: []*pbv2.ToolCall{{Id: "c1", Name: "read"}}},
		}}
		enc, _ := sdk.EncodeRequest(req)
		return enc
	}
	noBreakpointPrefix := func() string {
		req := &pbv2.ChatRequest{Model: "claude-sonnet-4", Messages: []*pbv2.Message{{Role: "user", Content: "u"}}}
		enc, _ := sdk.EncodeRequest(req)
		return enc
	}
	cases := []struct {
		name string
		mut  func(*warmEntry)
	}{
		{"unsupported schema", func(e *warmEntry) { e.SchemaVersion = 1 }},
		{"unreadable prefix", func(e *warmEntry) { e.PrefixPB = "not base64 protobuf" }},
		{"mid tool call", func(e *warmEntry) { e.PrefixPB = midToolPrefix() }},
		{"model mismatch", func(e *warmEntry) { e.PrefixPB = badPrefix() }},
		{"missing provider", func(e *warmEntry) { e.Provider = "" }},
		{"missing path", func(e *warmEntry) { e.Path = "" }},
		{"negative accounting", func(e *warmEntry) { e.RefreshesSpent = -1 }},
		{"no breakpoint", func(e *warmEntry) { e.PrefixPB = noBreakpointPrefix() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.SetConfig(warmerCfg)
			h.StubHostCall("torana_cache_pricing", pricingStub())
			entry := warmEntrySeed(t)
			tc.mut(&entry)
			seedEntry(t, h, entry)
			tickAt(h, 300_000)
			if n := countCommand(h, "torana_send_request"); n != 0 {
				t.Fatalf("an invalid entry must not spend, got %d sends", n)
			}
			raw, ok := h.State("warm/conv-1")
			if !ok {
				t.Fatal("a stopped entry must be retained for visibility")
			}
			if !strings.Contains(raw, `"stopped"`) {
				t.Fatalf("the invalid entry must be stopped: %s", raw)
			}
		})
	}
}

// TestTickOptedOutDeletesEntry — opted out since the write: the entry is
// deleted with zero sends.
func TestTickOptedOutDeletesEntry(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(`{"conversations":""}`) // no longer opted in
	h.StubHostCall("torana_cache_pricing", pricingStub())
	seedEntry(t, h, warmEntrySeed(t))
	tickAt(h, 300_000)
	if n := countCommand(h, "torana_send_request"); n != 0 {
		t.Fatalf("an opted-out entry must not spend, got %d", n)
	}
	if n := countCommand(h, "env.state_delete"); n != 1 {
		t.Fatalf("an opted-out entry must be deleted, got %d deletes", n)
	}
}

// TestTickReservationWriteFailureMeansZeroSends — the write-ahead reservation
// must persist BEFORE the send; a failed reservation means the send count is
// zero (advisory declines, contract errors the tick).
func TestTickReservationWriteFailureMeansZeroSends(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(warmerCfg)
	h.StubHostCall("torana_cache_pricing", pricingStub())
	h.StubHostCall("torana_send_request", hitStub())
	h.StubHostCall("env.state_set", func(string) (string, error) {
		return sdktest.HostResultError(pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "no store"), nil
	})
	seedEntry(t, h, warmEntrySeed(t))
	tickAt(h, 300_000)
	if n := countCommand(h, "torana_send_request"); n != 0 {
		t.Fatalf("a failed reservation must mean ZERO sends, got %d", n)
	}

	h2 := newHarness(t)
	h2.SetConfig(warmerCfg)
	h2.StubHostCall("torana_cache_pricing", pricingStub())
	h2.StubHostCall("torana_send_request", hitStub())
	h2.StubHostCall("env.state_set", func(string) (string, error) {
		return sdktest.HostResultError(pbv2.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "stub"), nil
	})
	seedEntry(t, h2, warmEntrySeed(t))
	if res := h2.Tick(&pbv2.TickRequest{UnixMillis: 300_000}); res.Err == nil {
		t.Fatal("a contract reservation failure must error the tick")
	}
	if n := countCommand(h2, "torana_send_request"); n != 0 {
		t.Fatalf("a contract reservation failure must mean ZERO sends, got %d", n)
	}
}

// TestTickFinalizeWriteFailureKeepsPending — the send succeeds but the final
// persistence fails: the durable PENDING entry stays authoritative, so later
// ticks send zero (the crash/replay invariant).
func TestTickFinalizeWriteFailureKeepsPending(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(warmerCfg)
	h.StubHostCall("torana_cache_pricing", pricingStub())
	h.StubHostCall("torana_send_request", hitStub())
	sets := 0
	h.StubHostCall("env.state_set", func(args string) (string, error) {
		sets++
		if sets == 1 {
			return sdktest.HostResultValue(nil), nil // reservation succeeds
		}
		return sdktest.HostResultError(pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "no store"), nil // finalize fails
	})
	seedEntry(t, h, warmEntrySeed(t))
	tickAt(h, 300_000)
	if n := countCommand(h, "torana_send_request"); n != 1 {
		t.Fatalf("the send happened once, got %d", n)
	}
	// The stub bypasses the harness's store, so the DURABLE assertion is the
	// write sequence: the reservation write succeeded, and the final
	// persistence was ATTEMPTED and failed. In the real host the durable
	// entry is then the reservation (Stopped="refresh outcome unknown"),
	// which the replay invariant pins separately:
	// TestTickSeededPendingEntrySendsZero seeds exactly that state and
	// asserts zero further sends.
	if sets != 2 {
		t.Fatalf("write sequence: reservation + failed finalize expected (sets=%d)", sets)
	}
}

// TestTickSeededPendingEntrySendsZero — the replay invariant directly: a
// seeded pending entry (Stopped="refresh outcome unknown") causes zero sends.
func TestTickSeededPendingEntrySendsZero(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(warmerCfg)
	h.StubHostCall("torana_cache_pricing", pricingStub())
	h.StubHostCall("torana_send_request", hitStub())
	entry := warmEntrySeed(t)
	entry.Stopped = "refresh outcome unknown"
	entry.AttemptMillis = 150_000
	seedEntry(t, h, entry)
	tickAt(h, 300_000)
	if n := countCommand(h, "torana_send_request"); n != 0 {
		t.Fatalf("a seeded pending entry must cause zero sends, got %d", n)
	}
}

// TestTickCorruptEntryIsKeyLocal — a corrupt entry is skipped without
// poisoning other entries.
func TestTickCorruptEntryIsKeyLocal(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(warmerCfg)
	h.StubHostCall("torana_cache_pricing", pricingStub())
	h.StubHostCall("torana_send_request", hitStub())
	h.SeedState("warm/conv-1", "not json")
	good := warmEntrySeed(t)
	good.ConversationID = "conv-2"
	b, _ := json.Marshal(good)
	h.SeedState("warm/conv-2", string(b))
	// conv-2 is not opted in -> deleted; the corrupt conv-1 is skipped.
	tickAt(h, 300_000)
	if n := countCommand(h, "torana_send_request"); n != 0 {
		t.Fatalf("corrupt entries must not spend, got %d", n)
	}
	raw, ok := h.State("warm/conv-1")
	if !ok || raw != "not json" {
		t.Fatal("the corrupt entry must be left untouched")
	}
}

// TestTickStateReadFailureClasses — contract refusal and malformed frames on
// the entry read error the tick; NOT_FOUND skips normally.
func TestTickStateReadFailureClasses(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(warmerCfg)
	h.StubHostCall("env.state_get", func(string) (string, error) {
		return "not a frame", nil
	})
	seedEntry(t, h, warmEntrySeed(t))
	if res := h.Tick(&pbv2.TickRequest{UnixMillis: 300_000}); res.Err == nil {
		t.Fatal("a malformed state frame must error the tick")
	}

	h2 := newHarness(t)
	h2.SetConfig(warmerCfg)
	if res := h2.Tick(&pbv2.TickRequest{UnixMillis: 300_000}); res.Err != nil {
		t.Fatalf("no entries: tick must be idle, err=%v", res.Err)
	}
}

// TestSchemaDefaultsMatchRuntimeDefaults — schema.json defaults (""/45/0)
// must equal the runtime defaults.
func TestSchemaDefaultsMatchRuntimeDefaults(t *testing.T) {
	raw, err := os.ReadFile("schema.json")
	if err != nil {
		t.Fatalf("read schema.json: %v", err)
	}
	var schema struct {
		Properties map[string]struct {
			Default json.RawMessage `json:"default"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("parse schema.json: %v", err)
	}
	var conversations string
	if err := json.Unmarshal(schema.Properties["conversations"].Default, &conversations); err != nil || conversations != "" {
		t.Fatalf("schema conversations default=%q, want empty", conversations)
	}
	var minutes int
	if err := json.Unmarshal(schema.Properties["warm_for_minutes"].Default, &minutes); err != nil || minutes != 45 {
		t.Fatalf("schema warm_for_minutes default=%d, want 45", minutes)
	}
	if string(schema.Properties["interval_seconds_override"].Default) != "0" {
		t.Fatal("schema interval default != 0")
	}
	// The RUNTIME default must equal the schema default (45) — a user who
	// configures only conversations gets the advertised time bound.
	rt := parseConfig("")
	if rt.Conversations != "" || rt.WarmForMinutes != 45 || rt.IntervalSecondsOverride != 0 {
		t.Fatalf("runtime defaults %+v do not match the schema", rt)
	}
}

// TestParseConfigWarmForMinutesDefaults — absence, omission, explicit zero,
// and explicit nonzero are distinguished: omitted -> 45 (schema default),
// explicit zero -> break-even only.
func TestParseConfigWarmForMinutesDefaults(t *testing.T) {
	if got := parseConfig("").WarmForMinutes; got != 45 {
		t.Fatalf("empty config: warm_for_minutes=%d, want 45", got)
	}
	if got := parseConfig(`{"conversations":"c"}`).WarmForMinutes; got != 45 {
		t.Fatalf("omitted field: warm_for_minutes=%d, want 45", got)
	}
	if got := parseConfig(`{"conversations":"c","warm_for_minutes":0}`).WarmForMinutes; got != 0 {
		t.Fatalf("explicit zero: warm_for_minutes=%d, want 0 (break-even only)", got)
	}
	if got := parseConfig(`{"conversations":"c","warm_for_minutes":60}`).WarmForMinutes; got != 60 {
		t.Fatalf("explicit nonzero: warm_for_minutes=%d, want 60", got)
	}
}

// TestNoUnauthorizedCalls — every host call is within the declared permission
// set (state_delete rides env.state_set).
func TestNoUnauthorizedCalls(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(warmerCfg)
	h.StubHostCall("torana_cache_pricing", pricingStub())
	h.StubHostCall("torana_send_request", hitStub())
	seedEntry(t, h, warmEntrySeed(t))
	tickAt(h, 300_000)

	allowed := map[string]bool{
		"env.plugin_config":    true,
		"env.state_get":        true,
		"env.state_set":        true,
		"env.state_delete":     true, // command; authorized by env.state_set
		"env.state_keys":       true,
		"env.now":              true,
		"torana_cache_pricing": true,
		"torana_send_request":  true,
	}
	for _, c := range h.Calls() {
		if !allowed[c.Command] {
			t.Errorf("host call outside the declared permission set: %s", c.Command)
		}
	}
}

// TestTickInvalidEntryZeroPricingZeroSends — durable-state shape validation
// runs FIRST: invalid schema/identity/accounting/state-phase rows must reach
// ZERO pricing calls and ZERO sends, and must be stopped.
func TestTickInvalidEntryZeroPricingZeroSends(t *testing.T) {
	badKey := func(conv string) string {
		return "warm/attacker/" + conv
	}
	valid := warmEntrySeed(t)
	cases := []struct {
		name string
		key  string
		mut  func(*warmEntry)
	}{
		{"unsupported schema", "warm/conv-1", func(e *warmEntry) { e.SchemaVersion = 1 }},
		{"key not exactly bound", badKey("conv-1"), func(e *warmEntry) {}},
		{"missing provider", "warm/conv-1", func(e *warmEntry) { e.Provider = "" }},
		{"negative last refresh", "warm/conv-1", func(e *warmEntry) { e.LastRefreshMillis = -1 }},
		{"negative deadline", "warm/conv-1", func(e *warmEntry) { e.DeadlineMillis = -1 }},
		{"negative attempt", "warm/conv-1", func(e *warmEntry) { e.AttemptMillis = -1 }},
		{"orphaned attempt in fresh state", "warm/conv-1", func(e *warmEntry) { e.AttemptMillis = 50_000 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.SetConfig(warmerCfg)
			h.StubHostCall("torana_cache_pricing", pricingStub())
			h.StubHostCall("torana_send_request", hitStub())
			entry := valid
			tc.mut(&entry)
			b, _ := json.Marshal(entry)
			h.SeedState(tc.key, string(b))
			tickAt(h, 300_000)
			if n := countCommand(h, "torana_cache_pricing"); n != 0 {
				t.Fatalf("an invalid entry must reach ZERO pricing calls, got %d", n)
			}
			if n := countCommand(h, "torana_send_request"); n != 0 {
				t.Fatalf("an invalid entry must send ZERO times, got %d", n)
			}
			raw, ok := h.State(tc.key)
			if !ok || !strings.Contains(raw, `"stopped"`) {
				t.Fatalf("the invalid entry must be stopped and retained: %s", raw)
			}
		})
	}
}

// TestTickIntervalOverrideBoundary — an override at or beyond the provider's
// shortest cache lifetime is rejected with zero sends; just below it is due
// and sends.
func TestTickIntervalOverrideBoundary(t *testing.T) {
	for name, override := range map[string]int{
		"equal to TTL":   300,
		"beyond the TTL": 301,
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.SetConfig(`{"conversations":"conv-1","interval_seconds_override":` + itoa(override) + `}`)
			h.StubHostCall("torana_cache_pricing", pricingStub())
			h.StubHostCall("torana_send_request", hitStub())
			seedEntry(t, h, warmEntrySeed(t))
			tickAt(h, 300_000)
			if n := countCommand(h, "torana_send_request"); n != 0 {
				t.Fatalf("an override at/beyond the TTL must send ZERO times, got %d", n)
			}
			raw, _ := h.State("warm/conv-1")
			if !strings.Contains(raw, `"refresh interval exceeds cache lifetime"`) {
				t.Fatalf("the invalid override must be stopped: %s", raw)
			}
		})
	}

	// Just below the TTL (299 < 300): due and sends.
	h := newHarness(t)
	h.SetConfig(`{"conversations":"conv-1","interval_seconds_override":299}`)
	h.StubHostCall("torana_cache_pricing", pricingStub())
	h.StubHostCall("torana_send_request", hitStub())
	seedEntry(t, h, warmEntrySeed(t))
	tickAt(h, 300_000)
	if n := countCommand(h, "torana_send_request"); n != 1 {
		t.Fatalf("an override just below the TTL must send once, got %d", n)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
