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
	"unicode/utf8"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/pb/v1/jsontext"
	"google.golang.org/protobuf/proto"
)

const maximumShadowTimeoutMS = 1500

// Shadow mode is a separate, non-routing policy. It measures whether a
// conversation would benefit from a later ladder step without silently
// switching the model the harness believes it is using.
type shadowPolicy struct {
	Mode         string                  `json:"mode"`
	Ladders      map[string]shadowLadder `json:"ladders"`
	Classifier   shadowClassifier        `json:"classifier"`
	Triggers     shadowTriggers          `json:"triggers"`
	Escalation   shadowEscalation        `json:"escalation"`
	ManageEffort bool                    `json:"manage_effort"`
}

type shadowLadder struct {
	Start string       `json:"start"`
	Steps []shadowStep `json:"steps"`
}

type shadowStep struct {
	ID          string         `json:"id"`
	Description string         `json:"description"`
	Model       string         `json:"model"`
	Effort      string         `json:"effort,omitempty"`
	Aliases     []string       `json:"aliases,omitempty"`
	Pricing     *shadowPricing `json:"pricing,omitempty"`
}

type shadowPricing struct {
	Input      *float64 `json:"input"`
	Output     *float64 `json:"output"`
	CacheRead  *float64 `json:"cache_read"`
	CacheWrite *float64 `json:"cache_write"`
}

type shadowEscalation struct {
	MaxModelSwitches               int     `json:"max_model_switches"`
	MaxSwitchCostUSD               float64 `json:"max_switch_cost_usd"`
	SuggestDeescalation            bool    `json:"suggest_deescalation"`
	MinUserTurnsBeforeDeescalation int     `json:"min_user_turns_before_deescalation"`
}

type shadowClassifier struct {
	Enabled           bool     `json:"enabled"`
	DecisionModel     string   `json:"decision_model"`
	Question          string   `json:"question"`
	Inputs            string   `json:"inputs"`
	MinimumConfidence *float64 `json:"minimum_confidence"`
	MaxStateBytes     int      `json:"max_state_bytes"`
	TimeoutMS         uint32   `json:"timeout_ms"`
	Authentication    string   `json:"authentication"`
}

type shadowTriggers struct {
	ToolErrorWindow     int `json:"tool_error_window"`
	ToolErrorThreshold  int `json:"tool_error_threshold"`
	RetryStreak         int `json:"retry_streak"`
	RequestsPerUserTurn int `json:"requests_per_user_turn"`
	MaxTokensFinishes   int `json:"max_tokens_finishes"`
	ReevaluateUserTurns int `json:"reevaluate_every_user_turns"`
	SuggestionCooldown  int `json:"suggestion_cooldown_user_turns"`
}

