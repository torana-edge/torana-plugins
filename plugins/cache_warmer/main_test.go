package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pbv2 "github.com/torana-edge/torana-plugin-sdk/pb/v2"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
	"google.golang.org/protobuf/proto"
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

// warmRequest is a valid ordered-ABI request carrying a cache-breakpoint
// carrier (the last marker on the system message), exactly as the request
// path observes one.
func warmRequest() *pbv2.ChatRequest {
	return &pbv2.ChatRequest{
		Model: "claude-sonnet-4",
		Messages: []*pbv2.Message{
			{Role: "system", Blocks: []*pbv2.RequestBlock{
				{Kind: &pbv2.RequestBlock_Text{Text: &pbv2.RequestTextBlock{Text: "you are a coding agent"}}},
				{Kind: &pbv2.RequestBlock_CacheBreakpoint{CacheBreakpoint: &pbv2.RequestCacheBreakpoint{MarkerJson: []byte(`{"type":"ephemeral"}`)}}},
			}},
			{Role: "user", Blocks: []*pbv2.RequestBlock{{Kind: &pbv2.RequestBlock_Text{Text: &pbv2.RequestTextBlock{Text: "find the bug"}}}}},
		},
	}
}

// warmPrefix encodes the sanitized replay request exactly as the request
// path stores it.
func warmPrefix(t *testing.T) string {
	t.Helper()
	enc, err := sdk.EncodeRequest(warmRequest())
	if err != nil {
		t.Fatal(err)
	}
	return enc
}

// warmFingerprint is the domain-separated fingerprint of the SAME request,
// via the production helper (write and replay must agree).
func warmFingerprint(t *testing.T) string {
	t.Helper()
	prefix, _, err := pbv2.RequestObservablePrefix(warmRequest())
	if err != nil {
		t.Fatal(err)
	}
	return prefixFingerprint(prefix)
}

// textMsg builds an ordered message: text block, plus the cache-breakpoint
// carrier when marker is true.
func textMsg(role, text string, marker bool) *pbv2.Message {
	blocks := []*pbv2.RequestBlock{{Kind: &pbv2.RequestBlock_Text{Text: &pbv2.RequestTextBlock{Text: text}}}}
	if marker {
		blocks = append(blocks, &pbv2.RequestBlock{Kind: &pbv2.RequestBlock_CacheBreakpoint{CacheBreakpoint: &pbv2.RequestCacheBreakpoint{MarkerJson: []byte(`{"type":"ephemeral"}`)}}})
	}
	return &pbv2.Message{Role: role, Blocks: blocks}
}

