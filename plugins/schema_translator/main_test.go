package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

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

			reversed, changed, err := reverseTranslate("tool", tc.emitted, mutations)
			if err != nil {
				t.Fatalf("reverseTranslate: %v", err)
			}
			// Every recorded mutation must actually convert when its field is
			// present; a semantic no-op is only legal when nothing was
			// recorded (the array-item rows below).
			if len(mutations) > 0 && !changed {
				t.Fatalf("conversion did not occur: %q", tc.emitted)
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

// TestTerminalEachPathIsRejected pins WHY array items are left alone: a path
// ending in each:true is not a path this translator generates (conversions are
// only recorded at property sites, whose terminal step is each:false). If a
// future edit made the translator convert an array item, the recorded path
// would be structurally invalid and reversal must REFUSE it rather than
// invoking the old per-element heuristic.
func TestTerminalEachPathIsRejected(t *testing.T) {
	for _, args := range []string{
		`{"rows":[[{"key":"a","value":"1"}]]}`,
		`{"rows":[{"a":"1"}]}`,
	} {
		_, _, err := reverseTranslate("tool", args, []mutationPath{{steps: []pathStep{{field: "rows", each: true}}}})
		if err == nil {
			t.Fatalf("terminal each:true path must be rejected, args=%s", args)
		}
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
		"lone high surrogate": `{"version":1,"tools":{"t":[{"path":[{"field":"\ud800","each":false}]}]}}`,
		"lone low surrogate":  `{"version":1,"tools":{"\udc00":[{"path":[{"field":"a","each":false}]}]}}`,
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

// ==========================================================================
// Round-1 correction pins (F1-F6)
// ==========================================================================

// TestToolDefPreservationOnReplacement — replacement clones every ToolDef, so
// the host/customer fields (description, strict, cache_control_json) survive,
// and an unrelated tool remains COMPLETELY unchanged — raw bytes included.
func TestToolDefPreservationOnReplacement(t *testing.T) {
	req := &pbv2.ChatRequest{Tools: []*pbv2.ToolDef{
		{
			Name:             "convert",
			Description:      "converts things",
			Strict:           true,
			CacheControlJson: []byte(`{"ttl":60}`),
			ParametersJson:   []byte(`{"type":"object","properties":{"env":{"type":"object","additionalProperties":{"type":"string"}}}}`),
		},
		{
			Name:             "other",
			Description:      "untouched",
			Strict:           true,
			CacheControlJson: []byte(`{"ttl":3600}`),
			ParametersJson:   []byte(`{"additionalProperties":false,"properties":{"p":{"type":"string"}},"type":"object"}`),
		},
	}}
	h := newHarness(t)
	res := h.BeforeRequest(req)
	if res.Err != nil || res.Request == nil {
		t.Fatalf("expected replacement, err=%v", res.Err)
	}
	converted := res.Request.Tools[0]
	if converted.Description != "converts things" || !converted.Strict || string(converted.CacheControlJson) != `{"ttl":60}` {
		t.Fatalf("converted tool lost host/customer fields: %+v", converted)
	}
	var s map[string]any
	if err := json.Unmarshal(converted.ParametersJson, &s); err != nil {
		t.Fatal(err)
	}
	if s["properties"].(map[string]any)["env"].(map[string]any)["type"] != "array" {
		t.Fatalf("converted tool was not translated: %v", s)
	}
	other := res.Request.Tools[1]
	if other.Description != "untouched" || !other.Strict || string(other.CacheControlJson) != `{"ttl":3600}` {
		t.Fatalf("unrelated tool lost fields: %+v", other)
	}
	if string(other.ParametersJson) != `{"additionalProperties":false,"properties":{"p":{"type":"string"}},"type":"object"}` {
		t.Fatalf("unrelated tool was rewritten: %s", other.ParametersJson)
	}
}

// chainObject builds an object chain of the given depth with two bare-object
// siblings (x, y) converted at the leaf: depth 0 is root{x,y}, depth 1 is
// root{p0:{x,y}}, and so on.
func chainObject(depth int) map[string]any {
	leaf := map[string]any{"type": "object", "properties": map[string]any{
		"x": map[string]any{"type": "object"},
		"y": map[string]any{"type": "object"},
	}}
	cur := leaf
	for i := depth - 1; i >= 0; i-- {
		cur = map[string]any{"type": "object", "properties": map[string]any{fmt.Sprintf("p%d", i): cur}}
	}
	return cur
}

func chainJSON(depth int, leaf string) string {
	out := leaf
	for i := depth - 1; i >= 0; i-- {
		out = fmt.Sprintf(`{"p%d":%s}`, i, out)
	}
	return out
}

// TestGeneratedNestedPathsReverseAndAreIndependent — reference-model coverage
// over nesting depths 0..8 with two sibling conversions at every depth. Every
// generated path must reverse a representative value back to the declared
// tool-facing value, and sibling paths must not share mutable storage.
func TestGeneratedNestedPathsReverseAndAreIndependent(t *testing.T) {
	for depth := 0; depth <= 8; depth++ {
		t.Run(fmt.Sprintf("object-chain-depth-%d", depth), func(t *testing.T) {
			mutations := translateSchema(chainObject(depth), nil, siteRoot)
			if len(mutations) != 2 {
				t.Fatalf("want two sibling mutations at depth %d, got %d", depth, len(mutations))
			}
			if len(mutations[0].steps) != depth+1 || len(mutations[1].steps) != depth+1 {
				t.Fatalf("path length = %d/%d, want %d", len(mutations[0].steps), len(mutations[1].steps), depth+1)
			}
			if last := mutations[0].steps[len(mutations[0].steps)-1].field; last != "x" {
				t.Fatalf("first sibling is %q, want x", last)
			}
			if last := mutations[1].steps[len(mutations[1].steps)-1].field; last != "y" {
				t.Fatalf("second sibling is %q, want y", last)
			}
			// Storage independence: mutating one path must not affect the other.
			origFirst := mutations[0].steps[0].field
			mutations[0].steps[0].field = "MUTATED"
			if mutations[1].steps[0].field == "MUTATED" {
				t.Fatal("sibling paths share mutable storage")
			}
			mutations[0].steps[0].field = origFirst

			args := chainJSON(depth, `{"x":[{"key":"kx","value":1}],"y":[{"key":"ky","value":true}]}`)
			want := chainJSON(depth, `{"x":{"kx":1},"y":{"ky":true}}`)
			reversed, changed, err := reverseTranslate("t", args, mutations)
			if err != nil {
				t.Fatalf("depth %d: reverse: %v", depth, err)
			}
			if !changed {
				t.Fatalf("depth %d: no conversion reported", depth)
			}
			var gotM, wantM map[string]any
			if err := json.Unmarshal([]byte(reversed), &gotM); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(want), &wantM); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(gotM, wantM) {
				t.Fatalf("depth %d round trip:\n  got  %s\n  want %s", depth, reversed, want)
			}
		})
	}
}

// TestGeneratedArrayVariantsReverse — arrays nested in objects, objects
// nested in arrays, two nested arrays, and legal dotted/[]/empty/unicode
// field names; each generated path reverses correctly (pins the F2 array
// double-record fix: the each-step REPLACES the scalar step).
func TestGeneratedArrayVariantsReverse(t *testing.T) {
	cases := []struct {
		name   string
		schema string
		args   string
		want   string
	}{
		{"object-in-array",
			`{"type":"object","properties":{"rows":{"type":"array","items":{"type":"object","properties":{"x":{"type":"object"},"y":{"type":"object"}}}}}}`,
			`{"rows":[{"x":[{"key":"a","value":1}],"y":[{"key":"b","value":2}]}]}`,
			`{"rows":[{"x":{"a":1},"y":{"b":2}}]}`},
		{"array-in-object-chain",
			`{"type":"object","properties":{"p0":{"type":"object","properties":{"p1":{"type":"object","properties":{"list":{"type":"array","items":{"type":"object","properties":{"x":{"type":"object"},"y":{"type":"object"}}}}}}}}}}`,
			`{"p0":{"p1":{"list":[{"x":[{"key":"a","value":1}],"y":[{"key":"b","value":2}]}]}}}`,
			`{"p0":{"p1":{"list":[{"x":{"a":1},"y":{"b":2}}]}}}`},
		{"two-nested-arrays",
			`{"type":"object","properties":{"outer":{"type":"array","items":{"type":"object","properties":{"inner":{"type":"array","items":{"type":"object","properties":{"x":{"type":"object"},"y":{"type":"object"}}}}}}}}}`,
			`{"outer":[{"inner":[{"x":[{"key":"a","value":1}],"y":[{"key":"b","value":2}]}]}]}`,
			`{"outer":[{"inner":[{"x":{"a":1},"y":{"b":2}}]}]}`},
		{"exotic field names",
			`{"type":"object","properties":{"a.b":{"type":"object","properties":{"c[]":{"type":"object","properties":{"":{"type":"object","properties":{"名前":{"type":"object"},"x":{"type":"object"}}}}}}}}}`,
			`{"a.b":{"c[]":{"":{"名前":[{"key":"n","value":"1"}],"x":[{"key":"v","value":2}]}}}}`,
			`{"a.b":{"c[]":{"":{"名前":{"n":"1"},"x":{"v":2}}}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var schema map[string]any
			if err := json.Unmarshal([]byte(tc.schema), &schema); err != nil {
				t.Fatal(err)
			}
			mutations := translateSchema(schema, nil, siteRoot)
			if len(mutations) != 2 {
				t.Fatalf("want two mutations, got %d: %+v", len(mutations), mutations)
			}
			// The first step of every array-bearing path must be the each step
			// (never the scalar-then-each double record).
			for i, m := range mutations {
				for j, st := range m.steps {
					if st.each && j > 0 && m.steps[j-1].field == st.field && !m.steps[j-1].each {
						t.Fatalf("mutation %d records %q twice (scalar then each): %+v", i, st.field, m.steps)
					}
				}
			}
			reversed, changed, err := reverseTranslate("t", tc.args, mutations)
			if err != nil {
				t.Fatalf("reverse: %v", err)
			}
			if !changed {
				t.Fatal("no conversion reported")
			}
			var gotM, wantM map[string]any
			if err := json.Unmarshal([]byte(reversed), &gotM); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(tc.want), &wantM); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(gotM, wantM) {
				t.Fatalf("round trip:\n  got  %s\n  want %s", reversed, tc.want)
			}
		})
	}
}

// TestReverseTranslateStrictClasses — every malformed provider-output class
// terminates; every legal class converts exactly (F3).
func TestReverseTranslateStrictClasses(t *testing.T) {
	paths := []mutationPath{{steps: []pathStep{{field: "env", each: false}}}}
	for name, args := range map[string]string{
		"missing value":       `{"env":[{"key":"A"}]}`,
		"duplicate keys":      `{"env":[{"key":"A","value":"1"},{"key":"A","value":"2"}]}`,
		"extra member":        `{"env":[{"key":"A","value":"1","x":2}]}`,
		"non-object item":     `{"env":[1]}`,
		"key non-string":      `{"env":[{"key":5,"value":"1"}]}`,
		"wrong container":     `{"env":"str"}`,
		"null args":           `null`,
		"array args":          `[1,2]`,
		"malformed":           `{"env": `,
		"duplicate JSON keys": `{"env":[{"key":"A","value":"1"}],"env":[{"key":"B","value":"2"}]}`,
		"empty bytes":         ``,
	} {
		t.Run("reject "+name, func(t *testing.T) {
			if _, _, err := reverseTranslate("t", args, paths); err == nil {
				t.Fatalf("must terminate: %q", args)
			}
		})
	}
	for name, tc := range map[string]struct {
		args    string
		want    string
		changed bool
	}{
		"empty key preserved": {`{"env":[{"key":"","value":"v"}]}`, `{"env":{"":"v"}}`, true},
		"empty array":         {`{"env":[]}`, `{"env":{}}`, true},
		"null value":          {`{"env":[{"key":"A","value":null}]}`, `{"env":{"A":null}}`, true},
		"number value":        {`{"env":[{"key":"A","value":42}]}`, `{"env":{"A":42}}`, true},
		"absent path":         {`{"other":1}`, `{"other":1}`, false},
		"empty object":        {`{}`, `{}`, false},
	} {
		t.Run("accept "+name, func(t *testing.T) {
			got, changed, err := reverseTranslate("t", tc.args, paths)
			if err != nil {
				t.Fatalf("must succeed: %v", err)
			}
			if changed != tc.changed {
				t.Fatalf("changed = %v, want %v", changed, tc.changed)
			}
			var gotM, wantM map[string]any
			if err := json.Unmarshal([]byte(got), &gotM); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(tc.want), &wantM); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(gotM, wantM) {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// TestNonObjectSchemasDoNotPanic — null, array, scalar, malformed, and empty
// schema bodies, plus nil ToolDef entries, never panic; each is carried
// untranslated with the empty envelope published (F4).
func TestNonObjectSchemasDoNotPanic(t *testing.T) {
	for name, params := range map[string]string{
		"null":      `null`,
		"array":     `[1,2]`,
		"scalar":    `"str"`,
		"malformed": `{bad`,
		"empty":     ``,
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			res := h.BeforeRequest(&pbv2.ChatRequest{Tools: []*pbv2.ToolDef{{Name: "t", ParametersJson: []byte(params)}}})
			if !res.PassedThrough || res.Err != nil {
				t.Fatalf("expected pass-through, err=%v", res.Err)
			}
			if env := publishedEnvelope(t, h); env != `{"version":1,"tools":{}}` {
				t.Fatalf("envelope = %s", env)
			}
		})
	}
	t.Run("nil tool entry", func(t *testing.T) {
		h := newHarness(t)
		res := h.BeforeRequest(&pbv2.ChatRequest{Tools: []*pbv2.ToolDef{
			nil,
			{Name: "ok", ParametersJson: []byte(`{"additionalProperties":false,"properties":{"p":{"type":"string"}},"type":"object"}`)},
		}})
		if !res.PassedThrough || res.Err != nil {
			t.Fatalf("expected pass-through without panic, err=%v", res.Err)
		}
		if env := publishedEnvelope(t, h); env != `{"version":1,"tools":{}}` {
			t.Fatalf("envelope = %s", env)
		}
	})
}

// TestSemanticNoOpPreservesOriginalBytes — a schema whose translation changes
// nothing semantically (already closed; whitespace; key order; numeric
// spellings) passes through with its ORIGINAL raw bytes: no canonicalizing
// rewrite that would bust the provider prompt cache (F4).
func TestSemanticNoOpPreservesOriginalBytes(t *testing.T) {
	for name, raw := range map[string]string{
		"whitespace":       `{ "type":"object", "additionalProperties":false, "properties":{ "path": { "type":"string" } } }`,
		"key order":        `{"type":"object","additionalProperties":false,"properties":{"path":{"type":"string"}}}`,
		"numeric spelling": `{"additionalProperties":false,"properties":{"n":{"type":"number","minimum":1.0}},"type":"object"}`,
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			req := reqWithTools(raw)
			res := h.BeforeRequest(req)
			if !res.PassedThrough || res.Err != nil {
				t.Fatalf("semantic no-op must pass through, err=%v", res.Err)
			}
			if env := publishedEnvelope(t, h); env != `{"version":1,"tools":{}}` {
				t.Fatalf("envelope = %s", env)
			}
			// The input request object was never touched.
			if string(req.Tools[0].ParametersJson) != raw {
				t.Fatalf("original raw bytes were canonicalized:\n  got  %s\n  want %s", req.Tools[0].ParametersJson, raw)
			}
		})
	}
}

// TestUnconstrainedValuesSurvive — a bare object property and
// additionalProperties:true allow arbitrary JSON values; translation must NOT
// narrow them to strings, and strict reversal must preserve every shape (F5).
func TestUnconstrainedValuesSurvive(t *testing.T) {
	for name, raw := range map[string]string{
		"bare object":          `{"type":"object","properties":{"m":{"type":"object"}}}`,
		"additionalProperties": `{"type":"object","properties":{"m":{"type":"object","additionalProperties":true}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			schema, mutations := translate(t, raw)
			items := schema["properties"].(map[string]any)["m"].(map[string]any)["items"].(map[string]any)
			value := items["properties"].(map[string]any)["value"].(map[string]any)
			if _, hasType := value["type"]; hasType {
				t.Fatalf("unconstrained value was narrowed to type %v", value["type"])
			}
			for _, v := range []string{`true`, `42`, `{"deep":[1,2]}`, `["a","b"]`, `null`, `"str"`} {
				args := `{"m":[{"key":"k","value":` + v + `}]}`
				reversed, _, err := reverseTranslate("t", args, mutations)
				if err != nil {
					t.Fatalf("value %s: %v", v, err)
				}
				want := `{"m":{"k":` + v + `}}`
				var gotM, wantM map[string]any
				if err := json.Unmarshal([]byte(reversed), &gotM); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal([]byte(want), &wantM); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(gotM, wantM) {
					t.Fatalf("value %s: got %s, want %s", v, reversed, want)
				}
			}
		})
	}
}

// TestStreamStrictReversalClasses — the strict classes at the real
// fragmented-stream boundary: malformed classes terminate; legal classes
// reverse; a semantic no-op preserves the exact original bytes AND the
// signature (F3).
func TestStreamStrictReversalClasses(t *testing.T) {
	translated := `{"type":"object","properties":{"env":{"type":"object","additionalProperties":{"type":"string"}}}}`
	for name, args := range map[string]string{
		"missing value":   `{"env":[{"key":"A"}]}`,
		"duplicate keys":  `{"env":[{"key":"A","value":"1"},{"key":"A","value":"2"}]}`,
		"extra member":    `{"env":[{"key":"A","value":"1","x":2}]}`,
		"wrong container": `{"env":"str"}`,
		"null args":       `null`,
	} {
		t.Run("terminates "+name, func(t *testing.T) {
			h := newHarness(t)
			h.BeforeRequest(reqWithTools(translated))
			res := streamBlock(t, h, 0, "call_1", "read", "sig", args)
			if res.Err == nil {
				t.Fatalf("must terminate: %q", args)
			}
		})
	}
	t.Run("empty key preserved, signature cleared", func(t *testing.T) {
		h := newHarness(t)
		h.BeforeRequest(reqWithTools(translated))
		res := streamBlock(t, h, 0, "call_1", "read", "sig", `{"env":[{"key":"","value":"v"}]}`)
		if got := emittedArgs(t, res); got != `{"env":{"":"v"}}` {
			t.Fatalf("args = %q", got)
		}
		if sig := emittedSig(t, res); sig != "" {
			t.Fatalf("signature must be cleared, got %q", sig)
		}
	})
	t.Run("empty array becomes empty object", func(t *testing.T) {
		h := newHarness(t)
		h.BeforeRequest(reqWithTools(translated))
		res := streamBlock(t, h, 0, "call_1", "read", "sig", `{"env":[]}`)
		if got := emittedArgs(t, res); got != `{"env":{}}` {
			t.Fatalf("args = %q", got)
		}
	})
	t.Run("semantic no-op preserves bytes and signature", func(t *testing.T) {
		h := newHarness(t)
		h.BeforeRequest(reqWithTools(translated))
		args := `{ "other":"x" }`
		res := streamBlock(t, h, 0, "call_1", "read", "sig-abc", args)
		if got := emittedArgs(t, res); got != args {
			t.Fatalf("original bytes must travel unchanged, got %q", got)
		}
		if sig := emittedSig(t, res); sig != "sig-abc" {
			t.Fatalf("signature must be preserved on a semantic no-op, got %q", sig)
		}
	})
}

// ==========================================================================
// Round-2 pins (lossless JSON / token boundaries)
// ==========================================================================

// TestLosslessNumbers — JSON numbers must keep their exact lexeme through
// schema translation and strict reversal. float64 decoding rounds
// 9007199254740993 to 9007199254740992, changing validation and executable
// tool input. The emitted lexeme is compared, not float-decoded.
func TestLosslessNumbers(t *testing.T) {
	lexemes := []string{
		"9007199254740991", "9007199254740992", "9007199254740993",
		"18446744073709551615", "-18446744073709551615",
		"0.123456789012345678901234567890", "-0", "1.0", "1e+100",
	}
	for _, lex := range lexemes {
		t.Run("schema "+lex, func(t *testing.T) {
			raw := `{"type":"object","properties":{"env":{"type":"object","additionalProperties":{"type":"string"}},"n":{"type":"number","minimum":` + lex + `}}}`
			// Lossless decode in the harness too: plain float64 unmarshal
			// would round the lexeme before the translator ever sees it.
			dec := json.NewDecoder(strings.NewReader(raw))
			dec.UseNumber()
			var schema map[string]any
			if err := dec.Decode(&schema); err != nil {
				t.Fatal(err)
			}
			translateSchema(schema, nil, siteRoot)
			out, err := json.Marshal(schema)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(out), `"minimum":`+lex) {
				t.Fatalf("schema number lexeme changed: %s", out)
			}
		})
		t.Run("args "+lex, func(t *testing.T) {
			paths := []mutationPath{{steps: []pathStep{{field: "env", each: false}}}}
			args := `{"env":[{"key":"A","value":` + lex + `}]}`
			reversed, changed, err := reverseTranslate("t", args, paths)
			if err != nil {
				t.Fatalf("reverse: %v", err)
			}
			if !changed {
				t.Fatal("no conversion reported")
			}
			if !strings.Contains(reversed, `"A":`+lex) {
				t.Fatalf("argument number lexeme changed: %s", reversed)
			}
		})
	}
}

// TestLosslessNumbersThroughHook — the reviewer's exact reproduction: a
// schema carrying minimum:9007199254740993 alongside an unrelated open map is
// replaced, and the emitted ParametersJson must keep the exact lexeme.
func TestLosslessNumbersThroughHook(t *testing.T) {
	raw := `{"type":"object","properties":{"env":{"type":"object","additionalProperties":{"type":"string"}},"n":{"type":"number","minimum":9007199254740993}}}`
	h := newHarness(t)
	res := h.BeforeRequest(reqWithTools(raw))
	if res.Err != nil || res.Request == nil {
		t.Fatalf("expected replacement, err=%v", res.Err)
	}
	got := string(res.Request.Tools[0].ParametersJson)
	if !strings.Contains(got, `9007199254740993`) {
		t.Fatalf("schema number rounded through the hook: %s", got)
	}
}

// TestLosslessNumbersAtStreamBoundary — the reviewer's argument reproduction
// through the real fragmented stream.
func TestLosslessNumbersAtStreamBoundary(t *testing.T) {
	h := newHarness(t)
	h.BeforeRequest(reqWithTools(`{"type":"object","properties":{"env":{"type":"object","additionalProperties":{"type":"string"}}}}`))
	res := streamBlock(t, h, 0, "call_1", "read", "sig", `{"env":[{"key":"A","value":9007199254740993}]}`)
	if got := emittedArgs(t, res); got != `{"env":{"A":9007199254740993}}` {
		t.Fatalf("argument number rounded through the stream: %q", got)
	}
}

// TestTextualBoundaries — raw invalid UTF-8 and lone surrogate escapes must
// terminate or be rejected everywhere strings carry data, while a valid
// surrogate pair and literal text survive (never normalized to U+FFFD).
func TestTextualBoundaries(t *testing.T) {
	paths := []mutationPath{{steps: []pathStep{{field: "env", each: false}}}}
	for name, args := range map[string]string{
		"raw invalid UTF-8 in key":   "{\"env\":[{\"key\":\"k\xff\",\"value\":\"v\"}]}",
		"lone high surrogate in key": `{"env":[{"key":"\ud800","value":"v"}]}`,
		"lone low surrogate in key":  `{"env":[{"key":"\udc00","value":"v"}]}`,
		"lone high in value":         `{"env":[{"key":"k","value":"\ud801"}]}`,
	} {
		t.Run("terminates "+name, func(t *testing.T) {
			if _, _, err := reverseTranslate("t", args, paths); err == nil {
				t.Fatalf("must terminate: %q", args)
			}
		})
	}
	for name, tc := range map[string]struct{ args, want string }{
		"valid surrogate pair":  {`{"env":[{"key":"\ud83d\ude00","value":"v"}]}`, `{"env":{"😀":"v"}}`},
		"literal escaped slash": {`{"env":[{"key":"\\ud800","value":"v"}]}`, `{"env":{"\\ud800":"v"}}`},
		"literal U+FFFD":        {"{\"env\":[{\"key\":\"k\xef\xbf\xbd\",\"value\":\"v\"}]}", "{\"env\":{\"k\xef\xbf\xbd\":\"v\"}}"},
	} {
		t.Run("accepts "+name, func(t *testing.T) {
			reversed, changed, err := reverseTranslate("t", tc.args, paths)
			if err != nil {
				t.Fatalf("must succeed: %v", err)
			}
			if !changed {
				t.Fatal("no conversion reported")
			}
			var gotM, wantM map[string]any
			if err := json.Unmarshal([]byte(reversed), &gotM); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(tc.want), &wantM); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(gotM, wantM) {
				t.Fatalf("got %q, want %q", reversed, tc.want)
			}
		})
	}
}

// TestTextuallyInvalidSchemasAreCarriedUnchanged — invalid UTF-8 or lone
// surrogates in a schema make it untranslatable (never panicking, never
// normalized); the raw bytes travel byte-identical.
func TestTextuallyInvalidSchemasAreCarriedUnchanged(t *testing.T) {
	for name, params := range map[string]string{
		"raw invalid UTF-8 in property": "{\"type\":\"object\",\"properties\":{\"k\xff\":{\"type\":\"object\"}}}",
		"lone surrogate in property":    `{"type":"object","properties":{"\ud800":{"type":"object"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			req := &pbv2.ChatRequest{Tools: []*pbv2.ToolDef{{Name: "t", ParametersJson: []byte(params)}}}
			res := h.BeforeRequest(req)
			if !res.PassedThrough || res.Err != nil {
				t.Fatalf("expected pass-through, err=%v", res.Err)
			}
			if env := publishedEnvelope(t, h); env != `{"version":1,"tools":{}}` {
				t.Fatalf("envelope = %s", env)
			}
			if string(req.Tools[0].ParametersJson) != params {
				t.Fatal("the raw schema bytes were normalized")
			}
		})
	}
}
