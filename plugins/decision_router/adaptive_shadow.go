package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/pb/v1/jsontext"
)

// Shadow mode is a separate, non-routing policy. It measures whether a
// conversation would benefit from a later ladder step without silently
// switching the model the harness believes it is using.
type shadowPolicy struct {
	Mode       string                  `json:"mode"`
	Ladders    map[string]shadowLadder `json:"ladders"`
	Classifier shadowClassifier        `json:"classifier"`
	Triggers   shadowTriggers          `json:"triggers"`
}

type shadowLadder struct {
	Start string       `json:"start"`
	Steps []shadowStep `json:"steps"`
}

type shadowStep struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Model       string `json:"model"`
}

type shadowClassifier struct {
	Enabled           bool    `json:"enabled"`
	DecisionModel     string  `json:"decision_model"`
	Question          string  `json:"question"`
	MinimumConfidence float64 `json:"minimum_confidence"`
	MaxStateBytes     int     `json:"max_state_bytes"`
	TimeoutMS         uint32  `json:"timeout_ms"`
	Authentication    string  `json:"authentication"`
}

type shadowTriggers struct {
	ToolErrorWindow     int `json:"tool_error_window"`
	ToolErrorThreshold  int `json:"tool_error_threshold"`
	ReevaluateUserTurns int `json:"reevaluate_every_user_turns"`
	SuggestionCooldown  int `json:"suggestion_cooldown_user_turns"`
}

type shadowState struct {
	PolicyHash      string `json:"policy_hash"`
	Provider        string `json:"provider"`
	Step            string `json:"step"`
	LastClientModel string `json:"last_client_model"`
	LastUserTurnKey string `json:"last_user_turn_key"`
	UserTurns       int    `json:"user_turns"`
	LastEvaluation  int    `json:"last_evaluation"`
	RecentErrors    []bool `json:"recent_errors"`
	LastResultID    string `json:"last_result_id"`
	LastSuggestion  string `json:"last_suggestion"`
	SuggestedAtTurn int    `json:"suggested_at_turn"`
}

func shadowPolicySelected(raw string) bool {
	var header struct {
		Mode string `json:"mode"`
	}
	return json.Unmarshal([]byte(raw), &header) == nil && header.Mode == "shadow"
}

