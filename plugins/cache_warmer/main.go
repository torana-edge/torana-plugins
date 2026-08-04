// cache_warmer keeps a chosen conversation's prompt cache alive across an idle
// gap, so resuming work does not pay to rebuild a prefix that was about to be
// thrown away.
//
// # The arithmetic, and why this plugin refuses to run forever
//
// Holding an entry open costs one cache read per refresh. Letting it lapse
// costs one cache write on the next turn. So refreshing wins only while:
//
//	refreshes_spent  <  (write_rate / read_rate) - 1
//
// The prefix size cancels out of both sides — this is a pure price ratio,
// independent of how large the conversation is. On Anthropic's 5-minute tier,
// where writes cost 12.5x reads, that is about eleven refreshes: roughly
// forty-five minutes at the default interval.
//
// Past that point refreshing has cost more than the miss it was avoiding, and
// it keeps diverging rather than settling at break-even. That is the whole
// reason warming is opt-in per conversation and bounded by a deadline and a
// refresh budget. A plugin that quietly warmed everything forever would lose
// money on every conversation nobody came back to.
//
// # What it will not do
//
//   - Warm a provider whose cache lifetime does not refresh on read. There is
//     nothing to keep alive, and every refresh is pure cost.
//   - Warm without pricing. Unknown economics is exactly when guessing is most
//     expensive.
//   - Keep warming after a refresh reports a cache WRITE. That means the entry
//     had already lapsed and the refresh paid to rebuild it, so continuing
//     would be paying to hold something the user may never return to.
//   - Spend without a durable reservation: the tick persists a pending state
//     BEFORE sending, so a crash or a failed final write can never leave a
//     due entry that later ticks keep spending on (replay invariant: a seeded
//     pending entry causes zero sends).
//
// # v2 semantics
//
//   - Durable entries carry a schema version; anything else stops with zero
//     sends (no v1 decoder, no compatibility branch — v1 wire layouts overlap
//     and can misdecode as v2).
//   - State refusals split by class: advisory (NOT_CONFIGURED/UNAVAILABLE)
//     declines safely; contract/protocol refusals error the hook so a broken
//     enabled plugin is visible. Corrupt stored JSON is a key-local data
//     error: that entry is skipped, others are unaffected.
//   - The request path is observational and never mutates the request.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pbv2 "github.com/torana-edge/torana-plugin-sdk/pb/v2"
	"google.golang.org/protobuf/proto"
)

func main() {}

// schemaVersion marks the durable entry format written by this plugin. Any
// other version — including anything a v1/v2 plugin stored — is stopped with
// zero sends. v3 added the domain-separated PrefixFingerprint; there is NO
// v2 fallback/decoder (pre-release policy).
const schemaVersion = 3

// warmEntry is everything needed to refresh one conversation, stored durably so
// a restart does not lose track of what it was keeping alive.
type warmEntry struct {
	SchemaVersion  int    `json:"schema_version"`
	ConversationID string `json:"conversation_id"`
	Provider       string `json:"provider"`
	Model          string `json:"model"`
	Path           string `json:"path"`

	// PrefixPB is the base64 protobuf of the SANITIZED REPLAY REQUEST: a
	// clone of the accepted request with stream=false and torana_meta_json
	// cleared, preserving every provider-visible field and ordered block
	// exactly. It is the artifact replayed on a warming tick (with
	// max_tokens set to 1), not a truncated prefix.
	PrefixPB string `json:"prefix_pb"`

	// PrefixFingerprint is the fixed, domain-separated digest of the SDK
	// observable projection at observation time — the identity the replay
	// must match. v3 schema; required non-empty before any pricing call.
	PrefixFingerprint string `json:"prefix_fingerprint"`

	LastRefreshMillis int64 `json:"last_refresh_millis"`
	LastSeenMillis    int64 `json:"last_seen_millis"`
	RefreshesSpent    int   `json:"refreshes_spent"`
	DeadlineMillis    int64 `json:"deadline_millis"`

	// AttemptMillis records the write-ahead reservation timestamp: a pending
	// entry (Stopped="refresh outcome unknown") marks a send that may have
	// been attempted, so later ticks must not spend again.
	AttemptMillis int64 `json:"attempt_millis,omitempty"`

	// Stopped records why warming ended, so an operator reading state can see
	// that it finished deliberately rather than silently failing.
	Stopped string `json:"stopped,omitempty"`
}

