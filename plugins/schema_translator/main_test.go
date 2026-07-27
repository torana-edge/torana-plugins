package main

import (
	"encoding/json"
	"testing"
)

// Two rules, asserted here and mirrored in intent's tests:
//
//  1. Never change a tool's ROOT type. Tool parameters must be a JSON Schema
//     object; OpenAI, Anthropic and Gemini all reject "type": "array" there.
//  2. Never make a schema stricter than its author wrote it.
//
// Rule 1 was violated by the commonest MCP shape there is — a no-argument tool
// declaring a bare {"type": "object"} — which was rewritten into an array and
// became uncallable on every major provider.

func translate(t *testing.T, raw string) map[string]any {
	t.Helper()
	var schema map[string]any
	if err := json.Unmarshal([]byte(raw), &schema); err != nil {
		t.Fatalf("bad fixture: %v", err)
	}
	translateSchema("test_tool", schema, "", true)
	return schema
}

// TestRootTypeIsAlwaysPreserved is the golden test for the bug: whatever the
// input, a tool's root parameters come back as an object.
func TestRootTypeIsAlwaysPreserved(t *testing.T) {
	for name, raw := range map[string]string{
		"bare object (no-argument tool)": `{"type":"object"}`,
		"bare object with description":   `{"type":"object","description":"no args"}`,
		"empty properties":               `{"type":"object","properties":{}}`,
		"open map at root":               `{"type":"object","additionalProperties":{"type":"string"}}`,
		"open map at root, bool form":    `{"type":"object","additionalProperties":true}`,
		"normal object":                  `{"type":"object","properties":{"path":{"type":"string"}}}`,
		"object with nested open map":    `{"type":"object","properties":{"env":{"type":"object","additionalProperties":{"type":"string"}}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			got := translate(t, raw)
			if got["type"] != "object" {
				t.Errorf("root type became %v — providers reject a non-object at the root, making the tool uncallable", got["type"])
			}
			if _, isArray := got["items"]; isArray {
				t.Error("root was rewritten into a key-value array")
			}
		})
	}
}

// TestRootOpenMapSurvives covers rule 2 at the root, where the rescue that
// exists inside the property loop never ran — so an author's deliberate open
// map was silently closed.
func TestRootOpenMapSurvives(t *testing.T) {
	for name, raw := range map[string]string{
		"schema form": `{"type":"object","additionalProperties":{"type":"string"}}`,
		"bool form":   `{"type":"object","additionalProperties":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			got := translate(t, raw)
			if got["additionalProperties"] == false {
				t.Error("author declared an open map at the root and it was closed — stricter than written")
			}
		})
	}
}

// TestNestedOpenMapStillConverts pins the behaviour that is correct and must
// not regress: below the root, an open map becomes a KV array, because a model
// that cannot emit free-form maps can still populate one.
func TestNestedOpenMapStillConverts(t *testing.T) {
	got := translate(t, `{"type":"object","properties":{"env":{"type":"object","additionalProperties":{"type":"string"}}}}`)

	props, _ := got["properties"].(map[string]any)
	env, ok := props["env"].(map[string]any)
	if !ok {
		t.Fatalf("env property missing: %v", got)
	}
	if env["type"] != "array" {
		t.Errorf("nested open map should convert to a KV array, got type=%v", env["type"])
	}
	items, ok := env["items"].(map[string]any)
	if !ok {
		t.Fatalf("KV array has no items: %v", env)
	}
	itemProps, _ := items["properties"].(map[string]any)
	if _, hasKey := itemProps["key"]; !hasKey {
		t.Errorf("KV array items missing the key field: %v", items)
	}
}

