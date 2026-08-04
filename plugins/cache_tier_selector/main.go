// cache_tier_selector picks which prompt-cache lifetime to buy for a
// conversation.
//
// Providers that sell explicit cache breakpoints often sell more than one
// lifetime at different write prices — Anthropic's are 5 minutes at 1.25x the
// base input rate and 1 hour at 2.0x. Which is cheaper depends entirely on how
// long the conversation is about to sit idle, and a harness always asks for the
// default.
//
// The arithmetic, for a prefix of P tokens at base rate B:
//
//	short tier, cache lapses:   pay (1.25 - 1.0) x B x P again on resume
//	long tier, bought upfront:  pay (2.0 - 1.25) x B x P once, no lapse
//
// So the long tier costs about 3x the short tier's *premium*, but only once —
// while a lapsed short tier costs its full write every time the gap exceeds
// five minutes. Buying the hour wins whenever a session has more than a few
// multi-minute pauses in it, which is what real coding sessions look like.
//
// # Why the decision must be sticky
//
// This is the part that makes the plugin subtle. The tier is selected by a
// marker inside the request's cache breakpoint, so changing the marker
// *changes the prefix bytes*. Sending {"type":"ephemeral"} on one turn and
// {"type":"ephemeral","ttl":"1h"} on the next invalidates the very entry the
// plugin is trying to preserve, and pays a fresh write for the privilege.
//
// So the decision is fixed for the lifetime of the provider cache entry. It may
// be reconsidered only after that tier's TTL has elapsed, when no live entry can
// be invalidated. A plugin that re-decided per turn would cost money rather than
// save it, and would fail Torana's determinism test — which is the test that
// exists to catch precisely this class of bug.
//
// # v2 semantics (ordered ABI, cache-tier reconciliation)
//
//   - The only request mutation is the cache breakpoint marker, governed by
//     the dedicated ir.cache_control.write grant — never content, role, tool
//     schema, or any other field. The marker is applied with the SDK's
//     exact-carrier operation (ReplaceLastCacheBreakpoint): it replaces the
//     LAST EXISTING carrier in the tools-first/outer/nested order, never
//     inserts or guesses a position, and is atomic — every error leaves the
//     request unchanged.
//   - The decision key is the SDK-owned observable request prefix
//     (RequestObservablePrefix) under this plugin's decision domain — the
//     SAME projection the Edge host frames its cache key from (parity
//     invariant I1): the projection owns the full-domain validation gate and
//     the marker/truncation model, so no second request-prefix algorithm
//     exists to drift. The projection excludes only stream and host
//     metadata; provider extensions, safety settings, and the generation
//     params fold (they are model-visible prefix state).
//   - Fail-closed parity (I3): a projection error declines the request
//     BEFORE any state lookup/write or mutation; the no-marker sentinel
//     (ErrNoCacheBreakpoint) also declines without state or mutation — this
//     plugin never decides that something should be cached, it only chooses
//     which lifetime to buy for an existing carrier.
//   - State refusals split by class: advisory declines safely (the request
//     passes unchanged); contract/protocol failures error the hook. Corrupt
//     stored JSON is key-local. The decision persistence must succeed before
//     any marker is applied — an unpersisted marker would churn bytes next
//     turn and bust the cache.
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

// decision is what the plugin remembers per cache prefix.
type decision struct {
	// Marker is the breakpoint value chosen for this prefix, stored so every
	// later turn re-applies exactly the same bytes.
	Marker map[string]any `json:"marker"`
	// TierTTL is the lifetime that marker buys, kept for diagnostics.
	TierTTL int `json:"tier_ttl_seconds"`
	// DecidedAtMillis is when the choice was made.
	DecidedAtMillis int64 `json:"decided_at_millis"`
}

// activity is the per-conversation gap history the decision is based on.
type activity struct {
	LastSeenMillis int64 `json:"last_seen_millis"`
	// LongestGapMillis is the largest idle gap observed so far. A single long
	// pause is enough to justify the longer tier for the rest of the session:
	// a conversation that has gone quiet once will do it again.
	LongestGapMillis int64 `json:"longest_gap_millis"`
	Turns            int   `json:"turns"`
}

type config struct {
	// Mode is "auto" (choose per observed gaps), "short", "long", or "off".
	Mode string `json:"mode"`
	// MinGapSecondsForLongTier is how long an observed pause must be before the
	// longer tier is bought. Defaults to 30% of the long tier's TTL.
	MinGapSecondsForLongTier int `json:"min_gap_seconds_for_long_tier"`
	// ActivityRetentionDays bounds per-conversation history.
	ActivityRetentionDays int `json:"activity_retention_days"`
}

