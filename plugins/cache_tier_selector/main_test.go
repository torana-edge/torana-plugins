package main

import (
	"testing"

	"github.com/torana-edge/torana-plugin-sdk/pb"
)

// cachePrefixKey fingerprints the cacheable prefix so a tier decision sticks
// across turns of the same conversation. It has to agree with what the PROVIDER
// hashes: if Torana thinks two prefixes are the same and the provider does not,
// the sticky long-tier decision is reused for something that cannot hit, and
// the expensive cache write it implies is wasted — the exact cost the plugin
// exists to avoid.

func msg(role, content string) *pb.Message {
	return &pb.Message{Role: role, Content: content, CacheControlJson: []byte(`{"type":"ephemeral"}`)}
}

func baseRequest() *pb.ChatRequest {
	return &pb.ChatRequest{
		Model:    "claude-sonnet-4",
		Messages: []*pb.Message{msg("system", "you are a coding agent"), msg("user", "find the bug")},
	}
}

// TestFingerprintCoversEverythingInThePrefix — each of these fields is sent
// upstream and is part of the provider's cached prefix, so changing one must
// change the fingerprint.
func TestFingerprintCoversEverythingInThePrefix(t *testing.T) {
	for name, mutate := range map[string]func(*pb.ChatRequest){
		"content":            func(r *pb.ChatRequest) { r.Messages[1].Content = "different" },
		"model":              func(r *pb.ChatRequest) { r.Model = "claude-opus-4" },
		"role":               func(r *pb.ChatRequest) { r.Messages[1].Role = "assistant" },
		"thinking":           func(r *pb.ChatRequest) { r.Messages[1].Thinking = "reasoned about it" },
		"thinking signature": func(r *pb.ChatRequest) { r.Messages[1].ThinkingSignature = "sig-abc" },
		"redacted thinking":  func(r *pb.ChatRequest) { r.Messages[1].RedactedThinking = "[redacted]" },
		"cache_control":      func(r *pb.ChatRequest) { r.Messages[1].CacheControlJson = []byte(`{"type":"persistent"}`) },
		"tool result id":     func(r *pb.ChatRequest) { r.Messages[1].ToolCallId = "call_9" },
		"tool result name":   func(r *pb.ChatRequest) { r.Messages[1].ToolName = "read" },
		"content parts":      func(r *pb.ChatRequest) { r.Messages[1].ContentPartsJson = []byte(`[{"type":"text"}]`) },
		"tool definition": func(r *pb.ChatRequest) {
			r.Tools = []*pb.ToolDef{{Name: "read", ParametersJson: []byte(`{}`)}}
		},
		"assistant tool call": func(r *pb.ChatRequest) {
			r.Messages[1].ToolCalls = []*pb.ToolCall{{Id: "call_1", Name: "read", ArgumentsJson: []byte(`{}`)}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			before := cachePrefixKey(baseRequest())
			if before == "" {
				t.Fatal("no breakpoint found in the fixture; the test would be vacuous")
			}
			changed := baseRequest()
			mutate(changed)

			if got := cachePrefixKey(changed); got == before {
				t.Errorf("changing %s did not change the fingerprint.\n"+
					"It is part of what goes upstream, so the provider sees a different "+
					"prefix while Torana reuses the previous tier decision.", name)
			}
		})
	}
}

// The other direction: an identical prefix must fingerprint identically, or
// stickiness never holds and every turn re-decides.
func TestFingerprintIsStableForAnIdenticalPrefix(t *testing.T) {
	if a, b := cachePrefixKey(baseRequest()), cachePrefixKey(baseRequest()); a != b {
		t.Errorf("identical requests fingerprinted differently: %q vs %q", a, b)
	}
}

// Messages after the last breakpoint are not part of the cached prefix, so they
// must not affect it — otherwise the decision churns on every new turn.
func TestFingerprintIgnoresContentAfterTheBreakpoint(t *testing.T) {
	before := cachePrefixKey(baseRequest())

	extended := baseRequest()
	extended.Messages = append(extended.Messages, &pb.Message{Role: "assistant", Content: "thinking out loud"})

	if got := cachePrefixKey(extended); got != before {
		t.Errorf("an unmarked message after the breakpoint changed the fingerprint:\n  %q\n  %q", before, got)
	}
}

func TestDecisionExpiresWithProviderTier(t *testing.T) {
	value := decision{DecidedAtMillis: 1_000, TierTTL: 300}
	if decisionExpired(value, 300_999) {
		t.Fatal("decision expired before its provider cache tier")
	}
	if !decisionExpired(value, 301_000) {
		t.Fatal("decision remained sticky after its provider cache tier expired")
	}
}

func TestDecisionWithoutUsableClockOrTTLDoesNotExpire(t *testing.T) {
	for _, value := range []decision{
		{},
		{DecidedAtMillis: 1_000},
		{TierTTL: 300},
	} {
		if decisionExpired(value, 999_999) {
			t.Errorf("incomplete decision unexpectedly expired: %+v", value)
		}
	}
}
