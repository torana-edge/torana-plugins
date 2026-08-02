package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pbv2 "github.com/torana-edge/torana-plugin-sdk/pb/v2"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
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

// translate runs the request-side translation on a fresh schema and returns
// the mutated schema plus the recorded structural mutation paths.
func translate(t *testing.T, raw string) (map[string]any, []mutationPath) {
	t.Helper()
	var schema map[string]any
	if err := json.Unmarshal([]byte(raw), &schema); err != nil {
		t.Fatal(err)
	}
	mutations := translateSchema(schema, nil, siteRoot)
	return schema, mutations
}

func translateSchemaMap(t *testing.T, raw string) map[string]any {
	t.Helper()
	schema, _ := translate(t, raw)
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
			got := translateSchemaMap(t, raw)
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
			got := translateSchemaMap(t, raw)
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
	got, mutations := translate(t, `{"type":"object","properties":{"env":{"type":"object","additionalProperties":{"type":"string"}}}}`)

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
	if len(mutations) != 1 || !reflect.DeepEqual(mutations[0].steps, []pathStep{{field: "env", each: false}}) {
		t.Errorf("recorded mutations = %+v, want one property step for env", mutations)
	}
}

// TestNestedBareObjectStillConverts is the same guard for the bare-object case:
// restricting rule 1 to the root must not disable the conversion below it.
func TestNestedBareObjectStillConverts(t *testing.T) {
	got := translateSchemaMap(t, `{"type":"object","properties":{"meta":{"type":"object"}}}`)

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
	got := translateSchemaMap(t, `{"type":"object"}`)
	if got["additionalProperties"] != false {
		t.Errorf("a no-argument tool should be closed for strict providers, got additionalProperties=%v", got["additionalProperties"])
	}
}

// TestBareObjectArrayItemsAreLeftAlone pins the SITE rule: nothing at an array
// item may be rewritten, because no rewrite there survives reverseTranslate.
func TestBareObjectArrayItemsAreLeftAlone(t *testing.T) {
	got := translateSchemaMap(t, `{"type":"object","properties":{"rows":{"type":"array","items":{"type":"object"}}}}`)

	props, _ := got["properties"].(map[string]any)
	rows, _ := props["rows"].(map[string]any)
	items, ok := rows["items"].(map[string]any)
	if !ok {
		t.Fatalf("rows.items missing: %v", rows)
	}
	if items["type"] != "object" {
		t.Errorf("the array's element type changed to %v; the tool would receive a "+
			"list of lists that reverseTranslate cannot undo", items["type"])
	}
	if items["additionalProperties"] == false {
		t.Error("a free-form record type at an array item was closed")
	}
}

func TestOpenMapInsideArrayItemsIsLeftAlone(t *testing.T) {
	got := translateSchemaMap(t, `{"type":"object","properties":{"rows":{"type":"array","items":{"type":"object","additionalProperties":true}}}}`)

	props, _ := got["properties"].(map[string]any)
	rows, _ := props["rows"].(map[string]any)
	items, ok := rows["items"].(map[string]any)
	if !ok {
		t.Fatalf("rows.items missing: %v", rows)
	}
	if items["additionalProperties"] == false {
		t.Error("an author-declared open map on an array item schema was closed")
	}
	if items["type"] != "object" {
		t.Errorf("the array's element type changed to %v; the tool would receive a "+
			"list of lists", items["type"])
	}
}

