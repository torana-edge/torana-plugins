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
)

func main() {}

// schemaVersion marks the durable entry format written by this plugin. Any
// other version — including anything a v1 plugin stored — is stopped with
// zero sends.
const schemaVersion = 2

// warmEntry is everything needed to refresh one conversation, stored durably so
// a restart does not lose track of what it was keeping alive.
type warmEntry struct {
	SchemaVersion  int    `json:"schema_version"`
	ConversationID string `json:"conversation_id"`
	Provider       string `json:"provider"`
	Model          string `json:"model"`
	Path           string `json:"path"`

	// PrefixPB is the base64 protobuf of the cached prefix: the conversation
	// truncated at its last cache breakpoint. Storing the prefix rather than
	// the whole conversation keeps the refresh request small and means the
	// bytes sent are exactly the bytes the provider has cached.
	PrefixPB string `json:"prefix_pb"`

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
func parseConfig(raw string) config {
	var cfg config
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &cfg)
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

		prefix := prefixThroughBreakpoint(req)
		if prefix == nil {
			// No breakpoint means no explicitly cached prefix to refresh.
			return sdk.PassRequest(), nil
		}
		encoded, err := sdk.EncodeRequest(prefix)
		if err != nil {
			// Local encode failure: a protocol/plugin defect, not a condition
			// to absorb.
			return sdk.RequestResult{}, fmt.Errorf("cache_warmer: encode prefix: %w", err)
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
			SchemaVersion:  schemaVersion,
			ConversationID: meta.ConversationID,
			Provider:       meta.Provider,
			Model:          req.Model,
			Path:           meta.Path,
			PrefixPB:       encoded,
			LastSeenMillis: now,
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
	// No-spend gates first: nothing durable happens until every one passes.
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

	// Durable-entry validation (schema + identity) BEFORE any reservation.
	if entry.SchemaVersion != schemaVersion {
		entry.Stopped = "unsupported entry schema"
		persistStop(key, entry)
		return false, fmt.Sprintf("%s: stopped, unsupported entry schema", short(entry.ConversationID)), nil
	}
	if entry.Provider == "" || entry.Model == "" || entry.Path == "" || entry.ConversationID == "" ||
		!strings.HasSuffix(key, "/"+entry.ConversationID) ||
		entry.RefreshesSpent < 0 || entry.LastSeenMillis < 0 {
		entry.Stopped = "invalid warm entry"
		persistStop(key, entry)
		return false, fmt.Sprintf("%s: stopped, invalid warm entry", short(entry.ConversationID)), nil
	}

	req, err := sdk.DecodeRequest(entry.PrefixPB)
	if err != nil {
		entry.Stopped = "stored prefix is unreadable"
		persistStop(key, entry)
		return false, "", nil
	}
	if req.Model != entry.Model {
		entry.Stopped = "stored prefix model mismatch"
		persistStop(key, entry)
		return false, fmt.Sprintf("%s: stopped, stored prefix model mismatch", short(entry.ConversationID)), nil
	}
	// A prefix ending on an unanswered tool call is not a request that can be
	// sent on its own — the provider expects a tool result next, and rejects
	// the turn without one.
	if endsWithUnansweredToolCall(req) {
		entry.Stopped = "prefix ends on an unanswered tool call"
		persistStop(key, entry)
		return false, fmt.Sprintf("%s: stopped, cached prefix ends mid tool call",
			short(entry.ConversationID)), nil
	}
	if prefixThroughBreakpoint(req) == nil {
		entry.Stopped = "stored prefix has no cache breakpoint"
		persistStop(key, entry)
		return false, fmt.Sprintf("%s: stopped, stored prefix has no cache breakpoint", short(entry.ConversationID)), nil
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

// endsWithUnansweredToolCall reports whether the last message is an assistant
// turn holding tool calls that nothing answers.
func endsWithUnansweredToolCall(req *pbv2.ChatRequest) bool {
	if len(req.Messages) == 0 {
		return false
	}
	last := req.Messages[len(req.Messages)-1]
	return last.Role == "assistant" && len(last.ToolCalls) > 0
}

// prefixThroughBreakpoint returns a copy of the request truncated at its last
// cache breakpoint — the bytes the provider actually has cached.
func prefixThroughBreakpoint(req *pbv2.ChatRequest) *pbv2.ChatRequest {
	last := -1
	for i, m := range req.Messages {
		if len(m.CacheControlJson) > 0 {
			last = i
		}
	}
	if last < 0 {
		return nil
	}
	clone := &pbv2.ChatRequest{
		Model:    req.Model,
		Tools:    req.Tools,
		Messages: req.Messages[:last+1],
	}
	return clone
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
