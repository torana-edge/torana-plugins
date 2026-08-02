package main

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"

	"google.golang.org/protobuf/proto"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pbv2 "github.com/torana-edge/torana-plugin-sdk/pb/v2"
)

func main() {}

// mutationsKey is the single request-scoped meta key holding the registry
// envelope (see registryWire below). The stream hook reads it back to reverse
// the exact conversions this request's schema translation recorded.
const mutationsKey = "mutations"

// ==========================================================================
// Registry wire format (approved batch-5 shape)
// ==========================================================================
//
//	{
//	  "version": 1,
//	  "tools": {
//	    "tool_name": [
//	      {"path": [{"field": "a.b", "each": false}]}
//	    ]
//	  }
//	}
//
// The envelope has exactly `version` and `tools`; each mutation object has
// exactly `path`; each step has exactly `field` and `each`. Every path is
// non-empty, duplicate JSON keys are rejected at every object level (including
// duplicate tool names), and canonical encoding always emits the complete
// shape. The empty registry is {"version":1,"tools":{}}.
//
// The wire steps are PRESENCE-sensitive: a missing `field` member is a decode
// error even though `field:""` is a legal empty JSON property name, and a
// missing `each` member is an error even though `each:false` is ordinary. The
// presence distinction lives in the wire types (pointer fields); the internal
// pathStep value type makes malformed states unrepresentable afterwards.

type pathStep struct {
	field string
	each  bool
}

type mutationPath struct {
	steps []pathStep
}

type registry struct {
	version int
	tools   map[string][]mutationPath
}

// appendStep is the ONLY way steps are added to a path. It allocates exactly
// len(path)+1, so sibling paths never share a backing array: an ordinary
// append onto a slice with spare capacity lets a later sibling overwrite the
// same position, silently corrupting the earlier path (review round-1 F2).
func appendStep(path []pathStep, step pathStep) []pathStep {
	out := make([]pathStep, len(path)+1)
	copy(out, path)
	out[len(path)] = step
	return out
}

// wireStep mirrors the canonical step encoding; no omitempty so canonical
// output always emits both members.
type wireStep struct {
	Field string `json:"field"`
	Each  bool   `json:"each"`
}

// marshal emits the canonical envelope: struct field order (version then
// tools), sorted tool names (encoding/json sorts map keys), complete step
// shapes, explicit empty tools map.
func (r *registry) marshal() ([]byte, error) {
	tools := make(map[string]any, len(r.tools))
	for name, paths := range r.tools {
		muts := make([]any, 0, len(paths))
		for _, p := range paths {
			steps := make([]any, 0, len(p.steps))
			for _, s := range p.steps {
				steps = append(steps, wireStep{Field: s.field, Each: s.each})
			}
			muts = append(muts, map[string]any{"path": steps})
		}
		tools[name] = muts
	}
	return json.Marshal(wireEnvelope{Version: 1, Tools: tools})
}

type wireEnvelope struct {
	Version int            `json:"version"`
	Tools   map[string]any `json:"tools"`
}

// decodeRegistry strictly validates an envelope. Every structural violation —
// unknown member, missing required member, null, duplicate key, non-empty-path
// breach, unsupported version, non-array where an array is required — is an
// error; there is no lenient branch.
func decodeRegistry(data []byte) (*registry, error) {
	raw, err := decodeObjectStrict(data, map[string]bool{"version": true, "tools": true})
	if err != nil {
		return nil, fmt.Errorf("registry envelope: %w", err)
	}
	versionRaw, ok := raw["version"]
	if !ok {
		return nil, fmt.Errorf("registry envelope: missing required member %q", "version")
	}
	var version int
	if err := json.Unmarshal(versionRaw, &version); err != nil || version != 1 {
		return nil, fmt.Errorf("registry envelope: unsupported version")
	}
	toolsRaw, ok := raw["tools"]
	if !ok {
		return nil, fmt.Errorf("registry envelope: missing required member %q", "tools")
	}
	// Duplicate tool names are duplicate object keys and were already rejected
	// by rejectDuplicateKeys on the raw tools object.
	var toolsMap map[string]json.RawMessage
	if err := json.Unmarshal(toolsRaw, &toolsMap); err != nil {
		return nil, fmt.Errorf("registry envelope: tools must be an object")
	}
	reg := &registry{version: 1, tools: make(map[string][]mutationPath, len(toolsMap))}
	for name, mutsRaw := range toolsMap {
		paths, err := decodeMutations(mutsRaw)
		if err != nil {
			return nil, fmt.Errorf("registry envelope: tool %q: %w", name, err)
		}
		reg.tools[name] = paths
	}
	return reg, nil
}