type config struct {
	// Conversations lists conversation IDs to keep warm, comma-separated.
	// Empty means the plugin observes but never spends — the safe default,
	// since warming everything is how this feature loses money.
	Conversations string `json:"conversations"`
	// WarmForMinutes bounds how long after the last real turn a conversation
	// stays warm. Zero uses the break-even count alone.
	WarmForMinutes int `json:"warm_for_minutes"`
	// IntervalSecondsOverride replaces the provider's derived refresh cadence.
	IntervalSecondsOverride int `json:"interval_seconds_override"`
}

// parseConfig is the pure config decoder; the host validates config against
// schema.json at write time, so an unmarshal failure is unreachable in
// practice and falls back to defaults. Loaded per call — no process globals.
//
// warm_for_minutes defaults to 45 (the schema default): a user who configures
// only `conversations` gets the advertised time-bounded warming. An EXPLICIT
// zero is preserved as "break-even count alone" — absence and zero are
// distinguished by the pointer decode.
func parseConfig(raw string) config {
	var c struct {
		Conversations           string `json:"conversations"`
		WarmForMinutes          *int   `json:"warm_for_minutes"`
		IntervalSecondsOverride int    `json:"interval_seconds_override"`
	}
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &c)
	}
	cfg := config{
		Conversations:           c.Conversations,
		WarmForMinutes:          45,
		IntervalSecondsOverride: c.IntervalSecondsOverride,
	}
	if c.WarmForMinutes != nil {
		cfg.WarmForMinutes = *c.WarmForMinutes
	}
	return cfg
}

func loadConfig() config { return parseConfig(sdk.PluginConfig()) }

// warms reports whether this conversation is opted in.
func (c config) warms(conversationID string) bool {
	if conversationID == "" {
		return false
	}
	for _, id := range strings.Split(c.Conversations, ",") {
		if strings.TrimSpace(id) == conversationID {
			return true
		}
	}
	return false
}

func (c config) any() bool { return strings.TrimSpace(c.Conversations) != "" }

const entryPrefix = "warm/"

// isAdvisory reports whether err is an advisory refusal (NOT_CONFIGURED or
// UNAVAILABLE) — the operator/transient class a plugin may decline safely.
func isAdvisory(err error) bool {
	if errors.Is(err, sdk.ErrStateUnavailable) {
		return true
	}
	var refusal *sdk.HostCallRefusalError
	if errors.As(err, &refusal) {
		return refusal.Code == pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED ||
			refusal.Code == pbv2.ErrorCode_ERROR_CODE_UNAVAILABLE
	}
	return false
}

