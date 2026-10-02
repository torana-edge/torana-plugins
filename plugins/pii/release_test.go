package main

import (
	"strings"
	"testing"

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