// parseConfig applies the runtime defaults. Loaded per call — no process
// globals.
func parseConfig(raw string) config {
	cfg := config{Mode: "auto", ActivityRetentionDays: 30}
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &cfg)
	}
	if cfg.Mode == "" {
		cfg.Mode = "auto"
	}
	if cfg.ActivityRetentionDays <= 0 {
		cfg.ActivityRetentionDays = 30
	}
	return cfg
}

func loadConfig() config { return parseConfig(sdk.PluginConfig()) }

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
	sdk.OnBeforeRequest(func(ctx context.Context, req *pbv2.ChatRequest) (sdk.RequestResult, error) {
		cfg := loadConfig()
		if cfg.Mode == "off" {
			return sdk.PassRequest(), nil
		}

		// Fail-closed parity (I3): the SDK-owned observable projection is the
		// single gate. An out-of-domain request declines BEFORE any state
		// lookup/write or mutation.
		prefix, err := pbv2.RequestObservablePrefix(req)
		if err != nil {
			return sdk.PassRequest(), nil
		}

		// The request must already carry a breakpoint. This plugin chooses
		// which lifetime to buy; it never decides that something should be
		// cached. The no-marker sentinel is the SDK's only oracle that does
		// not re-implement the carrier traversal: probe a DISCARDED clone —
		// atomic, no mutation of the live request, no state — and decline on
		// ErrNoCacheBreakpoint.
		probe := proto.Clone(req).(*pbv2.ChatRequest)
		if _, err := pbv2.ReplaceLastCacheBreakpoint(probe, noMarkerProbeBytes); err != nil {
			if errors.Is(err, pbv2.ErrNoCacheBreakpoint) {
				return sdk.PassRequest(), nil
			}
			return sdk.RequestResult{}, err
		}

		meta := readHostMeta(req)
		pricing, err := sdk.GetCachePricing(meta.Provider, req.Model)
		if err != nil {
			// Authoritative read: advisory pricing declines (unknown
			// economics — guessing spends the operator's money on a hunch);
			// contract/protocol defects surface.
			if isAdvisory(err) {
				return sdk.PassRequest(), nil
			}
			return sdk.RequestResult{}, err
		}
		if !pricing.Available() {
			return sdk.PassRequest(), nil
		}

		// The decision domain framing over the shared projection (parity
		// invariant I1): identical observable prefixes key the same sticky
		// decision; the projection error already declined above.
		prefixKey := sdk.ContentAddressedCacheKey("tier", string(prefix))

		now, clockErr := sdk.Now()

		// A decision already exists for this exact prefix — reapply it
		// verbatim until the provider cache itself has expired.
		var prior decision
		decisionKey := "decision/" + prefixKey
		// The decision read is AUTHORITATIVE, so the failure classes must be
		// distinguishable: a malformed host frame or transport failure is a
		// protocol defect (hook error); a refusal is advisory or contract by
		// code; a value that is present but not the expected JSON is a
		// key-local data error (decline for this key only, never absence).
		// StateGetJSON would collapse the frame error and the decode error
		// into one plain-error channel, so the raw typed read is used here.
		raw, herr, err := sdk.StateGet(decisionKey)
		found := false
		switch {
		case err != nil:
			return sdk.RequestResult{}, err
		case herr != nil && sdk.IsNotFound(herr):
			// fresh
		case herr != nil && (herr.Code == pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED || herr.Code == pbv2.ErrorCode_ERROR_CODE_UNAVAILABLE):
			return sdk.PassRequest(), nil
		case herr != nil:
			return sdk.RequestResult{}, fmt.Errorf("cache_tier_selector: state_get %s refused: %s", decisionKey, herr.Message)
		case json.Unmarshal([]byte(raw), &prior) != nil:
			// Corrupt stored JSON: key-local data error — decline for this
			// key without poisoning unrelated ones and without treating it
			// as absence.
			return sdk.PassRequest(), nil
		default:
			found = true
		}
		if found {
			switch {
			case clockErr != nil && !isAdvisory(clockErr):
				// Contract/protocol clock failure: surface exactly as on the
				// fresh-decision path — never swallow it because a sticky
				// decision happens to exist.
				return sdk.RequestResult{}, clockErr
			case clockErr != nil:
				// Advisory clock failure: the decision is already sticky and
				// byte-identical, so reapplying it is safe.
				if changed, err := replaceMarker(req, prior.Marker); err != nil {
					return sdk.RequestResult{}, err
				} else if changed {
					return sdk.ReplaceRequest(req), nil
				}
				return sdk.PassRequest(), nil
			case !decisionExpired(prior, now):
				if changed, err := replaceMarker(req, prior.Marker); err != nil {
					return sdk.RequestResult{}, err
				} else if changed {
					return sdk.ReplaceRequest(req), nil
				}
				return sdk.PassRequest(), nil
			}
			// The provider tier has elapsed; the decision may be reconsidered.
			// Deleting is governed by env.state_set (StateDeletePermission).
			if herr, err := sdk.StateDelete(decisionKey); err != nil || herr != nil {
				if err != nil {
					return sdk.RequestResult{}, err
				}
				if herr.Code == pbv2.ErrorCode_ERROR_CODE_NOT_FOUND {
					// Already gone — fine.
				} else if herr.Code == pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED ||
					herr.Code == pbv2.ErrorCode_ERROR_CODE_UNAVAILABLE {
					// Advisory: decline (an expired decision held in place is
					// harmless — the marker stays sticky).
					return sdk.PassRequest(), nil
				} else {
					return sdk.RequestResult{}, fmt.Errorf("cache_tier_selector: delete expired %s refused: %s", decisionKey, herr.Message)
				}
			}
		}

		// Without a clock there is no gap history to reason from, so the
		// conversation keeps whatever tier the harness asked for.
		if clockErr != nil {
			if isAdvisory(clockErr) {
				return sdk.PassRequest(), nil
			}
			return sdk.RequestResult{}, clockErr
		}
		cleanupExpiredState(now, cfg)
		act, err := recordActivity(meta.ConversationID, now)
		if err != nil {
			// A failed advisory activity write must not create an
			// unpersisted/churning mutation: continue with the in-memory
			// history, but the DECISION persistence below is the gate that
			// decides whether any marker is applied.
			if !isAdvisory(err) {
				return sdk.RequestResult{}, err
			}
		}

		marker, ttl := chooseTier(cfg, pricing, act, now)
		if marker == nil {
			return sdk.PassRequest(), nil
		}

		// Persist BEFORE applying: an unpersisted marker would allow the next
		// identical request to choose different bytes and bust the cache.
		if err := sdk.StateSetJSON(decisionKey, decision{
			Marker:          marker,
			TierTTL:         ttl,
			DecidedAtMillis: now,
		}); err != nil {
			// Advisory: pass unchanged (no marker applied). Contract/protocol:
			// surface — a plugin that can never persist a decision is broken.
			if isAdvisory(err) {
				return sdk.PassRequest(), nil
			}
			return sdk.RequestResult{}, err
		}
		// Best-effort observability; the decision is already durable.
		payload, _ := json.Marshal(map[string]any{"counter": "tier_decisions", "delta": 1})
		_, _, _ = sdk.HostCallExtension("torana_plugin_counter", payload)

		if changed, err := replaceMarker(req, marker); err != nil {
			return sdk.RequestResult{}, err
		} else if changed {
			return sdk.ReplaceRequest(req), nil
		}
		return sdk.PassRequest(), nil
	})
}