func decodeMutations(raw json.RawMessage) ([]mutationPath, error) {
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, fmt.Errorf("mutations must be an array")
	}
	if len(arr) == 0 {
		return nil, fmt.Errorf("mutations must be non-empty")
	}
	paths := make([]mutationPath, 0, len(arr))
	for i, m := range arr {
		mem, err := decodeObjectStrict(m, map[string]bool{"path": true})
		if err != nil {
			return nil, fmt.Errorf("mutation %d: %w", i, err)
		}
		pathRaw, ok := mem["path"]
		if !ok {
			return nil, fmt.Errorf("mutation %d: missing required member %q", i, "path")
		}
		steps, err := decodeSteps(pathRaw)
		if err != nil {
			return nil, fmt.Errorf("mutation %d: %w", i, err)
		}
		paths = append(paths, mutationPath{steps: steps})
	}
	return paths, nil
}

func decodeSteps(raw json.RawMessage) ([]pathStep, error) {
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, fmt.Errorf("path must be an array")
	}
	if len(arr) == 0 {
		return nil, fmt.Errorf("path must be non-empty")
	}
	steps := make([]pathStep, 0, len(arr))
	for i, s := range arr {
		mem, err := decodeObjectStrict(s, map[string]bool{"field": true, "each": true})
		if err != nil {
			return nil, fmt.Errorf("step %d: %w", i, err)
		}
		fieldRaw, okF := mem["field"]
		eachRaw, okE := mem["each"]
		if !okF {
			return nil, fmt.Errorf("step %d: missing required member %q", i, "field")
		}
		if !okE {
			return nil, fmt.Errorf("step %d: missing required member %q", i, "each")
		}
		var field string
		if err := json.Unmarshal(fieldRaw, &field); err != nil {
			return nil, fmt.Errorf("step %d: field must be a string", i)
		}
		var each bool
		if err := json.Unmarshal(eachRaw, &each); err != nil {
			return nil, fmt.Errorf("step %d: each must be a boolean", i)
		}
		steps = append(steps, pathStep{field: field, each: each})
	}
	return steps, nil
}

// ==========================================================================
// Hook registration
// ==========================================================================

func init() {
	// ── Request side: translate schemas, publish the registry FIRST ─────
	//
	// Registry publication is the PREREQUISITE to any replacement: the stream
	// hook can only reverse what this request recorded, and a replacement
	// whose reversal registry was never persisted must not escape. Every
	// before-request invocation containing any tool definition publishes
	// EXACTLY ONE valid envelope — including an explicit empty tools map when
	// nothing changed — before returning either ReplaceRequest or PassRequest.
	// An advisory publication refusal returns the original request unchanged
	// (no partial schema mutation escapes); a contract/protocol failure is a
	// hook error. A request with no tools needs no envelope: a conforming
	// response cannot contain a tool call for it.
	sdk.OnBeforeRequest(func(ctx context.Context, req *pbv2.ChatRequest) (sdk.RequestResult, error) {
		if len(req.Tools) == 0 {
			return sdk.PassRequest(), nil
		}

		reg, newTools, changed := translateTools(req.Tools)
		envJSON, err := reg.marshal()
		if err != nil {
			return sdk.RequestResult{}, err
		}
		if herr, err := sdk.MetaSet(mutationsKey, string(envJSON)); err != nil {
			return sdk.RequestResult{}, err
		} else if herr != nil {
			if isAdvisory(herr.Code) {
				// The reversal registry was not persisted. The original
				// request travels untouched — a translated schema whose
				// reversal cannot be proven must not be sent.
				return sdk.PassRequest(), nil
			}
			return sdk.RequestResult{}, fmt.Errorf("schema_translator: registry publish refused (code=%s)", herr.Code)
		}

		if !changed {
			return sdk.PassRequest(), nil
		}
		req.Tools = newTools
		return sdk.ReplaceRequest(req), nil
	})

	// ── Stream side: reverse KV arrays via StreamAssembler.Feed ─────────
	//
	// NOT StreamHandler: its callback fail-open would emit the assembled
	// MODEL-FACING KV-array to the original tool, changing types and executing
	// the wrong call. Feed assembles host-side and hands the complete call to
	// this hook, which decides:
	//
	//   - present, valid envelope explicitly lacking this tool → emit the
	//     assembled call byte-identically (signature preserved);
	//   - recorded paths, reversal succeeds → emit the reversed call
	//     (the SDK emitter clears the signature when arguments change);
	//   - missing, advisory-unavailable, malformed, or unsupported registry
	//     state, or a recorded tool whose arguments cannot be reversed →
	//     HOOK ERROR. An absent envelope cannot prove "nothing was
	//     translated" — successive calls may land on different WASM
	//     instances — so pass-through requires positive proof.
	asm := sdk.NewStreamAssembler().WithToolAssembly()
	sdk.OnStreamChunk(func(ctx context.Context, ev *pbv2.StreamEvent) (sdk.StreamResult, error) {
		fr := asm.Feed(ev)
		if fr.Err != nil {
			return sdk.StreamResult{}, fr.Err
		}
		if fr.Complete != nil {
			// Complete carries the assembled call AND marks the stop suppressed;
			// the completion decision must win over the generic suppression.
			return handleAssembled(*fr.Complete)
		}
		if fr.Suppress {
			return sdk.SuppressEvent(), nil
		}
		return sdk.EmitEvents(fr.Emit...), nil
	})
}

