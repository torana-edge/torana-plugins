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
		// Identical to the line above in JSON Schema terms: an empty
		// properties map permits any property, exactly as an absent one does.
		// This case used to be asserted as "should be closed", which enshrined
		// the bug — a tool accepting anything became one accepting only "i".
		"explicitly empty properties (same schema, spelled out)": `{"type":"object","properties":{}}`,
		"open map alongside properties":                          `{"type":"object","properties":{"path":{"type":"string"}},"additionalProperties":true}`,
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
		"named properties":          `{"type":"object","properties":{"path":{"type":"string"}}}`,
		"several named properties":  `{"type":"object","properties":{"path":{"type":"string"},"mode":{"type":"string"}}}`,
		"named plus closed already": `{"type":"object","properties":{"path":{"type":"string"}},"additionalProperties":false}`,
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
// TestIntentFieldIsRequiredExactlyOnce guards the dedup in the required list.
//
// Calling injectIntentSchema twice does NOT exercise it — the second call takes
// the "already declared" branch and never reaches the append. The case that
// matters is a tool that lists "i" as required without declaring it as a
// property, which is what this now uses.
func TestIntentFieldIsRequiredExactlyOnce(t *testing.T) {
	got := inject(t, `{"type":"object","properties":{"path":{"type":"string"}},"required":["path","i"]}`)

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
	if !hasIntentField(t, got) {
		t.Error("the property should still be added even though required already named it")
	}
}

// TestStrictSchemasSurviveInjection settles the OpenAI strict-mode question.
//
// Under strict function calling the caller's own schema must already declare
// additionalProperties:false. This plugin only ever SETS that key, never
// unsets it, so a schema that was strict-compliant on the way in is still
// strict-compliant on the way out — with "i" added to properties and required,
// which is what strict mode demands of every declared property.
//
// The corollary matters too: leaving a bare {"type":"object"} open cannot break
// strict mode, because such a schema was already non-compliant before Torana
// saw it.
func TestStrictSchemasSurviveInjection(t *testing.T) {
	got := inject(t, `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`)

	if got["additionalProperties"] != false {
		t.Errorf("a strict-compliant schema lost additionalProperties:false: %v", got["additionalProperties"])
	}
	if !hasIntentField(t, got) {
		t.Fatal("intent field not injected")
	}
	// Strict mode requires every property to be listed in required.
	required, _ := got["required"].([]any)
	var found bool
	for _, r := range required {
		if s, ok := r.(string); ok && s == intentField {
			found = true
		}
	}
	if !found {
		t.Errorf("under strict mode every property must be required; %q is not in %v", intentField, required)
	}
}
