package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/strictjson"
)

type sessionToolState struct {
	Allowed        []string `json:"allowed,omitempty"`
	Previous       []string `json:"previous,omitempty"`
	Policy         string   `json:"policy,omitempty"`
	PreviousPolicy string   `json:"previous_policy,omitempty"`
	LastChange     string   `json:"last_change,omitempty"`
	LastUndo       string   `json:"last_undo,omitempty"`
	LastTool       string   `json:"last_tool,omitempty"`
}

func sessionToolKey(conversation string) string {
	sum := sha256.Sum256([]byte("torana/tool-governor/session-tools/v1\x00" + conversation))
	return "session-tools/v1/" + hex.EncodeToString(sum[:])
}

func callFingerprint(callID string) string {
	sum := sha256.Sum256([]byte("torana/tool-governor/change/v1\x00" + callID))
	return hex.EncodeToString(sum[:])
}

func policyFingerprint(raw string) string {
	sum := sha256.Sum256([]byte("torana/tool-governor/policy/v1\x00" + raw))
	return hex.EncodeToString(sum[:])
}

func decodeSessionToolState(raw string) (sessionToolState, error) {
	object, err := strictjson.DecodeObject([]byte(raw))
	if err != nil || object == nil {
		return sessionToolState{}, errors.New("tool_governor: invalid session tool state")
	}
	for key := range object {
		if key != "allowed" && key != "previous" && key != "policy" && key != "previous_policy" && key != "last_change" && key != "last_undo" && key != "last_tool" {
			return sessionToolState{}, errors.New("tool_governor: invalid session tool state")
		}
	}
	var state sessionToolState
	if json.Unmarshal([]byte(raw), &state) != nil || !validStoredNames(state.Allowed) || !validStoredNames(state.Previous) ||
		!validFingerprint(state.Policy) || !validFingerprint(state.PreviousPolicy) ||
		!validFingerprint(state.LastChange) || !validFingerprint(state.LastUndo) ||
		(state.LastTool != "" && !validToolName(state.LastTool)) {
		return sessionToolState{}, errors.New("tool_governor: invalid session tool state")
	}
	return state, nil
}

func validStoredNames(names []string) bool {
	if len(names) > 128 {
		return false
	}
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if !validToolName(name) {
			return false
		}
		if _, duplicate := seen[name]; duplicate {
			return false
		}
		seen[name] = struct{}{}
	}
	return true
}

func validToolName(name string) bool {
	return name != "" && len(name) <= 256 && utf8.ValidString(name) &&
		strings.TrimSpace(name) == name && !containsControl(name)
}