// TestNestedMapKeepsItsValueSchema — convertToKVArray wiped the map and rebuilt
// it from the value's TYPE NAME alone, so a map whose values were declared
// objects told the model only "value is an object", with none of its shape.
func TestNestedMapKeepsItsValueSchema(t *testing.T) {
	got := translateSchemaMap(t, `{"type":"object","properties":{"m":{"type":"object","additionalProperties":{"type":"object","properties":{"deep":{"type":"string"}}}}}}`)

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

// TestEmbeddedValueSchemaIsDeepCopied — the embedded copy must not alias the
// caller's map. The v1 shallow copy shared nested maps, so mutating the
// embedded value schema reached back into the author's schema.
func TestEmbeddedValueSchemaIsDeepCopied(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal([]byte(`{"type":"object","properties":{"m":{"type":"object","additionalProperties":{"type":"object","properties":{"deep":{"type":"string"}}}}}}`), &schema); err != nil {
		t.Fatal(err)
	}
	authorValue := schema["properties"].(map[string]any)["m"].(map[string]any)["additionalProperties"]

	translateSchema(schema, nil, siteRoot)

	props, _ := schema["properties"].(map[string]any)
	m, _ := props["m"].(map[string]any)
	items, _ := m["items"].(map[string]any)
	itemProps, _ := items["properties"].(map[string]any)
	embedded, ok := itemProps["value"].(map[string]any)
	if !ok {
		t.Fatal("embedded value schema missing")
	}
	embeddedProps := embedded["properties"].(map[string]any)
	embeddedProps["deep"].(map[string]any)["type"] = "number"

	authorProps := authorValue.(map[string]any)["properties"].(map[string]any)
	if authorProps["deep"].(map[string]any)["type"] != "string" {
		t.Error("mutating the embedded value schema reached back into the author's schema — the copy is not deep")
	}
}

// TestScalarPropertiesAreNotGivenAdditionalProperties — the key is meaningful
// only on an object. Stamping it on strings and numbers was noise carried
// upstream on every tool schema.
func TestScalarPropertiesAreNotGivenAdditionalProperties(t *testing.T) {
	got := translateSchemaMap(t, `{"type":"object","properties":{"path":{"type":"string"},"count":{"type":"number"},"flag":{"type":"boolean"}}}`)

	props, _ := got["properties"].(map[string]any)
	for name, raw := range props {
		p, _ := raw.(map[string]any)
		if _, present := p["additionalProperties"]; present {
			t.Errorf("%s is a %v and was given additionalProperties", name, p["type"])
		}
	}
	if got["additionalProperties"] != false {
		t.Errorf("root should still be closed, got %v", got["additionalProperties"])
	}
}

// TestNonObjectSchemaIsLeftAlone — additionalProperties is meaningful only on
// an object.
func TestNonObjectSchemaIsLeftAlone(t *testing.T) {
	for _, raw := range []string{
		`{"type":"string"}`,
		`{"type":"array","items":{"type":"string"}}`,
		`{"type":"number"}`,
	} {
		got := translateSchemaMap(t, raw)
		if _, present := got["additionalProperties"]; present {
			t.Errorf("%s was given additionalProperties: %v", raw, got)
		}
	}
	got := translateSchemaMap(t, `{"properties":{"path":{"type":"string"}}}`)
	if got["additionalProperties"] != false {
		t.Errorf("an untyped schema with properties should still be closed: %v", got)
	}
}

// TestMutationsSurviveReversal — every mutation the translator records must be
// undoable, or the tool receives arguments in a shape it did not declare.
func TestMutationsSurviveReversal(t *testing.T) {
	for name, tc := range map[string]struct {
		schema  string
		emitted string
		want    string
	}{
		"open map at a property": {
			schema:  `{"type":"object","properties":{"env":{"type":"object","additionalProperties":{"type":"string"}}}}`,
			emitted: `{"env":[{"key":"A","value":"1"}]}`,
			want:    `{"env":{"A":"1"}}`,
		},
		"bare object at a property": {
			schema:  `{"type":"object","properties":{"meta":{"type":"object"}}}`,
			emitted: `{"meta":[{"key":"k","value":"v"}]}`,
			want:    `{"meta":{"k":"v"}}`,
		},
		// Both array-item shapes: nothing is rewritten, so nothing needs
		// reversing and the model's arguments pass through unchanged.
		"bare object inside array items": {
			schema:  `{"type":"object","properties":{"rows":{"type":"array","items":{"type":"object"}}}}`,
			emitted: `{"rows":[{"a":"1"}]}`,
			want:    `{"rows":[{"a":"1"}]}`,
		},
		"open map inside array items": {
			schema:  `{"type":"object","properties":{"rows":{"type":"array","items":{"type":"object","additionalProperties":{"type":"string"}}}}}`,
			emitted: `{"rows":[{"a":"1"}]}`,
			want:    `{"rows":[{"a":"1"}]}`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			var schema map[string]any
			if err := json.Unmarshal([]byte(tc.schema), &schema); err != nil {
				t.Fatal(err)
			}
			mutations := translateSchema(schema, nil, siteRoot)

			reversed, err := reverseTranslate("tool", tc.emitted, mutations)
			if err != nil {
				t.Fatalf("reverseTranslate: %v", err)
			}

			var got, want map[string]any
			if err := json.Unmarshal([]byte(reversed), &got); err != nil {
				t.Fatalf("reversed output is not valid JSON: %v (%q)", err, reversed)
			}
			if err := json.Unmarshal([]byte(tc.want), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("round trip lost the shape the tool declared\n  mutations: %v\n  model emitted: %s\n  reversed:      %s\n  want:          %s",
					mutations, tc.emitted, reversed, tc.want)
			}
		})
	}
}

// TestArrayItemConversionWouldNotSurviveReversal pins WHY array items are left
// alone: an each-step reversal requires each element to be a MAP.
func TestArrayItemConversionWouldNotSurviveReversal(t *testing.T) {
	args := `{"rows":[[{"key":"a","value":"1"}]]}`
	reversed, err := reverseTranslate("tool", args, []mutationPath{{steps: []pathStep{{field: "rows", each: true}}}})
	if err != nil {
		t.Fatalf("reverseTranslate: %v", err)
	}
	if reversed != args {
		t.Fatalf("reversal is expected to be a no-op on list-of-lists; got %s", reversed)
	}
}

