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
// # Safety semantics
//
//   - Durable entries carry the current schema version; unsupported versions
//     stop with zero sends rather than being guessed or partially decoded.
//   - State refusals split by class: advisory (NOT_CONFIGURED/UNAVAILABLE)
//     declines safely; contract/protocol refusals error the hook so a broken
//     enabled plugin is visible. Corrupt stored JSON is a key-local data
//     error: that entry is skipped, others are unaffected.
//   - The request path is observational and never mutates the request.
//   - The replay artifact is bounded (see the prefix budget). A conversation
//     whose artifact does not fit is NOT warmed, and the entry says so; the
//     warmer never refreshes a partial prefix or reports warming it did not
//     perform.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"google.golang.org/protobuf/proto"
)

func main() {}

// schemaVersion marks the durable entry format written by this plugin. Any
// other version stops with zero sends. The format requires the
// domain-separated PrefixFingerprint and has no fallback decoder. Version 2
// moved the replay artifact out of the entry into bounded part values (see
// the prefix budget below); a version-1 entry has no readable prefix and is
// stopped rather than guessed at.
const schemaVersion = 2

// The prefix budget.
//
// The replay artifact is the whole request, base64-encoded, and a realistic
// coding conversation is far larger than one durable value: Torana's store
// bounds a single value at 256 KiB by default, which a conversation passes
// somewhere around forty thousand tokens — well inside the range where
// prompt caching is worth paying for in the first place. Storing it as one
// value meant the warmer failed exactly where warming matters most.
//
// So the artifact is split across numbered part values, and the budget is
// declared here rather than discovered at the store:
//
//   - maxPartBytes keeps each value comfortably inside the default limit,
//     with room for the key and the store's own overhead;
//   - maxPrefixParts caps one conversation's artifact. It is a hard ceiling,
//     not a target: the per-plugin key cap and the store's total byte budget
//     are shared with every other plugin, and warming is opt-in for a handful
//     of conversations at a time.
//
// Above the ceiling the warmer does NOT warm. It records why, durably, and
// spends nothing — see declineWarming. The alternative (store what fits and
// refresh a truncated prefix) would send a request that is not the
// conversation, pay for it, and report success.
const (
	maxPartBytes   = 128 << 10 // 128 KiB per durable value
	maxPrefixParts = 8         // ~1 MiB of encoded replay per conversation
)

// Stop reasons for an artifact that cannot be persisted. They are durable
// state, not log lines: this plugin holds no logging grant, and the entry is
// where an operator (or `plugin-state.json`) can see that a conversation they
// opted in is deliberately not being warmed.
const (
	stopPrefixTooLarge      = "replay artifact exceeds the prefix budget"
	stopPrefixStorageFailed = "durable state refused the replay artifact"
)

