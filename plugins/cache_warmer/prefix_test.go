package main

import (
	"encoding/json"
	"strings"
	"testing"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
	"google.golang.org/protobuf/proto"
)

// ==========================================================================
// The prefix budget
// ==========================================================================
//
// Torana's durable store bounds one value at 256 KiB by default, and the
// replay artifact is the whole request. A conversation big enough to be worth
// warming does not fit in one value, so the artifact is split across bounded
// part values — and above a declared ceiling the warmer refuses, visibly,
// rather than promising warming it cannot perform.

// opted is a conversation the warmer is configured to keep warm, carrying a
// system prompt of the requested size and an explicit cache breakpoint.
func opted(systemBytes int) *pbv1.ChatRequest {
	return &pbv1.ChatRequest{
		Model: "claude-sonnet-4",
		Messages: []*pbv1.Message{
			textMsg("system", strings.Repeat("context ", systemBytes/8), true),
			textMsg("user", "find the bug", false),
		},
		ToranaMetaJson: []byte(`{"_provider":"anthropic","_conversation_id":"conv-1","_path":"/v1/messages"}`),
	}
}

func storedEntry(t *testing.T, h *sdktest.Harness) warmEntry {
	t.Helper()
	raw, ok := h.State("warm/conv-1")
	if !ok {
		t.Fatal("no entry stored")
	}
	var entry warmEntry
	if err := json.Unmarshal([]byte(raw), &entry); err != nil {
		t.Fatal(err)
	}
	return entry
}

// stateSetKeys lists the durable keys a run actually wrote, in order.
func stateSetKeys(t *testing.T, h *sdktest.Harness) []string {
	t.Helper()
	var keys []string
	for _, c := range h.Calls() {
		if c.Command != "env.state_set" {
			continue
		}
		var args pbv1.StateSetArgs
		if err := proto.Unmarshal([]byte(c.Args), &args); err != nil {
			t.Fatalf("state_set args not a StateSetArgs proto: %v", err)
		}
		keys = append(keys, args.Key)
	}
	return keys
}

// TestLargeArtifactIsSplitAndReplayable is the gap this closes: a
// conversation whose encoded replay exceeds one durable value used to be
// unstorable, which is precisely the size where warming is worth paying for.
func TestLargeArtifactIsSplitAndReplayable(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(warmerCfg)
	h.SetNow(100_000)
	h.StubHostCall("env.cache_policy", pricingStub())
	h.StubHostCall("torana_send_request", hitStub())

	req := opted(300_000)
	if res := h.BeforeRequest(req); res.Err != nil || !res.PassedThrough {
		t.Fatalf("the warmer must never mutate a request, err=%v", res.Err)
	}

	entry := storedEntry(t, h)
	if entry.PrefixParts < 2 {
		t.Fatalf("a %d-byte conversation was stored in %d part(s) — it would not fit one durable value", len(req.Messages[0].Blocks[0].GetText().Text), entry.PrefixParts)
	}
	if entry.PrefixParts > maxPrefixParts {
		t.Fatalf("stored %d parts, above the declared ceiling of %d", entry.PrefixParts, maxPrefixParts)
	}
	for i := 0; i < entry.PrefixParts; i++ {
		part, ok := h.State(partKey(entry.PrefixDigest, i))
		if !ok {
			t.Fatalf("part %d was never written", i)
		}
		if len(part) > maxPartBytes {
			t.Fatalf("part %d is %d bytes, over the %d-byte value budget", i, len(part), maxPartBytes)
		}
	}

	// Reassembly is exact: the parts are the sanitized replay, byte for byte.
	sanitized := proto.Clone(req).(*pbv1.ChatRequest)
	sanitized.Stream = false
	sanitized.ToranaMetaJson = nil
	want, err := sdk.EncodeRequest(sanitized)
	if err != nil {
		t.Fatal(err)
	}
	if got := storedPrefix(t, h, entry); got != want {
		t.Fatalf("reassembled artifact differs from the sanitized replay (%d vs %d bytes)", len(got), len(want))
	}

	// And it still replays: the tick reads the parts back from durable state
	// alone, exactly as it would after a restart.
	tickAt(h, 400_000)
	if n := countCommand(h, "torana_send_request"); n != 1 {
		t.Fatalf("sends=%d, want exactly 1 refresh replayed from the stored parts", n)
	}
}