// chooseTier returns the breakpoint marker to use, or nil to leave the request
// untouched.
func chooseTier(cfg config, pricing sdk.CachePricing, act activity, now int64) (map[string]any, int) {
	// Without a declared long tier there is nothing to choose between.
	if pricing.ShortestTTLSeconds <= 0 {
		return nil, 0
	}

	long, ok := pricing.LongestTier()
	if !ok || long.Marker == nil {
		return nil, 0
	}

	switch cfg.Mode {
	case "short":
		return nil, 0 // the harness default already selects the short tier
	case "long":
		return long.Marker, long.TTLSeconds
	}

	// auto: buy the longer tier once this conversation has demonstrated it
	// pauses for long enough to lose a short-tier entry.
	threshold := int64(cfg.MinGapSecondsForLongTier) * 1000
	if threshold <= 0 {
		threshold = int64(float64(long.TTLSeconds) * 0.3 * 1000)
	}
	if threshold <= 0 || act.LongestGapMillis < threshold {
		return nil, 0
	}
	return long.Marker, long.TTLSeconds
}

// recordActivity updates and returns this conversation's gap history.
// Advisory refusals return the in-memory history so the caller can still
// decide (the decision persistence gates any mutation); contract/protocol
// errors surface.
func recordActivity(convKey string, now int64) (activity, error) {
	var act activity
	if convKey == "" || now == 0 {
		return act, nil
	}
	key := "activity/" + convKey
	raw, herr, err := sdk.StateGet(key)
	switch {
	case err != nil:
		return act, err // transport/protocol/frame
	case herr != nil && !sdk.IsNotFound(herr):
		if herr.Code != pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED && herr.Code != pbv2.ErrorCode_ERROR_CODE_UNAVAILABLE {
			return act, fmt.Errorf("cache_tier_selector: state_get %s refused: %s", key, herr.Message)
		}
		// Advisory: proceed with fresh history; the decision persist gates
		// any mutation.
	case herr == nil && json.Unmarshal([]byte(raw), &act) != nil:
		// Corrupt stored JSON: key-local — proceed with fresh history.
	}

	if act.LastSeenMillis > 0 {
		if gap := now - act.LastSeenMillis; gap > act.LongestGapMillis {
			act.LongestGapMillis = gap
		}
	}
	act.LastSeenMillis = now
	act.Turns++
	if err := sdk.StateSetJSON(key, act); err != nil {
		// Swallow ONLY advisory refusals: an unpersisted activity must not
		// create a churning mutation, and the decision persist gates any
		// application. Contract refusals and malformed frames (plain protocol
		// errors) surface.
		if isAdvisory(err) {
			return act, nil
		}
		return act, err
	}
	return act, nil
}