// uReq is a minimal opted-in request (user message with a marker + meta).
func uReq() *pbv2.ChatRequest {
	req := &pbv2.ChatRequest{Model: "m", Messages: []*pbv2.Message{textMsg("user", "u", true)}}
	req.ToranaMetaJson = []byte(`{"_provider":"p","_conversation_id":"conv-1","_path":"/x"}`)
	return req
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
		PrefixFingerprint: warmFingerprint(t),
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
		Model:          "claude-sonnet-4",
		Messages:       []*pbv2.Message{textMsg("system", "s", true), textMsg("user", "u", false)},
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
			req := &pbv2.ChatRequest{Model: "m", Messages: []*pbv2.Message{textMsg("user", "u", true)}}
			req.ToranaMetaJson = []byte(`{"_provider":"p","_conversation_id":"other","_path":"/x"}`)
			return req
		},
		"no meta": func(h *sdktest.Harness) *pbv2.ChatRequest {
			return &pbv2.ChatRequest{Model: "m", Messages: []*pbv2.Message{textMsg("user", "u", true)}}
		},
		"no breakpoint": func(h *sdktest.Harness) *pbv2.ChatRequest {
			req := &pbv2.ChatRequest{Model: "m", Messages: []*pbv2.Message{textMsg("user", "u", false)}}
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
	req := &pbv2.ChatRequest{Model: "m", Messages: []*pbv2.Message{textMsg("user", "u", true)}}
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
		req := &pbv2.ChatRequest{Model: "m", Messages: []*pbv2.Message{textMsg("user", "u", true)}}
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
		req := &pbv2.ChatRequest{Model: "m", Messages: []*pbv2.Message{textMsg("user", "u", true)}}
		enc, _ := sdk.EncodeRequest(req)
		return enc
	}
	midToolPrefix := func() string {
		req := &pbv2.ChatRequest{Model: "claude-sonnet-4", Messages: []*pbv2.Message{
			textMsg("system", "s", true),
			{Role: "assistant", Blocks: []*pbv2.RequestBlock{{Kind: &pbv2.RequestBlock_ToolUse{ToolUse: &pbv2.RequestToolUseBlock{Id: "c1", Name: "read", ArgumentsJson: []byte(`{}`)}}}}},
		}}
		enc, _ := sdk.EncodeRequest(req)
		return enc
	}
	noBreakpointPrefix := func() string {
		req := &pbv2.ChatRequest{Model: "claude-sonnet-4", Messages: []*pbv2.Message{textMsg("user", "u", false)}}
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
		{"fingerprint drift", func(e *warmEntry) {
			// REAL drift: a provider-visible field BEFORE the marker changes
			// and the request is re-encoded, while the stored fingerprint is
			// the ORIGINAL (now wrong) one.
			req := warmRequest()
			req.Messages[0].Blocks[0].GetText().Text = "mutated before the marker"
			enc, _ := sdk.EncodeRequest(req)
			e.PrefixPB = enc
		}},
		{"missing fingerprint", func(e *warmEntry) { e.PrefixFingerprint = "" }},
		{"malformed fingerprint", func(e *warmEntry) { e.PrefixFingerprint = "not-a-valid-shape" }},
		{"out of domain replay", func(e *warmEntry) {
			bad := warmRequest()
			bad.Messages[0].Blocks = bad.Messages[0].Blocks[:0]
			enc, _ := sdk.EncodeRequest(bad)
			e.PrefixPB = enc
		}},
		{"v2 schema", func(e *warmEntry) { e.SchemaVersion = 2 }},
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
			if n := countCommand(h, "torana_cache_pricing"); n != 0 {
				t.Fatalf("an invalid entry reached pricing %d times", n)
			}
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
			// EXACT state_set sequence: exactly ONE durable write — the
			// permitted stop-reason persistence. The call is decoded and
			// must carry the terminal stop with NO attempt_millis (a pending
			// reservation would set it); a second/pending write fails.
			var stateSets []string
			for _, c := range h.Calls() {
				if c.Command == "env.state_set" {
					stateSets = append(stateSets, c.Args)
				}
			}
			if len(stateSets) != 1 {
				t.Fatalf("env.state_set calls = %d, want exactly 1 (stop-reason persistence only)", len(stateSets))
			}
			// The typed host call carries protobuf-encoded StateSetArgs.
			var setArgs pbv2.StateSetArgs
			if err := proto.Unmarshal([]byte(stateSets[0]), &setArgs); err != nil {
				t.Fatalf("state_set args not a StateSetArgs proto: %v", err)
			}
			if setArgs.Key != "warm/conv-1" || !strings.Contains(setArgs.Value, `"stopped"`) {
				t.Fatalf("the single write is not the stop persistence: %s", stateSets[0])
			}
			if strings.Contains(setArgs.Value, "attempt_millis") {
				t.Fatalf("an invalid entry must never reserve: %s", setArgs.Value)
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

// ==========================================================================
// Ordered-ABI port pins (checkpoint REV 3)
// ==========================================================================

// TestRequestPathSanitizedReplayAndFingerprint — the stored artifact is the
// SANITIZED replay request (stream=false, torana_meta_json cleared, every
// provider-visible field/block preserved), the fingerprint is the fixed
// domain-separated production digest, and the hook input is never mutated.
func TestRequestPathSanitizedReplayAndFingerprint(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(warmerCfg)
	h.SetNow(100_000)
	req := &pbv2.ChatRequest{
		Model:          "claude-sonnet-4",
		Stream:         true,
		Messages:       []*pbv2.Message{textMsg("system", "s", true), textMsg("user", "u", false)},
		ToranaMetaJson: []byte(`{"_provider":"anthropic","_conversation_id":"conv-1","_path":"/v1/messages","_request_headers":"x"}`),
	}
	before := proto.Clone(req).(*pbv2.ChatRequest)
	res := h.BeforeRequest(req)
	if res.Err != nil || !res.PassedThrough {
		t.Fatalf("must pass through, err=%v", res.Err)
	}
	if !proto.Equal(req, before) {
		t.Fatal("the request path mutated the hook input")
	}
	raw, ok := h.State("warm/conv-1")
	if !ok {
		t.Fatal("no entry stored")
	}
	var entry warmEntry
	if err := json.Unmarshal([]byte(raw), &entry); err != nil {
		t.Fatal(err)
	}
	// Decoded replay == the INDEPENDENT sanitized expected (clone with
	// stream=false + meta cleared; nothing else changes).
	decoded, err := sdk.DecodeRequest(entry.PrefixPB)
	if err != nil {
		t.Fatalf("decode replay: %v", err)
	}
	// INDEPENDENT expected: a direct clone with the two fields cleared by
	// hand — never the production sanitizeReplay (the oracle must not share
	// mutation code with the implementation).
	expected := proto.Clone(req).(*pbv2.ChatRequest)
	expected.Stream = false
	expected.ToranaMetaJson = nil
	if !proto.Equal(decoded, expected) {
		t.Fatalf("replay is not the sanitized request\n got: %v\nwant: %v", decoded, expected)
	}
	if decoded.Stream {
		t.Fatal("the replay must be non-streaming")
	}
	if len(decoded.ToranaMetaJson) != 0 {
		t.Fatalf("the replay must clear torana_meta_json, got %s", decoded.ToranaMetaJson)
	}
	// Fingerprint: the production helper over the projection of the ORIGINAL
	// request (stream/meta excluded by the projection itself).
	prefix, _, err := pbv2.RequestObservablePrefix(req)
	if err != nil {
		t.Fatal(err)
	}
	if entry.PrefixFingerprint != prefixFingerprint(prefix) {
		t.Fatalf("fingerprint %q, want %q", entry.PrefixFingerprint, prefixFingerprint(prefix))
	}
}

// TestRequestPathExactCallMultisets — for an opted-in malformed/out-of-domain
// request, an opted-in no-marker request, and an opted-in terminal-suffix
// request: EXACTLY ONE env.plugin_config call (torana_meta_json is local),
// nothing else, no clock/state, and exact input preservation.
func TestRequestPathExactCallMultisets(t *testing.T) {
	outOfDomain := uReq()
	outOfDomain.Messages[0].Blocks = outOfDomain.Messages[0].Blocks[:0]
	noMarker := uReq()
	noMarker.Messages = []*pbv2.Message{textMsg("user", "u", false)}
	terminalSuffix := &pbv2.ChatRequest{
		Model: "m",
		Messages: []*pbv2.Message{
			textMsg("user", "u", true),
			{Role: "assistant", Blocks: []*pbv2.RequestBlock{{Kind: &pbv2.RequestBlock_ToolUse{ToolUse: &pbv2.RequestToolUseBlock{Id: "c1", Name: "read", ArgumentsJson: []byte(`{}`)}}}}},
		},
	}
	terminalSuffix.ToranaMetaJson = []byte(`{"_provider":"p","_conversation_id":"conv-1","_path":"/x"}`)
	for _, row := range []struct {
		name string
		req  *pbv2.ChatRequest
	}{
		{"out of domain", outOfDomain},
		{"no marker", noMarker},
		{"terminal suffix", terminalSuffix},
	} {
		t.Run(row.name, func(t *testing.T) {
			h := newHarness(t)
			h.SetConfig(warmerCfg)
			h.SetNow(100_000)
			before := proto.Clone(row.req).(*pbv2.ChatRequest)
			res := h.BeforeRequest(row.req)
			if res.Err != nil || !res.PassedThrough {
				t.Fatalf("must pass, err=%v", res.Err)
			}
			if !proto.Equal(row.req, before) {
				t.Fatal("the decline mutated the request")
			}
			if _, ok := h.State("warm/conv-1"); ok {
				t.Fatal("nothing may be stored")
			}
			calls := h.Calls()
			if len(calls) != 1 || calls[0].Command != "env.plugin_config" {
				var got []string
				for _, c := range calls {
					got = append(got, c.Command)
				}
				t.Fatalf("call multiset = %v, want exactly [env.plugin_config]", got)
			}
		})
	}
}

// TestEndsWithUnansweredToolCallOrdered — the ordered terminal-shape pins:
// one call, multiple calls, text+call in both block orders are terminal; a
// later tool-result/user turn is the control (not terminal).
func TestEndsWithUnansweredToolCallOrdered(t *testing.T) {
	toolUse := func(id string) *pbv2.RequestBlock {
		return &pbv2.RequestBlock{Kind: &pbv2.RequestBlock_ToolUse{ToolUse: &pbv2.RequestToolUseBlock{Id: id, Name: "read", ArgumentsJson: []byte(`{}`)}}}
	}
	rows := []struct {
		name string
		req  *pbv2.ChatRequest
		want bool
	}{
		{"one call", &pbv2.ChatRequest{Messages: []*pbv2.Message{{Role: "assistant", Blocks: []*pbv2.RequestBlock{toolUse("c1")}}}}, true},
		{"multiple calls", &pbv2.ChatRequest{Messages: []*pbv2.Message{{Role: "assistant", Blocks: []*pbv2.RequestBlock{toolUse("c1"), toolUse("c2")}}}}, true},
		{"text then call", &pbv2.ChatRequest{Messages: []*pbv2.Message{{Role: "assistant", Blocks: []*pbv2.RequestBlock{textMsg("assistant", "t", false).Blocks[0], toolUse("c1")}}}}, true},
		{"call then text", &pbv2.ChatRequest{Messages: []*pbv2.Message{{Role: "assistant", Blocks: []*pbv2.RequestBlock{toolUse("c1"), textMsg("assistant", "t", false).Blocks[0]}}}}, true},
		{"control: later tool-result turn", &pbv2.ChatRequest{Messages: []*pbv2.Message{
			{Role: "assistant", Blocks: []*pbv2.RequestBlock{toolUse("c1")}},
			{Role: "tool", Blocks: []*pbv2.RequestBlock{{Kind: &pbv2.RequestBlock_ToolResult{ToolResult: &pbv2.RequestToolResultBlock{ToolCallId: "c1", Content: []*pbv2.ToolResultContentBlock{{Kind: &pbv2.ToolResultContentBlock_Text{Text: &pbv2.ToolResultTextBlock{Text: "ok"}}}}}}}}},
		}}, false},
		{"control: later user turn", &pbv2.ChatRequest{Messages: []*pbv2.Message{
			{Role: "assistant", Blocks: []*pbv2.RequestBlock{toolUse("c1")}},
			textMsg("user", "continue", false),
		}}, false},
		{"control: no messages", &pbv2.ChatRequest{}, false},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			if got := endsWithUnansweredToolCall(row.req); got != row.want {
				t.Fatalf("endsWithUnansweredToolCall = %v, want %v", got, row.want)
			}
		})
	}
}

