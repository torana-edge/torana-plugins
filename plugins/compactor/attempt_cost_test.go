package main

import (
	"encoding/json"
	"strings"
	"testing"

	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
)

// A paid but unusable response still belongs in the final batch's cost, and
// unknown usage must decline the entire batch instead of looking free.
func TestBatchCostIncludesDiscardedCompletions(t *testing.T) {
	for _, mode := range []string{"empty", "longer", "unknown", "nil"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t)
			h.SetConfig(modelConfig)
			calls := 0
			h.StubModelComplete(func(args *pbv1.ModelCompleteArgs) (*pbv1.ModelCompleteResult, *pbv1.HostError, error) {
				calls++
				if calls == 1 {
					return modelStub("useful summary")(args)
				}
				if mode == "nil" {
					return modelResult("", nil), nil, nil
				}
				result := modelResult("", &pbv1.Usage{InputTokens: 10000, OutputTokens: 50})
				if mode == "longer" {
					result = modelResult(strings.Repeat("no reduction", 10000), result.Usage)
				}
				if mode == "unknown" {
					result.Usage = nil
				}
				return result, nil, nil
			})
			var reportedInput float64
			h.StubHostCall("torana_evaluate_compaction", func(args string) (string, error) {
				var report map[string]any
				if err := json.Unmarshal([]byte(args), &report); err != nil {
					t.Fatal(err)
				}
				if summarizer, ok := report["summarizer"].(map[string]any); ok {
					reportedInput = summarizer["usage"].(map[string]any)["input_tokens"].(float64)
				}
				return sdktest.HostResultValue([]byte("{\"apply\":true}")), nil
			})
			req := bigToolRequest(bigContent())
			req.Messages = append(req.Messages, toolMsg("call_2", "read", strings.Repeat("second output\n", 1000)), &pbv1.Message{Role: "assistant", Blocks: []*pbv1.RequestBlock{requestTextBlock("reviewed")}})
			res := h.BeforeRequest(req)
			if res.Err != nil {
				t.Fatal(res.Err)
			}
			if calls != 2 {
				t.Fatalf("model calls = %d", calls)
			}
			if mode == "unknown" || mode == "nil" {
				if !res.PassedThrough || countCommand(h, "torana_record_savings") != 0 {
					t.Fatal("unknown attempt cost allowed compaction")
				}
				return
			}
			if reportedInput != 10100 {
				t.Fatalf("gate saw %.0f input tokens, want 10100", reportedInput)
			}
			if res.Request == nil || toolText(t, res.Request, 3) != "useful summary" {
				t.Fatal("usable candidate not applied")
			}
			for _, call := range h.Calls() {
				if call.Command != "torana_record_savings" {
					continue
				}
				var report struct{ Summarizer struct{ Usage tokenUsage } }
				if err := json.Unmarshal([]byte(call.Args), &report); err != nil {
					t.Fatal(err)
				}
				if report.Summarizer.Usage.InputTokens != 10100 {
					t.Fatalf("savings undercounted attempts: %+v", report)
				}
			}
		})
	}
}

func TestUnusablePaidBatchDoesNotClaimAnAppliedCompaction(t *testing.T) {
	for _, content := range []string{"", strings.Repeat("no reduction", 10000)} {
		h := newHarness(t)
		h.SetConfig(modelConfig)
		h.StubHostCall("torana_evaluate_compaction", applyStub(true))
		h.StubModelComplete(func(*pbv1.ModelCompleteArgs) (*pbv1.ModelCompleteResult, *pbv1.HostError, error) {
			return modelResult(content, &pbv1.Usage{InputTokens: 100, OutputTokens: 20}), nil, nil
		})
		res := h.BeforeRequest(bigToolRequest(bigContent()))
		if res.Err != nil || !res.PassedThrough {
			t.Fatalf("unusable output changed request: %v", res.Err)
		}
		if countCommand(h, "env.model_complete") != 1 {
			t.Fatal("missing paid attempt")
		}
		if countCommand(h, "torana_record_savings") != 0 {
			t.Fatal("unapplied batch claimed a compaction")
		}
	}
}
