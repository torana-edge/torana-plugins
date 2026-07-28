package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	"github.com/torana-edge/torana-plugin-sdk/pb"
)

func main() {}

const mutationsKey = "mutations"

// activeToolKey returns the meta key for tracking the current tool call ID by index.
func activeToolKey(index int32) string { return fmt.Sprintf("tool:%d", index) }

// activeNameKey returns the meta key for tracking the current tool name by index.
func activeNameKey(index int32) string { return fmt.Sprintf("name:%d", index) }

// fragmentKey returns the meta key for accumulating tool call argument fragments.
func fragmentKey(toolCallID string) string { return "frag:" + toolCallID }

func init() {
	// ── Request side: KV-array schema conversion ──────────────────────
	sdk.OnBeforeRequest(func(ctx context.Context, req *pb.ChatRequest) (*pb.ChatRequest, error) {
		if len(req.Tools) == 0 {
			return nil, nil
		}

		modified := false
		registry := make(map[string][]string)

		for _, tool := range req.Tools {
			if len(tool.ParametersJson) == 0 {
				continue
			}
			var params map[string]any
			if err := json.Unmarshal(tool.ParametersJson, &params); err != nil {
				continue
			}

			mutations := translateSchema(tool.Name, params, "", true)
			if len(mutations) > 0 {
				registry[tool.Name] = mutations
			}

			newJSON, err := json.Marshal(params)
			if err != nil {
				continue
			}
			if string(newJSON) != string(tool.ParametersJson) {
				tool.ParametersJson = newJSON
				modified = true
			}
		}

		if len(registry) > 0 {
			regJSON, _ := json.Marshal(registry)
			sdk.HostCall("env.meta_set", fmt.Sprintf(`{"key":"%s","value":%s}`, mutationsKey, string(regJSON)))
		}

		if !modified {
			return nil, nil
		}
		return req, nil
	})

	// ── Response side: reverse KV arrays ────────────────────────────
	//
	// Argument deltas are buffered and suppressed; at ToolCallEnd the
	// assembled arguments are reversed and emitted as one complete
	// ToolCallDelta followed by the ToolCallEnd.
	sdk.OnStreamChunk(func(ctx context.Context, chunk *pb.StreamEvent) (*pb.StreamEventResult, error) {
		// Track the current tool call ID and name by index
		// (ToolCallStart is the only event carrying them).
		if ts := chunk.GetToolCallStart(); ts != nil {
			sdk.HostCall("env.meta_set", fmt.Sprintf(`{"key":"%s","value":%q}`, activeToolKey(ts.Index), ts.Id))
			sdk.HostCall("env.meta_set", fmt.Sprintf(`{"key":"%s","value":%q}`, activeNameKey(ts.Index), ts.Name))
			return sdk.Pass(), nil
		}

		// Buffer and suppress ToolCallDelta fragments.
		if td := chunk.GetToolCallDelta(); td != nil {
			toolID, _ := sdk.HostCall("env.meta_get", activeToolKey(td.Index))
			if toolID == "" {
				return sdk.Pass(), nil
			}
			key := fragmentKey(toolID)
			prev, _ := sdk.HostCall("env.meta_get", key)
			accumulated := prev + td.ArgumentsDelta
			sdk.HostCall("env.meta_set", fmt.Sprintf(`{"key":"%s","value":%q}`, key, accumulated))
			return sdk.Suppress(), nil
		}

		// On ToolCallEnd: emit the reversed assembled args, then the end.
		if te := chunk.GetToolCallEnd(); te != nil {
			toolID, _ := sdk.HostCall("env.meta_get", activeToolKey(te.Index))
			toolName, _ := sdk.HostCall("env.meta_get", activeNameKey(te.Index))
			if toolID == "" {
				return sdk.Pass(), nil
			}
			key := fragmentKey(toolID)
			fullArgs, _ := sdk.HostCall("env.meta_get", key)
			// Clean up (empty value deletes the key).
			sdk.HostCall("env.meta_set", fmt.Sprintf(`{"key":"%s","value":""}`, key))
			sdk.HostCall("env.meta_set", fmt.Sprintf(`{"key":"%s","value":""}`, activeToolKey(te.Index)))
			sdk.HostCall("env.meta_set", fmt.Sprintf(`{"key":"%s","value":""}`, activeNameKey(te.Index)))

			if fullArgs == "" {
				// No fragments were buffered — nothing to re-emit.
				return sdk.Pass(), nil
			}

			regRaw, _ := sdk.HostCall("env.meta_get", mutationsKey)
			var registry map[string][]string
			if regRaw != "" {
				json.Unmarshal([]byte(regRaw), &registry)
			}
			// reverseTranslate returns the input unchanged on parse failure,
			// so the buffered args are always re-emitted intact at worst.
			reversed, _ := reverseTranslate(toolName, fullArgs, registry)

			// The fragments were suppressed, so the complete arguments MUST
			// be emitted here regardless of whether reversal changed them.
			return sdk.Emit(
				&pb.StreamEvent{
					Event: &pb.StreamEvent_ToolCallDelta{
						ToolCallDelta: &pb.ToolCallDelta{
							Index:          te.Index,
							ArgumentsDelta: reversed,
						},
					},
				},
				chunk,
			), nil
		}

		return sdk.Pass(), nil
	})
}