// TestRequestPathValidNonTerminalSuffix — content after the last marker is a
// VALID suffix: the replay includes it (bytes change vs the marker-only
// request) while the fingerprint does NOT change (the cache identity did not
// move).
func TestRequestPathValidNonTerminalSuffix(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(warmerCfg)
	h.SetNow(100_000)
	withSuffix := &pbv2.ChatRequest{
		Model:          "m",
		Messages:       []*pbv2.Message{textMsg("user", "u", true), textMsg("user", "suffix", false)},
		ToranaMetaJson: []byte(`{"_provider":"p","_conversation_id":"conv-1","_path":"/x"}`),
	}
	res := h.BeforeRequest(withSuffix)
	if res.Err != nil {
		t.Fatalf("err=%v", res.Err)
	}
	raw, _ := h.State("warm/conv-1")
	var entry warmEntry
	_ = json.Unmarshal([]byte(raw), &entry)
	decoded, err := sdk.DecodeRequest(entry.PrefixPB)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Messages) != 2 {
		t.Fatalf("the replay must include the valid suffix, got %d messages", len(decoded.Messages))
	}
	// Fingerprint equals the marker-only projection (identity did not move).
	only, _, err := pbv2.RequestObservablePrefix(&pbv2.ChatRequest{Model: "m", Messages: []*pbv2.Message{textMsg("user", "u", true)}})
	if err != nil {
		t.Fatal(err)
	}
	if entry.PrefixFingerprint != prefixFingerprint(only) {
		t.Fatalf("suffix moved the fingerprint: %q != %q", entry.PrefixFingerprint, prefixFingerprint(only))
	}
}

