package main

import (
	"encoding/json"
	"testing"

	"github.com/torana-edge/torana-plugin-sdk/pb"
)

// Same rule as schema_translator's tests: never make a tool's schema stricter
// than its author wrote it. This plugin adds an "i" field to every tool, and it
// used to close additionalProperties unconditionally while doing so — turning a
// tool that accepted free-form arguments into one that accepts nothing but "i".

func inject(t *testing.T, params string) map[string]any {
	t.Helper()
	req := &pb.ChatRequest{Tools: []*pb.ToolDef{{
		Name:           "test_tool",
		ParametersJson: []byte(params),
	}}}
	injectIntentSchema(req)

	var got map[string]any
	if err := json.Unmarshal(req.Tools[0].ParametersJson, &got); err != nil {
		t.Fatalf("plugin emitted unparseable schema: %v", err)
	}
	return got
}

func hasIntentField(t *testing.T, schema map[string]any) bool {
	t.Helper()
	props, _ := schema["properties"].(map[string]any)
	_, ok := props[intentField]
	return ok
}

// TestOpenSchemasAreNotClosed is the regression test. Injecting "i" is fine in
// every one of these cases; forbidding everything else is not.
func TestOpenSchemasAreNotClosed(t *testing.T) {
	for name, params := range map[string]string{
		"author declared an open map":             `{"type":"object","additionalProperties":{"type":"string"}}`,
		"author declared an open map, bool":       `{"type":"object","additionalProperties":true}`,
		"no properties at all (accepts anything)": `{"type":"object"}`,
		"open map alongside properties":           `{"type":"object","properties":{"path":{"type":"string"}},"additionalProperties":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			got := inject(t, params)

			if got["additionalProperties"] == false {
				t.Error("schema was closed — stricter than the author wrote it, and a tool that accepted free-form arguments now accepts only \"i\"")
			}
			if !hasIntentField(t, got) {
				t.Error("the intent field should still be injected; only the closing is withheld")
			}
		})
	}
}

// TestNamedArgumentSchemasAreStillClosed pins the behaviour that is correct: a
// tool that listed its arguments by name and did not ask to stay open is closed
// as before. Narrowing the rule must not disable it.
func TestNamedArgumentSchemasAreStillClosed(t *testing.T) {
	for name, params := range map[string]string{
		"named properties":            `{"type":"object","properties":{"path":{"type":"string"}}}`,
		"explicitly empty properties": `{"type":"object","properties":{}}`,
	} {
		t.Run(name, func(t *testing.T) {
			got := inject(t, params)

			if got["additionalProperties"] != false {
				t.Errorf("expected the schema to be closed, got additionalProperties=%v", got["additionalProperties"])
			}
			if !hasIntentField(t, got) {
				t.Error("intent field not injected")
			}
		})
	}
}

// TestRootTypeIsPreserved — this plugin has no reason to change the root type,
// and asserting it keeps the two plugins honest about the same contract.
func TestRootTypeIsPreserved(t *testing.T) {
	for _, params := range []string{
		`{"type":"object"}`,
		`{"type":"object","properties":{"path":{"type":"string"}}}`,
		`{"type":"object","additionalProperties":true}`,
	} {
		got := inject(t, params)
		if got["type"] != "object" {
			t.Errorf("%s: root type became %v", params, got["type"])
		}
	}
}

// TestIntentFieldIsRequiredExactlyOnce guards the required list against
// duplicate entries across repeated injection.
func TestIntentFieldIsRequiredExactlyOnce(t *testing.T) {
	req := &pb.ChatRequest{Tools: []*pb.ToolDef{{
		Name:           "test_tool",
		ParametersJson: []byte(`{"type":"object","properties":{"path":{"type":"string"}}}`),
	}}}
	injectIntentSchema(req)
	injectIntentSchema(req)

	var got map[string]any
	if err := json.Unmarshal(req.Tools[0].ParametersJson, &got); err != nil {
		t.Fatal(err)
	}
	required, _ := got["required"].([]any)
	count := 0
	for _, r := range required {
		if s, ok := r.(string); ok && s == intentField {
			count++
		}
	}
	if count != 1 {
		t.Errorf("intent field appears %d times in required, want 1: %v", count, required)
	}
}