// TestArtifactBeyondTheBudgetIsDeclinedVisibly — over the ceiling the warmer
// stores no artifact, spends nothing, and says why. Silence here would be a
// plugin that is enabled, configured, and quietly doing nothing.
func TestArtifactBeyondTheBudgetIsDeclinedVisibly(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(warmerCfg)
	h.SetNow(100_000)
	h.StubHostCall("env.cache_policy", pricingStub())
	h.StubHostCall("torana_send_request", hitStub())

	if res := h.BeforeRequest(opted(1_200_000)); res.Err != nil || !res.PassedThrough {
		t.Fatalf("an oversized conversation must still pass through, err=%v", res.Err)
	}

	entry := storedEntry(t, h)
	if entry.Stopped != stopPrefixTooLarge {
		t.Fatalf("stopped = %q, want %q", entry.Stopped, stopPrefixTooLarge)
	}
	if entry.PrefixDigest != "" || entry.PrefixParts != 0 {
		t.Fatalf("a declined conversation must name no artifact: %+v", entry)
	}
	for _, key := range stateSetKeys(t, h) {
		if strings.HasPrefix(key, partPrefix) {
			t.Fatalf("wrote artifact part %q for a conversation over the budget", key)
		}
	}

	tickAt(h, 400_000)
	if n := countCommand(h, "torana_send_request"); n != 0 {
		t.Fatalf("a declined conversation must never be refreshed, got %d sends", n)
	}
	if n := countCommand(h, "env.cache_policy"); n != 0 {
		t.Fatalf("a declined conversation reached pricing %d times", n)
	}
}

// TestStoreRefusalIsDeclinedVisibly — the ceiling is this plugin's, but the
// store has its own (a lower configured value limit, the per-plugin key cap,
// a full store). A refusal of the artifact is the same answer: no warming,
// and a reason an operator can read.
func TestStoreRefusalIsDeclinedVisibly(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(warmerCfg)
	h.SetNow(100_000)
	var entryWrites []string
	h.StubHostCall("env.state_set", func(raw string) (string, error) {
		var args pbv1.StateSetArgs
		if err := proto.Unmarshal([]byte(raw), &args); err != nil {
			return "", err
		}
		if strings.HasPrefix(args.Key, partPrefix) {
			return sdktest.HostResultError(pbv1.ErrorCode_ERROR_CODE_INTERNAL, "value is 131072 bytes, limit is 65536"), nil
		}
		entryWrites = append(entryWrites, args.Value)
		return sdktest.HostResultValue(nil), nil
	})

	res := h.BeforeRequest(opted(300_000))
	if res.Err != nil || !res.PassedThrough {
		t.Fatalf("a store refusal must not fail the request, err=%v", res.Err)
	}
	if len(entryWrites) != 1 {
		t.Fatalf("entry writes = %d, want exactly the decline", len(entryWrites))
	}
	var entry warmEntry
	if err := json.Unmarshal([]byte(entryWrites[0]), &entry); err != nil {
		t.Fatal(err)
	}
	if entry.Stopped != stopPrefixStorageFailed {
		t.Fatalf("stopped = %q, want %q", entry.Stopped, stopPrefixStorageFailed)
	}
	if entry.PrefixDigest != "" || entry.PrefixParts != 0 {
		t.Fatalf("a refused artifact must not be named by the entry: %+v", entry)
	}
}

// TestUnconfiguredStateStoresNothing — no durable state at all is advisory:
// the request passes and nothing is stored, so nothing is ever spent.
func TestUnconfiguredStateStoresNothing(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(warmerCfg)
	h.SetNow(100_000)
	h.StubHostCall("env.state_set", func(string) (string, error) {
		return sdktest.HostResultError(pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "no durable state"), nil
	})
	res := h.BeforeRequest(opted(300_000))
	if res.Err != nil || !res.PassedThrough {
		t.Fatalf("an unconfigured store must pass through, err=%v", res.Err)
	}
	if _, ok := h.State("warm/conv-1"); ok {
		t.Fatal("an unconfigured store must leave no entry")
	}
}