func validFingerprint(value string) bool {
	if value == "" {
		return true
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func containsControl(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func allowedForExecution(ctx context.Context, p policy, rawPolicy string) (map[string]struct{}, error) {
	if !p.allowPresent && len(p.deny) == 0 {
		return nil, nil
	}
	execution := sdk.Execution(ctx)
	if execution == nil || execution.GetConversationId() == "" {
		return nil, nil
	}
	raw, found, err := sdk.StateGet(sessionToolKey(execution.GetConversationId()))
	if err != nil || !found {
		return nil, err
	}
	state, err := decodeSessionToolState(raw)
	if err != nil {
		return nil, err
	}
	if state.Policy != policyFingerprint(rawPolicy) {
		return nil, nil
	}
	allowed := make(map[string]struct{}, len(state.Allowed))
	for _, name := range state.Allowed {
		allowed[name] = struct{}{}
	}
	return allowed, nil
}

func changeSessionTool(req *pb.HttpRequest, undo bool) (sdk.HTTPResult, error) {
	binding, present, err := sdk.HTTPConversation(req)
	if err != nil {
		return sdk.PassHTTP(), err
	}
	if !present || !binding.Bound {
		return jsonHTTP(409, map[string]any{"error": "unbound_conversation"}), nil
	}
	object, err := strictjson.DecodeObject(req.Body)
	if err != nil || object == nil || len(object) != 1 {
		return jsonHTTP(400, map[string]any{"error": "invalid_input"}), nil
	}
	if _, ok := object["tool"]; !ok {
		return jsonHTTP(400, map[string]any{"error": "invalid_input"}), nil
	}
	var input struct {
		Tool string `json:"tool"`
	}
	if json.Unmarshal(req.Body, &input) != nil || !validToolName(input.Tool) {
		return jsonHTTP(400, map[string]any{"error": "invalid_tool"}), nil
	}
	tool := input.Tool
	currentPolicy := ""
	if !undo {
		rawPolicy, err := sdk.PluginConfig()
		if err != nil {
			return sdk.PassHTTP(), err
		}
		configured, err := configuredPolicy(rawPolicy)
		if err != nil {
			return sdk.PassHTTP(), err
		}
		if _, denied := configured.deny[tool]; denied {
			return jsonHTTP(409, map[string]any{"error": "denied_by_policy"}), nil
		}
		_, alreadyAllowed := configured.allow[tool]
		if !configured.allowPresent || alreadyAllowed {
			return jsonHTTP(409, map[string]any{"error": "already_allowed_by_policy"}), nil
		}
		currentPolicy = policyFingerprint(rawPolicy)
	}

	key := sessionToolKey(binding.ConversationID)
	change := callFingerprint(binding.CallID)
	for attempt := 0; attempt < 3; attempt++ {
		stored, found, err := sdk.StateGetVersioned(key)
		if err != nil {
			return sdk.PassHTTP(), err
		}
		var state sessionToolState
		var version *string
		if found {
			state, err = decodeSessionToolState(stored.Value)
			if err != nil {
				return sdk.PassHTTP(), err
			}
			version = &stored.Version
		}
		if undo {
			if state.LastUndo == change {
				if state.LastTool != tool {
					return jsonHTTP(409, map[string]any{"error": "call_conflict"}), nil
				}
				return jsonHTTP(200, map[string]any{"status": "undone", "tool": tool}), nil
			}
			if state.LastChange != change || state.LastTool != tool {
				return jsonHTTP(409, map[string]any{"error": "change_superseded"}), nil
			}
			state.Allowed = slices.Clone(state.Previous)
			state.Policy = state.PreviousPolicy
			state.Previous = nil
			state.PreviousPolicy = ""
			state.LastChange = ""
			state.LastUndo = change
		} else {
			if state.LastUndo == change {
				if state.LastTool != tool {
					return jsonHTTP(409, map[string]any{"error": "call_conflict"}), nil
				}
				return jsonHTTP(409, map[string]any{"error": "change_already_undone"}), nil
			}
			if state.LastChange == change {
				if state.LastTool != tool {
					return jsonHTTP(409, map[string]any{"error": "call_conflict"}), nil
				}
				return jsonHTTP(200, map[string]any{"status": "allowed", "tool": tool}), nil
			}
			if state.Policy != currentPolicy {
				state.Allowed = nil
				state.Policy = currentPolicy
			}
			state.Previous = slices.Clone(state.Allowed)
			state.PreviousPolicy = state.Policy
			if !slices.Contains(state.Allowed, tool) {
				if len(state.Allowed) >= 128 {
					return jsonHTTP(409, map[string]any{"error": "session_limit"}), nil
				}
				state.Allowed = append(state.Allowed, tool)
			}
			state.LastChange = change
			state.LastUndo = ""
			state.LastTool = tool
			state.Policy = currentPolicy
		}
		encoded, err := json.Marshal(state)
		if err != nil {
			return sdk.PassHTTP(), err
		}
		result, err := sdk.StateCompareAndSet(key, string(encoded), version)
		if err != nil {
			return sdk.PassHTTP(), err
		}
		if result.GetApplied() {
			status := "allowed"
			if undo {
				status = "undone"
			}
			return jsonHTTP(200, map[string]any{"status": status, "tool": tool}), nil
		}
	}
	return jsonHTTP(409, map[string]any{"error": "concurrent_change"}), nil
}

func jsonHTTP(status int32, value any) sdk.HTTPResult {
	body, _ := json.Marshal(value)
	return sdk.ServeHTTP(&pb.HttpResponse{Status: status, HeadersJson: []byte(`{"Content-Type":["application/json"]}`), Body: body})
}