// TestTranslationIsDeterministic — property names are visited in sorted order,
// so both the schema bytes and the recorded mutation paths are pure functions
// of the input. Repeated fresh parses must produce identical bytes (prompt-
// cache compliance) and identical paths (registry bytes).
func TestTranslationIsDeterministic(t *testing.T) {
	raw := `{"type":"object","properties":{"zebra":{"type":"object","additionalProperties":{"type":"string"}},"alpha":{"type":"object"},"middle":{"type":"object","properties":{"nested":{"type":"object","additionalProperties":true}}}},"additionalProperties":{"type":"string"}}`
	run := func() ([]byte, []mutationPath) {
		var schema map[string]any
		if err := json.Unmarshal([]byte(raw), &schema); err != nil {
			t.Fatal(err)
		}
		mutations := translateSchema(schema, nil, siteRoot)
		out, err := json.Marshal(schema)
		if err != nil {
			t.Fatal(err)
		}
		return out, mutations
	}
	firstSchema, firstPaths := run()
	for i := 0; i < 20; i++ {
		s, paths := run()
		if string(s) != string(firstSchema) {
			t.Fatalf("schema bytes differ on iteration %d:\n%s\nvs\n%s", i, firstSchema, s)
		}
		if !reflect.DeepEqual(paths, firstPaths) {
			t.Fatalf("mutation paths differ on iteration %d: %+v vs %+v", i, firstPaths, paths)
		}
	}
	// Sorted order: alpha's conversion is recorded before middle's before
	// zebra's, regardless of map iteration order.
	if len(firstPaths) != 3 {
		t.Fatalf("expected 3 mutations, got %d", len(firstPaths))
	}
	if firstPaths[0].steps[0].field != "alpha" || firstPaths[1].steps[0].field != "middle" || firstPaths[2].steps[0].field != "zebra" {
		t.Errorf("mutations not in sorted property order: %+v", firstPaths)
	}
}

// ==========================================================================
// Registry wire format
// ==========================================================================

// TestRegistryWireCanonicalShape pins the approved wire shape:
// {"version":1,"tools":{"tool_name":[{"path":[{"field":"a.b","each":false}]}]}}
// — envelope exactly version+tools, mutation exactly path, step exactly
// field+each, canonical output always emitting the complete shape.
func TestRegistryWireCanonicalShape(t *testing.T) {
	reg := &registry{version: 1, tools: map[string][]mutationPath{
		"toolA": {{steps: []pathStep{{field: "a.b", each: false}}}},
		"toolB": {{steps: []pathStep{{field: "env", each: false}, {field: "x", each: true}}}},
	}}
	got, err := reg.marshal()
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"version":1,"tools":{"toolA":[{"path":[{"field":"a.b","each":false}]}],"toolB":[{"path":[{"field":"env","each":false},{"field":"x","each":true}]}]}}`
	if string(got) != want {
		t.Fatalf("canonical encoding drifted:\n  got  %s\n  want %s", got, want)
	}

	// Round trip: decode re-encodes identically.
	decoded, err := decodeRegistry(got)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	again, err := decoded.marshal()
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(got) {
		t.Fatalf("decode→encode is not stable:\n  %s\n  %s", got, again)
	}

	// Sorted tool names, even when inserted out of order.
	unsorted := &registry{version: 1, tools: map[string][]mutationPath{
		"zeta":  {{steps: []pathStep{{field: "a", each: false}}}},
		"alpha": {{steps: []pathStep{{field: "b", each: false}}}},
	}}
	out, err := unsorted.marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"alpha"`) || strings.Index(string(out), `"alpha"`) > strings.Index(string(out), `"zeta"`) {
		t.Fatalf("tool names not sorted canonically: %s", out)
	}
}

// TestRegistryWireEmptyEnvelope — the empty registry is
// {"version":1,"tools":{}}, and it round-trips.
func TestRegistryWireEmptyEnvelope(t *testing.T) {
	reg := &registry{version: 1, tools: map[string][]mutationPath{}}
	got, err := reg.marshal()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"version":1,"tools":{}}` {
		t.Fatalf("empty envelope: %s", got)
	}
	decoded, err := decodeRegistry(got)
	if err != nil {
		t.Fatalf("empty envelope must decode: %v", err)
	}
	if len(decoded.tools) != 0 {
		t.Fatalf("empty envelope decoded with tools: %+v", decoded.tools)
	}
}

// TestRegistryWirePresenceDistinctions — a missing member is NOT the same as
// an empty/false member. `field:""` is a legal empty JSON property name and
// must round-trip; a MISSING field member is a decode error. Same for each.
func TestRegistryWirePresenceDistinctions(t *testing.T) {
	// field:"" is legal and round-trips.
	emptyField := `{"version":1,"tools":{"t":[{"path":[{"field":"","each":false}]}]}}`
	decoded, err := decodeRegistry([]byte(emptyField))
	if err != nil {
		t.Fatalf("empty field must decode: %v", err)
	}
	if decoded.tools["t"][0].steps[0].field != "" {
		t.Fatalf("empty field lost: %+v", decoded.tools)
	}

	for name, raw := range map[string]string{
		"missing field":       `{"version":1,"tools":{"t":[{"path":[{"each":false}]}]}}`,
		"missing each":        `{"version":1,"tools":{"t":[{"path":[{"field":"a"}]}]}}`,
		"missing path":        `{"version":1,"tools":{"t":[{}]}}`,
		"missing tools":       `{"version":1}`,
		"missing version":     `{"tools":{}}`,
		"null field":          `{"version":1,"tools":{"t":[{"path":[{"field":null,"each":false}]}]}}`,
		"null each":           `{"version":1,"tools":{"t":[{"path":[{"field":"a","each":null}]}]}}`,
		"duplicate member":    `{"version":1,"version":1,"tools":{}}`,
		"duplicate step key":  `{"version":1,"tools":{"t":[{"path":[{"field":"a","field":"b","each":false}]}]}}`,
		"duplicate tool name": `{"version":1,"tools":{"t":[{"path":[{"field":"a","each":false}]}],"t":[{"path":[{"field":"b","each":false}]}]}}`,
		"unknown member":      `{"version":1,"tools":{},"extra":1}`,
		"unknown step member": `{"version":1,"tools":{"t":[{"path":[{"field":"a","each":false,"idx":0}]}]}}`,
		"empty path":          `{"version":1,"tools":{"t":[{"path":[]}]}}`,
		"empty mutations":     `{"version":1,"tools":{"t":[]}}`,
		"unknown version":     `{"version":2,"tools":{}}`,
		"trailing JSON":       `{"version":1,"tools":{}} {}`,
		"null envelope":       `null`,
		"field wrong type":    `{"version":1,"tools":{"t":[{"path":[{"field":7,"each":false}]}]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeRegistry([]byte(raw)); err == nil {
				t.Fatalf("must be rejected: %s", raw)
			}
		})
	}
}