func init() {
	// Request path: remember the cached prefix of any conversation the operator
	// opted in. This hook only observes and stores — it never modifies the
	// request, so it cannot affect the prefix it is trying to preserve.
	sdk.OnBeforeRequest(func(ctx context.Context, req *pbv2.ChatRequest) (sdk.RequestResult, error) {
		cfg := loadConfig()
		if !cfg.any() {
			return sdk.PassRequest(), nil
		}
		meta := readHostMeta(req)
		if meta.ConversationID == "" || !cfg.warms(meta.ConversationID) {
			return sdk.PassRequest(), nil
		}

		// The SDK-owned projection is the identity oracle (I1 parity with the
		// host cache key): an out-of-domain request declines with NO entry
		// (I3), and no marker means no explicitly cached prefix to refresh.
		prefix, hasBreakpoint, err := pbv2.RequestObservablePrefix(req)
		if err != nil {
			return sdk.PassRequest(), nil
		}
		if !hasBreakpoint {
			return sdk.PassRequest(), nil
		}

		// The replay artifact is the SANITIZED full request (never the hook
		// input): stream=false and torana_meta_json cleared, every
		// provider-visible field and ordered block preserved exactly.
		replay := sanitizeReplay(req)

		// A replay already known to be unsendable (an assistant final turn
		// with any tool_use block) declines BEFORE the clock and before any
		// state write — with no entry, so an existing valid warm entry is
		// never overwritten with an artifact the next tick must stop.
		if endsWithUnansweredToolCall(replay) {
			return sdk.PassRequest(), nil
		}

		encoded, err := sdk.EncodeRequest(replay)
		if err != nil {
			// Local encode failure: a protocol/plugin defect, not a condition
			// to absorb.
			return sdk.RequestResult{}, fmt.Errorf("cache_warmer: encode replay: %w", err)
		}

		// A plugin without the clock cannot reason about elapsed time at all,
		// and every downstream number would be measured from the epoch.
		// Storing nothing is better than storing a deadline 45 minutes after
		// 1970. An advisory clock refusal declines silently; a contract or
		// protocol failure surfaces.
		now, err := sdk.Now()
		if err != nil {
			if isAdvisory(err) {
				return sdk.PassRequest(), nil
			}
			return sdk.RequestResult{}, err
		}
		entry := warmEntry{
			SchemaVersion:     schemaVersion,
			ConversationID:    meta.ConversationID,
			Provider:          meta.Provider,
			Model:             req.Model,
			Path:              meta.Path,
			PrefixPB:          encoded,
			PrefixFingerprint: prefixFingerprint(prefix),
			LastSeenMillis:    now,
		}
		// A real turn resets the budget: the user is active, the cache was just
		// refreshed for free, and whatever was spent before is spent.
		if cfg.WarmForMinutes > 0 {
			entry.DeadlineMillis = now + int64(cfg.WarmForMinutes)*60_000
		}
		if err := sdk.StateSetJSON(entryPrefix+meta.ConversationID, entry); err != nil {
			// Entry store: advisory refusal means pass with no entry and no
			// future spend; contract/protocol failure is a hook error so a
			// broken enabled plugin is visible.
			if isAdvisory(err) {
				return sdk.PassRequest(), nil
			}
			return sdk.RequestResult{}, err
		}
		return sdk.PassRequest(), nil
	})

	// Tick path: refresh whatever is still worth refreshing, under the
	// write-ahead spend reservation (see refreshOne).
	sdk.OnTick(func(ctx context.Context, tick *pbv2.TickRequest) (sdk.TickResult, error) {
		cfg := loadConfig()
		keys, herr, err := sdk.StateKeys()
		if err != nil {
			return sdk.TickResult{}, err
		}
		if herr != nil {
			if herr.Code == pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED || herr.Code == pbv2.ErrorCode_ERROR_CODE_UNAVAILABLE {
				return sdk.TickIdle(), nil
			}
			return sdk.TickResult{}, fmt.Errorf("cache_warmer: state_keys refused: %s", herr.Message)
		}

		refreshed := 0
		var notes []string
		for _, key := range keys {
			if len(key) <= len(entryPrefix) || key[:len(entryPrefix)] != entryPrefix {
				continue
			}
			// The entry read is AUTHORITATIVE, so the failure classes must be
			// distinguishable: a malformed frame or transport failure is a
			// protocol defect (tick error); a refusal is advisory or contract
			// by code; a present value that is not the expected JSON is a
			// key-local data error (skip this entry, never absence).
			// StateGetJSON would collapse frame and decode errors into one
			// plain-error channel, so the raw typed read is used here.
			var entry warmEntry
			raw, herr, err := sdk.StateGet(key)
			switch {
			case err != nil:
				return sdk.TickResult{}, err
			case herr != nil && sdk.IsNotFound(herr):
				continue
			case herr != nil && (herr.Code == pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED || herr.Code == pbv2.ErrorCode_ERROR_CODE_UNAVAILABLE):
				continue
			case herr != nil:
				return sdk.TickResult{}, fmt.Errorf("cache_warmer: state_get %s refused: %s", key, herr.Message)
			case json.Unmarshal([]byte(raw), &entry) != nil:
				continue // corrupt stored JSON: key-local
			}
			if entry.Stopped != "" {
				// Pending or terminal: no further spend, ever, until a real
				// turn rewrites the entry.
				continue
			}
			if !cfg.warms(entry.ConversationID) {
				// Opted out since the entry was written: delete it (best-effort).
				_, _ = sdk.StateDelete(key)
				continue
			}
			// Durable-state shape validation is the FIRST step after decoding
			// — before pricing and before any spend-related decision. Invalid
			// entries must never reach a pricing call or a send.
			entry.Stopped = validateEntry(&entry, key)
			if entry.Stopped != "" {
				persistStop(key, &entry)
				notes = append(notes, fmt.Sprintf("%s: stopped, %s", short(entry.ConversationID), entry.Stopped))
				continue
			}

			action, note, err := refreshOne(&entry, cfg, key, tick.UnixMillis)
			if err != nil {
				return sdk.TickResult{}, err
			}
			if note != "" {
				notes = append(notes, note)
			}
			if action {
				refreshed++
			}
		}

		if refreshed == 0 && len(notes) == 0 {
			return sdk.TickIdle(), nil
		}
		return sdk.TickDid(int32(refreshed), joinNotes(notes)), nil
	})
}

