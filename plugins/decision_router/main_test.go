package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
	"google.golang.org/protobuf/proto"
)

const baseConfig = `{
  "decision_model":"jev-latest",
  "question":"Which model tier should handle this request?",
  "routes":{
    "fast":{"description":"Mechanical or straightforward work","provider":"fast-provider","model":"fast-model"},
    "reasoning":{"description":"Complex or ambiguous work","provider":"reasoning-provider","model":"reasoning-model"}
  },
  "minimum_confidence":0.8,
  "max_state_bytes":8192,
  "timeout_ms":5000,
  "authentication":"none",
  "sticky":true
}`

func textBlock(text string) *pbv1.RequestBlock {
	return &pbv1.RequestBlock{Kind: &pbv1.RequestBlock_Text{Text: &pbv1.RequestTextBlock{Text: text}}}
}

func request(conversation, text string) *pbv1.ChatRequest {
	return &pbv1.ChatRequest{
		Model: "original-model",
		Messages: []*pbv1.Message{
			{Role: "system", Blocks: []*pbv1.RequestBlock{textBlock("private historical system prompt")}},
			{Role: "user", Blocks: []*pbv1.RequestBlock{textBlock("old user turn must not be sent")}},
			{Role: "assistant", Blocks: []*pbv1.RequestBlock{textBlock("old answer must not be sent")}},
			{Role: "user", Blocks: []*pbv1.RequestBlock{textBlock(text)}},
		},
		Tools:          []*pbv1.ToolDef{{Name: "read", ParametersJson: []byte(`{"type":"object"}`)}, {Name: "shell", ParametersJson: []byte(`{"type":"object"}`)}},
		ToranaMetaJson: []byte(`{"_conversation_id":"` + conversation + `","_provider":"original"}`),
	}
}

func response(choice string, confidence float64) []byte {
	raw, _ := json.Marshal(map[string]any{"model": "jev-1.13", "answers": map[string]any{"route": map[string]any{
		"type": "choice", "choice": choice, "confidence": confidence,
		"probabilities": map[string]float64{"fast": 0.1, "reasoning": 0.9},
	}}})
	return raw
}

func stubDecision(h *sdktest.Harness, status int32, body []byte, capture func(*pbv1.OutboundHTTPRequestArgs)) {
	h.StubHostCall("env.http_request", func(args string) (string, error) {
		var request pbv1.OutboundHTTPRequestArgs
		if err := proto.Unmarshal([]byte(args), &request); err != nil {
			return "", err
		}
		if capture != nil {
			capture(&request)
		}
		raw, err := proto.Marshal(&pbv1.OutboundHTTPResponse{Status: status, Body: body})
		if err != nil {
			return "", err
		}
		return sdktest.HostResultValue(raw), nil
	})
}

func routes(h *sdktest.Harness) []*pbv1.RouteRequestArgs {
	var out []*pbv1.RouteRequestArgs
	for _, call := range h.Calls() {
		if call.Command != "env.route_request" {
			continue
		}
		var route pbv1.RouteRequestArgs
		if proto.Unmarshal([]byte(call.Args), &route) == nil {
			out = append(out, &route)
		}
	}
	return out
}

func httpCalls(h *sdktest.Harness) int {
	n := 0
	for _, call := range h.Calls() {
		if call.Command == "env.http_request" {
			n++
		}
	}
	return n
}