// handleAssembled resolves one completed tool call against the registry.
func handleAssembled(call sdk.ToolCall) (sdk.StreamResult, error) {
	raw, herr, err := sdk.MetaGet(mutationsKey)
	if err != nil {
		return sdk.StreamResult{}, err
	}
	if herr != nil {
		// NOT_FOUND, NOT_CONFIGURED, UNAVAILABLE, PERMISSION_DENIED: every
		// non-success is terminal at stream completion. There is no
		// pass-through without a present, valid envelope.
		return sdk.StreamResult{}, fmt.Errorf("schema_translator: registry unavailable (code=%s)", herr.Code)
	}
	reg, err := decodeRegistry([]byte(raw))
	if err != nil {
		return sdk.StreamResult{}, fmt.Errorf("schema_translator: registry corrupt: %w", err)
	}
	paths, recorded := reg.tools[call.Name]
	if !recorded || len(paths) == 0 {
		// Explicit absence in a valid envelope: this tool was not translated.
		return sdk.EmitEvents(sdk.EmitAssembledToolCall(call, call.Arguments)...), nil
	}
	reversed, changed, err := reverseTranslate(call.Name, call.Arguments, paths)
	if err != nil {
		return sdk.StreamResult{}, err
	}
	if !changed {
		// Every translated property was absent: a semantic no-op. Emit the
		// ORIGINAL argument bytes so the bound signature stays valid.
		return sdk.EmitEvents(sdk.EmitAssembledToolCall(call, call.Arguments)...), nil
	}
	return sdk.EmitEvents(sdk.EmitAssembledToolCall(call, reversed)...), nil
}

// translateTools translates every eligible tool into a proto.Clone-derived
// ToolDef list, recording structural mutation paths. Cloning preserves every
// field the host or customer set — description, strict, cache_control_json,
// and unknown protobuf bytes — so a replacement never drops them (review
// round-1 F1). Deterministic: property names are visited in sorted order, so
// both the schema bytes and the registry bytes are pure functions of the
// request. Duplicate tool names are pre-scanned and EVERY occurrence is left
// untranslated — no last-write-wins reversal schema.
//
// `changed` tracks SEMANTIC mutation: a schema whose translation produced no
// value change (already closed, different key order, whitespace, numeric
// spellings) keeps its ORIGINAL raw bytes instead of being canonicalized, so
// a pass stays a pass and prompt-cache bytes are untouched (review F4).
func translateTools(tools []*pbv2.ToolDef) (*registry, []*pbv2.ToolDef, bool) {
	counts := make(map[string]int, len(tools))
	for _, t := range tools {
		if t == nil {
			continue
		}
		counts[t.Name]++
	}

	reg := &registry{version: 1, tools: make(map[string][]mutationPath)}
	newTools := make([]*pbv2.ToolDef, 0, len(tools))
	changed := false

	for _, tool := range tools {
		if tool == nil {
			// Defensive: a nil entry is carried as-is and never dereferenced.
			newTools = append(newTools, nil)
			continue
		}
		nt := proto.Clone(tool).(*pbv2.ToolDef)
		if counts[tool.Name] > 1 || len(tool.ParametersJson) == 0 {
			newTools = append(newTools, nt)
			continue
		}
		var params map[string]any
		if err := json.Unmarshal(tool.ParametersJson, &params); err != nil || params == nil {
			// Malformed, null, array, or scalar schema: cannot translate,
			// not recorded, carried byte-identical.
			newTools = append(newTools, nt)
			continue
		}
		orig := deepCopyMap(params)
		mutations := translateSchema(params, nil, siteRoot)
		if len(mutations) > 0 {
			reg.tools[tool.Name] = mutations
		}
		if reflect.DeepEqual(orig, params) {
			// Semantic no-op: preserve the author's raw bytes.
			newTools = append(newTools, nt)
			continue
		}
		newJSON, err := json.Marshal(params)
		if err != nil {
			newTools = append(newTools, nt)
			continue
		}
		nt.ParametersJson = newJSON
		changed = true
		newTools = append(newTools, nt)
	}
	return reg, newTools, changed
}