// persistStop writes a terminal stop reason. Best-effort: if the write fails
// the durable entry remains PENDING (Stopped="refresh outcome unknown" was
// already written before the send), which still guarantees zero further
// sends.
func persistStop(key string, entry *warmEntry) {
	_ = sdk.StateSetJSON(key, entry)
}

// refreshOne decides and, if warranted, sends — under the write-ahead
// reservation. It mutates entry in place and reports whether a refresh was
// sent, a human-readable outcome, and any error that must surface on the tick.
//
// State machine (per entry): fresh -> due -> pending -> done.
//
//  1. validate the entry and every no-spend gate;
//  2. durably persist the PENDING reservation BEFORE the send — if that
//     write does not succeed, the send count is ZERO;
//  3. send exactly once;
//  4. finalize: only a confirmed cache hit PLUS successful final persistence
//     clears the pending state; rebuilt, unknown-outcome, and refusals stop
//     permanently. Any failure between send and finalize leaves the durable
//     pending entry, so a later tick sends zero (replay/crash invariant; the
//     sequential scheduler needs no CAS).
func refreshOne(entry *warmEntry, cfg config, key string, now int64) (bool, string, error) {
	// REPLAY INTEGRITY FIRST (batch-3 boundary): every validation below runs
	// BEFORE pricing. Each failure stops the entry with ZERO pricing and ZERO
	// sends, and the stop reason is persisted durably.
	req, err := sdk.DecodeRequest(entry.PrefixPB)
	if err != nil {
		entry.Stopped = "stored prefix is unreadable"
		persistStop(key, entry)
		return false, "", nil
	}
	if req.Model != entry.Model {
		entry.Stopped = "stored prefix model mismatch"
		persistStop(key, entry)
		return false, "", nil
	}
	// SDK replacement domain + marker presence (the projection is the same
	// identity oracle the request path used).
	prefix, hasBreakpoint, err := pbv2.RequestObservablePrefix(req)
	if err != nil {
		entry.Stopped = "stored prefix is out of domain"
		persistStop(key, entry)
		return false, "", nil
	}
	if !hasBreakpoint {
		entry.Stopped = "stored prefix has no cache breakpoint"
		persistStop(key, entry)
		return false, "", nil
	}
	// The warmed identity must be the priced identity: the recomputed
	// domain-separated fingerprint has to equal the stored one.
	if prefixFingerprint(prefix) != entry.PrefixFingerprint {
		entry.Stopped = "stored prefix drifted"
		persistStop(key, entry)
		return false, "", nil
	}
	// Defensive durable-state validation (the request path already declined
	// terminal suffixes before storing): a prefix ending on an unanswered
	// tool call is not sendable on its own.
	if endsWithUnansweredToolCall(req) {
		entry.Stopped = "prefix ends on an unanswered tool call"
		persistStop(key, entry)
		return false, "", nil
	}

	// No-spend gates next: nothing durable happens until every one passes.
	pricing, err := sdk.GetCachePricing(entry.Provider, entry.Model)
	if err != nil {
		if isAdvisory(err) {
			entry.Stopped = "pricing unavailable"
			persistStop(key, entry)
			return false, fmt.Sprintf("%s: stopped, pricing unavailable", short(entry.ConversationID)), nil
		}
		return false, "", err
	}
	if !pricing.Available() {
		entry.Stopped = "pricing unavailable"
		persistStop(key, entry)
		return false, fmt.Sprintf("%s: stopped, pricing unavailable", short(entry.ConversationID)), nil
	}
	if !pricing.Warmable() {
		// Automatic prefix caching: no lifetime the caller owns, so nothing a
		// request can keep alive.
		entry.Stopped = "provider cache does not refresh on read"
		persistStop(key, entry)
		return false, fmt.Sprintf("%s: stopped, %s cache cannot be refreshed", short(entry.ConversationID), entry.Provider), nil
	}

	if entry.DeadlineMillis > 0 && now >= entry.DeadlineMillis {
		entry.Stopped = "deadline reached"
		persistStop(key, entry)
		return false, fmt.Sprintf("%s: stopped, deadline reached", short(entry.ConversationID)), nil
	}
	if pricing.BreakEvenRefreshes > 0 && entry.RefreshesSpent >= pricing.BreakEvenRefreshes {
		entry.Stopped = "break-even reached"
		persistStop(key, entry)
		return false, fmt.Sprintf("%s: stopped after %d refreshes, past break-even",
			short(entry.ConversationID), entry.RefreshesSpent), nil
	}

	// An interval override at or beyond the provider's shortest cache
	// lifetime would always arrive after the entry expired — the refresh
	// could only rebuild and waste money. Zero means the provider-derived
	// cadence; anything else must be strictly inside the lifetime.
	if cfg.IntervalSecondsOverride > 0 &&
		cfg.IntervalSecondsOverride >= pricing.ShortestTTLSeconds {
		entry.Stopped = "refresh interval exceeds cache lifetime"
		persistStop(key, entry)
		return false, fmt.Sprintf("%s: stopped, refresh interval %ds not below the %ds cache lifetime",
			short(entry.ConversationID), cfg.IntervalSecondsOverride, pricing.ShortestTTLSeconds), nil
	}
	interval := int64(pricing.WarmIntervalSeconds) * 1000
	if cfg.IntervalSecondsOverride > 0 {
		interval = int64(cfg.IntervalSecondsOverride) * 1000
	}
	if interval <= 0 {
		entry.Stopped = "no refresh interval available"
		persistStop(key, entry)
		return false, "", nil
	}
	last := entry.LastRefreshMillis
	if last == 0 {
		last = entry.LastSeenMillis
	}
	if now-last < interval {
		return false, "", nil // not due yet
	}

	// WRITE-AHEAD RESERVATION: persist the pending state BEFORE spending. If
	// this write does not succeed, the send count is ZERO — the entry stays
	// as it was and a later tick may try again.
	pending := *entry
	pending.Stopped = "refresh outcome unknown"
	pending.AttemptMillis = now
	if err := sdk.StateSetJSON(key, &pending); err != nil {
		if isAdvisory(err) {
			return false, "", nil
		}
		return false, "", err
	}

	// Send the stored prefix verbatim, appending nothing. One output token,
	// because the point is to touch the entry rather than to get an answer.
	one := int32(1)
	req.MaxTokens = &one
	res, err := sdk.SendRequest(req, sdk.SendRequestOptions{
		Provider: entry.Provider,
		Path:     entry.Path,
	})
	entry.LastRefreshMillis = now
	entry.RefreshesSpent++

	if err != nil {
		var refusal *sdk.HostCallRefusalError
		switch {
		case errors.As(err, &refusal):
			if isAdvisory(err) {
				entry.Stopped = "refresh failed"
				persistStop(key, entry)
				return true, fmt.Sprintf("%s: stopped, refresh failed", short(entry.ConversationID)), nil
			}
			// Contract/protocol refusal: surface on the tick. The durable
			// pending reservation still prevents replay.
			return true, "", err
		case res.HTTPStatus != 0:
			// Upstream non-2xx: the provider refused the refresh. The result
			// carries the status; no string branching.
			entry.Stopped = "refresh failed"
			persistStop(key, entry)
			return true, fmt.Sprintf("%s: stopped, refresh failed (HTTP %d)", short(entry.ConversationID), res.HTTPStatus), nil
		default:
			// Local/protocol decode defect.
			return true, "", err
		}
	}

	switch {
	case res.CacheRebuilt():
		// The entry had already lapsed and this refresh paid to recreate it.
		entry.Stopped = "cache had already expired"
		persistStop(key, entry)
		return true, fmt.Sprintf("%s: stopped, cache had already expired", short(entry.ConversationID)), nil
	case res.CacheHit():
		// The ONLY confirmed outcome allowed to clear the pending state — and
		// only if the final persistence succeeds (otherwise the durable
		// pending entry stays authoritative: zero further sends).
		entry.Stopped = ""
		entry.AttemptMillis = 0
		if err := sdk.StateSetJSON(key, entry); err != nil {
			if isAdvisory(err) {
				// Pending remains durable; the accounting is lost but replay
				// is prevented — the safe direction.
				return true, "", nil
			}
			return true, "", err
		}
		return true, "", nil
	default:
		// Missing usage or both counters zero: unknown outcome. Never clear
		// the stopped/pending state and never retry automatically.
		entry.Stopped = "refresh outcome unknown"
		persistStop(key, entry)
		return true, fmt.Sprintf("%s: stopped, refresh outcome unknown", short(entry.ConversationID)), nil
	}
}