func TestRoutesFromValidatedClosedChoice(t *testing.T) {
	h := sdktest.New(t).SetConfig(baseConfig)
	var sent *pbv1.OutboundHTTPRequestArgs
	stubDecision(h, 200, response("reasoning", 0.91), func(req *pbv1.OutboundHTTPRequestArgs) { sent = proto.Clone(req).(*pbv1.OutboundHTTPRequestArgs) })
	req := request("conversation-a", "Debug this race condition")
	original := proto.Clone(req).(*pbv1.ChatRequest)
	result := h.BeforeRequest(req)
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	if !proto.Equal(req, original) {
		t.Fatal("plugin mutated request content")
	}
	got := routes(h)
	if len(got) != 1 || got[0].Provider != "reasoning-provider" || got[0].Model != "reasoning-model" {
		t.Fatalf("routes = %+v", got)
	}
	if sent.Endpoint != endpointSlot || sent.Path != decisionPath || sent.Method != "POST" || sent.TimeoutMs != 5000 {
		t.Fatalf("outbound request = %+v", sent)
	}
	var body systemOneRequest
	if err := json.Unmarshal(sent.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body.State.LatestUserTurn != "Debug this race condition" {
		t.Fatalf("state = %+v", body.State)
	}
	wire := string(sent.Body)
	for _, forbidden := range []string{"private historical", "old user turn", "old answer"} {
		if strings.Contains(wire, forbidden) {
			t.Fatalf("sent historical content %q", forbidden)
		}
	}
}

func TestStickyDecisionAvoidsRepeatedDecisionCalls(t *testing.T) {
	h := sdktest.New(t).SetConfig(baseConfig)
	stubDecision(h, 200, response("fast", 0.95), nil)
	if res := h.BeforeRequest(request("sticky", "first")); res.Err != nil {
		t.Fatal(res.Err)
	}
	if res := h.BeforeRequest(request("sticky", "a completely different later turn")); res.Err != nil {
		t.Fatal(res.Err)
	}
	if got := httpCalls(h); got != 1 {
		t.Fatalf("HTTP calls = %d, want 1", got)
	}
	if got := routes(h); len(got) != 2 || got[0].Model != "fast-model" || got[1].Model != "fast-model" {
		t.Fatalf("routes = %+v", got)
	}
}

func TestDecisionSurvivesRestart(t *testing.T) {
	h1 := sdktest.New(t).SetConfig(baseConfig)
	stubDecision(h1, 200, response("reasoning", 0.95), nil)
	if res := h1.BeforeRequest(request("restart", "first")); res.Err != nil {
		t.Fatal(res.Err)
	}
	key := decisionStateKey("restart")
	raw, ok := h1.State(key)
	if !ok {
		t.Fatal("decision was not persisted")
	}

	h2 := sdktest.New(t).SetConfig(baseConfig).SeedState(key, raw)
	if res := h2.BeforeRequest(request("restart", "resumed")); res.Err != nil {
		t.Fatal(res.Err)
	}
	if httpCalls(h2) != 0 {
		t.Fatal("restart re-called decision endpoint")
	}
	if got := routes(h2); len(got) != 1 || got[0].Model != "reasoning-model" {
		t.Fatalf("routes = %+v", got)
	}
}

func TestConfigChangeDoesNotReplayStaleRoute(t *testing.T) {
	h := sdktest.New(t).SetConfig(baseConfig)
	stubDecision(h, 200, response("fast", 0.95), nil)
	if res := h.BeforeRequest(request("policy-change", "first")); res.Err != nil {
		t.Fatal(res.Err)
	}
	changed := strings.Replace(baseConfig, "fast-model", "new-fast-model", 1)
	h.SetConfig(changed)
	if res := h.BeforeRequest(request("policy-change", "second")); res.Err != nil {
		t.Fatal(res.Err)
	}
	if httpCalls(h) != 2 {
		t.Fatalf("HTTP calls = %d, want 2", httpCalls(h))
	}
	got := routes(h)
	if len(got) != 2 || got[0].Model != "fast-model" || got[1].Model != "new-fast-model" {
		t.Fatalf("routes = %+v", got)
	}
}

func TestOmittedStickyDefaultHasSamePolicyHashAsExplicitTrue(t *testing.T) {
	omitted := strings.Replace(baseConfig, ",\n  \"sticky\":true", "", 1)
	h := sdktest.New(t).SetConfig(omitted)
	stubDecision(h, 200, response("fast", 0.95), nil)
	if res := h.BeforeRequest(request("normalized-default", "first")); res.Err != nil {
		t.Fatal(res.Err)
	}
	h.SetConfig(baseConfig)
	if res := h.BeforeRequest(request("normalized-default", "second")); res.Err != nil {
		t.Fatal(res.Err)
	}
	if httpCalls(h) != 1 {
		t.Fatalf("equivalent default changed policy hash: HTTP calls = %d", httpCalls(h))
	}
}

func TestUnavailableDurableStateDeclinesBeforeDecisionCall(t *testing.T) {
	h := sdktest.New(t).SetConfig(baseConfig)
	h.StateConfigured = false
	if res := h.BeforeRequest(request("no-state", "do not send")); res.Err != nil {
		t.Fatal(res.Err)
	}
	if httpCalls(h) != 0 || len(routes(h)) != 0 {
		t.Fatal("unavailable durable state made an external or route call")
	}
}

func TestSafeFallbacksNeverRoute(t *testing.T) {
	tests := []struct {
		name      string
		status    int32
		body      []byte
		config    string
		stubError bool
	}{
		{"endpoint error", 0, nil, baseConfig, true},
		{"HTTP status", 503, []byte(`{"error":"busy"}`), baseConfig, false},
		{"malformed JSON", 200, []byte(`{"answers":`), baseConfig, false},
		{"missing answer", 200, []byte(`{"answers":{}}`), baseConfig, false},
		{"unknown choice", 200, response("attacker-provider", 0.99), baseConfig, false},
		{"low confidence", 200, response("fast", 0.79), baseConfig, false},
		{"adversarial extra field", 200, []byte(`{"answers":{"route":{"type":"choice","choice":"fast","confidence":0.99,"provider":"attacker"}}}`), baseConfig, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := sdktest.New(t).SetConfig(tc.config)
			if tc.stubError {
				h.StubHostCall("env.http_request", func(string) (string, error) { return "", errors.New("offline") })
			} else {
				stubDecision(h, tc.status, tc.body, nil)
			}
			req := request("fallback-"+tc.name, "route me")
			before := proto.Clone(req).(*pbv1.ChatRequest)
			res := h.BeforeRequest(req)
			if res.Err != nil {
				t.Fatalf("fallback returned error: %v", res.Err)
			}
			if len(routes(h)) != 0 {
				t.Fatalf("unexpected route: %+v", routes(h))
			}
			if !proto.Equal(req, before) {
				t.Fatal("fallback mutated request")
			}
		})
	}
}

