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

const replayPolicyVersion = "pii_guard/v2"

type replayRecord struct {
	Version     int    `json:"version"`
	Replacement string `json:"replacement"`
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

func replayPrior(ctx context.Context, msg *pbv1.Message, view sdk.ToolResultView) (bool, error) {
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
	if record.Version != 2 || record.Replacement == "" {
		return false, fmt.Errorf("invalid replay record at %q", key)
	}
	_, err = sdk.ReplaceToolResultWithError(msg, view.Block, record.Replacement)
	return true, err
}

func replaceAndRemember(ctx context.Context, msg *pbv1.Message, view sdk.ToolResultView, replacement string) error {
	digest, err := occurrenceDigest(ctx, msg, view)
	if err != nil {
		return err
	}
	record := replayRecord{Version: 2, Replacement: replacement}
	winner, err := firstWriteWins(replayKey(digest), record)
	if err != nil {
		return err
	}
	_, err = sdk.ReplaceToolResultWithError(msg, view.Block, winner.Replacement)
	return err
}

func firstWriteWins(key string, proposed replayRecord) (replayRecord, error) {
	raw, err := json.Marshal(proposed)
	if err != nil {
		return replayRecord{}, fmt.Errorf("encode replay record: %w", err)
	}
	result, err := sdk.StateCompareAndSet(key, string(raw), nil)
	if err != nil {
		return replayRecord{}, err
	}
	if result.GetApplied() {
		return proposed, nil
	}
	var winner replayRecord
	found, err := sdk.StateGetJSON(key, &winner)
	if err != nil {
		return replayRecord{}, err
	}
	if !found || winner.Version != 2 || winner.Replacement == "" {
		return replayRecord{}, fmt.Errorf("replay winner at %q is unavailable or invalid", key)
	}
	return winner, nil
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