// endsWithUnansweredToolCall reports whether the FINAL message is an
// assistant turn containing any tool_use block (ordered request-block
// model) — a prefix ending on an unanswered tool call is not a request that
// can be sent on its own, and the provider rejects the turn without a tool
// result. The check inspects only the final message's explicit blocks; it is
// NOT a general body traversal.
func endsWithUnansweredToolCall(req *pbv2.ChatRequest) bool {
	if len(req.Messages) == 0 {
		return false
	}
	last := req.Messages[len(req.Messages)-1]
	if last.Role != "assistant" {
		return false
	}
	for _, b := range last.Blocks {
		if b.GetToolUse() != nil {
			return true
		}
	}
	return false
}

// sanitizeReplay builds the replay artifact: a CLONE of the accepted request
// with stream=false and torana_meta_json cleared, preserving every
// provider-visible field and ordered block exactly. The hook input is never
// mutated.
func sanitizeReplay(req *pbv2.ChatRequest) *pbv2.ChatRequest {
	out := proto.Clone(req).(*pbv2.ChatRequest)
	out.Stream = false
	out.ToranaMetaJson = nil
	return out
}

// prefixFingerprint is the ONE production helper for the durable identity: a
// fixed, domain-separated digest of the SDK observable projection, used on
// BOTH write and replay validation. The projection bytes themselves are
// unbounded and never stored in JSON state.
func prefixFingerprint(prefix []byte) string {
	return sdk.ContentAddressedCacheKey("cache_warmer/prefix", string(prefix))
}

