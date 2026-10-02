package main

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
	"google.golang.org/protobuf/proto"
)

func TestHumanApprovalOverridesReplayWithoutRescanOrCleanCache(t *testing.T) {
	h := newHarness(t)
	h.StubModelComplete(modelStub(`{"pii":true,"findings":[{"type":"unspecified","line":1}]}`))
	original := toolMsg("stable-call", "Read", textArm("ordinary README content"), markerArm())
	first := reqWith(proto.Clone(original).(*pb.Message))
	if result := h.BeforeRequest(first); result.Err != nil {
		t.Fatal(result.Err)
	}
	blocked := proto.Clone(first).(*pb.ChatRequest)
	text, _ := incrementalResultText(t, first.Messages[0])
	if !strings.Contains(text, "redactions.request_release") || strings.Contains(text, "ordinary README content") {
		t.Fatalf("diagnostic=%s", text)
	}
	calls := countCommand(h, "env.model_complete")
	h.StubHostCall("torana_tool_result_release", func(string) (string, error) {
		return sdktest.HostResultValue([]byte(`{"reference":"tr_` + strings.Repeat("a", 64) + `","approved":true}`)), nil
	})
	for i := 0; i < 3; i++ {
		request := reqWith(proto.Clone(original).(*pb.Message), incrementalTextMessage("continue"))
		if result := h.BeforeRequest(request); result.Err != nil {
			t.Fatal(result.Err)
		}
		if !proto.Equal(request.Messages[0], original) {
			t.Fatal("allowed history changed content or cache carriers")
		}
	}
	latest := reqWith(proto.Clone(original).(*pb.Message))
	if result := h.BeforeRequest(latest); result.Err != nil {
		t.Fatal(result.Err)
	}
	if !proto.Equal(latest.Messages[0], original) || countCommand(h, "env.model_complete") != calls || countCommand(h, "env.cache_set") != 0 {
		t.Fatal("allowance rescanned, rewrote or cached result as clean")
	}
	// Revocation restores the original deterministic replacement; it never
	// requires another inference call to rediscover the previous decision.
	h.StubHostCall("torana_tool_result_release", func(string) (string, error) {
		return sdktest.HostResultValue([]byte(`{"reference":"tr_` + strings.Repeat("a", 64) + `","approved":false}`)), nil
	})
	revoked := reqWith(proto.Clone(original).(*pb.Message))
	if result := h.BeforeRequest(revoked); result.Err != nil {
		t.Fatal(result.Err)
	}
	if !proto.Equal(revoked, blocked) || countCommand(h, "env.model_complete") != calls {
		t.Fatal("revocation did not restore stable replay")
	}
}

func TestSavedReasonRegistersCurrentBundleReferenceWithoutRescanning(t *testing.T) {
	h := newHarness(t)
	h.StubModelComplete(modelStub(`{"pii":true,"findings":[{"type":"api_key","line":2}]}`))
	original := toolMsg("stable-call", "Read", textArm("safe first line\nsynthetic output"))
	if result := h.BeforeRequest(reqWith(proto.Clone(original).(*pb.Message))); result.Err != nil {
		t.Fatal(result.Err)
	}
	scans := countCommand(h, "env.model_complete")
	registered := false
	h.StubHostCall("torana_tool_result_release", func(args string) (string, error) {
		var input struct {
			Register bool                         `json:"register"`
			Reason   *sdk.ToolResultReleaseReason `json:"reason"`
		}
		if err := json.Unmarshal([]byte(args), &input); err != nil {
			t.Fatal(err)
		}
		if !input.Register {
			return sdktest.HostResultValue([]byte(`{"reference":"","approved":false}`)), nil
		}
		if input.Reason == nil || input.Reason.Kind != "findings" || input.Reason.Findings[0].Type != "api_key" || input.Reason.Findings[0].Line != 2 {
			t.Fatalf("lost saved reason: %+v", input)
		}
		registered = true
		return sdktest.HostResultValue([]byte(`{"reference":"tr_` + strings.Repeat("b", 64) + `","approved":false}`)), nil
	})
	request := reqWith(proto.Clone(original).(*pb.Message), incrementalTextMessage("continue"))
	if result := h.BeforeRequest(request); result.Err != nil {
		t.Fatal(result.Err)
	}
	text, _ := incrementalResultText(t, request.Messages[0])
	if !registered || !strings.Contains(text, "tr_"+strings.Repeat("b", 64)) || strings.Contains(text, "tr_"+strings.Repeat("a", 64)) || countCommand(h, "env.model_complete") != scans {
		t.Fatalf("stale reference or rescan: %s", text)
	}
}

