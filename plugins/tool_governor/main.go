// tool_governor controls the tool definitions advertised to the model. It is
// deliberately not an execution sandbox: a harness still owns tool execution,
// and Torana cannot police commands that never cross the inference wire.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pbv2 "github.com/torana-edge/torana-plugin-sdk/pb/v2"
	"github.com/torana-edge/torana-plugin-sdk/pb/v2/jsontext"
	"google.golang.org/protobuf/proto"
)

func main() {}

type replacement struct {
	description *string
	parameters  []byte
	hasParams   bool
	strict      *bool
}

type policy struct {
	allowPresent bool
	allow        map[string]struct{}
	deny         map[string]struct{}
	replace      map[string]replacement
}

func init() {
	sdk.OnBeforeRequest(func(_ context.Context, req *pbv2.ChatRequest) (sdk.RequestResult, error) {
		p, err := parsePolicy([]byte(sdk.PluginConfig()))
		if err != nil {
			return sdk.RequestResult{}, fmt.Errorf("tool_governor: invalid configuration: %w", err)
		}
		out, changed, err := applyPolicy(req, p)
		if err != nil {
			return sdk.RequestResult{}, err
		}
		if !changed {
			return sdk.PassRequest(), nil
		}
		return sdk.ReplaceRequest(out), nil
	})
}

func parsePolicy(data []byte) (policy, error) {
	if len(data) == 0 {
		data = []byte(`{}`)
	}
	raw, err := strictObject(data, map[string]struct{}{
		"allow": {}, "deny": {}, "replace": {},
	})
	if err != nil {
		return policy{}, err
	}
	p := policy{
		allow:   map[string]struct{}{},
		deny:    map[string]struct{}{},
		replace: map[string]replacement{},
	}
	if value, ok := raw["allow"]; ok {
		p.allowPresent = true
		p.allow, err = decodeNames(value, "allow")
		if err != nil {
			return policy{}, err
		}
	}
	if value, ok := raw["deny"]; ok {
		p.deny, err = decodeNames(value, "deny")
		if err != nil {
			return policy{}, err
		}
	}
	for name := range p.allow {
		if _, denied := p.deny[name]; denied {
			return policy{}, fmt.Errorf("tool %q appears in both allow and deny", name)
		}
	}
	if value, ok := raw["replace"]; ok {
		p.replace, err = decodeReplacements(value)
		if err != nil {
			return policy{}, err
		}
	}
	return p, nil
}

func strictObject(data []byte, known map[string]struct{}) (map[string]json.RawMessage, error) {
	raw, err := dynamicObject(data)
	if err != nil {
		return nil, err
	}
	for name, value := range raw {
		if _, ok := known[name]; !ok {
			return nil, fmt.Errorf("unknown member %q", name)
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, fmt.Errorf("member %q must not be null", name)
		}
	}
	return raw, nil
}

func dynamicObject(data []byte) (map[string]json.RawMessage, error) {
	if err := jsontext.Validate(data); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	var raw map[string]json.RawMessage
	if err := dec.Decode(&raw); err != nil || raw == nil {
		return nil, fmt.Errorf("must be a JSON object")
	}
	if err := expectEOF(dec); err != nil {
		return nil, err
	}
	return raw, nil
}

func decodeNames(data []byte, field string) (map[string]struct{}, error) {
	var names []string
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&names); err != nil || names == nil {
		return nil, fmt.Errorf("%s must be an array", field)
	}
	if err := expectEOF(dec); err != nil {
		return nil, fmt.Errorf("%s: %w", field, err)
	}
	out := make(map[string]struct{}, len(names))
	for _, name := range names {
		if name == "" {
			return nil, fmt.Errorf("%s contains an empty tool name", field)
		}
		if _, duplicate := out[name]; duplicate {
			return nil, fmt.Errorf("%s contains duplicate tool %q", field, name)
		}
		out[name] = struct{}{}
	}
	return out, nil
}