// TestNestedBareObjectStillConverts is the same guard for the bare-object case:
// restricting rule 1 to the root must not disable the conversion below it.
func TestNestedBareObjectStillConverts(t *testing.T) {
	got := translate(t, `{"type":"object","properties":{"meta":{"type":"object"}}}`)

	props, _ := got["properties"].(map[string]any)
	meta, ok := props["meta"].(map[string]any)
	if !ok {
		t.Fatalf("meta property missing: %v", got)
	}
	if meta["type"] != "array" {
		t.Errorf("nested bare object should still convert to a KV array, got type=%v", meta["type"])
	}
}

// TestNoArgumentToolIsStillClosed asserts the plugin keeps doing its actual job
// on the shape it used to break: a no-argument tool is closed for strict
// providers, it is simply no longer turned into an array to do it.
func TestNoArgumentToolIsStillClosed(t *testing.T) {
	got := translate(t, `{"type":"object"}`)
	if got["additionalProperties"] != false {
		t.Errorf("a no-argument tool should be closed for strict providers, got additionalProperties=%v", got["additionalProperties"])
	}
}

// TestOpenMapInsideArrayItemsIsNotClosed — the rule is "never make a schema
// stricter than its author wrote it", and it used to hold only at the root. An
// open map on an array's ITEM schema fell through to the blanket close, so a
// tool declaring rows of free-form objects had them silently forbidden.
func TestOpenMapInsideArrayItemsIsNotClosed(t *testing.T) {
	got := translate(t, `{"type":"object","properties":{"rows":{"type":"array","items":{"type":"object","additionalProperties":true}}}}`)

	props, _ := got["properties"].(map[string]any)
	rows, _ := props["rows"].(map[string]any)
	items, ok := rows["items"].(map[string]any)
	if !ok {
		t.Fatalf("rows.items missing: %v", rows)
	}
	if items["additionalProperties"] == false {
		t.Error("an author-declared open map on an array item schema was closed")
	}
	// It converts, like an open map anywhere else below the root.
	if items["type"] != "array" {
		t.Errorf("expected the open map to become a KV array, got type=%v", items["type"])
	}
}

// TestNestedMapKeepsItsValueSchema — convertToKVArray wiped the map and rebuilt
// it from the value's TYPE NAME alone, so a map whose values were declared
// objects told the model only "value is an object", with none of its shape.
func TestNestedMapKeepsItsValueSchema(t *testing.T) {
	got := translate(t, `{"type":"object","properties":{"m":{"type":"object","additionalProperties":{"type":"object","properties":{"deep":{"type":"string"}}}}}}`)

	props, _ := got["properties"].(map[string]any)
	m, _ := props["m"].(map[string]any)
	items, _ := m["items"].(map[string]any)
	itemProps, _ := items["properties"].(map[string]any)
	value, ok := itemProps["value"].(map[string]any)
	if !ok {
		t.Fatalf("KV value schema missing: %v", items)
	}
	valueProps, ok := value["properties"].(map[string]any)
	if !ok {
		t.Fatalf("the declared value schema was discarded: %v", value)
	}
	if _, hasDeep := valueProps["deep"]; !hasDeep {
		t.Errorf("the value schema lost its declared properties: %v", valueProps)
	}
}

// TestScalarPropertiesAreNotGivenAdditionalProperties — the key is meaningful
// only on an object. Stamping it on strings and numbers was noise carried
// upstream on every tool schema.
func TestScalarPropertiesAreNotGivenAdditionalProperties(t *testing.T) {
	got := translate(t, `{"type":"object","properties":{"path":{"type":"string"},"count":{"type":"number"},"flag":{"type":"boolean"}}}`)

	props, _ := got["properties"].(map[string]any)
	for name, raw := range props {
		p, _ := raw.(map[string]any)
		if _, present := p["additionalProperties"]; present {
			t.Errorf("%s is a %v and was given additionalProperties", name, p["type"])
		}
	}
	// The root itself is still closed — that is the plugin's job.
	if got["additionalProperties"] != false {
		t.Errorf("root should still be closed, got %v", got["additionalProperties"])
	}
}