// TestTickDriftStopsZeroPricingZeroSends — a seeded entry whose stored
// fingerprint does not match the replay's recomputed projection stops with
// zero pricing and zero sends; the entry is retained with the stop reason.
func TestTickDriftStopsZeroPricingZeroSends(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(warmerCfg)
	h.StubHostCall("torana_cache_pricing", pricingStub())
	entry := warmEntrySeed(t)
	entry.PrefixFingerprint = "not-the-replay"
	seedEntry(t, h, entry)
	tickAt(h, 300_000)
	if n := countCommand(h, "torana_cache_pricing"); n != 0 {
		t.Fatalf("pricing called %d times on a drifted entry", n)
	}
	if n := countCommand(h, "torana_send_request"); n != 0 {
		t.Fatalf("a drifted entry sent %d times", n)
	}
	raw, ok := h.State("warm/conv-1")
	if !ok || !strings.Contains(raw, `"stored prefix drifted"`) {
		t.Fatalf("the drifted entry must be retained with the drift stop: %s", raw)
	}
}

// TestTickSeededPendingNeverRetries — the crash/pending invariant: a durable
// pending entry (Stopped="refresh outcome unknown", AttemptMillis set) sends
// ZERO on later ticks, forever, with zero pricing — only a new real observed
// turn replaces it.
func TestTickSeededPendingNeverRetries(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(warmerCfg)
	h.StubHostCall("torana_cache_pricing", pricingStub())
	entry := warmEntrySeed(t)
	entry.Stopped = "refresh outcome unknown"
	entry.AttemptMillis = 250_000
	seedEntry(t, h, entry)
	for _, now := range []int64{300_000, 900_000, 9_000_000} {
		tickAt(h, now)
	}
	if n := countCommand(h, "torana_cache_pricing"); n != 0 {
		t.Fatalf("a pending entry reached pricing %d times", n)
	}
	if n := countCommand(h, "torana_send_request"); n != 0 {
		t.Fatalf("a pending entry sent %d times", n)
	}
	raw, ok := h.State("warm/conv-1")
	if !ok || !strings.Contains(raw, "refresh outcome unknown") {
		t.Fatal("the pending entry must remain durable")
	}
}

// ==========================================================================
// Proof batch (review round): exact send payload, carrier/fingerprint
// matrix, zero-pricing failure table, terminal preservation
// ==========================================================================

