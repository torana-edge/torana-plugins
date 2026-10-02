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
	Version     int                         `json:"version"`
	Outcome     replayOutcome               `json:"outcome"`
	Replacement string                      `json:"replacement"`
	Reason      sdk.ToolResultReleaseReason `json:"reason"`
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
func replayPrior(ctx context.Context, messageIndex int, msg *pbv1.Message, view sdk.ToolResultView, latest bool) (bool, error) {
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
	approval, err := sdk.ToolResultRelease(messageIndex, view.Block, nil)
	if err != nil {
		return false, err
	}
	if approval.Approved {
		// Handled, but deliberately unchanged: skip replay AND scanning, for
		// latest results as well as history. Approval is never a clean verdict.
		return true, nil
	}
	if latest && record.Outcome == outcomeTransient {
		return false, nil
	}
	if approval.Reference == "" {
		approval, err = sdk.ToolResultRelease(messageIndex, view.Block, &record.Reason)
		if err != nil {
			return false, err
		}
	}
	_, err = sdk.ReplaceToolResultWithError(msg, view.Block, releaseDiagnostic(record.Replacement, approval))
	return true, err
}

func replaceAndRemember(ctx context.Context, messageIndex int, msg *pbv1.Message, view sdk.ToolResultView, replacement string, outcome replayOutcome, findings ...finding) error {
	digest, err := occurrenceDigest(ctx, msg, view)
	if err != nil {
		return err
	}
	reason := sdk.ToolResultReleaseReason{Kind: "scan_failure"}
	if outcome == outcomeSensitive {
		reason.Kind = "findings"
		for _, finding := range findings[:min(len(findings), maxReportedFindings)] {
			reason.Findings = append(reason.Findings, sdk.ToolResultReleaseFinding{Type: normalizeCategory(finding.Type), Line: max(0, finding.Line)})
		}
	}
	record := replayRecord{Version: 3, Outcome: outcome, Replacement: replacement, Reason: reason}
	winner, err := writeReplayDecision(replayKey(digest), record)
	if err != nil {
		return err
	}
	approval, err := sdk.ToolResultRelease(messageIndex, view.Block, &winner.Reason)
	if err != nil {
		return err
	}
	if approval.Approved {
		return nil
	}
	_, err = sdk.ReplaceToolResultWithError(msg, view.Block, releaseDiagnostic(winner.Replacement, approval))
	return err
}

func releaseDiagnostic(replacement string, approval sdk.ToolResultReleaseInfo) string {
	if approval.Reference != "" {
		replacement += " If this seems mistaken, request user review with Torana MCP: torana_invoke, namespace torana, operation redactions.request_release, input {\"reference\":\"" + approval.Reference + "\"}. Only the user can allow this exact result in Torana's Approvals page or CLI. Do not bypass the scan by re-reading smaller pieces."
	} else {
		replacement += " This result has no stable tool-call ID for exact-result human review."
	}
	return replacement
}

// resolveCleanReplay removes a stale transient failure before clean content is
// forwarded. If a concurrent scan already committed a sensitive verdict, that
// winner is applied instead and the caller must return a replacement request.
func resolveCleanReplay(ctx context.Context, messageIndex int, msg *pbv1.Message, view sdk.ToolResultView) (bool, error) {
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
			approval, err := sdk.ToolResultRelease(messageIndex, view.Block, &record.Reason)
			if err != nil {
				return false, err
			}
			if approval.Approved {
				return false, nil
			}
			_, err = sdk.ReplaceToolResultWithError(msg, view.Block, releaseDiagnostic(record.Replacement, approval))
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
	if err := validateReplayRecord(proposed); err != nil {
		return replayRecord{}, err
	}
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
			currentJSON, _ := json.Marshal(current)
			proposedJSON, _ := json.Marshal(proposed)
			if current.Outcome == outcomeSensitive || string(currentJSON) == string(proposedJSON) {
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
	if record.Version != 3 || record.Replacement == "" {
		return fmt.Errorf("unsupported record")
	}
	if err := record.Reason.Validate(); err != nil {
		return err
	}
	if record.Outcome == outcomeSensitive && record.Reason.Kind != "findings" || record.Outcome == outcomeTransient && record.Reason.Kind != "scan_failure" {
		return fmt.Errorf("replay reason does not match outcome")
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