// isAdvisory reports whether a refusal code means "try without this
// capability" rather than "this call was wrong".
func isAdvisory(code pbv2.ErrorCode) bool {
	return code == pbv2.ErrorCode_ERROR_CODE_NOT_CONFIGURED ||
		code == pbv2.ErrorCode_ERROR_CODE_UNAVAILABLE
}

// ==========================================================================
// Schema translation
// ==========================================================================

// schemaSite says where in a tool's schema we are, because the same rewrite is
// correct in one place and destructive in another.
type schemaSite int

const (
	// siteRoot is a tool's top-level parameters.
	siteRoot schemaSite = iota
	// siteProperty is a named property's schema, reached by recursion after the
	// property loop has already decided whether to convert it.
	siteProperty
	// siteArrayItem is an array's items schema.
	siteArrayItem
)

// The rules this function follows:
//
//  1. Never change a tool's ROOT type. Parameters must be a JSON Schema object;
//     OpenAI, Anthropic and Gemini all reject "type": "array" there.
//  2. Never rewrite a shape reverseTranslate cannot undo.
//  3. Never make a schema stricter than its author wrote it, except at the root,
//     where closing an unspecified object is this plugin's declared purpose and
//     the operator opted into it by enabling the plugin.
//
// Rule 2 is why NO conversion happens in this function's head. The head is only
// ever reached for the root or an array's item schema — a nested object property
// has already been converted-or-not by the loop below before recursing — and
// converting an item changes the array's element type from object to array. The
// model then emits [[{key,value},…]] instead of [{…}], and reverseAtPath cannot
// undo it: it requires each element of an each-step mutation to be a MAP. The
// tool receives a list of lists.
//
// Conversion happens only at a property, in the loop, where the recorded path is
// one reverseTranslate can actually reverse.
func translateSchema(schema map[string]any, path []pathStep, site schemaSite) []mutationPath {
	if schema == nil {
		return nil
	}
	var mutations []mutationPath

	props, hasProps := schema["properties"].(map[string]any)

	if schemaType, _ := schema["type"].(string); schemaType != "" && schemaType != "object" {
		return mutations
	}

	if hasAdditionalProperties(schema) {
		// An author-declared open map, left exactly as written.
		if !hasProps {
			return mutations
		}
		// Properties sit alongside it: translate them, leave the map declared.
	} else if site != siteArrayItem {
		schema["additionalProperties"] = false
	}
	// An array item is never closed. At the root, closing a no-argument tool is
	// harmless — there are no arguments to forbid. At an item, closing forbids
	// the very keys a free-form record type exists to carry.

	if !hasProps {
		return mutations
	}

	// Sorted property iteration: the traversal is a pure function of the
	// schema, so the recorded mutation order (and therefore the registry
	// bytes) is deterministic across repeated parses of identical input.
	names := make([]string, 0, len(props))
	for propName := range props {
		names = append(names, propName)
	}
	sort.Strings(names)

	for _, propName := range names {
		propSchema, ok := props[propName].(map[string]any)
		if !ok {
			continue
		}
		currentPath := appendStep(path, pathStep{field: propName, each: false})
		propType, _ := propSchema["type"].(string)
		_, propHasProps := propSchema["properties"].(map[string]any)
		_, propHasAP := propSchema["additionalProperties"]

		if propType == "object" && !propHasProps && !propHasAP {
			convertToKVArray(propSchema, nil)
			mutations = append(mutations, mutationPath{steps: currentPath})
			continue
		}
		if hasAdditionalProperties(propSchema) {
			valueSchema, _ := propSchema["additionalProperties"].(map[string]any)
			convertToKVArray(propSchema, valueSchema)
			mutations = append(mutations, mutationPath{steps: currentPath})
			continue
		}

		switch propType {
		case "object":
			propSchema["additionalProperties"] = false
			mutations = append(mutations, translateSchema(propSchema, currentPath, siteProperty)...)
		case "array":
			if items, ok := propSchema["items"].(map[string]any); ok {
				if itemType, _ := items["type"].(string); itemType == "object" {
					// The each-step REPLACES the just-created scalar step: the
					// mutation site is the array's ITEMS, so the recorded path
					// is parent+{propName,each:true}, never parent+
					// {propName}+{propName,each:true} (review round-1 F2).
					itemPath := appendStep(path, pathStep{field: propName, each: true})
					mutations = append(mutations, translateSchema(items, itemPath, siteArrayItem)...)
				}
			}
		}
	}
	return mutations
}