func decodeReplacements(data []byte) (map[string]replacement, error) {
	raw, err := dynamicObject(data)
	if err != nil {
		return nil, fmt.Errorf("replace: %w", err)
	}
	out := make(map[string]replacement, len(raw))
	for name, encoded := range raw {
		if name == "" {
			return nil, fmt.Errorf("replace contains an empty tool name")
		}
		members, err := strictObject(encoded, map[string]struct{}{
			"description": {}, "parameters": {}, "strict": {},
		})
		if err != nil {
			return nil, fmt.Errorf("replacement for %q: %w", name, err)
		}
		if len(members) == 0 {
			return nil, fmt.Errorf("replacement for %q must change at least one field", name)
		}
		var r replacement
		if value, ok := members["description"]; ok {
			var description string
			if err := json.Unmarshal(value, &description); err != nil {
				return nil, fmt.Errorf("replacement for %q: description must be a string", name)
			}
			r.description = &description
		}
		if value, ok := members["strict"]; ok {
			var strict bool
			if err := json.Unmarshal(value, &strict); err != nil {
				return nil, fmt.Errorf("replacement for %q: strict must be a boolean", name)
			}
			r.strict = &strict
		}
		if value, ok := members["parameters"]; ok {
			if err := validateJSONObject(value); err != nil {
				return nil, fmt.Errorf("replacement for %q: parameters: %w", name, err)
			}
			r.parameters = slices.Clone(value)
			r.hasParams = true
		}
		out[name] = r
	}
	return out, nil
}

func validateJSONObject(data []byte) error {
	if err := jsontext.Validate(data); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var value map[string]any
	if err := dec.Decode(&value); err != nil || value == nil {
		return fmt.Errorf("must be a JSON object")
	}
	return expectEOF(dec)
}

func expectEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON after the value")
		}
		return err
	}
	return nil
}

func applyPolicy(req *pbv2.ChatRequest, p policy) (*pbv2.ChatRequest, bool, error) {
	if req == nil {
		return nil, false, fmt.Errorf("tool_governor: nil request")
	}
	seen := make(map[string]struct{}, len(req.Tools))
	for i, tool := range req.Tools {
		if tool == nil {
			return nil, false, fmt.Errorf("tool_governor: tools[%d] is nil", i)
		}
		if _, duplicate := seen[tool.Name]; duplicate {
			return nil, false, fmt.Errorf("tool_governor: duplicate tool definition %q", tool.Name)
		}
		seen[tool.Name] = struct{}{}
	}

	// Decide from the validated immutable input first. The default/no-op policy
	// must not clone a potentially multi-megabyte conversation just to discover
	// that its tiny tool-definition section is unchanged.
	keep := make([]bool, len(req.Tools))
	changed := false
	for i, tool := range req.Tools {
		_, allowed := p.allow[tool.Name]
		_, denied := p.deny[tool.Name]
		if (p.allowPresent && !allowed) || denied {
			changed = true
			continue
		}
		keep[i] = true
		if replacement, ok := p.replace[tool.Name]; ok {
			changed = changed || replacement.description != nil && tool.Description != *replacement.description ||
				replacement.hasParams && !bytes.Equal(tool.ParametersJson, replacement.parameters) ||
				replacement.strict != nil && tool.Strict != *replacement.strict
		}
	}
	if !changed {
		return req, false, nil
	}

	out := proto.Clone(req).(*pbv2.ChatRequest)
	retained := make([]*pbv2.ToolDef, 0, len(out.Tools))
	for i, tool := range out.Tools {
		if !keep[i] {
			continue
		}
		if replacement, ok := p.replace[tool.Name]; ok {
			if replacement.description != nil {
				tool.Description = *replacement.description
			}
			if replacement.hasParams {
				tool.ParametersJson = slices.Clone(replacement.parameters)
			}
			if replacement.strict != nil {
				tool.Strict = *replacement.strict
			}
		}
		retained = append(retained, tool)
	}
	out.Tools = retained
	return out, true, nil
}