func TestV2ReplayUpgradesWithoutRescanOrDisclosingHistory(t *testing.T) {
	for _, outcome := range []replayOutcome{outcomeSensitive, outcomeTransient} {
		t.Run(string(outcome), func(t *testing.T) {
			h := newHarness(t)
			original := toolMsg("old-call", "Read", textArm("synthetic private output"), markerArm())
			// Resolve the occurrence key using the hook's real conversation context.
			if result := h.BeforeRequest(reqWith(proto.Clone(original).(*pb.Message))); result.Err != nil {
				t.Fatal(result.Err)
			}
			var key string
			for _, call := range h.Calls() {
				if call.Command == "env.state_get_versioned" {
					var args pb.StateGetArgs
					if err := proto.Unmarshal([]byte(call.Args), &args); err != nil {
						t.Fatal(err)
					}
					key = args.Key
					break
				}
			}
			if key == "" {
				t.Fatal("missing occurrence key")
			}
			oldRef := "tr_" + strings.Repeat("c", 64)
			base := "Tool output was withheld by the scanner."
			raw, _ := json.Marshal(replayRecord{Version: 2, Outcome: outcome, Replacement: releaseDiagnostic(base, sdk.ToolResultReleaseInfo{Reference: oldRef})})
			h.SeedState(key, string(raw))
			scans := countCommand(h, "env.model_complete")
			registered := false
			h.StubHostCall("torana_tool_result_release", func(args string) (string, error) {
				var input struct {
					Register bool                         `json:"register"`
					Reason   *sdk.ToolResultReleaseReason `json:"reason"`
				}
				if err := json.Unmarshal([]byte(args), &input); err != nil {
					t.Fatal(err)
				}
				if !input.Register {
					return sdktest.HostResultValue([]byte(`{"reference":"","approved":false}`)), nil
				}
				if input.Reason == nil {
					t.Fatal("missing migrated reason")
				}
				if outcome == outcomeTransient {
					if input.Reason.Kind != "scan_failure" || len(input.Reason.Findings) != 0 {
						t.Fatal("incorrect failure reason")
					}
				} else if input.Reason.Kind != "findings" || len(input.Reason.Findings) != 1 || input.Reason.Findings[0] != (sdk.ToolResultReleaseFinding{Type: "unspecified", Line: 0}) {
					t.Fatal("invented old finding metadata")
				}
				registered = true
				return sdktest.HostResultValue([]byte(`{"reference":"tr_` + strings.Repeat("b", 64) + `","approved":false}`)), nil
			})
			var stable *pb.Message
			for attempt := 0; attempt < 2; attempt++ {
				request := reqWith(proto.Clone(original).(*pb.Message), incrementalTextMessage("continue"))
				if result := h.BeforeRequest(request); result.Err != nil {
					t.Fatal(result.Err)
				}
				text, isError := incrementalResultText(t, request.Messages[0])
				if !registered || !isError || strings.Contains(text, "synthetic private output") || strings.Contains(text, oldRef) || !strings.Contains(text, "tr_"+strings.Repeat("b", 64)) {
					t.Fatalf("unsafe replay: %s", text)
				}
				if stable != nil && !proto.Equal(stable, request.Messages[0]) {
					t.Fatal("migration changed repeated historical prefix")
				}
				stable = proto.Clone(request.Messages[0]).(*pb.Message)
			}
			if countCommand(h, "env.model_complete") != scans {
				t.Fatal("rescanned migrated history")
			}
			h.Run(func() {
				record, _, found, err := readReplayDecision(key)
				if err != nil || !found || record.Version != 3 || record.Replacement != base {
					t.Fatalf("migration not persisted: %+v, %v", record, err)
				}
			})
		})
	}
}