func hasAdditionalProperties(schema map[string]any) bool {
	ap, exists := schema["additionalProperties"]
	if !exists {
		return false
	}
	switch v := ap.(type) {
	case bool:
		return v
	case map[string]any:
		return true
	}
	return false
}

// convertToKVArray rewrites an open map into an array of {key, value} pairs, so
// a model that cannot emit free-form object keys can still populate it.
//
// valueSchema is the author's declared schema for the map's values, when there
// was one; it is DEEP-copied before embedding (the v1 shallow copy aliased
// nested maps and slices). When the author declared NO value constraint (a
// bare object property, or additionalProperties:true), the value schema is
// UNCONSTRAINED — only a description, no type — so arbitrary JSON values
// (booleans, numbers, objects, arrays, null) survive translation and strict
// reversal. Narrowing unconstrained values to type:string made the schema
// stricter than the author wrote it (review round-1 F5).
func convertToKVArray(schema map[string]any, valueSchema map[string]any) {
	desc := ""
	if d, ok := schema["description"].(string); ok {
		desc = d
	}
	for k := range schema {
		delete(schema, k)
	}

	value := map[string]any{"description": "the value"}
	if valueSchema != nil {
		value = deepCopyMap(valueSchema)
		if _, described := value["description"]; !described {
			value["description"] = "the value"
		}
	}

	schema["type"] = "array"
	schema["description"] = desc + " (as key-value pairs: [{\"key\": \"...\", \"value\": \"...\"}])"
	schema["items"] = map[string]any{
		"type": "object",
		"properties": map[string]any{
			"key":   map[string]any{"type": "string", "description": "the key name"},
			"value": value,
		},
		"additionalProperties": false,
		"required":             []any{"key", "value"},
	}
}

func deepCopyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		switch t := v.(type) {
		case map[string]any:
			out[k] = deepCopyMap(t)
		case []any:
			out[k] = deepCopySlice(t)
		default:
			out[k] = v
		}
	}
	return out
}

func deepCopySlice(s []any) []any {
	out := make([]any, len(s))
	for i, v := range s {
		switch t := v.(type) {
		case map[string]any:
			out[i] = deepCopyMap(t)
		case []any:
			out[i] = deepCopySlice(t)
		default:
			out[i] = v
		}
	}
	return out
}

// ==========================================================================
// Reverse translation (strict, error-returning)
// ==========================================================================

