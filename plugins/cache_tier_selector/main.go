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
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	"github.com/torana-edge/torana-plugin-sdk/pb"
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
	// longer tier is bought. Defaults to 30% of the long tier's TTL, which
	// keeps a single unusual pause from committing a short conversation.
	MinGapSecondsForLongTier int `json:"min_gap_seconds_for_long_tier"`
	// ActivityRetentionDays bounds per-conversation history. Decisions have a
	// shorter natural lifetime: once the provider cache tier expires, changing
	// its marker cannot invalidate a live cache entry.
	ActivityRetentionDays int `json:"activity_retention_days"`
}

func loadConfig() config {
	cfg := config{Mode: "auto", ActivityRetentionDays: 30}
	if raw := sdk.PluginConfig(); raw != "" {
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

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		cfg := loadConfig()
		if cfg.Mode == "off" {
			return nil, nil
		}

		// The request must already carry a breakpoint. This plugin chooses
		// which lifetime to buy; it never decides that something should be
		// cached, because that judgement belongs to whoever placed the marker.
		idx := lastCacheBreakpoint(req)
		if idx < 0 {
			return nil, nil
		}

		meta := readHostMeta(req)
		pricing, err := sdk.GetCachePricing(meta.Provider, req.Model)
		if err != nil || !pricing.Available() {
			// Unknown economics: leave the request exactly as it arrived.
			// Guessing here spends the operator's money on a hunch.
			return nil, nil
		}

		prefixKey := cachePrefixKey(req)
		if prefixKey == "" {
			return nil, nil
		}

		now, clockErr := sdk.Now()

		// A decision already exists for this exact prefix — reapply it
		// verbatim until the provider cache itself has expired.
		var prior decision
		decisionKey := "decision/" + prefixKey
		found, stateErr := sdk.StateGetJSON(decisionKey, &prior)
		if stateErr != nil {
			logStateError("read "+decisionKey, stateErr)
			return nil, nil
		}
		if found {
			if clockErr != nil || !decisionExpired(prior, now) {
				return applyMarker(req, idx, prior.Marker), nil
			}
			if err := sdk.StateSet(decisionKey, ""); err != nil {
				logStateError("delete expired "+decisionKey, err)
				return nil, nil
			}
		}

		// Without a clock there is no gap history to reason from, so the
		// conversation keeps whatever tier the harness asked for.
		if clockErr != nil {
			return nil, nil
		}
		cleanupExpiredState(now, cfg)
		act, err := recordActivity(meta.ConversationID, now)
		if err != nil {
			logStateError("record activity", err)
			return nil, nil
		}

		marker, ttl := chooseTier(cfg, pricing, act, now)
		if marker == nil {
			return nil, nil
		}

		if err := sdk.StateSetJSON(decisionKey, decision{
			Marker:          marker,
			TierTTL:         ttl,
			DecidedAtMillis: now,
		}); err != nil {
			// Applying a marker that was not persisted would allow the next
			// identical request to choose different bytes and bust the cache.
			logStateError("persist "+decisionKey, err)
			return nil, nil
		}
		_, _ = sdk.HostCall("torana_plugin_counter", counterPayload("tier_decisions", 1))

		return applyMarker(req, idx, marker), nil
	})
}