// ==========================================================================
// Schema translation
// ==========================================================================

// The rule this function follows, at every level:
//
//   - never change a tool's ROOT type. Tool parameters must be a JSON Schema
//     object; OpenAI, Anthropic and Gemini all reject "type": "array" there.
//   - never make a schema stricter than its author wrote it. Closing an
//     unspecified object is this plugin's job; closing one the author
//     deliberately left open is not.
//
// isRoot marks the tool's top-level parameters, where both rules bind hardest.
func translateSchema(toolName string, schema map[string]any, path string, isRoot bool) []string {
	var mutations []string

	props, hasProps := schema["properties"].(map[string]any)
	_, hasExplicitAP := schema["additionalProperties"]
	schemaType, _ := schema["type"].(string)

	// A bare {"type": "object"} becomes a key-value array so models that cannot
	// emit free-form maps can still populate it — but never at the root. A bare
	// object is the commonest shape for a NO-ARGUMENT tool, and rewriting its
	// root parameters to an array made every such tool uncallable on all three
	// major providers.
	if !isRoot && schemaType == "object" && !hasProps && !hasExplicitAP {
		convertToKVArray(schema, "string", nil)
		mutations = append(mutations, path)
		return mutations
	}

	// An author-declared open map. Below the root it becomes a KV array, which
	// preserves the intent in a shape a model can emit. At the root it cannot —
	// the root type must stay "object" — so the declaration is left exactly as
	// written.
	//
	// This check used to live only inside the property loop, so an open map at
	// the root, or on an array's item schema, fell through to the blanket close
	// below and was silently made stricter than its author wrote it.
	// An author-declared open map is left exactly as written here, and that is
	// the whole of the rule at this level: never make a schema stricter than
	// its author wrote it.
	//
	// It is NOT converted to a KV array here. The only schemas reaching this
	// branch are a tool's root and an array's ITEM schema — a nested object
	// property has already been handled by the loop below before recursing —
	// and converting either one changes a shape the caller cannot use:
	//
	//   root: parameters must be a JSON Schema object; providers reject an
	//   array outright.
	//
	//   array item: the element type changes from object to array, so the model
	//   emits [[{key,value},…]] instead of [{…}], and reverseAtPath cannot undo
	//   it — it expects each element of a "path[]" mutation to be a MAP. The
	//   tool then receives a list of lists.
	//
	// KV conversion happens only at a property, in the loop below, where the
	// mutation path is one reverseTranslate can actually reverse.
	if hasAdditionalProperties(schema) {
		if !hasProps {
			return mutations
		}
		// Properties sit alongside the open map: translate them, but leave
		// additionalProperties as declared.
	} else {
		schema["additionalProperties"] = false
	}
	if !hasProps {
		return mutations
	}

	for propName, propVal := range props {
		propSchema, ok := propVal.(map[string]any)
		if !ok {
			continue
		}
		currentPath := joinPath(path, propName)
		propType, _ := propSchema["type"].(string)
		_, propHasProps := propSchema["properties"].(map[string]any)
		_, propHasAP := propSchema["additionalProperties"]

		if propType == "object" && !propHasProps && !propHasAP {
			convertToKVArray(propSchema, "string", nil)
			mutations = append(mutations, currentPath)
			continue
		}
		if hasAdditionalProperties(propSchema) {
			valueSchema, _ := propSchema["additionalProperties"].(map[string]any)
			convertToKVArray(propSchema, extractAdditionalPropertiesType(propSchema), valueSchema)
			mutations = append(mutations, currentPath)
			continue
		}

		// additionalProperties is meaningful only on an object. Setting it on a
		// string or a number is noise that every tool schema then carries
		// upstream.
		switch propType {
		case "object":
			propSchema["additionalProperties"] = false
			mutations = append(mutations, translateSchema(toolName, propSchema, currentPath, false)...)
		case "array":
			if items, ok := propSchema["items"].(map[string]any); ok {
				if itemType, _ := items["type"].(string); itemType == "object" {
					// isRoot=false: an item schema is not a tool's root, so an
					// open map on it converts rather than being closed.
					mutations = append(mutations, translateSchema(toolName, items, currentPath+"[]", false)...)
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

func extractAdditionalPropertiesType(schema map[string]any) string {
	if ap, ok := schema["additionalProperties"].(map[string]any); ok {
		if t, ok := ap["type"].(string); ok {
			return t
		}
	}
	return "string"
}

// convertToKVArray rewrites an open map into an array of {key, value} pairs, so
// a model that cannot emit free-form object keys can still populate it.
//
// valueSchema is the author's declared schema for the map's values, when there
// was one. It used to be discarded and replaced with a bare {"type": <name>},
// so a map declared as {"type":"object","properties":{...}} told the model only
// "value is an object" — with none of its shape.
func convertToKVArray(schema map[string]any, valueType string, valueSchema map[string]any) {
	desc := ""
	if d, ok := schema["description"].(string); ok {
		desc = d
	}
	for k := range schema {
		delete(schema, k)
	}

	value := map[string]any{"type": valueType, "description": "the value"}
	if valueSchema != nil {
		// Preserve the declared shape, copied so later mutation of the result
		// cannot reach back into the caller's map.
		value = make(map[string]any, len(valueSchema)+1)
		for k, v := range valueSchema {
			value[k] = v
		}
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

// ==========================================================================
// Reverse translation
// ==========================================================================

// reverseTranslate undoes KV-array conversions using ONLY the per-request
// mutation registry — the exact paths this plugin converted on the request
// side. It deliberately does not touch KV-array shapes it did not create: an
// agent may legitimately pass [{"key":..,"value":..}] arrays as real arguments,
// and no heuristic can tell those apart from our translations. A tool with no
// recorded mutation (nothing was translated) therefore passes through intact.
func reverseTranslate(toolName string, argsJSON string, registry map[string][]string) (string, string) {
	if argsJSON == "" || argsJSON == "{}" || toolName == "" {
		return argsJSON, ""
	}
	paths, ok := registry[toolName]
	if !ok || len(paths) == 0 {
		return argsJSON, ""
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return argsJSON, ""
	}
	for _, path := range paths {
		reverseKVArrayAtPath(args, path)
	}
	b, err := json.Marshal(args)
	if err != nil {
		return argsJSON, ""
	}
	return string(b), ""
}

func reverseKVArrayAtPath(args map[string]any, path string) {
	reverseAtPath(args, strings.Split(path, "."))
}

func reverseAtPath(obj map[string]any, parts []string) {
	if len(parts) == 0 {
		return
	}
	current := parts[0]
	rest := parts[1:]

	if strings.HasSuffix(current, "[]") {
		fieldName := strings.TrimSuffix(current, "[]")
		arr, ok := obj[fieldName].([]any)
		if !ok {
			return
		}
		if len(rest) == 0 {
			for i, item := range arr {
				if itemMap, ok := item.(map[string]any); ok {
					arr[i] = reverseKVObject(itemMap)
				}
			}
		} else {
			for _, item := range arr {
				if itemMap, ok := item.(map[string]any); ok {
					reverseAtPath(itemMap, rest)
				}
			}
		}
		return
	}

	if len(rest) == 0 {
		if val, ok := obj[current]; ok {
			if arr, ok := val.([]any); ok {
				obj[current] = reverseKVArray(arr)
			}
		}
		return
	}

	if nested, ok := obj[current].(map[string]any); ok {
		reverseAtPath(nested, rest)
	}
}

func reverseKVArray(arr []any) map[string]any {
	result := make(map[string]any, len(arr))
	for _, item := range arr {
		kv, ok := item.(map[string]any)
		if !ok {
			continue
		}
		key, _ := kv["key"].(string)
		if key == "" {
			continue
		}
		if val, exists := kv["value"]; exists {
			result[key] = val
		}
	}
	return result
}

func reverseKVObject(obj map[string]any) map[string]any {
	for k, v := range obj {
		if arr, ok := v.([]any); ok && isKVArray(arr) {
			obj[k] = reverseKVArray(arr)
		}
	}
	return obj
}

func isKVArray(arr []any) bool {
	if len(arr) == 0 {
		return false
	}
	if m, ok := arr[0].(map[string]any); ok {
		_, hasKey := m["key"]
		_, hasValue := m["value"]
		return hasKey && hasValue
	}
	return false
}

func joinPath(base, segment string) string {
	if base == "" {
		return segment
	}
	return base + "." + segment
}