func decisionExpired(value decision, now int64) bool {
	if now <= 0 || value.DecidedAtMillis <= 0 || value.TierTTL <= 0 {
		return false
	}
	return now >= value.DecidedAtMillis+int64(value.TierTTL)*1000
}

// cleanupExpiredState runs at most hourly and deletes a bounded number of keys
// per request, so a large legacy store cannot turn one request into a long
// pause. Ancillary: every failure is best-effort and logged — garbage
// collection must never discard an otherwise safe sticky decision.
func cleanupExpiredState(now int64, cfg config) {
	const (
		cleanupKey      = "_cleanup_at"
		cleanupInterval = int64(60 * 60 * 1000)
		maxDeletes      = 100
	)
	var last int64
	if found, err := sdk.StateGetJSON(cleanupKey, &last); err != nil {
		logStateError("read cleanup marker", err)
		return
	} else if found && now-last < cleanupInterval {
		return
	}
	keys, herr, err := sdk.StateKeys()
	if err != nil || herr != nil {
		logStateError("list state for cleanup", err)
		return
	}
	retentionMillis := int64(cfg.ActivityRetentionDays) * 24 * 60 * 60 * 1000
	deleted := 0
	for _, key := range keys {
		if deleted >= maxDeletes {
			break
		}
		expired := false
		switch {
		case strings.HasPrefix(key, "decision/"):
			var value decision
			found, readErr := sdk.StateGetJSON(key, &value)
			if readErr != nil {
				logStateError("read "+key, readErr)
				continue
			}
			expired = found && decisionExpired(value, now)
		case strings.HasPrefix(key, "activity/"):
			var value activity
			found, readErr := sdk.StateGetJSON(key, &value)
			if readErr != nil {
				logStateError("read "+key, readErr)
				continue
			}
			expired = found && value.LastSeenMillis > 0 && now-value.LastSeenMillis >= retentionMillis
		}
		if expired {
			if herr, err := sdk.StateDelete(key); err != nil || herr != nil {
				logStateError("delete "+key, err)
				continue
			}
			deleted++
		}
	}
	if err := sdk.StateSetJSON(cleanupKey, now); err != nil {
		logStateError("persist cleanup marker", err)
	}
}

func logStateError(operation string, err error) {
	sdk.Log(fmt.Sprintf("cache_tier_selector: %s: %v", operation, err), sdk.LogLevelInfo)
}

// noMarkerProbeBytes is the strict-object marker used ONLY for the
// no-marker sentinel probe on a discarded clone. Its content is irrelevant
// (the probe result is discarded); the sentinel distinguishes "no carrier"
// from "carrier exists" without re-implementing the traversal.
var noMarkerProbeBytes = []byte(`{"type":"ephemeral"}`)

// replaceMarker applies the tier marker with the SDK's exact-carrier
// operation: the LAST EXISTING carrier (tools-first section, outer block, or
// nested tool-result content) is replaced; the operation never inserts and
// is atomic (every error leaves the request unchanged). changed=false when
// the existing marker bytes are byte-identical — an already-matching marker
// avoids a pointless re-serialisation. The ONLY request mutation this
// plugin performs is the cache-control marker — governed by
// ir.cache_control.write.
func replaceMarker(req *pbv2.ChatRequest, marker map[string]any) (bool, error) {
	if marker == nil {
		return false, nil
	}
	raw, err := json.Marshal(marker)
	if err != nil {
		return false, err
	}
	changed, err := pbv2.ReplaceLastCacheBreakpoint(req, raw)
	if errors.Is(err, pbv2.ErrNoCacheBreakpoint) {
		// The carrier vanished (e.g. a downstream plugin removed it): decline
		// without mutation — the sentinel guarantees the request is
		// unchanged — and without touching state.
		return false, nil
	}
	return changed, err
}

type hostMeta struct {
	Provider       string `json:"_provider"`
	ConversationID string `json:"_conversation_id"`
}

// readHostMeta decodes it. Empty fields mean the host did not supply them, in
// which case the plugin declines to act rather than guessing.
func readHostMeta(req *pbv2.ChatRequest) hostMeta {
	var meta hostMeta
	if len(req.ToranaMetaJson) == 0 {
		return meta
	}
	_ = json.Unmarshal(req.ToranaMetaJson, &meta)
	return meta
}