func TestReplayUpgradeRereadsConcurrentWinner(t *testing.T) {
	h := newHarness(t)
	key := "replay/occurrence/upgrade-conflict"
	old, _ := json.Marshal(replayRecord{Version: 2, Outcome: outcomeTransient, Replacement: "old failure"})
	h.SeedState(key, string(old))
	winner := replayRecord{Version: 3, Outcome: outcomeSensitive, Replacement: "concurrent protected result", Reason: sdk.ToolResultReleaseReason{Kind: "findings", Findings: []sdk.ToolResultReleaseFinding{{Type: "api_key", Line: 1}}}}
	h.StubHostCall("env.state_compare_and_set", func(args string) (string, error) {
		var input pb.StateCompareAndSetArgs
		if err := proto.Unmarshal([]byte(args), &input); err != nil {
			t.Fatal(err)
		}
		if input.ExpectedVersion == nil {
			t.Fatal("upgrade must compare the version read")
		}
		raw, _ := json.Marshal(winner)
		if err := sdk.StateSet(key, string(raw)); err != nil {
			t.Fatal(err)
		}
		response, _ := proto.Marshal(&pb.StateMutationResult{Applied: false})
		return sdktest.HostResultValue(response), nil
	})
	h.Run(func() {
		record, _, found, err := readReplayDecision(key)
		if err != nil || !found || record.Replacement != winner.Replacement || record.Outcome != outcomeSensitive {
			t.Fatalf("ignored concurrent decision: %+v, %v", record, err)
		}
	})
}

func TestReplayUpgradeStorageFailureAndUnknownVersionFailClosed(t *testing.T) {
	for _, version := range []int{2, 99} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			h := newHarness(t)
			key := "replay/occurrence/failed-upgrade"
			raw, _ := json.Marshal(replayRecord{Version: version, Outcome: outcomeSensitive, Replacement: "protected result"})
			h.SeedState(key, string(raw))
			h.DenyPermission("env.state_compare_and_set")
			h.Run(func() {
				if _, _, _, err := readReplayDecision(key); err == nil {
					t.Fatal("unsafe record or failed upgrade accepted")
				}
			})
		})
	}
}

func TestApprovalReadFailureDoesNotDiscloseHistory(t *testing.T) {
	h := newHarness(t)
	h.StubModelComplete(modelStub(`{"pii":true,"findings":[{"type":"unspecified","line":1}]}`))
	original := toolMsg("stable-call", "Read", textArm("synthetic private output"))
	if result := h.BeforeRequest(reqWith(proto.Clone(original).(*pb.Message))); result.Err != nil {
		t.Fatal(result.Err)
	}
	h.StubHostCall("torana_tool_result_release", func(string) (string, error) {
		return sdktest.HostResultError(pb.ErrorCode_ERROR_CODE_UNAVAILABLE, "approval state unavailable"), nil
	})
	if result := h.BeforeRequest(reqWith(proto.Clone(original).(*pb.Message), incrementalTextMessage("continue"))); result.Err == nil {
		t.Fatal("approval read failed open")
	}
}

func TestUnidentifiableToolCallStillWithholdsWithoutOfferingApproval(t *testing.T) {
	h := newHarness(t)
	h.StubModelComplete(modelStub(`{"pii":true,"findings":[{"type":"api_key","line":1}]}`))
	h.StubHostCall("torana_tool_result_release", func(string) (string, error) {
		return sdktest.HostResultValue([]byte(`{"reference":"","approved":false}`)), nil
	})
	request := reqWith(toolMsg("torana_gemini_semantic_0", "Read", textArm("synthetic private output")))
	if result := h.BeforeRequest(request); result.Err != nil {
		t.Fatal(result.Err)
	}
	text, _ := incrementalResultText(t, request.Messages[0])
	if strings.Contains(text, "synthetic private output") || strings.Contains(text, "redactions.request_release") || !strings.Contains(text, "no stable tool-call ID") {
		t.Fatalf("unsafe diagnostic: %s", text)
	}
}