type hostMeta struct {
	Provider       string `json:"_provider"`
	ConversationID string `json:"_conversation_id"`
	Path           string `json:"_path"`
}

func readHostMeta(req *pbv2.ChatRequest) hostMeta {
	var meta hostMeta
	if len(req.ToranaMetaJson) == 0 {
		return meta
	}
	_ = json.Unmarshal(req.ToranaMetaJson, &meta)
	return meta
}

// validateEntry checks the durable-state SHAPE of an entry and returns a stop
// reason, or "" when the entry is coherent. It runs before any pricing or
// spend-related decision. The key must bind EXACTLY to the conversation
// (warm/<conversation_id> — a suffix match would let warm/attacker/conv-1
// masquerade as conv-1); every accounting/timestamp field must be
// nonnegative; and a fresh state (Stopped == "") must carry no prior attempt
// (an orphaned positive AttemptMillis would let a crashed reservation spend
// again). There is no compatibility handling for old entries.
func validateEntry(entry *warmEntry, key string) string {
	if entry.SchemaVersion != schemaVersion {
		return "unsupported entry schema"
	}
	if key != entryPrefix+entry.ConversationID {
		return "invalid warm entry"
	}
	if entry.Provider == "" || entry.Model == "" || entry.Path == "" || entry.ConversationID == "" {
		return "invalid warm entry"
	}
	if entry.RefreshesSpent < 0 || entry.LastSeenMillis < 0 || entry.LastRefreshMillis < 0 ||
		entry.DeadlineMillis < 0 || entry.AttemptMillis < 0 {
		return "invalid warm entry"
	}
	if entry.PrefixFingerprint == "" {
		return "invalid warm entry"
	}
	if entry.Stopped == "" && entry.AttemptMillis != 0 {
		return "invalid warm entry"
	}
	return ""
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func joinNotes(notes []string) string {
	switch len(notes) {
	case 0:
		return ""
	case 1:
		return notes[0]
	}
	out := notes[0]
	for _, n := range notes[1:] {
		out += "; " + n
	}
	return out
}