// TestRegistryWireNestedPathsAndUnicode — dotted fields, "[]"-suffixed field
// names (the v1 ambiguity), and unicode/empty names round-trip exactly.
func TestRegistryWireNestedPathsAndUnicode(t *testing.T) {
	raw := `{"version":1,"tools":{"t":[{"path":[{"field":"a.b","each":false},{"field":"c[]","each":false},{"field":"","each":true},{"field":"名前","each":false}]}]}}`
	decoded, err := decodeRegistry([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	steps := decoded.tools["t"][0].steps
	want := []pathStep{{field: "a.b", each: false}, {field: "c[]", each: false}, {field: "", each: true}, {field: "名前", each: false}}
	if !reflect.DeepEqual(steps, want) {
		t.Fatalf("steps = %+v, want %+v", steps, want)
	}
}

// ==========================================================================
// Request hook (sdktest; the plugin registers in init(), so every dispatch
// exercises the real hook)
// ==========================================================================

func newHarness(t *testing.T) *sdktest.Harness {
	t.Helper()
	return sdktest.New(t)
}

func reqWithTools(raw string) *pbv2.ChatRequest {
	return &pbv2.ChatRequest{Tools: []*pbv2.ToolDef{{
		Name:           "read",
		ParametersJson: []byte(raw),
	}}}
}

// publishedEnvelope reads the exact registry envelope the request hook
// published to meta (there must be exactly one).
func publishedEnvelope(t *testing.T, h *sdktest.Harness) string {
	t.Helper()
	var found string
	for _, c := range h.Calls() {
		if c.Command != "env.meta_set" {
			continue
		}
		var a pbv2.MetaSetArgs
		if err := proto.Unmarshal([]byte(c.Args), &a); err != nil {
			t.Fatalf("meta_set args not a MetaSetArgs: %v", err)
		}
		if a.Key != mutationsKey {
			continue
		}
		if found != "" {
			t.Fatalf("more than one envelope published")
		}
		found = a.Value
	}
	if found == "" {
		t.Fatalf("no registry envelope published; calls: %+v", h.Calls())
	}
	return found
}

// TestBeforeRequestNoToolsPublishesNothing — a request with no tools needs no
// envelope (a conforming response cannot contain a tool call for it).
func TestBeforeRequestNoToolsPublishesNothing(t *testing.T) {
	h := newHarness(t)
	res := h.BeforeRequest(&pbv2.ChatRequest{Messages: []*pbv2.Message{{Role: "user", Content: "hi"}}})
	if !res.PassedThrough || res.Err != nil {
		t.Fatalf("expected pass-through, got err=%v", res.Err)
	}
	for _, c := range h.Calls() {
		t.Errorf("unexpected host call with no tools: %s", c.Command)
	}
}

// TestBeforeRequestReplacesAndPublishesEnvelopeFirst — a translated request is
// replaced ONLY after the exact envelope needed to reverse it was persisted;
// the published envelope contains the recorded path for the tool.
func TestBeforeRequestReplacesAndPublishesEnvelopeFirst(t *testing.T) {
	h := newHarness(t)
	req := reqWithTools(`{"type":"object","properties":{"env":{"type":"object","additionalProperties":{"type":"string"}}}}`)
	res := h.BeforeRequest(req)
	if res.Err != nil || res.Request == nil {
		t.Fatalf("expected replacement, err=%v", res.Err)
	}
	env := publishedEnvelope(t, h)
	if env != `{"version":1,"tools":{"read":[{"path":[{"field":"env","each":false}]}]}}` {
		t.Fatalf("envelope: %s", env)
	}
	// The replacement schema carries the KV-array conversion.
	var schema map[string]any
	if err := json.Unmarshal(res.Request.Tools[0].ParametersJson, &schema); err != nil {
		t.Fatal(err)
	}
	props := schema["properties"].(map[string]any)
	if props["env"].(map[string]any)["type"] != "array" {
		t.Fatalf("replacement did not translate: %v", schema)
	}
	// meta_set happened before the replacement frame: the meta call is
	// recorded, and the dispatch succeeded.
	order := -1
	for i, c := range h.Calls() {
		if c.Command == "env.meta_set" {
			order = i
		}
	}
	if order < 0 {
		t.Fatal("no meta_set recorded")
	}
}

// TestBeforeRequestUnchangedStillPublishesEmptyEnvelope — a request whose
// schemas need no bytes changed still publishes the explicit empty envelope
// (the stream hook's positive no-translation proof).
func TestBeforeRequestUnchangedStillPublishesEmptyEnvelope(t *testing.T) {
	h := newHarness(t)
	req := reqWithTools(`{"additionalProperties":false,"properties":{"path":{"type":"string"}},"type":"object"}`)
	res := h.BeforeRequest(req)
	if !res.PassedThrough || res.Err != nil {
		t.Fatalf("expected pass-through, err=%v", res.Err)
	}
	env := publishedEnvelope(t, h)
	if env != `{"version":1,"tools":{}}` {
		t.Fatalf("envelope should be empty but is: %s", env)
	}
}

// TestAdvisoryPublicationFailureReturnsOriginalUnchanged — an advisory
// MetaSet refusal means the reversal registry was not persisted; the ORIGINAL
// request travels untouched (no partial schema mutation escapes).
func TestAdvisoryPublicationFailureReturnsOriginalUnchanged(t *testing.T) {
	h := newHarness(t)
	h.StubHostCall("env.meta_set", func(args string) (string, error) {
		return sdktest.HostResultError(pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED, "state dir missing"), nil
	})
	req := reqWithTools(`{"type":"object","properties":{"env":{"type":"object","additionalProperties":{"type":"string"}}}}`)
	res := h.BeforeRequest(req)
	if !res.PassedThrough || res.Err != nil {
		t.Fatalf("advisory refusal must pass the original request through, err=%v", res.Err)
	}
	if res.Request != nil {
		t.Fatalf("a request was replaced despite the failed publication: %+v", res.Request)
	}
	// The input request object was not mutated.
	var schema map[string]any
	if err := json.Unmarshal(req.Tools[0].ParametersJson, &schema); err != nil {
		t.Fatal(err)
	}
	if schema["properties"].(map[string]any)["env"].(map[string]any)["type"] == "array" {
		t.Fatal("input request mutated in place despite pass-through")
	}
}

// TestContractPublicationFailureIsHookError — a contract-class MetaSet
// refusal (PERMISSION_DENIED) is a hook error, not a silent pass.
func TestContractPublicationFailureIsHookError(t *testing.T) {
	h := newHarness(t)
	h.DenyPermission("env.meta_set")
	req := reqWithTools(`{"type":"object","properties":{"env":{"type":"object","additionalProperties":{"type":"string"}}}}`)
	res := h.BeforeRequest(req)
	if res.Err == nil {
		t.Fatal("expected a hook error on contract-class publication refusal")
	}
}

// TestReplacedRequestImpliesPresentValidEnvelope — the published envelope is
// always decodable and always records exactly the translated tool.
func TestReplacedRequestImpliesPresentValidEnvelope(t *testing.T) {
	for _, raw := range []string{
		`{"type":"object","properties":{"env":{"type":"object","additionalProperties":{"type":"string"}}}}`,
		`{"type":"object","properties":{"a":{"type":"object"},"b":{"type":"object","properties":{"c":{"type":"object","additionalProperties":true}}}}}`,
	} {
		h := newHarness(t)
		res := h.BeforeRequest(reqWithTools(raw))
		if res.Request == nil {
			t.Fatal("expected replacement")
		}
		env := publishedEnvelope(t, h)
		if _, err := decodeRegistry([]byte(env)); err != nil {
			t.Fatalf("published envelope is corrupt: %v", err)
		}
	}
}

// TestDuplicateToolNamesAreAllUntranslated — duplicate names are pre-scanned;
// EVERY occurrence is left untranslated, in either input order, with same or
// different schemas. No last-write-wins reversal schema.
func TestDuplicateToolNamesAreAllUntranslated(t *testing.T) {
	cases := []struct {
		name string
		req  *pbv2.ChatRequest
	}{
		{"same schema, both orders", &pbv2.ChatRequest{Tools: []*pbv2.ToolDef{
			{Name: "dup", ParametersJson: []byte(`{"type":"object","properties":{"x":{"type":"object"}}}`)},
			{Name: "dup", ParametersJson: []byte(`{"type":"object","properties":{"x":{"type":"object"}}}`)},
		}}},
		{"different schemas", &pbv2.ChatRequest{Tools: []*pbv2.ToolDef{
			{Name: "dup", ParametersJson: []byte(`{"type":"object","properties":{"x":{"type":"object"}}}`)},
			{Name: "dup", ParametersJson: []byte(`{"type":"object","properties":{"y":{"type":"object","additionalProperties":{"type":"string"}}}}`)},
		}}},
		{"reversed order", &pbv2.ChatRequest{Tools: []*pbv2.ToolDef{
			{Name: "dup", ParametersJson: []byte(`{"type":"object","properties":{"y":{"type":"object","additionalProperties":{"type":"string"}}}}`)},
			{Name: "dup", ParametersJson: []byte(`{"type":"object","properties":{"x":{"type":"object"}}}`)},
		}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			res := h.BeforeRequest(tc.req)
			if !res.PassedThrough || res.Err != nil {
				t.Fatalf("duplicate names must leave every occurrence untranslated (pass-through), err=%v", res.Err)
			}
			env := publishedEnvelope(t, h)
			if env != `{"version":1,"tools":{}}` {
				t.Fatalf("duplicate-name tools must not be recorded: %s", env)
			}
			// The originals travel byte-identical.
			for _, tool := range tc.req.Tools {
				var schema map[string]any
				if err := json.Unmarshal(tool.ParametersJson, &schema); err != nil {
					t.Fatal(err)
				}
				if props := schema["properties"].(map[string]any); props != nil {
					for _, p := range props {
						if p.(map[string]any)["type"] == "array" {
							t.Fatal("a duplicate-name tool was translated")
						}
					}
				}
			}
		})
	}
}

// ==========================================================================
// Stream hook (assembler terminal-safety matrix)
// ==========================================================================

func streamBlock(t *testing.T, h *sdktest.Harness, index int32, id, name, sig, args string) sdktest.StreamResult {
	t.Helper()
	h.StreamChunk(&pbv2.StreamEvent{Event: &pbv2.StreamEvent_ContentBlockStart{
		ContentBlockStart: &pbv2.ContentBlockStart{Index: index, Block: &pbv2.ContentBlockStart_ToolCall{
			ToolCall: &pbv2.ToolCallRef{Id: id, Name: name, Signature: sig},
		}},
	}})
	h.StreamChunk(&pbv2.StreamEvent{Event: &pbv2.StreamEvent_ToolCallDelta{
		ToolCallDelta: &pbv2.ToolCallDelta{Index: index, ArgumentsDelta: args},
	}})
	return h.StreamChunk(&pbv2.StreamEvent{Event: &pbv2.StreamEvent_ContentBlockStop{
		ContentBlockStop: &pbv2.ContentBlockStop{Index: index},
	}})
}

func emittedArgs(t *testing.T, res sdktest.StreamResult) string {
	t.Helper()
	if res.Err != nil {
		t.Fatalf("stream dispatch error: %v", res.Err)
	}
	if len(res.Events) != 3 {
		t.Fatalf("expected assembled start+delta+stop, got %d events", len(res.Events))
	}
	delta := res.Events[1].GetToolCallDelta()
	if delta == nil {
		t.Fatalf("second event is not the tool-call delta: %v", res.Events[1])
	}
	return delta.ArgumentsDelta
}

func emittedSig(t *testing.T, res sdktest.StreamResult) string {
	t.Helper()
	start := res.Events[0].GetContentBlockStart()
	if start == nil {
		t.Fatalf("first event is not a content-block start: %v", res.Events[0])
	}
	if tc := start.GetToolCall(); tc != nil {
		return tc.Signature
	}
	return ""
}

// TestStreamPassThroughForUnrecordedTool — a present, valid envelope that
// explicitly lacks the tool permits byte-identical pass-through with the
// signature preserved.
func TestStreamPassThroughForUnrecordedTool(t *testing.T) {
	h := newHarness(t)
	h.BeforeRequest(reqWithTools(`{"type":"object","properties":{"path":{"type":"string"}},"additionalProperties":false}`))
	res := streamBlock(t, h, 0, "call_1", "read", "sig-abc", `{"path":"a.go"}`)
	if got := emittedArgs(t, res); got != `{"path":"a.go"}` {
		t.Fatalf("byte-identical pass expected, got %q", got)
	}
	if sig := emittedSig(t, res); sig != "sig-abc" {
		t.Fatalf("signature must be preserved on byte-identical pass, got %q", sig)
	}
}

// TestStreamReversesRecordedTool — a translated tool's assembled call is
// reversed and re-emitted; the signature is cleared because the arguments
// changed.
func TestStreamReversesRecordedTool(t *testing.T) {
	h := newHarness(t)
	h.BeforeRequest(reqWithTools(`{"type":"object","properties":{"env":{"type":"object","additionalProperties":{"type":"string"}}}}`))
	res := streamBlock(t, h, 0, "call_1", "read", "sig-abc", `{"env":[{"key":"A","value":"1"}]}`)
	if got := emittedArgs(t, res); got != `{"env":{"A":"1"}}` {
		t.Fatalf("reversed args = %q", got)
	}
	if sig := emittedSig(t, res); sig != "" {
		t.Fatalf("signature must be cleared when arguments change, got %q", sig)
	}
}

// TestStreamTerminalOnMissingRegistry — no tools in the request means no
// envelope; a forged tool call has no positive no-translation proof and must
// terminate.
func TestStreamTerminalOnMissingRegistry(t *testing.T) {
	h := newHarness(t)
	h.BeforeRequest(&pbv2.ChatRequest{Messages: []*pbv2.Message{{Role: "user", Content: "hi"}}})
	res := streamBlock(t, h, 0, "call_1", "read", "", `{"path":"a.go"}`)
	if res.Err == nil {
		t.Fatal("a tool call without any published envelope must terminate")
	}
}

// TestStreamTerminalOnAdvisoryRegistryRead — NOT_CONFIGURED/UNAVAILABLE on the
// stream-side MetaGet is terminal (the absent envelope cannot prove "nothing
// was translated").
func TestStreamTerminalOnAdvisoryRegistryRead(t *testing.T) {
	h := newHarness(t)
	h.StubHostCall("env.meta_get", func(args string) (string, error) {
		return sdktest.HostResultError(pbv2.ErrorCode_ERROR_CODE_UNAVAILABLE, "backing store down"), nil
	})
	res := streamBlock(t, h, 0, "call_1", "read", "", `{"path":"a.go"}`)
	if res.Err == nil {
		t.Fatal("advisory registry read at stream completion must terminate")
	}
}

// TestStreamTerminalOnDeletedRegistry — the SAME flow whose envelope was
// published at request time terminates if the envelope is gone at stream time.
func TestStreamTerminalOnDeletedRegistry(t *testing.T) {
	h := newHarness(t)
	h.BeforeRequest(reqWithTools(`{"type":"object","properties":{"env":{"type":"object","additionalProperties":{"type":"string"}}}}`))
	// Simulate the envelope being lost between hooks (e.g. a different WASM
	// instance state): overwrite the key with nothing via a stub that reports
	// NOT_FOUND.
	h.StubHostCall("env.meta_get", func(args string) (string, error) {
		return sdktest.HostResultError(pbv2.ErrorCode_ERROR_CODE_NOT_FOUND, "no such key"), nil
	})
	res := streamBlock(t, h, 0, "call_1", "read", "", `{"env":[{"key":"A","value":"1"}]}`)
	if res.Err == nil {
		t.Fatal("a missing envelope at stream completion must terminate")
	}
}

// TestStreamTerminalOnCorruptRegistry — an envelope that no longer decodes is
// terminal, never guessed-around.
func TestStreamTerminalOnCorruptRegistry(t *testing.T) {
	h := newHarness(t)
	h.StubHostCall("env.meta_get", func(args string) (string, error) {
		var a pbv2.MetaGetArgs
		if err := proto.Unmarshal([]byte(args), &a); err != nil {
			t.Fatalf("meta_get args: %v", err)
		}
		if a.Key == mutationsKey {
			return sdktest.HostResultValue([]byte(`{"version":1,"tools":`)), nil
		}
		return sdktest.HostResultValue(nil), nil
	})
	res := streamBlock(t, h, 0, "call_1", "read", "", `{"path":"a.go"}`)
	if res.Err == nil {
		t.Fatal("a corrupt registry must terminate")
	}
}

// TestStreamTerminalOnUnreversibleArgs — a RECORDED tool whose assembled
// arguments are not a JSON object cannot be reversed; fail-open would execute
// the wrong call.
func TestStreamTerminalOnUnreversibleArgs(t *testing.T) {
	h := newHarness(t)
	h.BeforeRequest(reqWithTools(`{"type":"object","properties":{"env":{"type":"object","additionalProperties":{"type":"string"}}}}`))
	res := streamBlock(t, h, 0, "call_1", "read", "", `{"env": not-json`)
	if res.Err == nil {
		t.Fatal("unreversible arguments for a recorded tool must terminate")
	}
}

// TestStreamFragmentedAndConcurrentBlocks — deltas arrive fragmented across
// many chunks, and two tool blocks may be open concurrently (bound by index);
// both complete calls reverse correctly.
func TestStreamFragmentedAndConcurrentBlocks(t *testing.T) {
	h := newHarness(t)
	h.BeforeRequest(reqWithTools(`{"type":"object","properties":{"env":{"type":"object","additionalProperties":{"type":"string"}}}}`))

	h.StreamChunk(&pbv2.StreamEvent{Event: &pbv2.StreamEvent_ContentBlockStart{
		ContentBlockStart: &pbv2.ContentBlockStart{Index: 0, Block: &pbv2.ContentBlockStart_ToolCall{
			ToolCall: &pbv2.ToolCallRef{Id: "call_0", Name: "read", Signature: "s0"},
		}},
	}})
	h.StreamChunk(&pbv2.StreamEvent{Event: &pbv2.StreamEvent_ContentBlockStart{
		ContentBlockStart: &pbv2.ContentBlockStart{Index: 1, Block: &pbv2.ContentBlockStart_ToolCall{
			ToolCall: &pbv2.ToolCallRef{Id: "call_1", Name: "read", Signature: "s1"},
		}},
	}})
	h.StreamChunk(&pbv2.StreamEvent{Event: &pbv2.StreamEvent_ToolCallDelta{
		ToolCallDelta: &pbv2.ToolCallDelta{Index: 0, ArgumentsDelta: `{"env":[{"key":"A",`},
	}})
	h.StreamChunk(&pbv2.StreamEvent{Event: &pbv2.StreamEvent_ToolCallDelta{
		ToolCallDelta: &pbv2.ToolCallDelta{Index: 1, ArgumentsDelta: `{"env":[{"key":"B",`},
	}})
	h.StreamChunk(&pbv2.StreamEvent{Event: &pbv2.StreamEvent_ToolCallDelta{
		ToolCallDelta: &pbv2.ToolCallDelta{Index: 0, ArgumentsDelta: `"value":"1"}]}`},
	}})
	h.StreamChunk(&pbv2.StreamEvent{Event: &pbv2.StreamEvent_ToolCallDelta{
		ToolCallDelta: &pbv2.ToolCallDelta{Index: 1, ArgumentsDelta: `"value":"2"}]}`},
	}})
	res0 := h.StreamChunk(&pbv2.StreamEvent{Event: &pbv2.StreamEvent_ContentBlockStop{
		ContentBlockStop: &pbv2.ContentBlockStop{Index: 0},
	}})
	if got := emittedArgs(t, res0); got != `{"env":{"A":"1"}}` {
		t.Fatalf("block 0 reversed args = %q", got)
	}
	res1 := h.StreamChunk(&pbv2.StreamEvent{Event: &pbv2.StreamEvent_ContentBlockStop{
		ContentBlockStop: &pbv2.ContentBlockStop{Index: 1},
	}})
	if got := emittedArgs(t, res1); got != `{"env":{"B":"2"}}` {
		t.Fatalf("block 1 reversed args = %q", got)
	}
}

// TestStreamErrorEventPassesThrough — a mid-block error event is forwarded,
// never swallowed (the assembler's contract).
func TestStreamErrorEventPassesThrough(t *testing.T) {
	h := newHarness(t)
	h.BeforeRequest(reqWithTools(`{"type":"object","properties":{"path":{"type":"string"}},"additionalProperties":false}`))
	res := h.StreamChunk(&pbv2.StreamEvent{Event: &pbv2.StreamEvent_Error{
		Error: &pbv2.StreamError{Message: "upstream aborted"},
	}})
	if res.Err != nil {
		t.Fatalf("mid-block error must pass through, got err=%v", res.Err)
	}
	if len(res.Events) != 1 || res.Events[0].GetError() == nil {
		t.Fatalf("error event not forwarded: %+v", res.Events)
	}
}

// TestStreamNoUnauthorizedCalls — across the request and stream paths the
// plugin touches meta only: meta_set (+ meta_append under the same permission
// for assembly) and meta_get. No cache, state, pricing, or block traffic.
func TestStreamNoUnauthorizedCalls(t *testing.T) {
	h := newHarness(t)
	h.BeforeRequest(reqWithTools(`{"type":"object","properties":{"env":{"type":"object","additionalProperties":{"type":"string"}}}}`))
	streamBlock(t, h, 0, "call_1", "read", "", `{"env":[{"key":"A","value":"1"}]}`)
	for _, c := range h.Calls() {
		switch c.Command {
		case "env.meta_set", "env.meta_get", "env.meta_append":
		default:
			t.Errorf("unexpected host call: %s", c.Command)
		}
	}
}

// TestHookDeterminism — identical requests produce identical envelope bytes
// and identical replacement bytes; identical streamed calls produce identical
// reversal bytes.
func TestHookDeterminism(t *testing.T) {
	run := func() (string, string) {
		h := newHarness(t)
		req := reqWithTools(`{"type":"object","properties":{"z":{"type":"object"},"a":{"type":"object","additionalProperties":{"type":"string"}}}}`)
		res := h.BeforeRequest(req)
		env := publishedEnvelope(t, h)
		var reqBytes string
		if res.Request != nil {
			reqBytes = string(res.Request.Tools[0].ParametersJson)
		}
		return env, reqBytes
	}
	e1, r1 := run()
	for i := 0; i < 10; i++ {
		e2, r2 := run()
		if e1 != e2 {
			t.Fatalf("envelope bytes differ on iteration %d: %s vs %s", i, e1, e2)
		}
		if r1 != r2 {
			t.Fatalf("replacement bytes differ on iteration %d: %s vs %s", i, r1, r2)
		}
	}
}

// TestEnvLogNotUsed — the plugin never logs (env.log is not a grant it holds).
func TestEnvLogNotUsed(t *testing.T) {
	h := newHarness(t)
	h.BeforeRequest(reqWithTools(`{"type":"object","properties":{"env":{"type":"object","additionalProperties":{"type":"string"}}}}`))
	streamBlock(t, h, 0, "call_1", "read", "", `{"env":[{"key":"A","value":"1"}]}`)
	if logs := h.Logs(); len(logs) != 0 {
		t.Fatalf("plugin logged: %+v", logs)
	}
}

var _ = sdk.PassRequest