// reverseTranslate undoes KV-array conversions using ONLY the recorded
// mutation paths — the exact paths this plugin converted on the request side.
// It deliberately does not touch KV-array shapes it did not create: an agent
// may legitimately pass [{"key":..,"value":..}] arrays as real arguments, and
// no heuristic can tell those apart from our translations.
//
// The traversal is STRICT (review round-1 F3): a recorded tool whose assembled
// arguments are anything other than a non-null JSON object — empty bytes,
// null, arrays, scalars, malformed JSON, or duplicate JSON keys — TERMINATES,
// because emitting it would execute a call with a different meaning. Every
// present traversal step must have the expected container type; every KV item
// must be an object with EXACTLY `key` (a string; empty is legal and
// preserved) and `value`; duplicate KV keys, missing/extra members, and wrong
// types are errors. An empty KV array becomes an empty object. An absent
// optional path is a valid no-op.
//
// changed reports whether any conversion actually happened. When nothing was
// converted, the ORIGINAL argument bytes are returned unchanged so the bound
// signature stays valid (no unmarshal/remarshal of unrelated fields).
func reverseTranslate(toolName string, argsJSON string, paths []mutationPath) (string, bool, error) {
	if len(paths) == 0 {
		return argsJSON, false, nil
	}
	if err := rejectDuplicateKeys([]byte(argsJSON)); err != nil {
		return "", false, fmt.Errorf("schema_translator: cannot reverse %q: %w", toolName, err)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", false, fmt.Errorf("schema_translator: cannot reverse %q: arguments are not a JSON object", toolName)
	}
	if args == nil {
		return "", false, fmt.Errorf("schema_translator: cannot reverse %q: arguments are null", toolName)
	}
	changed := false
	for _, p := range paths {
		c, err := reverseAtPathStrict(args, p.steps)
		if err != nil {
			return "", false, fmt.Errorf("schema_translator: cannot reverse %q: %w", toolName, err)
		}
		changed = changed || c
	}
	if !changed {
		return argsJSON, false, nil
	}
	b, err := json.Marshal(args)
	if err != nil {
		return "", false, fmt.Errorf("schema_translator: cannot reverse %q: %w", toolName, err)
	}
	return string(b), true, nil
}

// reverseAtPathStrict reverses the conversions along steps. Returns whether
// the object was actually modified. An absent step is a valid no-op; a
// present step with the wrong container type is an error (never a silent
// skip). A TERMINAL each step is structurally invalid: the translator only
// records conversions at property sites (terminal each:false), so a registry
// path ending in each:true was not generated by this plugin and is rejected
// rather than invoking the old per-element heuristic.
func reverseAtPathStrict(obj map[string]any, steps []pathStep) (bool, error) {
	if len(steps) == 0 {
		return false, nil
	}
	step := steps[0]
	rest := steps[1:]

	if step.each {
		if len(rest) == 0 {
			return false, fmt.Errorf("registry path ends in each:true — not a path this translator generates")
		}
		val, present := obj[step.field]
		if !present {
			return false, nil
		}
		arr, ok := val.([]any)
		if !ok {
			return false, fmt.Errorf("step %q must be an array, got %T", step.field, val)
		}
		changed := false
		for _, item := range arr {
			itemMap, ok := item.(map[string]any)
			if !ok {
				return false, fmt.Errorf("step %q: element is not an object", step.field)
			}
			c, err := reverseAtPathStrict(itemMap, rest)
			if err != nil {
				return false, err
			}
			changed = changed || c
		}
		return changed, nil
	}

	if len(rest) == 0 {
		val, present := obj[step.field]
		if !present {
			return false, nil
		}
		arr, ok := val.([]any)
		if !ok {
			return false, fmt.Errorf("step %q must be a KV array, got %T", step.field, val)
		}
		reversed, err := reverseKVArrayStrict(arr)
		if err != nil {
			return false, err
		}
		obj[step.field] = reversed
		return true, nil
	}

	val, present := obj[step.field]
	if !present {
		return false, nil
	}
	nested, ok := val.(map[string]any)
	if !ok {
		return false, fmt.Errorf("step %q must be an object, got %T", step.field, val)
	}
	return reverseAtPathStrict(nested, rest)
}

// reverseKVArrayStrict converts a KV array into the tool-facing map. Every
// item must be an object with exactly `key` and `value`; `key` must be a
// string (empty keys are legal and preserved); duplicate keys are an error;
// an empty array yields an empty object.
func reverseKVArrayStrict(arr []any) (map[string]any, error) {
	result := make(map[string]any, len(arr))
	for _, item := range arr {
		kv, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("KV item is not an object")
		}
		if len(kv) != 2 {
			return nil, fmt.Errorf("KV item must have exactly key and value, got %d members", len(kv))
		}
		keyRaw, hasKey := kv["key"]
		valueRaw, hasValue := kv["value"]
		if !hasKey || !hasValue {
			return nil, fmt.Errorf("KV item missing key or value")
		}
		key, ok := keyRaw.(string)
		if !ok {
			return nil, fmt.Errorf("KV item key must be a string, got %T", keyRaw)
		}
		if _, dup := result[key]; dup {
			return nil, fmt.Errorf("duplicate KV key %q", key)
		}
		result[key] = valueRaw
	}
	return result, nil
}