// chooseTier returns the breakpoint marker to use, or nil to leave the request
// untouched.
func chooseTier(cfg config, pricing sdk.CachePricing, act activity, now int64) (map[string]any, int) {
	// Without a declared long tier there is nothing to choose between.
	if pricing.ShortestTTLSeconds <= 0 {
		return nil, 0
	}

	// The provider's own tier menu, as the operator declared it. A provider
	// selling only one lifetime has nothing to choose between.
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
func recordActivity(convKey string, now int64) (activity, error) {
	var act activity
	if convKey == "" || now == 0 {
		return act, nil
	}
	key := "activity/" + convKey
	if _, err := sdk.StateGetJSON(key, &act); err != nil {
		return act, err
	}

	if act.LastSeenMillis > 0 {
		if gap := now - act.LastSeenMillis; gap > act.LongestGapMillis {
			act.LongestGapMillis = gap
		}
	}
	act.LastSeenMillis = now
	act.Turns++
	if err := sdk.StateSetJSON(key, act); err != nil {
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
// pause. Repeated traffic eventually drains the whole backlog.
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
	keys, err := sdk.StateKeys()
	if err != nil {
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
			if err := sdk.StateSet(key, ""); err != nil {
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

// applyMarker sets the breakpoint marker on the message at idx. Returning the
// request unchanged when the marker already matches avoids a pointless
// re-serialisation.
func applyMarker(req *pb.ChatRequest, idx int, marker map[string]any) *pb.ChatRequest {
	if marker == nil || idx < 0 || idx >= len(req.Messages) {
		return nil
	}
	current := sdk.CacheControl(req.Messages[idx])
	if sameMarker(current, marker) {
		return nil
	}
	sdk.SetCacheBreakpoint(req.Messages[idx], marker)
	return req
}

func sameMarker(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	ab, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return string(ab) == string(bb)
}

// lastCacheBreakpoint returns the index of the last message carrying a cache
// breakpoint — the boundary of the cached prefix — or -1.
func lastCacheBreakpoint(req *pb.ChatRequest) int {
	last := -1
	for i, m := range req.Messages {
		if len(m.CacheControlJson) > 0 {
			last = i
		}
	}
	return last
}

// cachePrefixKey fingerprints the bytes the provider will cache: the model plus
// every message up to and including the breakpoint. It mirrors the host's own
// CachePrefixKey closely enough for stickiness, which only needs "has this
// prefix changed", not an exact match with the host's value.
func cachePrefixKey(req *pb.ChatRequest) string {
	idx := lastCacheBreakpoint(req)
	if idx < 0 {
		return ""
	}
	h := newHash()
	h.add(req.Model)
	for _, t := range req.Tools {
		h.add("tool")
		h.add(t.Name)
		h.add(string(t.ParametersJson))
	}
	for _, m := range req.Messages[:idx+1] {
		h.add(m.Role)
		h.add(m.Content)
		h.add(string(m.ContentPartsJson))
		// Extended-thinking blocks are part of what goes upstream and therefore
		// part of the prefix the provider hashes. Omitting them let two
		// conversations with identical text but different reasoning produce the
		// same Torana fingerprint — so a tier decision was reused for a prefix
		// the provider will treat as new, and the expensive long-tier write it
		// implies is made against something that cannot hit. That is precisely
		// the economics this plugin exists to protect.
		h.add(m.Thinking)
		h.add(m.ThinkingSignature)
		h.add(m.RedactedThinking)
		// cache_control is what DEFINES the breakpoint. The same content marked
		// differently is a different cached prefix upstream.
		h.add(string(m.CacheControlJson))
		// Tool results: the id and name mirror the originating assistant call,
		// which is hashed below, so these are usually redundant — but only when
		// that call is inside the prefix, and cheap enough not to reason about.
		h.add(m.ToolCallId)
		h.add(m.ToolName)
		for _, tc := range m.ToolCalls {
			h.add(tc.Id)
			h.add(tc.Name)
			h.add(string(tc.ArgumentsJson))
		}
	}
	return h.sum()
}

// hostMeta is the routing context the host publishes on every request.
type hostMeta struct {
	Provider       string `json:"_provider"`
	ConversationID string `json:"_conversation_id"`
}

// readHostMeta decodes it. Empty fields mean the host did not supply them, in
// which case the plugin declines to act rather than guessing.
func readHostMeta(req *pb.ChatRequest) hostMeta {
	var meta hostMeta
	if len(req.ToranaMetaJson) == 0 {
		return meta
	}
	_ = json.Unmarshal(req.ToranaMetaJson, &meta)
	return meta
}

func counterPayload(name string, delta int64) string {
	b, _ := json.Marshal(map[string]any{"counter": name, "delta": delta})
	return string(b)
}

// A tiny FNV-1a so the plugin carries no hashing dependency into WASM.
type hasher struct{ h uint64 }

func newHash() *hasher { return &hasher{h: 14695981039346656037} }

func (x *hasher) add(s string) {
	// Length-prefix every field so that field boundaries cannot be forged by
	// content: without it "ab"+"cd" and "a"+"bcd" hash identically, and two
	// different prefixes would share a decision.
	x.write(fmt.Sprintf("%d:", len(s)))
	x.write(s)
}

func (x *hasher) write(s string) {
	for i := 0; i < len(s); i++ {
		x.h ^= uint64(s[i])
		x.h *= 1099511628211
	}
}

func (x *hasher) sum() string { return fmt.Sprintf("%016x", x.h) }