// sendCall decodes the REAL torana_send_request host-call payload: the JSON
// args with provider/path/timeout_ms and the base64 request_pb, protobuf-
// decoded.
func sendCall(t *testing.T, payload string) (provider, path string, timeoutMS int, req *pbv2.ChatRequest) {
	t.Helper()
	var args struct {
		Provider  string `json:"provider"`
		RequestPB string `json:"request_pb"`
		Path      string `json:"path"`
		TimeoutMS int    `json:"timeout_ms"`
	}
	// STRICT decoding: no unknown fields, and a second decode must hit EOF —
	// a payload with extra/renamed members fails.
	dec := json.NewDecoder(strings.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&args); err != nil {
		t.Fatalf("send payload not JSON: %v", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		t.Fatalf("send payload has trailing content: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(args.RequestPB)
	if err != nil {
		t.Fatalf("request_pb not base64: %v", err)
	}
	req = &pbv2.ChatRequest{}
	if err := proto.Unmarshal(raw, req); err != nil {
		t.Fatalf("request_pb not a proto request: %v", err)
	}
	return args.Provider, args.Path, args.TimeoutMS, req
}

// richWarmRequest is a CONTRACT-VALID replay carrying non-default values for
// EVERY top-level provider-visible family (params, extensions, safety,
// stops) plus tools and rich ordered blocks, with a valid non-terminal
// suffix after the last marker — the exact-send pin must prove each of
// these survives the round trip.
func richWarmRequest() *pbv2.ChatRequest {
	return &pbv2.ChatRequest{
		Model:                  "claude-sonnet-4",
		MaxTokens:              proto.Int32(512),
		Temperature:            proto.Float64(0.7),
		TopP:                   proto.Float64(0.9),
		StopSequences:          []string{"END", "STOP"},
		ProviderExtensionsJson: []byte(`{"custom":{"b":1,"a":2}}`),
		SafetySettingsJson:     []byte(`[{"category":"A","threshold":"B"}]`),
		Tools: []*pbv2.ToolDef{{
			Name: "read", Description: "d", ParametersJson: []byte(`{"type":"object"}`),
		}},
		Messages: []*pbv2.Message{
			{Role: "system", Blocks: []*pbv2.RequestBlock{
				{Kind: &pbv2.RequestBlock_Text{Text: &pbv2.RequestTextBlock{Text: "sys"}}},
				{Kind: &pbv2.RequestBlock_CacheBreakpoint{CacheBreakpoint: &pbv2.RequestCacheBreakpoint{MarkerJson: []byte(`{"type":"ephemeral"}`)}}},
			}},
			{Role: "user", Blocks: []*pbv2.RequestBlock{
				{Kind: &pbv2.RequestBlock_Text{Text: &pbv2.RequestTextBlock{Text: "u"}}},
				{Kind: &pbv2.RequestBlock_ToolResult{ToolResult: &pbv2.RequestToolResultBlock{
					ToolCallId: "c1",
					Content: []*pbv2.ToolResultContentBlock{{
						Kind: &pbv2.ToolResultContentBlock_Text{Text: &pbv2.ToolResultTextBlock{Text: "ok"}},
					}},
				}}},
			}},
			{Role: "user", Blocks: []*pbv2.RequestBlock{{Kind: &pbv2.RequestBlock_Text{Text: &pbv2.RequestTextBlock{Text: "suffix"}}}}},
		},
	}
}

// TestTickExactSendPayload — the outgoing warming request is pinned EXACTLY
// with a RICH replay: the real torana_send_request host-call payload is
// STRICTLY decoded (DisallowUnknownFields + second-decode EOF, exact
// provider/path/timeout_ms, base64 request_pb → protobuf) and compared with
// proto.Equal against an INDEPENDENT expected: a direct clone of the decoded
// stored replay with ONLY max_tokens=1 set — no production mutation helper
// builds the oracle. Every seeded non-default field must remain present and
// exact; stream=false and empty torana_meta_json.
func TestTickExactSendPayload(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(warmerCfg)
	h.StubHostCall("torana_cache_pricing", pricingStub())
	var sent string
	h.StubHostCall("torana_send_request", func(payload string) (string, error) {
		sent = payload
		return sdktest.HostResultValue([]byte(`{"http_status":200,"usage":{"cache_read":1,"cache_write":0}}`)), nil
	})
	// Seed a RICH entry: the fingerprint is built INDEPENDENTLY (literal
	// approved domain over the SDK projection).
	rich := richWarmRequest()
	projection, _, err := pbv2.RequestObservablePrefix(rich)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := sdk.EncodeRequest(rich)
	if err != nil {
		t.Fatal(err)
	}
	richSeed := warmEntrySeed(t)
	richSeed.PrefixPB = enc
	richSeed.PrefixFingerprint = sdk.ContentAddressedCacheKey("cache_warmer/prefix", string(projection))
	seedEntry(t, h, richSeed)
	tickAt(h, 300_000)

	if n := countCommand(h, "torana_send_request"); n != 1 {
		t.Fatalf("sends=%d, want exactly 1", n)
	}
	provider, path, timeoutMS, sentReq := sendCall(t, sent)
	if provider != "anthropic" || path != "/v1/messages" || timeoutMS != 0 {
		t.Fatalf("egress args = (%q, %q, %d), want (anthropic, /v1/messages, 0)", provider, path, timeoutMS)
	}
	// INDEPENDENT expected: clone the decoded stored replay, set ONLY
	// MaxTokens=1.
	replay, err := sdk.DecodeRequest(richSeed.PrefixPB)
	if err != nil {
		t.Fatal(err)
	}
	expected := proto.Clone(replay).(*pbv2.ChatRequest)
	expected.MaxTokens = proto.Int32(1)
	if !proto.Equal(sentReq, expected) {
		t.Fatalf("sent request is not the sanitized replay with only max_tokens=1\n got: %v\nwant: %v", sentReq, expected)
	}
	if sentReq.Stream {
		t.Fatal("the sent request must be non-streaming")
	}
	if len(sentReq.ToranaMetaJson) != 0 {
		t.Fatalf("the sent request must carry no host metadata, got %s", sentReq.ToranaMetaJson)
	}
	// Every seeded non-default family survives: params, stops, extensions,
	// safety, tools, and the rich ordered blocks (incl. the suffix).
	if sentReq.MaxTokens == nil || *sentReq.MaxTokens != 1 {
		t.Fatalf("max_tokens = %v, want 1", sentReq.MaxTokens)
	}
	if sentReq.Temperature == nil || *sentReq.Temperature != 0.7 {
		t.Fatalf("temperature lost: %v", sentReq.Temperature)
	}
	if sentReq.TopP == nil || *sentReq.TopP != 0.9 {
		t.Fatalf("top_p lost: %v", sentReq.TopP)
	}
	if len(sentReq.StopSequences) != 2 || sentReq.StopSequences[0] != "END" || sentReq.StopSequences[1] != "STOP" {
		t.Fatalf("stops lost: %v", sentReq.StopSequences)
	}
	if string(sentReq.ProviderExtensionsJson) != `{"custom":{"b":1,"a":2}}` {
		t.Fatalf("extensions lost: %s", sentReq.ProviderExtensionsJson)
	}
	if string(sentReq.SafetySettingsJson) != `[{"category":"A","threshold":"B"}]` {
		t.Fatalf("safety lost: %s", sentReq.SafetySettingsJson)
	}
	if len(sentReq.Tools) != 1 || sentReq.Tools[0].Name != "read" {
		t.Fatalf("tools lost: %+v", sentReq.Tools)
	}
	if len(sentReq.Messages) != 3 || sentReq.Messages[2].Blocks[0].GetText().Text != "suffix" {
		t.Fatalf("ordered blocks/suffix lost: %+v", sentReq.Messages)
	}
	// Revert proof (a): an expected with ANY extra field changed must fail
	// the equality — the pin is not vacuous.
	tampered := proto.Clone(expected).(*pbv2.ChatRequest)
	tampered.Temperature = proto.Float64(0.5)
	if proto.Equal(sentReq, tampered) {
		t.Fatal("revert proof: an extra outgoing field change went undetected")
	}
}

// carrierInputs builds the four carrier shapes (tool-only, outer, nested,
// mixed tool+outer) as opted-in observation requests.
func carrierInputs() map[string]*pbv2.ChatRequest {
	trText := func(s string) *pbv2.ToolResultContentBlock {
		return &pbv2.ToolResultContentBlock{Kind: &pbv2.ToolResultContentBlock_Text{Text: &pbv2.ToolResultTextBlock{Text: s}}}
	}
	trMarker := func() *pbv2.ToolResultContentBlock {
		return &pbv2.ToolResultContentBlock{Kind: &pbv2.ToolResultContentBlock_CacheBreakpoint{CacheBreakpoint: &pbv2.ToolResultCacheBreakpoint{MarkerJson: []byte(`{"type":"ephemeral"}`)}}}
	}
	outer := func() *pbv2.ChatRequest {
		return &pbv2.ChatRequest{Model: "m", Messages: []*pbv2.Message{
			{Role: "user", Blocks: []*pbv2.RequestBlock{
				{Kind: &pbv2.RequestBlock_Text{Text: &pbv2.RequestTextBlock{Text: "u"}}},
				{Kind: &pbv2.RequestBlock_CacheBreakpoint{CacheBreakpoint: &pbv2.RequestCacheBreakpoint{MarkerJson: []byte(`{"type":"ephemeral"}`)}}},
			}},
		}}
	}
	return map[string]*pbv2.ChatRequest{
		"tool-only": func() *pbv2.ChatRequest {
			r := outer()
			r.Tools = []*pbv2.ToolDef{{Name: "read", Description: "d", ParametersJson: []byte(`{}`), CacheControlJson: []byte(`{"type":"ephemeral"}`)}}
			r.Messages[0].Blocks = r.Messages[0].Blocks[:1]
			return r
		}(),
		"outer": outer(),
		"nested": func() *pbv2.ChatRequest {
			r := outer()
			r.Messages[0].Blocks = []*pbv2.RequestBlock{
				{Kind: &pbv2.RequestBlock_Text{Text: &pbv2.RequestTextBlock{Text: "u"}}},
				{Kind: &pbv2.RequestBlock_ToolResult{ToolResult: &pbv2.RequestToolResultBlock{
					ToolCallId: "c1",
					Content:    []*pbv2.ToolResultContentBlock{trText("r"), trMarker()},
				}}},
			}
			return r
		}(),
		"mixed tool+outer (last = outer)": func() *pbv2.ChatRequest {
			r := outer()
			r.Tools = []*pbv2.ToolDef{{Name: "read", Description: "d", ParametersJson: []byte(`{}`), CacheControlJson: []byte(`{"type":"ephemeral"}`)}}
			return r
		}(),
	}
}

// TestRequestPathCarrierFingerprintMatrix — for every carrier shape through
// the REAL before-request hook: the stored replay equals the INDEPENDENTLY
// sanitized full input (direct clone + two cleared fields), and the stored
// fingerprint equals an INDEPENDENT digest oracle — ContentAddressedCacheKey
// called with the LITERAL approved domain over the SDK projection, never the
// production helper (a namespace change cannot make producer and test drift
// together).
func TestRequestPathCarrierFingerprintMatrix(t *testing.T) {
	const domain = "cache_warmer/prefix"
	indepFingerprint := func(projection []byte) string {
		return sdk.ContentAddressedCacheKey(domain, string(projection))
	}
	// A real VALID non-terminal suffix after the last marker: the sanitized
	// full replay and the SDK projection differ for the intended boundary
	// reason (the suffix is replayable but not part of the cached prefix).
	suffixInput := func() *pbv2.ChatRequest {
		r := carrierInputs()["outer"]
		r.Messages = append(r.Messages, &pbv2.Message{Role: "user", Blocks: []*pbv2.RequestBlock{{Kind: &pbv2.RequestBlock_Text{Text: &pbv2.RequestTextBlock{Text: "suffix"}}}}})
		return r
	}()
	inputs := carrierInputs()
	inputs["outer with valid suffix"] = suffixInput
	for name, req := range inputs {
		t.Run(name, func(t *testing.T) {
			req.ToranaMetaJson = []byte(`{"_provider":"p","_conversation_id":"conv-1","_path":"/x"}`)
			h := newHarness(t)
			h.SetConfig(warmerCfg)
			h.SetNow(100_000)
			res := h.BeforeRequest(req)
			if res.Err != nil || !res.PassedThrough {
				t.Fatalf("err=%v", res.Err)
			}
			raw, ok := h.State("warm/conv-1")
			if !ok {
				t.Fatal("no entry stored")
			}
			var entry warmEntry
			if err := json.Unmarshal([]byte(raw), &entry); err != nil {
				t.Fatal(err)
			}
			// Replay == the independent sanitized full input.
			decoded, err := sdk.DecodeRequest(entry.PrefixPB)
			if err != nil {
				t.Fatal(err)
			}
			expected := proto.Clone(req).(*pbv2.ChatRequest)
			expected.Stream = false
			expected.ToranaMetaJson = nil
			if !proto.Equal(decoded, expected) {
				t.Fatalf("replay != sanitized full input\n got: %v\nwant: %v", decoded, expected)
			}
			// Fingerprint == the independent digest oracle over the SDK
			// projection (never prefixFingerprint).
			projection, _, err := pbv2.RequestObservablePrefix(req)
			if err != nil {
				t.Fatal(err)
			}
			if want := indepFingerprint(projection); entry.PrefixFingerprint != want {
				t.Fatalf("fingerprint %q, want the independent oracle %q", entry.PrefixFingerprint, want)
			}
			// Revert proof (b): bypassing the SDK projection — hashing the
			// INDEPENDENTLY SANITIZED full replay (a direct clone with
			// stream/meta cleared — NOT the raw request, whose torana_meta
			// would dominate the comparison) — must FAIL the oracle exactly
			// where the sanitized full replay legitimately differs from the
			// projection: the tool-only shape (messages excluded) and the
			// suffix shape (suffix excluded). For the plain outer/nested/
			// mixed shapes the sanitized full replay legitimately EQUALS the
			// projection, so no divergence is demanded there.
			diverges := name == "tool-only" || name == "outer with valid suffix"
			sanitizedFull := proto.Clone(req).(*pbv2.ChatRequest)
			sanitizedFull.Stream = false
			sanitizedFull.ToranaMetaJson = nil
			rawBytes, _ := proto.Marshal(sanitizedFull)
			bypassed := indepFingerprint(rawBytes)
			if diverges && bypassed == entry.PrefixFingerprint {
				t.Fatal("revert proof: a projection-bypassing digest matched the stored fingerprint")
			}
			if !diverges && bypassed != entry.PrefixFingerprint {
				t.Fatalf("sanitized full replay must equal its projection for this shape: %q != %q", bypassed, entry.PrefixFingerprint)
			}
		})
	}
}

// TestRequestPathFingerprintSensitivity — the full sensitivity table through
// the hook: mutations before the boundary, the marker value/position, params,
// extensions, safety, and stops change the fingerprint; stream and
// torana metadata change NEITHER the fingerprint NOR the sanitized replay's
// corresponding fields (which stay cleared/absent).
func TestRequestPathFingerprintSensitivity(t *testing.T) {
	const domain = "cache_warmer/prefix"
	indepFingerprint := func(projection []byte) string {
		return sdk.ContentAddressedCacheKey(domain, string(projection))
	}
	store := func(req *pbv2.ChatRequest) (warmEntry, *pbv2.ChatRequest) {
		t.Helper()
		// Preserve CALLER-PROVIDED valid opted-in metadata; inject the
		// default only when absent (a caller-supplied payload must actually
		// be the one dispatched).
		if len(req.ToranaMetaJson) == 0 {
			req.ToranaMetaJson = []byte(`{"_provider":"p","_conversation_id":"conv-1","_path":"/x"}`)
		}
		h := newHarness(t)
		h.SetConfig(warmerCfg)
		h.SetNow(100_000)
		if res := h.BeforeRequest(req); res.Err != nil {
			t.Fatalf("err=%v", res.Err)
		}
		raw, _ := h.State("warm/conv-1")
		var entry warmEntry
		_ = json.Unmarshal([]byte(raw), &entry)
		decoded, _ := sdk.DecodeRequest(entry.PrefixPB)
		return entry, decoded
	}
	base := carrierInputs()["outer"]
	baseEntry, _ := store(base)

	// Before-boundary mutation, marker value, marker position, params,
	// extensions, safety, stops: the fingerprint CHANGES.
	mutations := map[string]func(*pbv2.ChatRequest){
		"before-boundary text": func(r *pbv2.ChatRequest) { r.Messages[0].Blocks[0].GetText().Text = "changed" },
		"marker value": func(r *pbv2.ChatRequest) {
			r.Messages[0].Blocks[1].GetCacheBreakpoint().MarkerJson = []byte(`{"type":"standard"}`)
		},
		"marker position": func(r *pbv2.ChatRequest) {
			// ISOLATED: the same two blocks, only the marker moves from
			// blocks[1] to blocks[0] — the boundary changes (the text drops
			// out of the truncated prefix) with NO content added or removed.
			r.Messages[0].Blocks = []*pbv2.RequestBlock{
				r.Messages[0].Blocks[1],
				r.Messages[0].Blocks[0],
			}
		},
		"params":     func(r *pbv2.ChatRequest) { r.MaxTokens = proto.Int32(64) },
		"extensions": func(r *pbv2.ChatRequest) { r.ProviderExtensionsJson = []byte(`{"x":1}`) },
		"safety":     func(r *pbv2.ChatRequest) { r.SafetySettingsJson = []byte(`[]`) },
		"stops":      func(r *pbv2.ChatRequest) { r.StopSequences = []string{"END"} },
	}
	for name, mutate := range mutations {
		t.Run("changes/"+name, func(t *testing.T) {
			req := proto.Clone(base).(*pbv2.ChatRequest)
			mutate(req)
			entry, _ := store(req)
			projection, _, err := pbv2.RequestObservablePrefix(req)
			if err != nil {
				t.Fatal(err)
			}
			if want := indepFingerprint(projection); entry.PrefixFingerprint != want {
				t.Fatalf("fingerprint %q != oracle %q", entry.PrefixFingerprint, want)
			}
			if entry.PrefixFingerprint == baseEntry.PrefixFingerprint {
				t.Fatalf("mutation %q did not change the fingerprint", name)
			}
		})
	}

	// Stream and torana metadata: fingerprint UNCHANGED, and the sanitized
	// replay keeps stream=false + no metadata. NON-VACUOUS: the caller's
	// metadata payload is preserved by the store helper and actually
	// dispatched, so the comparison exercises the metadata difference.
	t.Run("stream and metadata neutral", func(t *testing.T) {
		reqA := proto.Clone(base).(*pbv2.ChatRequest)
		reqA.Stream = true
		reqA.ToranaMetaJson = []byte(`{"_provider":"p","_conversation_id":"conv-1","_path":"/x","_request_headers":"secret-a"}`)
		entryA, decodedA := store(reqA)
		reqB := proto.Clone(base).(*pbv2.ChatRequest)
		reqB.Stream = true
		reqB.ToranaMetaJson = []byte(`{"_provider":"p","_conversation_id":"conv-1","_path":"/x","_request_headers":"secret-b"}`)
		entryB, decodedB := store(reqB)
		if entryA.PrefixFingerprint != entryB.PrefixFingerprint {
			t.Fatal("distinct metadata changed the fingerprint")
		}
		if entryA.PrefixFingerprint != baseEntry.PrefixFingerprint {
			t.Fatal("stream/metadata changed the fingerprint")
		}
		for _, decoded := range []*pbv2.ChatRequest{decodedA, decodedB} {
			if decoded.Stream {
				t.Fatal("the sanitized replay kept stream=true")
			}
			if len(decoded.ToranaMetaJson) != 0 {
				t.Fatalf("the sanitized replay kept metadata: %s", decoded.ToranaMetaJson)
			}
		}
	})
}

// TestRequestPathTerminalSuffixPreservesEntry — the terminal-suffix
// observation NEVER overwrites an existing valid warm entry: with a
// byte-distinct valid entry seeded, the observed terminal request leaves the
// state bytes EXACTLY unchanged, with exactly one env.plugin_config call and
// no clock/state write.
func TestRequestPathTerminalSuffixPreservesEntry(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(warmerCfg)
	h.SetNow(100_000)
	// Byte-distinct valid entry: a different model + deadline so any rewrite
	// would be visible.
	seed := warmEntrySeed(t)
	seed.Model = "claude-opus-4"
	seed.PrefixPB = func() string {
		req := warmRequest()
		req.Model = "claude-opus-4"
		enc, _ := sdk.EncodeRequest(req)
		return enc
	}()
	seed.PrefixFingerprint = func() string {
		req := warmRequest()
		req.Model = "claude-opus-4"
		prefix, _, err := pbv2.RequestObservablePrefix(req)
		if err != nil {
			t.Fatal(err)
		}
		return prefixFingerprint(prefix)
	}()
	seedBytes, _ := json.Marshal(seed)
	h.SeedState("warm/conv-1", string(seedBytes))

	terminal := &pbv2.ChatRequest{
		Model: "m",
		Messages: []*pbv2.Message{
			textMsg("user", "u", true),
			{Role: "assistant", Blocks: []*pbv2.RequestBlock{{Kind: &pbv2.RequestBlock_ToolUse{ToolUse: &pbv2.RequestToolUseBlock{Id: "c1", Name: "read", ArgumentsJson: []byte(`{}`)}}}}},
		},
	}
	terminal.ToranaMetaJson = []byte(`{"_provider":"p","_conversation_id":"conv-1","_path":"/x"}`)
	res := h.BeforeRequest(terminal)
	if res.Err != nil || !res.PassedThrough {
		t.Fatalf("err=%v", res.Err)
	}
	raw, ok := h.State("warm/conv-1")
	if !ok {
		t.Fatal("the seeded entry vanished")
	}
	if raw != string(seedBytes) {
		t.Fatalf("the terminal observation rewrote the valid entry:\n got: %s\nwant: %s", raw, seedBytes)
	}
	calls := h.Calls()
	if len(calls) != 1 || calls[0].Command != "env.plugin_config" {
		var got []string
		for _, c := range calls {
			got = append(got, c.Command)
		}
		t.Fatalf("call multiset = %v, want exactly [env.plugin_config]", got)
	}
}

// TestTickActionsCountConfirmedCompletesOnly — the TickDid Actions field
// counts CONFIRMED completed refreshes ONLY: a cache hit or a rebuilt cache
// = 1 completed action; an advisory send refusal, an HTTP refusal, and an
// unknown outcome = 0 (the attempt consumed the entry but is not a
// completed refresh); a decode defect and a contract refusal ERROR the tick
// without an emitted result.
func TestTickActionsCountConfirmedCompletesOnly(t *testing.T) {
	rows := []struct {
		name        string
		send        func(string) (string, error)
		wantActions int32
		wantErr     bool
	}{
		{"cache hit", hitStub(), 1, false},
		{"cache rebuilt", rebuiltStub(), 1, false},
		{"advisory send refusal", func(string) (string, error) {
			return sdktest.HostResultError(pbv2.ErrorCode_ERROR_CODE_UNAVAILABLE, "transient"), nil
		}, 0, false},
		{"HTTP refusal", func(string) (string, error) {
			return sdktest.HostResultValue([]byte(`{"http_status":401}`)), nil
		}, 0, false},
		{"unknown outcome", noSignalStub(), 0, false},
		{"decode defect", func(string) (string, error) {
			return sdktest.HostResultValue([]byte(`not a domain body`)), nil
		}, 0, true},
		{"contract refusal", func(string) (string, error) {
			return sdktest.HostResultError(pbv2.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "denied"), nil
		}, 0, true},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			h := newHarness(t)
			h.SetConfig(warmerCfg)
			h.StubHostCall("torana_cache_pricing", pricingStub())
			h.StubHostCall("torana_send_request", row.send)
			seedEntry(t, h, warmEntrySeed(t))
			res := h.Tick(&pbv2.TickRequest{UnixMillis: 300_000})
			if row.wantErr {
				if res.Err == nil {
					t.Fatal("must error the tick without an emitted result")
				}
				return
			}
			if res.Err != nil {
				t.Fatalf("unexpected tick error: %v", res.Err)
			}
			if res.Outcome == nil || res.Outcome.Actions != row.wantActions {
				t.Fatalf("Actions = %+v, want %d", res.Outcome, row.wantActions)
			}
		})
	}
}