func TestMissingConversationDeclinesBeforeSendingContent(t *testing.T) {
	h := sdktest.New(t).SetConfig(baseConfig)
	req := request("", "do not send")
	req.ToranaMetaJson = nil
	if res := h.BeforeRequest(req); res.Err != nil {
		t.Fatal(res.Err)
	}
	if httpCalls(h) != 0 || len(routes(h)) != 0 {
		t.Fatal("missing identity made external or route call")
	}
}

func TestBearerCredentialIsSeparateAndNeverLogged(t *testing.T) {
	config := strings.Replace(baseConfig, `"authentication":"none"`, `"authentication":"bearer"`, 1)
	const secret = "token-that-must-stay-private"
	h := sdktest.New(t).SetConfig(config).SetCredential(credentialSlot, []byte(secret))
	var authorization string
	stubDecision(h, 200, response("fast", 0.95), func(req *pbv1.OutboundHTTPRequestArgs) {
		for _, header := range req.Headers {
			if strings.EqualFold(header.Name, "Authorization") && len(header.Values) == 1 {
				authorization = header.Values[0]
			}
		}
	})
	if res := h.BeforeRequest(request("auth", "hello")); res.Err != nil {
		t.Fatal(res.Err)
	}
	if authorization != "Bearer "+secret {
		t.Fatalf("authorization = %q", authorization)
	}
	for _, log := range h.Logs() {
		if strings.Contains(log.Message, secret) {
			t.Fatal("credential leaked to logs")
		}
	}
}

func TestLocalModeDoesNotReadCredential(t *testing.T) {
	h := sdktest.New(t).SetConfig(baseConfig)
	stubDecision(h, 200, response("fast", 0.95), nil)
	if res := h.BeforeRequest(request("local", "hello")); res.Err != nil {
		t.Fatal(res.Err)
	}
	for _, call := range h.Calls() {
		if call.Command == "env.credential_get" {
			t.Fatal("local mode read a credential")
		}
	}
}

func TestLatestTurnIsUTF8Bounded(t *testing.T) {
	config := strings.Replace(baseConfig, `"max_state_bytes":8192`, `"max_state_bytes":256`, 1)
	h := sdktest.New(t).SetConfig(config)
	var text string
	stubDecision(h, 200, response("fast", 0.95), func(req *pbv1.OutboundHTTPRequestArgs) {
		var body systemOneRequest
		if json.Unmarshal(req.Body, &body) != nil {
			t.Fatal("bad request body")
		}
		text = body.State.LatestUserTurn
	})
	if res := h.BeforeRequest(request("bounded", strings.Repeat("雪", 200))); res.Err != nil {
		t.Fatal(res.Err)
	}
	if len(text) > 256 || !json.Valid([]byte(`"`+text+`"`)) {
		t.Fatalf("invalid bound: bytes=%d", len(text))
	}
}

func TestInvalidConfigSurfacesOperatorError(t *testing.T) {
	h := sdktest.New(t).SetConfig(`{"decision_model":"jev","question":"q","routes":{"only":{"description":"one","model":"m"}}}`)
	if res := h.BeforeRequest(request("invalid", "hello")); res.Err == nil {
		t.Fatal("invalid config was silently accepted")
	}
}