// warmEntry is everything needed to refresh one conversation, stored durably so
// a restart does not lose track of what it was keeping alive.
type warmEntry struct {
	SchemaVersion  int    `json:"schema_version"`
	ConversationID string `json:"conversation_id"`
	Provider       string `json:"provider"`
	Model          string `json:"model"`
	Path           string `json:"path"`

	// PrefixDigest and PrefixParts locate the SANITIZED REPLAY REQUEST: a
	// clone of the accepted request with stream=false and torana_meta_json
	// cleared, preserving every provider-visible field and ordered block
	// exactly, base64-encoded and split across PrefixParts durable values
	// under PrefixDigest (see partKey). It is the artifact replayed on a
	// warming tick (with max_tokens set to 1), not a truncated prefix.
	//
	// The digest is content-addressed, which is what makes the split safe:
	// part keys are a pure function of the bytes they hold, so rewriting an
	// artifact never overwrites a committed one, and reassembly verifies the
	// digest before anything is decoded or sent. The ENTRY is the commit
	// point — parts are written first, and an entry naming them is what
	// makes them live.
	PrefixDigest string `json:"prefix_digest"`
	PrefixParts  int    `json:"prefix_parts"`

	// PrefixFingerprint is the fixed, domain-separated digest of the SDK
	// observable projection at observation time — the identity the replay
	// must match. Required non-empty before any pricing call.
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

func loadConfig() (config, error) {
	raw, err := sdk.PluginConfig()
	if err != nil {
		return config{}, fmt.Errorf("cache_warmer: load plugin config: %w", err)
	}
	return parseConfig(raw), nil
}

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

const (
	entryPrefix = "warm/"
	partPrefix  = "part/"
)

// isAdvisory reports whether err is an advisory refusal (NOT_CONFIGURED or
// UNAVAILABLE) — the operator/transient class a plugin may decline safely.
func isAdvisory(err error) bool {
	if errors.Is(err, sdk.ErrStateUnavailable) {
		return true
	}
	var refusal *sdk.HostCallRefusalError
	if errors.As(err, &refusal) {
		return refusal.Code == pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED ||
			refusal.Code == pbv1.ErrorCode_ERROR_CODE_UNAVAILABLE
	}
	return false
}

// isStoreRejection reports whether err is the durable store refusing the
// VALUE rather than the call: over the per-value limit, over the per-plugin
// key cap, or over the store's total byte budget. The host reports those as
// INTERNAL, which is neither advisory (retrying changes nothing) nor a
// contract defect by this plugin (the call was well formed and permitted).
// It is the signal that this conversation cannot be warmed.
func isStoreRejection(err error) bool {
	var refusal *sdk.HostCallRefusalError
	return errors.As(err, &refusal) && refusal.Code == pbv1.ErrorCode_ERROR_CODE_INTERNAL
}

// replayDigest is the content address of an encoded replay artifact. It names
// the artifact's part keys and is re-verified on reassembly, so a partially
// written or partially collected artifact can never be decoded as a whole one.
func replayDigest(encoded string) string {
	sum := sha256.Sum256([]byte(encoded))
	return hex.EncodeToString(sum[:])
}

func partKey(digest string, index int) string {
	return fmt.Sprintf("%s%s/%d", partPrefix, digest, index)
}

// partDigestOf reads the digest back out of a part key. An unparseable key in
// this plugin's own namespace has no owner and is garbage by definition.
func partDigestOf(key string) string {
	rest := strings.TrimPrefix(key, partPrefix)
	if i := strings.LastIndex(rest, "/"); i > 0 {
		return rest[:i]
	}
	return ""
}

// storePrefix writes the artifact's parts and returns the digest and count an
// entry needs to find them again. Parts are written BEFORE the entry that
// names them: a crash here leaves values nothing references, which the tick
// collects, rather than an entry pointing at bytes that were never stored.
func storePrefix(encoded string) (string, int, error) {
	if encoded == "" {
		return "", 0, fmt.Errorf("cache_warmer: empty replay artifact")
	}
	digest := replayDigest(encoded)
	parts := 0
	for offset := 0; offset < len(encoded); offset += maxPartBytes {
		end := offset + maxPartBytes
		if end > len(encoded) {
			end = len(encoded)
		}
		if err := sdk.StateSet(partKey(digest, parts), encoded[offset:end]); err != nil {
			return "", 0, err
		}
		parts++
	}
	return digest, parts, nil
}

// loadPrefix reassembles the artifact an entry names.
//
// ok is false when the artifact is not intact — a missing part, or bytes that
// do not hash to the recorded digest. That is a data defect that stops the
// entry; it is never treated as absence, and never sent. An error is a host
// failure the caller classifies.
func loadPrefix(entry *warmEntry) (string, bool, error) {
	var buf strings.Builder
	for i := 0; i < entry.PrefixParts; i++ {
		part, found, err := sdk.StateGet(partKey(entry.PrefixDigest, i))
		if err != nil {
			return "", false, err
		}
		if !found {
			return "", false, nil
		}
		buf.WriteString(part)
	}
	encoded := buf.String()
	if encoded == "" || replayDigest(encoded) != entry.PrefixDigest {
		return "", false, nil
	}
	return encoded, true, nil
}

// collectPrefixParts deletes every part value no live entry names.
//
// Necessary, not hygiene: each real turn stores a larger artifact under a new
// digest, so without collection one warmed conversation would add parts on
// every turn until it hit the store's key cap or its byte budget — and that
// budget is shared with every other plugin.
//
// referenced is built from the entries this tick actually read, so a part
// written by a request whose entry has not committed yet can be collected.
// The cost of losing that race is bounded and visible: the entry that arrives
// afterwards names parts that are gone, the next tick stops it with
// "stored prefix is incomplete" having sent nothing, and the next real turn
// stores the artifact again.
func collectPrefixParts(keys []string, referenced map[string]bool) error {
	for _, key := range keys {
		if !strings.HasPrefix(key, partPrefix) {
			continue
		}
		if referenced[partDigestOf(key)] {
			continue
		}
		if err := sdk.StateDelete(key); err != nil && !isAdvisory(err) {
			return err
		}
	}
	return nil
}

type thinkingReplayStatus uint8

const (
	thinkingReplaySafe thinkingReplayStatus = iota
	thinkingReplayManualBudget
	thinkingReplayUnsupported
)

// classifyThinkingReplay parses the closed provider-level thinking union that
// affects the warmer's one-token output limit. Manual enabled thinking retains
// budget_tokens and is invalid when max_tokens becomes one. Disabled and
// adaptive thinking carry no manual budget and remain valid. Unknown or
// malformed arms fail closed rather than being guessed.
func classifyThinkingReplay(req *pbv1.ChatRequest) thinkingReplayStatus {
	if len(req.ProviderExtensionsJson) == 0 {
		return thinkingReplaySafe
	}
	var extensions map[string]json.RawMessage
	if err := json.Unmarshal(req.ProviderExtensionsJson, &extensions); err != nil {
		return thinkingReplayUnsupported
	}
	raw, ok := extensions["thinking"]
	if !ok {
		return thinkingReplaySafe
	}
	var cfg struct {
		Type         string `json:"type"`
		BudgetTokens *int64 `json:"budget_tokens"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return thinkingReplayUnsupported
	}
	switch cfg.Type {
	case "enabled":
		if cfg.BudgetTokens == nil || *cfg.BudgetTokens <= 0 {
			return thinkingReplayUnsupported
		}
		return thinkingReplayManualBudget
	case "disabled", "adaptive":
		if cfg.BudgetTokens != nil {
			return thinkingReplayUnsupported
		}
		return thinkingReplaySafe
	default:
		return thinkingReplayUnsupported
	}
}

func init() {
	// Request path: remember the cached prefix of any conversation the operator
	// opted in. This hook only observes and stores — it never modifies the
	// request, so it cannot affect the prefix it is trying to preserve.
	sdk.OnBeforeRequest(func(ctx context.Context, req *pbv1.ChatRequest) (sdk.RequestResult, error) {
		cfg, err := loadConfig()
		if err != nil {
			return sdk.RequestResult{}, err
		}
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
		prefix, hasBreakpoint, err := pbv1.RequestObservablePrefix(req)
		if err != nil {
			return sdk.PassRequest(), nil
		}
		if !hasBreakpoint {
			return sdk.PassRequest(), nil
		}
		if classifyThinkingReplay(req) != thinkingReplaySafe {
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
			PrefixFingerprint: prefixFingerprint(prefix),
			LastSeenMillis:    now,
		}
		// A real turn resets the budget: the user is active, the cache was just
		// refreshed for free, and whatever was spent before is spent.
		if cfg.WarmForMinutes > 0 {
			entry.DeadlineMillis = now + int64(cfg.WarmForMinutes)*60_000
		}
		key := entryPrefix + meta.ConversationID

		// The bound is checked HERE, before anything is written, so an
		// artifact that cannot be persisted never half-lands.
		if len(encoded) > maxPrefixParts*maxPartBytes {
			if err := declineWarming(key, entry, stopPrefixTooLarge); err != nil {
				return sdk.RequestResult{}, err
			}
			return sdk.PassRequest(), nil
		}
		digest, parts, err := storePrefix(encoded)
		if err != nil {
			switch {
			case isAdvisory(err):
				// No durable state at all: pass with no entry and no future
				// spend, exactly as an unavailable clock does.
				return sdk.PassRequest(), nil
			case isStoreRejection(err):
				// The store would not take the artifact (a lower configured
				// value limit, the key cap, a full store). Record it where an
				// operator can see it instead of claiming warming.
				if err := declineWarming(key, entry, stopPrefixStorageFailed); err != nil {
					return sdk.RequestResult{}, err
				}
				return sdk.PassRequest(), nil
			default:
				return sdk.RequestResult{}, err
			}
		}
		entry.PrefixDigest = digest
		entry.PrefixParts = parts

		// The entry is the commit point: until it names them, the parts above
		// are unreferenced and the next tick collects them.
		if err := sdk.StateSetJSON(key, entry); err != nil {
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
	sdk.OnTick(func(ctx context.Context, tick *pbv1.TickRequest) (sdk.TickResult, error) {
		cfg, err := loadConfig()
		if err != nil {
			return sdk.TickResult{}, err
		}
		keys, err := sdk.StateKeys()
		if err != nil {
			return sdk.TickResult{}, err
		}

		refreshed := 0
		var notes []string
		// Digests the live entries name. Everything else under partPrefix is
		// a previous turn's artifact, a collected entry's, or a crash
		// remnant, and is deleted once the scan below is complete.
		referenced := make(map[string]bool, len(keys))
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
			raw, found, err := sdk.StateGet(key)
			switch {
			case err != nil:
				if isAdvisory(err) {
					continue
				}
				return sdk.TickResult{}, err
			case !found:
				continue
			case json.Unmarshal([]byte(raw), &entry) != nil:
				continue // corrupt stored JSON: key-local
			}
			if entry.Stopped != "" {
				// Pending or terminal: no further spend, ever, until a real
				// turn rewrites the entry.
				continue
			}
			if !cfg.warms(entry.ConversationID) {
				// Opted out since the entry was written: delete it, and leave
				// its artifact unreferenced so the collection below reclaims
				// it. Advisory unavailability can be retried next tick;
				// contract defects surface.
				if err := sdk.StateDelete(key); err != nil && !isAdvisory(err) {
					return sdk.TickResult{}, err
				}
				continue
			}
			if entry.PrefixDigest != "" {
				// Guarded: an entry naming nothing must not make the empty
				// digest "referenced", which is what an unparseable part key
				// resolves to — those are junk and must stay collectable.
				referenced[entry.PrefixDigest] = true
			}
			// Durable-state shape validation is the FIRST step after decoding
			// — before pricing and before any spend-related decision. Invalid
			// entries must never reach a pricing call or a send.
			entry.Stopped = validateEntry(&entry, key)
			if entry.Stopped != "" {
				if err := persistStop(key, &entry); err != nil {
					return sdk.TickResult{}, err
				}
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

		if err := collectPrefixParts(keys, referenced); err != nil {
			return sdk.TickResult{}, err
		}

		if refreshed == 0 && len(notes) == 0 {
			return sdk.TickIdle(), nil
		}
		return sdk.TickDid(int32(refreshed), joinNotes(notes)), nil
	})
}

// persistStop writes a terminal stop reason. Advisory state unavailability can
// be retried on a later tick; contract, protocol, and transport defects surface.
func persistStop(key string, entry *warmEntry) error {
	err := sdk.StateSetJSON(key, entry)
	if err != nil && isAdvisory(err) {
		return nil
	}
	return err
}

// declineWarming records that this conversation is NOT being warmed, and why.
//
// The entry carries no artifact, so the tick skips it (Stopped is set) and
// spends nothing, and the reason survives a restart. Silence was the failure
// this replaces: an operator who opted a conversation in would otherwise see
// a plugin that was enabled, configured, and doing nothing.
func declineWarming(key string, entry warmEntry, reason string) error {
	entry.PrefixDigest = ""
	entry.PrefixParts = 0
	entry.Stopped = reason
	return persistStop(key, &entry)
}

func stopped(key string, entry *warmEntry, action bool, note string) (bool, string, error) {
	return action, note, persistStop(key, entry)
}

// refreshOne decides and, if warranted, sends — under the write-ahead
// reservation. It mutates entry in place and reports whether a refresh was
// CONFIRMED COMPLETED (a cache hit or a rebuilt cache — the host's Actions
// field counts completed refreshes; refused or unknown-outcome sends are
// attempts, not actions, and still carry their explanatory note), a
// human-readable outcome, and any error that must surface on the tick.
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
	encoded, intact, err := loadPrefix(entry)
	if err != nil {
		if isAdvisory(err) {
			// Durable state is unconfigured or temporarily unavailable:
			// nothing about THIS entry is wrong, so leave it alone and try
			// again on a later tick rather than stopping it permanently.
			return false, "", nil
		}
		return false, "", err
	}
	if !intact {
		// A missing part or bytes that do not match the recorded digest. The
		// artifact is not the conversation, so it is never sent.
		entry.Stopped = "stored prefix is incomplete"
		return stopped(key, entry, false, fmt.Sprintf("%s: stopped, stored prefix is incomplete", short(entry.ConversationID)))
	}
	req, err := sdk.DecodeRequest(encoded)
	if err != nil {
		entry.Stopped = "stored prefix is unreadable"
		return stopped(key, entry, false, "")
	}
	if req.Model != entry.Model {
		entry.Stopped = "stored prefix model mismatch"
		return stopped(key, entry, false, "")
	}
	// SDK replacement domain + marker presence (the projection is the same
	// identity oracle the request path used).
	prefix, hasBreakpoint, err := pbv1.RequestObservablePrefix(req)
	if err != nil {
		entry.Stopped = "stored prefix is out of domain"
		return stopped(key, entry, false, "")
	}
	if !hasBreakpoint {
		entry.Stopped = "stored prefix has no cache breakpoint"
		return stopped(key, entry, false, "")
	}
	// The warmed identity must be the priced identity: the recomputed
	// domain-separated fingerprint has to equal the stored one.
	if prefixFingerprint(prefix) != entry.PrefixFingerprint {
		entry.Stopped = "stored prefix drifted"
		return stopped(key, entry, false, "")
	}
	// Defensive durable-state validation (the request path already declined
	// terminal suffixes before storing): a prefix ending on an unanswered
	// tool call is not sendable on its own.
	if endsWithUnansweredToolCall(req) {
		entry.Stopped = "prefix ends on an unanswered tool call"
		return stopped(key, entry, false, fmt.Sprintf("%s: stopped, prefix ends on an unanswered tool call", short(entry.ConversationID)))
	}
	switch classifyThinkingReplay(req) {
	case thinkingReplayManualBudget:
		entry.Stopped = "manual thinking budget is not warmable"
		return stopped(key, entry, false, fmt.Sprintf("%s: stopped, manual thinking budget is not warmable", short(entry.ConversationID)))
	case thinkingReplayUnsupported:
		entry.Stopped = "unsupported thinking configuration"
		return stopped(key, entry, false, fmt.Sprintf("%s: stopped, unsupported thinking configuration", short(entry.ConversationID)))
	}

	// No-spend gates next: nothing durable happens until every one passes.
	policy, err := sdk.GetPromptCachePolicy("warm-cache")
	if err != nil {
		if isAdvisory(err) {
			entry.Stopped = "pricing unavailable"
			return stopped(key, entry, false, fmt.Sprintf("%s: stopped, pricing unavailable", short(entry.ConversationID)))
		}
		return false, "", err
	}
	if !policy.RefreshOnRead {
		// Automatic prefix caching: no lifetime the caller owns, so nothing a
		// request can keep alive.
		entry.Stopped = "provider cache does not refresh on read"
		return stopped(key, entry, false, fmt.Sprintf("%s: stopped, %s cache cannot be refreshed", short(entry.ConversationID), entry.Provider))
	}
	shortestTTL, hasTTL := sdk.ShortestPromptCacheTTL(policy)
	breakEvenRefreshes, hasEconomics := sdk.PromptCacheBreakEvenRefreshes(policy)
	if !hasTTL || !hasEconomics {
		entry.Stopped = "pricing unavailable"
		return stopped(key, entry, false, fmt.Sprintf("%s: stopped, pricing unavailable", short(entry.ConversationID)))
	}

	if entry.DeadlineMillis > 0 && now >= entry.DeadlineMillis {
		entry.Stopped = "deadline reached"
		return stopped(key, entry, false, fmt.Sprintf("%s: stopped, deadline reached", short(entry.ConversationID)))
	}
	if entry.RefreshesSpent >= breakEvenRefreshes {
		entry.Stopped = "break-even reached"
		return stopped(key, entry, false, fmt.Sprintf("%s: stopped after %d refreshes, past break-even",
			short(entry.ConversationID), entry.RefreshesSpent))
	}

	// An interval override at or beyond the provider's shortest cache
	// lifetime would always arrive after the entry expired — the refresh
	// could only rebuild and waste money. Zero means the provider-derived
	// cadence; anything else must be strictly inside the lifetime.
	if cfg.IntervalSecondsOverride > 0 &&
		cfg.IntervalSecondsOverride >= int(shortestTTL) {
		entry.Stopped = "refresh interval exceeds cache lifetime"
		return stopped(key, entry, false, fmt.Sprintf("%s: stopped, refresh interval %ds not below the %ds cache lifetime",
			short(entry.ConversationID), cfg.IntervalSecondsOverride, shortestTTL))
	}
	interval := int64(policy.GetWarmIntervalSeconds()) * 1000
	if cfg.IntervalSecondsOverride > 0 {
		interval = int64(cfg.IntervalSecondsOverride) * 1000
	}
	if interval <= 0 {
		entry.Stopped = "no refresh interval available"
		return stopped(key, entry, false, "")
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
				return stopped(key, entry, false, fmt.Sprintf("%s: stopped, refresh failed", short(entry.ConversationID)))
			}
			// Contract/protocol refusal: surface on the tick. The durable
			// pending reservation still prevents replay. Not a completed
			// action (the bool contract: confirmed-completed only).
			return false, "", err
		case res.HTTPStatus != 0:
			// Upstream non-2xx: the provider refused the refresh. The result
			// carries the status; no string branching. Not a completed
			// action.
			entry.Stopped = "refresh failed"
			return stopped(key, entry, false, fmt.Sprintf("%s: stopped, refresh failed (HTTP %d)", short(entry.ConversationID), res.HTTPStatus))
		default:
			// Local/protocol decode defect.
			return false, "", err
		}
	}

	switch {
	case res.CacheRebuilt():
		// The entry had already lapsed and this refresh paid to recreate it.
		entry.Stopped = "cache had already expired"
		return stopped(key, entry, true, fmt.Sprintf("%s: stopped, cache had already expired", short(entry.ConversationID)))
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
		// the stopped/pending state and never retry automatically. Not a
		// completed action.
		entry.Stopped = "refresh outcome unknown"
		return stopped(key, entry, false, fmt.Sprintf("%s: stopped, refresh outcome unknown", short(entry.ConversationID)))
	}
}

// endsWithUnansweredToolCall reports whether the FINAL message is an
// assistant turn containing any tool_use block (ordered request-block
// model) — a prefix ending on an unanswered tool call is not a request that
// can be sent on its own, and the provider rejects the turn without a tool
// result. The check inspects only the final message's explicit blocks; it is
// NOT a general body traversal.
func endsWithUnansweredToolCall(req *pbv1.ChatRequest) bool {
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
func sanitizeReplay(req *pbv1.ChatRequest) *pbv1.ChatRequest {
	out := proto.Clone(req).(*pbv1.ChatRequest)
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

func readHostMeta(req *pbv1.ChatRequest) hostMeta {
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
	// The artifact locator must be within the declared budget: a part count
	// of zero has nothing to replay, and one above the ceiling was never
	// written by this plugin.
	if entry.PrefixDigest == "" || entry.PrefixParts < 1 || entry.PrefixParts > maxPrefixParts {
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