// TestTickCollectsSupersededArtifacts — every real turn stores a larger
// artifact under a new digest. Without collection one warmed conversation
// would grow the store on every turn until it hit a cap shared with every
// other plugin.
func TestTickCollectsSupersededArtifacts(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(warmerCfg)
	h.SetNow(100_000)
	h.StubHostCall("env.cache_policy", pricingStub())
	h.StubHostCall("torana_send_request", hitStub())

	if res := h.BeforeRequest(opted(200_000)); res.Err != nil {
		t.Fatalf("first turn: %v", res.Err)
	}
	first := storedEntry(t, h)
	if res := h.BeforeRequest(opted(260_000)); res.Err != nil {
		t.Fatalf("second turn: %v", res.Err)
	}
	second := storedEntry(t, h)
	if first.PrefixDigest == second.PrefixDigest {
		t.Fatal("the two turns produced the same artifact; this test proves nothing")
	}
	if _, ok := h.State(partKey(first.PrefixDigest, 0)); !ok {
		t.Fatal("the superseded artifact was expected to still be present before a tick")
	}

	tickAt(h, 400_000)

	for i := 0; i < first.PrefixParts; i++ {
		if _, ok := h.State(partKey(first.PrefixDigest, i)); ok {
			t.Fatalf("superseded part %d survived collection", i)
		}
	}
	for i := 0; i < second.PrefixParts; i++ {
		if _, ok := h.State(partKey(second.PrefixDigest, i)); !ok {
			t.Fatalf("the live artifact's part %d was collected", i)
		}
	}
	if n := countCommand(h, "torana_send_request"); n != 1 {
		t.Fatalf("collection must not disturb the refresh: sends=%d", n)
	}
}

// TestPendingEntryStillBlocksSpendAfterTheSplit — the write-ahead reservation
// is unchanged by where the artifact lives: a seeded pending entry causes
// zero sends, and the entry is small enough that the reservation write itself
// always fits.
func TestPendingEntryStillBlocksSpendAfterTheSplit(t *testing.T) {
	h := newHarness(t)
	h.SetConfig(warmerCfg)
	h.StubHostCall("env.cache_policy", pricingStub())
	h.StubHostCall("torana_send_request", hitStub())
	entry := warmEntrySeed(t)
	entry.Stopped = "refresh outcome unknown"
	entry.AttemptMillis = 200_000
	seedEntry(t, h, entry)
	tickAt(h, 400_000)
	if n := countCommand(h, "torana_send_request"); n != 0 {
		t.Fatalf("a pending reservation must block every later send, got %d", n)
	}
}

// TestArtifactRoundTripAndTamperDetection — reassembly is exact across many
// parts, and bytes that do not hash to the recorded digest are never
// presented as the conversation.
func TestArtifactRoundTripAndTamperDetection(t *testing.T) {
	h := newHarness(t)
	encoded := strings.Repeat("ABCDEFGH", maxPartBytes/2) // several parts
	h.Run(func() {
		digest, parts, err := storePrefix(encoded)
		if err != nil {
			t.Fatalf("storePrefix: %v", err)
		}
		if parts < 3 {
			t.Fatalf("parts = %d, want a genuinely multi-part artifact", parts)
		}
		entry := warmEntry{PrefixDigest: digest, PrefixParts: parts}
		got, intact, err := loadPrefix(&entry)
		if err != nil || !intact {
			t.Fatalf("loadPrefix: intact=%v err=%v", intact, err)
		}
		if got != encoded {
			t.Fatal("reassembled artifact differs from what was stored")
		}

		// A part rewritten under the same key no longer hashes to the digest.
		h.SeedState(partKey(digest, 1), "tampered")
		if _, intact, err := loadPrefix(&entry); err != nil || intact {
			t.Fatalf("a tampered artifact must not be intact: intact=%v err=%v", intact, err)
		}

		// A missing part is not absence-with-a-shorter-artifact either.
		entry.PrefixParts = parts + 1
		if _, intact, err := loadPrefix(&entry); err != nil || intact {
			t.Fatalf("a missing part must not be intact: intact=%v err=%v", intact, err)
		}
	})
}
