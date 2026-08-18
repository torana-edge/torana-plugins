package main

import (
	"bytes"
	"strings"
	"testing"

	pbv2 "github.com/torana-edge/torana-plugin-sdk/pb/v2"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
	"google.golang.org/protobuf/proto"
)

func tool(name, description, parameters string, strict bool, marker string) *pbv2.ToolDef {
	return &pbv2.ToolDef{
		Name:             name,
		Description:      description,
		ParametersJson:   []byte(parameters),
		Strict:           strict,
		CacheControlJson: []byte(marker),
	}
}

func requestWithTools(tools ...*pbv2.ToolDef) *pbv2.ChatRequest {
	return &pbv2.ChatRequest{
		Model: "model",
		Messages: []*pbv2.Message{{Role: "user", Blocks: []*pbv2.RequestBlock{{
			Kind: &pbv2.RequestBlock_Text{Text: &pbv2.RequestTextBlock{Text: "help"}},
		}}}},
		Tools: tools,
	}
}

func TestParsePolicyContract(t *testing.T) {
	p, err := parsePolicy([]byte(`{
		"allow":["read","search"],
		"deny":["shell"],
		"replace":{"read":{"description":"safe","parameters":{"z":1e999,"a":1.0},"strict":false}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if !p.allowPresent || len(p.allow) != 2 || len(p.deny) != 1 || len(p.replace) != 1 {
		t.Fatalf("policy = %#v", p)
	}
	r := p.replace["read"]
	if r.description == nil || *r.description != "safe" || r.strict == nil || *r.strict {
		t.Fatalf("replacement presence lost: %#v", r)
	}
	if got, want := string(r.parameters), `{"z":1e999,"a":1.0}`; got != want {
		t.Fatalf("parameters = %s, want exact %s", got, want)
	}
}

func TestParsePolicyRejectsAmbiguityAndParserDifferentials(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"top-level null", `null`, "object"},
		{"unknown member", `{"mode":"allow"}`, "unknown member"},
		{"duplicate top-level", `{"allow":[],"\u0061llow":[]}`, "duplicate"},
		{"allow null", `{"allow":null}`, "must not be null"},
		{"allow wrong shape", `{"allow":{}}`, "array"},
		{"allow empty name", `{"allow":[""]}`, "empty tool name"},
		{"allow duplicate", `{"allow":["read","read"]}`, "duplicate"},
		{"deny overlap", `{"allow":["read"],"deny":["read"]}`, "both allow and deny"},
		{"replace null", `{"replace":null}`, "must not be null"},
		{"replace empty name", `{"replace":{"":{"strict":true}}}`, "empty tool name"},
		{"replacement empty", `{"replace":{"read":{}}}`, "at least one"},
		{"replacement unknown", `{"replace":{"read":{"schema":{}}}}`, "unknown member"},
		{"description null", `{"replace":{"read":{"description":null}}}`, "must not be null"},
		{"parameters array", `{"replace":{"read":{"parameters":[]}}}`, "object"},
		{"parameters duplicate", `{"replace":{"read":{"parameters":{"a":1,"\u0061":2}}}}`, "duplicate"},
		{"parameters lone surrogate", `{"replace":{"read":{"parameters":{"a":"\ud800"}}}}`, "surrogate"},
		{"trailing JSON", `{} {}`, "trailing"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parsePolicy([]byte(tc.raw))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want substring %q", err, tc.want)
			}
		})
	}

	invalidUTF8 := []byte{'{', '"', 0xff, '"', ':', '{', '}', '}'}
	if _, err := parsePolicy(invalidUTF8); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("invalid UTF-8 error = %v", err)
	}
}

func TestApplyPolicyFiltersAndReplacesStructurally(t *testing.T) {
	input := requestWithTools(
		tool("read", "old", `{"old":1}`, true, `{"type":"ephemeral"}`),
		tool("search", "keep", `{"q":1}`, false, `{"ttl":"1h"}`),
		tool("shell", "remove", `{}`, false, ``),
	)
	before := proto.Clone(input).(*pbv2.ChatRequest)
	p, err := parsePolicy([]byte(`{
		"allow":["read","search"],
		"deny":["shell"],
		"replace":{"read":{"description":"approved","parameters":{"a":1.0},"strict":false}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	got, changed, err := applyPolicy(input, p)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if !proto.Equal(input, before) {
		t.Fatal("input request was mutated")
	}
	expected := proto.Clone(before).(*pbv2.ChatRequest)
	expected.Tools = expected.Tools[:2]
	expected.Tools[0].Description = "approved"
	expected.Tools[0].ParametersJson = []byte(`{"a":1.0}`)
	expected.Tools[0].Strict = false
	if !proto.Equal(got, expected) {
		t.Fatalf("result differs from independent expected\n got: %v\nwant: %v", got, expected)
	}
	if !bytes.Equal(got.Tools[0].CacheControlJson, before.Tools[0].CacheControlJson) ||
		!bytes.Equal(got.Tools[1].CacheControlJson, before.Tools[1].CacheControlJson) {
		t.Fatal("cache-control marker changed")
	}
}

func TestApplyPolicyNoopAndRemoveAll(t *testing.T) {
	input := requestWithTools(tool("read", "d", `{}`, false, `{"x":1}`))
	before := proto.Clone(input).(*pbv2.ChatRequest)

	noop, err := parsePolicy([]byte(`{"replace":{"absent":{"strict":true},"read":{"description":"d","parameters":{},"strict":false}}}`))
	if err != nil {
		t.Fatal(err)
	}
	got, changed, err := applyPolicy(input, noop)
	if err != nil || changed || got != input || !proto.Equal(input, before) {
		t.Fatalf("no-op changed=%v err=%v equal=%v same-pointer=%v", changed, err, proto.Equal(input, before), got == input)
	}

	empty, err := parsePolicy([]byte(`{"allow":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	got, changed, err = applyPolicy(input, empty)
	if err != nil || !changed || len(got.Tools) != 0 {
		t.Fatalf("remove-all changed=%v err=%v tools=%v", changed, err, got.Tools)
	}
	if !proto.Equal(input, before) {
		t.Fatal("remove-all mutated input")
	}
}

func TestApplyPolicyDuplicateInputIsAtomic(t *testing.T) {
	input := requestWithTools(tool("read", "a", `{}`, false, ``), tool("read", "b", `{}`, false, ``))
	before := proto.Clone(input).(*pbv2.ChatRequest)
	p, err := parsePolicy([]byte(`{"deny":["read"]}`))
	if err != nil {
		t.Fatal(err)
	}
	got, changed, err := applyPolicy(input, p)
	if err == nil || changed || got != nil || !strings.Contains(err.Error(), "duplicate tool definition") {
		t.Fatalf("got=%v changed=%v err=%v", got, changed, err)
	}
	if !proto.Equal(input, before) {
		t.Fatal("error path mutated input")
	}
}

func TestHookUsesOnlyConfigAndReturnsExactReplacement(t *testing.T) {
	h := sdktest.New(t)
	h.SetConfig(`{"deny":["shell"],"replace":{"read":{"description":"approved"}}}`)
	input := requestWithTools(
		tool("read", "old", `{}`, false, `{"type":"ephemeral"}`),
		tool("shell", "danger", `{}`, false, ``),
	)
	before := proto.Clone(input).(*pbv2.ChatRequest)
	res := h.BeforeRequest(input)
	if res.Err != nil || res.Request == nil || res.PassedThrough {
		t.Fatalf("result = %+v", res)
	}
	expected := proto.Clone(before).(*pbv2.ChatRequest)
	expected.Tools = expected.Tools[:1]
	expected.Tools[0].Description = "approved"
	if !proto.Equal(res.Request, expected) {
		t.Fatalf("replacement differs\n got: %v\nwant: %v", res.Request, expected)
	}
	if !proto.Equal(input, before) {
		t.Fatal("hook mutated input")
	}
	calls := h.Calls()
	if len(calls) != 1 || calls[0].Command != "env.plugin_config" {
		t.Fatalf("host calls = %+v, want exactly env.plugin_config", calls)
	}
}

func TestHookInvalidPolicyFailsClosedWithoutMutation(t *testing.T) {
	h := sdktest.New(t)
	h.SetConfig(`{"allow":["read"],"deny":["read"]}`)
	input := requestWithTools(tool("read", "d", `{}`, false, ``))
	before := proto.Clone(input).(*pbv2.ChatRequest)
	res := h.BeforeRequest(input)
	if res.Err == nil || !strings.Contains(res.Err.Error(), "invalid configuration") {
		t.Fatalf("error = %v", res.Err)
	}
	if res.Request != nil || !proto.Equal(input, before) {
		t.Fatal("invalid policy produced or mutated a request")
	}
	if calls := h.Calls(); len(calls) != 1 || calls[0].Command != "env.plugin_config" {
		t.Fatalf("host calls = %+v", calls)
	}
}
