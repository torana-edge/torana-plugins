package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

const replayPolicyVersion = "pii/v2"

type replayOutcome string

const (
	outcomeSensitive replayOutcome = "sensitive"
	outcomeTransient replayOutcome = "transient"
)

type replayRecord struct {
	Version     int           `json:"version"`
	Outcome     replayOutcome `json:"outcome"`
	Replacement string        `json:"replacement"`
}

func occurrenceDigest(ctx context.Context, msg *pbv1.Message, view sdk.ToolResultView) (string, error) {
	if msg == nil || view.Block < 0 || view.Block >= len(msg.Blocks) || msg.Blocks[view.Block] == nil {
		return "", fmt.Errorf("invalid tool-result position %d", view.Block)
	}
	tr := msg.Blocks[view.Block].GetToolResult()
	if tr == nil {
		return "", fmt.Errorf("block %d is not a tool result", view.Block)
	}
	execution := sdk.Execution(ctx)
	if execution == nil || execution.ConversationId == nil || execution.GetConversationId() == "" {
		return "", fmt.Errorf("stable conversation identity is unavailable")
	}
	visible := make([]*pbv1.ToolResultContentBlock, 0, len(tr.Content))
	for _, arm := range tr.Content {
		if arm == nil || arm.GetCacheBreakpoint() == nil {
			visible = append(visible, arm)
		}
	}
	content, err := sdk.ToolResultContentFingerprint(visible)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	write := func(value []byte) {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		h.Write(size[:])
		h.Write(value)
	}
	write([]byte(replayPolicyVersion))
	write([]byte(execution.GetConversationId()))
	write([]byte(stableReplayToolCallID(tr.ToolCallId)))
	write(content[:])
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Bare Gemini has no call ID. Edge emits a semantic hash plus a uniqueness
// ordinal; replay uses the semantic portion so unrelated compacted calls do not
// move the key. Identical id-less calls are indistinguishable after arbitrary
// history compaction, so their content verdict is intentionally shared within
// one conversation. Temporary failures are still retried for the newest call.
func stableReplayToolCallID(id string) string {
	if !strings.HasPrefix(id, "torana_gemini_") {
		return id
	}
	cut := strings.LastIndexByte(id, '_')
	if cut < len("torana_gemini_") || cut == len(id)-1 {
		return id
	}
	if _, err := strconv.ParseUint(id[cut+1:], 10, 64); err != nil {
		return id
	}
	return id[:cut]
}

func replayKey(digest string) string { return "replay/occurrence/" + digest }

// replayPrior reapplies a durable replacement. A temporary scanner failure is
// retried while the occurrence remains in the newest tool-result batch, but is
// replayed once it becomes history so old unscanned bytes cannot leak later.
func replayPrior(ctx context.Context, msg *pbv1.Message, view sdk.ToolResultView, latest bool) (bool, error) {
	digest, err := occurrenceDigest(ctx, msg, view)
	if err != nil {
		return false, err
	}
	key := replayKey(digest)
	var record replayRecord
	found, err := sdk.StateGetJSON(key, &record)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	if err := validateReplayRecord(record); err != nil {
		return false, fmt.Errorf("invalid replay record at %q: %w", key, err)
	}
	if latest && record.Outcome == outcomeTransient {
		return false, nil
	}
	_, err = sdk.ReplaceToolResultWithError(msg, view.Block, record.Replacement)
	return true, err
}

func replaceAndRemember(ctx context.Context, msg *pbv1.Message, view sdk.ToolResultView, replacement string, outcome replayOutcome) error {
	digest, err := occurrenceDigest(ctx, msg, view)
	if err != nil {
		return err
	}
	record := replayRecord{Version: 2, Outcome: outcome, Replacement: replacement}
	winner, err := writeReplayDecision(replayKey(digest), record)
	if err != nil {
		return err
	}
	_, err = sdk.ReplaceToolResultWithError(msg, view.Block, winner.Replacement)
	return err
}

// resolveCleanReplay removes a stale transient failure before clean content is
// forwarded. If a concurrent scan already committed a sensitive verdict, that
// winner is applied instead and the caller must return a replacement request.
func resolveCleanReplay(ctx context.Context, msg *pbv1.Message, view sdk.ToolResultView) (bool, error) {
	digest, err := occurrenceDigest(ctx, msg, view)
	if err != nil {
		return false, err
	}
	key := replayKey(digest)
	for attempt := 0; attempt < 4; attempt++ {
		value, found, err := sdk.StateGetVersioned(key)
		if err != nil || !found {
			return false, err
		}
		var record replayRecord
		if err := json.Unmarshal([]byte(value.Value), &record); err != nil {
			return false, fmt.Errorf("decode replay record at %q: %w", key, err)
		}
		if err := validateReplayRecord(record); err != nil {
			return false, fmt.Errorf("invalid replay record at %q: %w", key, err)
		}
		if record.Outcome == outcomeSensitive {
			_, err := sdk.ReplaceToolResultWithError(msg, view.Block, record.Replacement)
			return true, err
		}
		result, err := sdk.StateCompareAndDelete(key, value.Version)
		if err != nil {
			return false, err
		}
		if result.GetApplied() {
			return false, nil
		}
	}
	return false, fmt.Errorf("replay decision at %q changed repeatedly", key)
}

func writeReplayDecision(key string, proposed replayRecord) (replayRecord, error) {
	for attempt := 0; attempt < 4; attempt++ {
		value, found, err := sdk.StateGetVersioned(key)
		if err != nil {
			return replayRecord{}, err
		}
		var expected *string
		if found {
			var current replayRecord
			if err := json.Unmarshal([]byte(value.Value), &current); err != nil {
				return replayRecord{}, fmt.Errorf("decode replay record at %q: %w", key, err)
			}
			if err := validateReplayRecord(current); err != nil {
				return replayRecord{}, fmt.Errorf("invalid replay record at %q: %w", key, err)
			}
			if current.Outcome == outcomeSensitive || current == proposed {
				return current, nil
			}
			expected = &value.Version
		}
		raw, err := json.Marshal(proposed)
		if err != nil {
			return replayRecord{}, err
		}
		result, err := sdk.StateCompareAndSet(key, string(raw), expected)
		if err != nil {
			return replayRecord{}, err
		}
		if result.GetApplied() {
			return proposed, nil
		}
	}
	return replayRecord{}, fmt.Errorf("replay decision at %q changed repeatedly", key)
}

func validateReplayRecord(record replayRecord) error {
	if record.Version != 2 || record.Replacement == "" {
		return fmt.Errorf("unsupported record")
	}
	if record.Outcome != outcomeSensitive && record.Outcome != outcomeTransient {
		return fmt.Errorf("unknown outcome %q", record.Outcome)
	}
	return nil
}

func trailingToolResultMessages(messages []*pbv1.Message) map[int]bool {
	trailing := map[int]bool{}
	foundBatch := false
	for i := len(messages) - 1; i >= 0; i-- {
		if len(sdk.ToolResults(messages[i])) > 0 {
			trailing[i] = true
			foundBatch = true
			continue
		}
		if !foundBatch && (messages[i].GetRole() == "system" || messages[i].GetRole() == "developer") {
			continue
		}
		break
	}
	return trailing
}