type shadowState struct {
	PolicyHash         string         `json:"policy_hash"`
	Provider           string         `json:"provider"`
	Step               string         `json:"step"`
	LastClientModel    string         `json:"last_client_model"`
	OffLadder          bool           `json:"off_ladder"`
	LastUserTurnKey    string         `json:"last_user_turn_key"`
	UserTurns          int            `json:"user_turns"`
	LastEvaluation     int            `json:"last_evaluation"`
	RecentErrors       []bool         `json:"recent_errors"`
	LastResultID       string         `json:"last_result_id"`
	LastSuggestion     string         `json:"last_suggestion"`
	SuggestedAtTurn    int            `json:"suggested_at_turn"`
	ModelSwitches      int            `json:"model_switches"`
	ContextTokens      int64          `json:"context_tokens"`
	AvgOutputTokens    float64        `json:"avg_output_tokens"`
	ResponseCount      int            `json:"response_count"`
	MaxTokensFinishes  int            `json:"max_tokens_finishes"`
	RequestsPerTurn    int            `json:"requests_per_turn"`
	RetryStreak        int            `json:"retry_streak"`
	LastTurnRequests   int            `json:"last_turn_requests"`
	LastTurnRetries    int            `json:"last_turn_retries"`
	LastTurnMaxTokens  int            `json:"last_turn_max_tokens"`
	AvgRequestsPerTurn float64        `json:"avg_requests_per_turn"`
	CompletedTurns     int            `json:"completed_turns"`
	PendingSuggestion  *pendingAdvice `json:"pending_suggestion,omitempty"`
	History            []routeHistory `json:"history,omitempty"`
	ActiveRoute        string         `json:"active_route,omitempty"`
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
	if (policy.Mode != "shadow" && policy.Mode != "advise" && policy.Mode != "confirm" && policy.Mode != "auto") || len(policy.Ladders) == 0 || len(policy.Ladders) > maximumRoutes {
		return policy, "", errors.New("router policy needs shadow, advise, confirm or auto mode and 1-32 provider ladders")
	}
	for provider, ladder := range policy.Ladders {
		if !choiceIDPattern.MatchString(provider) || len(ladder.Steps) < 2 || len(ladder.Steps) > maximumRoutes {
			return policy, "", fmt.Errorf("invalid ladder for provider %q", provider)
		}
		ids := make(map[string]bool, len(ladder.Steps))
		coordinates := make(map[string]bool, len(ladder.Steps))
		modelAliases := make(map[string]bool, len(ladder.Steps))
		for _, step := range ladder.Steps {
			if !choiceIDPattern.MatchString(step.ID) || step.ID == "hold" || ids[step.ID] || strings.TrimSpace(step.Model) == "" ||
				step.Model != strings.TrimSpace(step.Model) || strings.TrimSpace(step.Description) == "" ||
				utf8.RuneCountInString(step.Model) > maximumModelBytes || utf8.RuneCountInString(step.Description) > 1000 {
				return policy, "", fmt.Errorf("invalid or repeated ladder step for provider %q", provider)
			}
			if step.Effort != "" {
				if !policy.ManageEffort || !validStepEffort(step.Effort) {
					return policy, "", fmt.Errorf("step %q needs explicit manage_effort and a valid effort", step.ID)
				}
			}
			if step.Pricing != nil && !validShadowPricing(*step.Pricing) {
				return policy, "", fmt.Errorf("step %q has invalid pricing", step.ID)
			}
			coordinate := step.Model + "\x00" + step.Effort
			if coordinates[coordinate] {
				return policy, "", fmt.Errorf("step %q repeats a model/effort pair", step.ID)
			}
			coordinates[coordinate] = true
			for _, alias := range step.Aliases {
				if alias == "" || alias != strings.TrimSpace(alias) || utf8.RuneCountInString(alias) > maximumModelBytes || modelAliases[alias] {
					return policy, "", fmt.Errorf("step %q has an invalid or repeated model alias", step.ID)
				}
				modelAliases[alias] = true
			}
			ids[step.ID] = true
		}
		if !ids[ladder.Start] {
			return policy, "", fmt.Errorf("start step %q is absent from provider %q", ladder.Start, provider)
		}
	}
	if policy.Escalation.MaxModelSwitches == 0 {
		policy.Escalation.MaxModelSwitches = 1
	}
	if policy.Escalation.MaxSwitchCostUSD == 0 {
		policy.Escalation.MaxSwitchCostUSD = 0.50
	}
	if policy.Escalation.MinUserTurnsBeforeDeescalation == 0 {
		policy.Escalation.MinUserTurnsBeforeDeescalation = 3
	}
	if policy.Escalation.MaxModelSwitches < 0 || policy.Escalation.MaxModelSwitches > 20 ||
		math.IsNaN(policy.Escalation.MaxSwitchCostUSD) || math.IsInf(policy.Escalation.MaxSwitchCostUSD, 0) ||
		policy.Escalation.MaxSwitchCostUSD < 0 || policy.Escalation.MaxSwitchCostUSD > 100 ||
		policy.Escalation.MinUserTurnsBeforeDeescalation < 1 || policy.Escalation.MinUserTurnsBeforeDeescalation > 100 {
		return policy, "", errors.New("invalid escalation limits")
	}
	if policy.Triggers.ToolErrorWindow == 0 {
		policy.Triggers.ToolErrorWindow = 6
	}
	if policy.Triggers.ToolErrorThreshold == 0 {
		policy.Triggers.ToolErrorThreshold = 3
	}
	if policy.Triggers.RetryStreak == 0 {
		policy.Triggers.RetryStreak = 3
	}
	if policy.Triggers.RequestsPerUserTurn == 0 {
		policy.Triggers.RequestsPerUserTurn = 25
	}
	if policy.Triggers.MaxTokensFinishes == 0 {
		policy.Triggers.MaxTokensFinishes = 2
	}
	if policy.Triggers.ReevaluateUserTurns == 0 {
		policy.Triggers.ReevaluateUserTurns = 3
	}
	if policy.Triggers.SuggestionCooldown == 0 {
		policy.Triggers.SuggestionCooldown = 3
	}
	if policy.Triggers.ToolErrorWindow < 1 || policy.Triggers.ToolErrorWindow > 64 ||
		policy.Triggers.ToolErrorThreshold < 1 || policy.Triggers.ToolErrorThreshold > policy.Triggers.ToolErrorWindow ||
		policy.Triggers.RetryStreak < 1 || policy.Triggers.RetryStreak > 100 ||
		policy.Triggers.RequestsPerUserTurn < 1 || policy.Triggers.RequestsPerUserTurn > 1000 ||
		policy.Triggers.MaxTokensFinishes < 1 || policy.Triggers.MaxTokensFinishes > 100 ||
		policy.Triggers.ReevaluateUserTurns < 1 || policy.Triggers.ReevaluateUserTurns > 100 ||
		policy.Triggers.SuggestionCooldown < 1 || policy.Triggers.SuggestionCooldown > 100 {
		return policy, "", errors.New("invalid shadow triggers")
	}
	if policy.Classifier.Inputs != "" && policy.Classifier.Inputs != "user_turn+signals" && policy.Classifier.Inputs != "signals" {
		return policy, "", errors.New("invalid classifier inputs")
	}
	if policy.Classifier.Enabled {
		if policy.Classifier.Inputs == "" {
			policy.Classifier.Inputs = "user_turn+signals"
		}
		if policy.Classifier.MinimumConfidence == nil {
			defaultConfidence := 0.8
			policy.Classifier.MinimumConfidence = &defaultConfidence
		}
		if policy.Classifier.MaxStateBytes == 0 {
			policy.Classifier.MaxStateBytes = defaultStateBytes
		}
		if policy.Classifier.TimeoutMS == 0 {
			policy.Classifier.TimeoutMS = maximumShadowTimeoutMS
		}
		if policy.Classifier.Authentication == "" {
			policy.Classifier.Authentication = "none"
		}
		if policy.Classifier.DecisionModel == "" || strings.TrimSpace(policy.Classifier.DecisionModel) != policy.Classifier.DecisionModel ||
			strings.TrimSpace(policy.Classifier.Question) == "" ||
			utf8.RuneCountInString(policy.Classifier.DecisionModel) > maximumModelBytes ||
			utf8.RuneCountInString(policy.Classifier.Question) > 2000 ||
			policy.Classifier.MaxStateBytes < 256 || policy.Classifier.MaxStateBytes > maximumStateBytes ||
			policy.Classifier.TimeoutMS < 100 || policy.Classifier.TimeoutMS > maximumShadowTimeoutMS ||
			(policy.Classifier.Authentication != "none" && policy.Classifier.Authentication != "bearer") ||
			math.IsNaN(*policy.Classifier.MinimumConfidence) || math.IsInf(*policy.Classifier.MinimumConfidence, 0) || *policy.Classifier.MinimumConfidence < 0 || *policy.Classifier.MinimumConfidence > 1 {
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

func validStepEffort(value string) bool {
	switch value {
	case "minimal", "low", "medium", "high", "xhigh", "max":
		return true
	default:
		return false
	}
}

func validShadowPricing(value shadowPricing) bool {
	for _, price := range []*float64{value.Input, value.Output, value.CacheRead, value.CacheWrite} {
		if price != nil && (math.IsNaN(*price) || math.IsInf(*price, 0) || *price < 0) {
			return false
		}
	}
	return true
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
	RecentToolErrors   int    `json:"recent_tool_errors"`
	UserTurns          int    `json:"user_turns"`
	LastTurnRequests   int    `json:"last_turn_requests"`
	LastTurnRetries    int    `json:"last_turn_retries"`
	LastTurnMaxTokens  int    `json:"last_turn_max_tokens"`
	AvgRequestsPerTurn int    `json:"avg_requests_per_turn"`
	ContextBucket      string `json:"context_bucket"`
}

func boundedShadowSignals(state shadowState, recentErrors int) shadowSignalFacts {
	capCount := func(value int) int {
		if value < 0 {
			return 0
		}
		if value > 1000 {
			return 1000
		}
		return value
	}
	bucket := "unknown"
	switch tokens := state.ContextTokens; {
	case tokens > 128000:
		bucket = "128k+"
	case tokens > 32000:
		bucket = "32k-128k"
	case tokens > 8000:
		bucket = "8k-32k"
	case tokens > 2000:
		bucket = "2k-8k"
	case tokens > 0:
		bucket = "0-2k"
	}
	average := 0
	if !math.IsNaN(state.AvgRequestsPerTurn) && !math.IsInf(state.AvgRequestsPerTurn, 0) {
		average = capCount(int(math.Round(state.AvgRequestsPerTurn)))
	}
	return shadowSignalFacts{
		RecentToolErrors: capCount(recentErrors), UserTurns: capCount(state.UserTurns),
		LastTurnRequests: capCount(state.LastTurnRequests), LastTurnRetries: capCount(state.LastTurnRetries),
		LastTurnMaxTokens: capCount(state.LastTurnMaxTokens), AvgRequestsPerTurn: average,
		ContextBucket: bucket,
	}
}

func shadowNewResultCandidates(req *pbv1.ChatRequest) []shadowResult {
	var results []shadowResult
	for _, msg := range req.Messages {
		for _, result := range sdk.ToolResults(msg) {
			// Call identity, not content, deduplicates history replay. Two
			// independent failures with identical text remain distinct.
			identity := result.ToolCallId
			if identity == "" {
				// Some formats have no call ID. Position distinguishes repeated
				// same-name calls while a stable replay prefix is present.
				identity = fmt.Sprintf("position:%d", len(results))
			}
			sum := sha256.Sum256([]byte(identity + "\x00" + result.ToolName))
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
		for _, alias := range step.Aliases {
			if alias == model {
				return step.ID
			}
		}
	}
	return ""
}

func shadowStateKey(conversationID string, req *pbv1.ChatRequest) string {
	// A harness session can contain title generation, side requests and
	// subagents. Use only stable text in their leading system messages and
	// first user message. Cache markers and signatures move between turns.
	if req == nil {
		return ""
	}
	type rootMessage struct {
		Role string   `json:"role"`
		Text []string `json:"text"`
	}
	var root []rootMessage
	seenUser := false
	for _, msg := range req.Messages {
		if msg == nil {
			continue
		}
		if msg.Role != "system" && msg.Role != "user" {
			continue
		}
		part := rootMessage{Role: msg.Role}
		for _, block := range msg.Blocks {
			if block != nil && block.GetText() != nil {
				part.Text = append(part.Text, block.GetText().Text)
			}
		}
		root = append(root, part)
		if msg.Role == "user" {
			seenUser = true
			break
		}
	}
	if !seenUser {
		return ""
	}
	encoded, _ := json.Marshal(root)
	sum := sha256.Sum256(append(append([]byte(conversationID), 0), encoded...))
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
	key := shadowStateKey(conversation, req)
	if key == "" {
		emit("shadow_missing_thread_root", "")
		return sdk.PassRequest(), nil
	}
	var classified bool
	var candidate string
	var confidence float64
	var classOK bool
	var pricesResolved bool
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
				emit("shadow_state_corrupt", "")
				found = false
			}
			version = &stored.Version
		}
		fresh := !found || state.PolicyHash != policyHash || state.Provider != provider || shadowStepIndex(ladder, state.Step) < 0
		if fresh {
			state = shadowState{PolicyHash: policyHash, Provider: provider, Step: ladder.Start}
			if observed := shadowStepForModel(ladder, req.Model); observed != "" {
				state.Step = observed
			} else {
				state.OffLadder = true
			}
		}
		turnKey, userTurn := shadowUserTurn(req)
		newTurn := userTurn && turnKey != state.LastUserTurnKey
		if newTurn {
			if state.UserTurns > 0 {
				state.LastTurnRequests = state.RequestsPerTurn
				state.LastTurnRetries = state.RetryStreak
				state.LastTurnMaxTokens = state.MaxTokensFinishes
				state.AvgRequestsPerTurn = (state.AvgRequestsPerTurn*float64(state.CompletedTurns) + float64(state.RequestsPerTurn)) / float64(state.CompletedTurns+1)
				state.CompletedTurns++
			}
			state.UserTurns++
			state.LastUserTurnKey = turnKey
			state.RequestsPerTurn = 1
			state.RetryStreak = 0
			state.MaxTokensFinishes = 0
		} else {
			state.RequestsPerTurn++
			if userTurn {
				state.RetryStreak++
			}
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
		previousStep := state.Step
		state.LastClientModel = req.Model
		if clientModelChanged {
			state.ActiveRoute = ""
			if observed := shadowStepForModel(ladder, req.Model); observed != "" {
				state.Step = observed
				state.OffLadder = false
			} else {
				state.OffLadder = true
			}
		}
		reconcileAdvice(req, ladder, &state, previousStep, clientModelChanged)
		errorCount := 0
		for _, failed := range state.RecentErrors {
			if failed {
				errorCount++
			}
		}
		previousTurnSignal := state.LastTurnRetries >= policy.Triggers.RetryStreak ||
			state.LastTurnRequests >= policy.Triggers.RequestsPerUserTurn ||
			state.LastTurnMaxTokens >= policy.Triggers.MaxTokensFinishes
		shouldEvaluate := newTurn && !state.OffLadder && (fresh || state.UserTurns-state.LastEvaluation >= policy.Triggers.ReevaluateUserTurns ||
			errorCount >= policy.Triggers.ToolErrorThreshold || previousTurnSignal)
		var decision policyDecision
		accepted := newTurn && !state.OffLadder && state.PendingSuggestion != nil && state.PendingSuggestion.Status == "accepted" && state.PendingSuggestion.Via != "harness_switch" && (policy.Mode == "confirm" || policy.Mode == "auto")
		shouldEvaluate = shouldEvaluate || accepted
		if shouldEvaluate {
			state.LastEvaluation = state.UserTurns
			if policy.Classifier.Enabled {
				if !classified {
					candidate, confidence, classOK = shadowClassify(req, policy.Classifier, ladder, boundedShadowSignals(state, errorCount))
					classified = true
					if !classOK {
						emit("shadow_classifier_unavailable", "")
					}
				}
			}
			if !policy.Classifier.Enabled || !classOK || confidence < *policy.Classifier.MinimumConfidence {
				candidate = ""
			}
			contextTokens := state.ContextTokens
			if contextTokens == 0 {
				contextTokens = int64(proto.Size(req) / 4)
			}
			if !pricesResolved {
				ladder = resolveLadderPricing(provider, ladder)
				pricesResolved = true
			}
			decision = Evaluate(policy, ladder, state, policySignals{
				RecentToolErrors: errorCount, RetryStreak: state.LastTurnRetries,
				RequestsPerUserTurn: state.LastTurnRequests, AvgRequestsPerTurn: state.AvgRequestsPerTurn,
				ContextTokens: contextTokens, AvgOutputTokens: state.AvgOutputTokens,
				MaxTokensFinishes: state.LastTurnMaxTokens,
			}, candidate)
			if accepted {
				decision = prepareAcceptance(policy, ladder, &state, contextTokens)
			}
		}
		repeatSuggestion := decision.Target != "" && decision.Target == state.LastSuggestion
		if decision.Target != "" && decision.BlockedBy == "" {
			state.LastSuggestion, state.SuggestedAtTurn = decision.Target, state.UserTurns
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
		if err := sdk.MetaSet("decision_router_state_key", key); err != nil {
			return sdk.RequestResult{}, err
		}
		if clientModelChanged {
			sdk.EmitMetric(metricDecisionName, sdk.MetricCounter, 1, map[string]string{
				"outcome": "user_switch_unprompted", "provider": provider,
			})
		}
		if (fresh || clientModelChanged) && state.OffLadder {
			sdk.EmitMetric(metricDecisionName, sdk.MetricCounter, 1, map[string]string{
				"outcome": "off_ladder", "provider": provider,
			})
		}
		if decision.Target != "" && decision.BlockedBy == "" {
			applyNow := accepted || (policy.Mode == "auto" && shadowStepIndex(ladder, decision.Target) > shadowStepIndex(ladder, state.Step))
			if policy.Mode != "shadow" && !applyNow {
				publishAdvice(key, policyHash, ladder, state, decision, policy.Mode)
			}
			if applyNow {
				via := "auto"
				if accepted {
					via = state.PendingSuggestion.Via
				}
				requestRoute(provider, ladder, state, decision.Target, via, policy.ManageEffort)
				return sdk.PassRequest(), nil
			}
			sdk.EmitMetric(metricDecisionName, sdk.MetricCounter, 1, map[string]string{
				"outcome": "would_suggest", "provider": provider, "from": state.Step, "to": decision.Target,
				"repeat": fmt.Sprintf("%t", repeatSuggestion),
			})
			sdk.EmitMetric("torana_decision_router_switch_cost_usd", sdk.MetricHistogram, decision.RebuildUSD, map[string]string{
				"provider": provider, "from": state.Step, "to": decision.Target,
			})
			sdk.EmitMetric("torana_decision_router_escalation_depth", sdk.MetricHistogram, float64(shadowStepIndex(ladder, decision.Target)), map[string]string{
				"provider": provider,
			})
		} else if decision.BlockedBy != "" {
			sdk.EmitMetric(metricDecisionName, sdk.MetricCounter, 1, map[string]string{
				"outcome": "shadow_blocked", "provider": provider, "reason": decision.BlockedBy,
			})
		} else if shouldEvaluate {
			emit("shadow_hold", state.Step)
		}
		if state.ActiveRoute != "" && (policy.Mode == "confirm" || policy.Mode == "auto") {
			requestRoute(provider, ladder, state, state.ActiveRoute, "continue", policy.ManageEffort)
		}
		return sdk.PassRequest(), nil
	}
	emit("shadow_state_conflict", "")
	return sdk.PassRequest(), nil
}

func shadowClassify(req *pbv1.ChatRequest, classifier shadowClassifier, ladder shadowLadder, signals shadowSignalFacts) (string, float64, bool) {
	routes := make(map[string]route, len(ladder.Steps)+1)
	for _, step := range ladder.Steps {
		routes[step.ID] = route{Description: step.Description}
	}
	routes["hold"] = route{Description: "The current model remains suitable for this turn"}
	state := requestState{
		Signals: &signals,
	}
	if classifier.Inputs != "signals" {
		text := latestUserText(req)
		if strings.TrimSpace(text) == "" {
			return "", 0, false
		}
		state.LatestUserTurn = truncateUTF8(text, classifier.MaxStateBytes)
	}
	return decide(config{
		DecisionModel: classifier.DecisionModel, Question: classifier.Question,
		Routes: routes, TimeoutMS: classifier.TimeoutMS, Authentication: classifier.Authentication,
	}, state)
}