func loadShadowPolicy(raw string) (shadowPolicy, string, error) {
	var policy shadowPolicy
	if err := jsontext.Validate([]byte(raw)); err != nil {
		return policy, "", fmt.Errorf("shadow policy: %w", err)
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&policy); err != nil {
		return policy, "", fmt.Errorf("shadow policy: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return policy, "", errors.New("shadow policy: trailing JSON")
	}
	if policy.Mode != "shadow" || len(policy.Ladders) == 0 || len(policy.Ladders) > maximumRoutes {
		return policy, "", errors.New("shadow policy needs mode=shadow and 1-32 provider ladders")
	}
	for provider, ladder := range policy.Ladders {
		if !choiceIDPattern.MatchString(provider) || len(ladder.Steps) < 2 || len(ladder.Steps) > maximumRoutes {
			return policy, "", fmt.Errorf("invalid ladder for provider %q", provider)
		}
		ids := make(map[string]bool, len(ladder.Steps))
		for _, step := range ladder.Steps {
			if !choiceIDPattern.MatchString(step.ID) || step.ID == "hold" || ids[step.ID] || strings.TrimSpace(step.Model) == "" ||
				strings.TrimSpace(step.Description) == "" || len(step.Model) > maximumModelBytes || len(step.Description) > 1000 {
				return policy, "", fmt.Errorf("invalid or repeated ladder step for provider %q", provider)
			}
			ids[step.ID] = true
		}
		if !ids[ladder.Start] {
			return policy, "", fmt.Errorf("start step %q is absent from provider %q", ladder.Start, provider)
		}
	}
	if policy.Triggers.ToolErrorWindow == 0 {
		policy.Triggers.ToolErrorWindow = 6
	}
	if policy.Triggers.ToolErrorThreshold == 0 {
		policy.Triggers.ToolErrorThreshold = 3
	}
	if policy.Triggers.ReevaluateUserTurns == 0 {
		policy.Triggers.ReevaluateUserTurns = 3
	}
	if policy.Triggers.SuggestionCooldown == 0 {
		policy.Triggers.SuggestionCooldown = 3
	}
	if policy.Triggers.ToolErrorWindow < 1 || policy.Triggers.ToolErrorWindow > 64 ||
		policy.Triggers.ToolErrorThreshold < 1 || policy.Triggers.ToolErrorThreshold > policy.Triggers.ToolErrorWindow ||
		policy.Triggers.ReevaluateUserTurns < 1 || policy.Triggers.ReevaluateUserTurns > 100 ||
		policy.Triggers.SuggestionCooldown < 1 || policy.Triggers.SuggestionCooldown > 100 {
		return policy, "", errors.New("invalid shadow triggers")
	}
	if policy.Classifier.Enabled {
		if policy.Classifier.MinimumConfidence == 0 {
			policy.Classifier.MinimumConfidence = 0.8
		}
		if policy.Classifier.MaxStateBytes == 0 {
			policy.Classifier.MaxStateBytes = defaultStateBytes
		}
		if policy.Classifier.TimeoutMS == 0 {
			policy.Classifier.TimeoutMS = defaultTimeoutMS
		}
		if policy.Classifier.Authentication == "" {
			policy.Classifier.Authentication = "none"
		}
		if policy.Classifier.DecisionModel == "" || policy.Classifier.Question == "" ||
			policy.Classifier.MaxStateBytes < 256 || policy.Classifier.MaxStateBytes > maximumStateBytes ||
			policy.Classifier.TimeoutMS < 100 || policy.Classifier.TimeoutMS > maximumTimeoutMS ||
			(policy.Classifier.Authentication != "none" && policy.Classifier.Authentication != "bearer") ||
			math.IsNaN(policy.Classifier.MinimumConfidence) || math.IsInf(policy.Classifier.MinimumConfidence, 0) || policy.Classifier.MinimumConfidence < 0 || policy.Classifier.MinimumConfidence > 1 {
			return policy, "", errors.New("invalid shadow classifier")
		}
	}
	canonical, err := json.Marshal(policy)
	if err != nil {
		return policy, "", err
	}
	sum := sha256.Sum256(canonical)
	return policy, hex.EncodeToString(sum[:]), nil
}

func shadowProvider(req *pbv1.ChatRequest) string {
	var meta struct {
		Provider string `json:"_provider"`
	}
	if req != nil {
		_ = json.Unmarshal(req.ToranaMetaJson, &meta)
	}
	return meta.Provider
}

func shadowUserTurn(req *pbv1.ChatRequest) (string, bool) {
	if req == nil || len(req.Messages) == 0 {
		return "", false
	}
	last := req.Messages[len(req.Messages)-1]
	if last == nil || last.Role != "user" {
		return "", false
	}
	for _, block := range last.Blocks {
		if block != nil && block.GetToolResult() != nil {
			return "", false
		}
	}
	text := latestUserText(req)
	if strings.TrimSpace(text) == "" {
		return "", false
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%s", len(req.Messages), text)))
	return hex.EncodeToString(sum[:]), true
}

type shadowResult struct {
	ID    string
	Error bool
}

type shadowSignalFacts struct {
	RecentToolErrors int `json:"recent_tool_errors"`
	UserTurns        int `json:"user_turns"`
}

func shadowNewResultCandidates(req *pbv1.ChatRequest) []shadowResult {
	var results []shadowResult
	for _, msg := range req.Messages {
		for _, result := range sdk.ToolResults(msg) {
			// Call identity, not content, deduplicates history replay. Two
			// independent failures with identical text remain distinct.
			sum := sha256.Sum256([]byte(result.ToolCallId + "\x00" + result.ToolName))
			results = append(results, shadowResult{
				ID: hex.EncodeToString(sum[:]), Error: result.IsError != nil && *result.IsError,
			})
		}
	}
	return results
}

func shadowStepIndex(ladder shadowLadder, id string) int {
	for i, step := range ladder.Steps {
		if step.ID == id {
			return i
		}
	}
	return -1
}

func shadowStepForModel(ladder shadowLadder, model string) string {
	for _, step := range ladder.Steps {
		if step.Model == model {
			return step.ID
		}
	}
	return ""
}

func shadowStateKey(conversationID string) string {
	sum := sha256.Sum256([]byte(conversationID))
	return "decision/v2/shadow/" + hex.EncodeToString(sum[:])
}

func saveShadowState(key string, state shadowState, version *string) (bool, error) {
	raw, err := json.Marshal(state)
	if err != nil {
		return false, err
	}
	result, err := sdk.StateCompareAndSet(key, string(raw), version)
	if err != nil {
		return false, err
	}
	return result.Applied, nil
}

func runAdaptiveShadow(req *pbv1.ChatRequest, raw string) (sdk.RequestResult, error) {
	policy, policyHash, err := loadShadowPolicy(raw)
	if err != nil {
		return sdk.RequestResult{}, err
	}
	provider := shadowProvider(req)
	ladder, ok := policy.Ladders[provider]
	if !ok {
		emit("shadow_no_ladder", "")
		return sdk.PassRequest(), nil
	}
	conversation := conversationID(req)
	if conversation == "" {
		emit("shadow_missing_conversation", "")
		return sdk.PassRequest(), nil
	}
	key := shadowStateKey(conversation)
	for attempt := 0; attempt < 3; attempt++ {
		stored, found, err := sdk.StateGetVersioned(key)
		if err != nil {
			emit("shadow_state_read_failed", "")
			return sdk.PassRequest(), nil
		}
		var state shadowState
		var version *string
		if found {
			if err := json.Unmarshal([]byte(stored.Value), &state); err != nil {
				return sdk.RequestResult{}, fmt.Errorf("shadow state: %w", err)
			}
			version = &stored.Version
		}
		fresh := !found || state.PolicyHash != policyHash || state.Provider != provider || shadowStepIndex(ladder, state.Step) < 0
		if fresh {
			state = shadowState{PolicyHash: policyHash, Provider: provider, Step: ladder.Start}
			if observed := shadowStepForModel(ladder, req.Model); observed != "" {
				state.Step = observed
			}
		}
		turnKey, userTurn := shadowUserTurn(req)
		newTurn := userTurn && turnKey != state.LastUserTurnKey
		if newTurn {
			state.UserTurns++
			state.LastUserTurnKey = turnKey
		}
		// An initial observation establishes the replay baseline; historical
		// failures must not masquerade as fresh failures on plugin activation.
		results := shadowNewResultCandidates(req)
		if len(results) > 0 {
			start := len(results) // no anchor: historical/compacted batch, baseline it
			if !fresh && state.LastResultID != "" {
				for i := len(results) - 1; i >= 0; i-- {
					if results[i].ID == state.LastResultID {
						start = i + 1
						break
					}
				}
			}
			for _, result := range results[start:] {
				state.RecentErrors = append(state.RecentErrors, result.Error)
			}
			state.LastResultID = results[len(results)-1].ID
		}
		if len(state.RecentErrors) > policy.Triggers.ToolErrorWindow {
			state.RecentErrors = state.RecentErrors[len(state.RecentErrors)-policy.Triggers.ToolErrorWindow:]
		}
		clientModelChanged := !fresh && state.LastClientModel != "" && state.LastClientModel != req.Model
		state.LastClientModel = req.Model
		if clientModelChanged {
			if observed := shadowStepForModel(ladder, req.Model); observed != "" {
				state.Step = observed
			}
		}
		target := state.Step
		errorCount := 0
		for _, failed := range state.RecentErrors {
			if failed {
				errorCount++
			}
		}
		shouldEvaluate := newTurn && (fresh || state.UserTurns-state.LastEvaluation >= policy.Triggers.ReevaluateUserTurns ||
			errorCount >= policy.Triggers.ToolErrorThreshold)
		if shouldEvaluate {
			state.LastEvaluation = state.UserTurns
			index := shadowStepIndex(ladder, state.Step)
			if errorCount >= policy.Triggers.ToolErrorThreshold && index+1 < len(ladder.Steps) {
				target = ladder.Steps[index+1].ID
			}
			if policy.Classifier.Enabled {
				candidate, confidence, classified := shadowClassify(req, policy.Classifier, ladder, errorCount, state.UserTurns)
				if classified && confidence >= policy.Classifier.MinimumConfidence && candidate != "hold" {
					want := shadowStepIndex(ladder, candidate)
					if want > index && index+1 < len(ladder.Steps) {
						target = ladder.Steps[index+1].ID
					}
				}
			}
		}
		if target != state.Step && target == state.LastSuggestion &&
			state.UserTurns-state.SuggestedAtTurn < policy.Triggers.SuggestionCooldown {
			target = state.Step
		}
		if target != state.Step {
			state.LastSuggestion, state.SuggestedAtTurn = target, state.UserTurns
			state.RecentErrors = nil // a new failure batch is needed to repeat this signal
		}
		applied, err := saveShadowState(key, state, version)
		if err != nil {
			emit("shadow_state_write_failed", "")
			return sdk.PassRequest(), nil
		}
		if !applied {
			continue
		}
		if clientModelChanged {
			sdk.EmitMetric(metricDecisionName, sdk.MetricCounter, 1, map[string]string{
				"outcome": "user_switch_unprompted", "provider": provider,
			})
		}
		if target != state.Step {
			sdk.EmitMetric(metricDecisionName, sdk.MetricCounter, 1, map[string]string{
				"outcome": "would_suggest", "provider": provider, "from": state.Step, "to": target,
			})
		} else if shouldEvaluate {
			emit("shadow_hold", state.Step)
		}
		return sdk.PassRequest(), nil
	}
	emit("shadow_state_conflict", "")
	return sdk.PassRequest(), nil
}

func shadowClassify(req *pbv1.ChatRequest, classifier shadowClassifier, ladder shadowLadder, recentErrors, userTurns int) (string, float64, bool) {
	routes := make(map[string]route, len(ladder.Steps)+1)
	for _, step := range ladder.Steps {
		routes[step.ID] = route{Description: step.Description}
	}
	routes["hold"] = route{Description: "The current model remains suitable for this turn"}
	text := latestUserText(req)
	if strings.TrimSpace(text) == "" {
		return "", 0, false
	}
	state := requestState{
		LatestUserTurn: truncateUTF8(text, classifier.MaxStateBytes),
		Signals:        &shadowSignalFacts{RecentToolErrors: recentErrors, UserTurns: userTurns},
	}
	return decide(config{
		DecisionModel: classifier.DecisionModel, Question: classifier.Question,
		Routes: routes, TimeoutMS: classifier.TimeoutMS, Authentication: classifier.Authentication,
	}, state)
}
