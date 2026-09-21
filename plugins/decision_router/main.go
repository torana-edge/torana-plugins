// decision_router asks an operator-bound System One service for one closed-set
// routing decision. It never sends conversation history: only the latest
// textual user turn plus bounded request facts are included in state.
//
// A decision is durable and conversation-sticky by default. The stored record
// is bound to the exact normalized policy hash; editing any policy field,
// including timeout or input bound, forces a fresh decision.
// Request contents are never mutated, preserving provider prompt-prefix bytes.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func main() {}

const (
	endpointSlot       = "decision-service"
	credentialSlot     = "api-key"
	decisionPath       = "/v1/systemone"
	maxResponseBytes   = 64 << 10
	defaultStateBytes  = 8 << 10
	maximumStateBytes  = 64 << 10
	defaultTimeoutMS   = 5_000
	maximumTimeoutMS   = 90_000
	maximumRoutes      = 32
	maximumTools       = 32
	maximumNameBytes   = 128
	maximumModelBytes  = 256
	metricDecisionName = "torana_decision_router_total"
)

var choiceIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

type route struct {
	Description string `json:"description"`
	Provider    string `json:"provider"`
	Model       string `json:"model"`
}

type config struct {
	DecisionModel     string           `json:"decision_model"`
	Question          string           `json:"question"`
	Routes            map[string]route `json:"routes"`
	MinimumConfidence float64          `json:"minimum_confidence"`
	MaxStateBytes     int              `json:"max_state_bytes"`
	TimeoutMS         uint32           `json:"timeout_ms"`
	Authentication    string           `json:"authentication"`
	Sticky            *bool            `json:"sticky"`
}

type storedDecision struct {
	PolicyHash string `json:"policy_hash"`
	Choice     string `json:"choice"`
	Provider   string `json:"provider"`
	Model      string `json:"model"`
}

type requestState struct {
	LatestUserTurn string       `json:"latest_user_turn"`
	Request        requestFacts `json:"request"`
}

type requestFacts struct {
	Model string   `json:"model,omitempty"`
	Tools []string `json:"tools,omitempty"`
}

type systemOneRequest struct {
	Model     string                       `json:"model"`
	State     requestState                 `json:"state"`
	Questions map[string]systemOneQuestion `json:"questions"`
}

type systemOneQuestion struct {
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	Criteria     map[string]string `json:"criteria"`
}

type systemOneResponse struct {
	Answers map[string]json.RawMessage `json:"answers"`
}

type choiceAnswer struct {
	Type       string   `json:"type"`
	Choice     string   `json:"choice"`
	Confidence *float64 `json:"confidence"`
}

func sticky(cfg config) bool { return cfg.Sticky == nil || *cfg.Sticky }

func loadConfig() (config, string, bool, error) {
	raw, err := sdk.PluginConfig()
	if err != nil {
		return config{}, "", false, fmt.Errorf("decision_router: load config: %w", err)
	}
	// Edge publishes the exact empty object when a plugin has no operator
	// settings. An enabled-but-not-yet-configured router must be a no-op: a
	// configuration form should not turn ordinary requests into guest traps.
	// Any non-empty document remains strict below, so partial policies and
	// malformed JSON are still operator errors rather than silent defaults.
	if strings.TrimSpace(raw) == "{}" || strings.TrimSpace(raw) == "" {
		return config{}, "", false, nil
	}
	cfg := config{MinimumConfidence: 0.8, MaxStateBytes: defaultStateBytes, TimeoutMS: defaultTimeoutMS, Authentication: "none"}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return config{}, "", false, fmt.Errorf("decision_router: invalid config: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return config{}, "", false, errors.New("decision_router: invalid config: trailing JSON")
	}
	if err := validateConfig(cfg); err != nil {
		return config{}, "", false, fmt.Errorf("decision_router: invalid config: %w", err)
	}
	normalizedSticky := sticky(cfg)
	cfg.Sticky = &normalizedSticky
	canonical, err := json.Marshal(cfg)
	if err != nil {
		return config{}, "", false, err
	}
	sum := sha256.Sum256(canonical)
	return cfg, hex.EncodeToString(sum[:]), true, nil
}

func validateConfig(cfg config) error {
	if cfg.DecisionModel != strings.TrimSpace(cfg.DecisionModel) || cfg.DecisionModel == "" || utf8.RuneCountInString(cfg.DecisionModel) > maximumModelBytes {
		return errors.New("decision_model must be 1-256 characters without leading or trailing whitespace")
	}
	if strings.TrimSpace(cfg.Question) == "" || utf8.RuneCountInString(cfg.Question) > 2_000 {
		return errors.New("question must be 1-2000 characters")
	}
	if len(cfg.Routes) < 2 || len(cfg.Routes) > maximumRoutes {
		return fmt.Errorf("routes must contain 2-%d choices", maximumRoutes)
	}
	for id, target := range cfg.Routes {
		if !choiceIDPattern.MatchString(id) {
			return fmt.Errorf("route %q has an invalid choice id", id)
		}
		if strings.TrimSpace(target.Description) == "" || utf8.RuneCountInString(target.Description) > 1_000 {
			return fmt.Errorf("route %q needs a description of at most 1000 characters", id)
		}
		if target.Provider != strings.TrimSpace(target.Provider) || target.Model != strings.TrimSpace(target.Model) {
			return fmt.Errorf("route %q provider/model must not have leading or trailing whitespace", id)
		}
		if target.Provider == "" && target.Model == "" {
			return fmt.Errorf("route %q must set provider or model", id)
		}
		if utf8.RuneCountInString(target.Provider) > maximumNameBytes || utf8.RuneCountInString(target.Model) > maximumModelBytes {
			return fmt.Errorf("route %q provider/model exceeds its character limit", id)
		}
	}
	if math.IsNaN(cfg.MinimumConfidence) || math.IsInf(cfg.MinimumConfidence, 0) || cfg.MinimumConfidence < 0 || cfg.MinimumConfidence > 1 {
		return errors.New("minimum_confidence must be between 0 and 1")
	}
	if cfg.MaxStateBytes < 256 || cfg.MaxStateBytes > maximumStateBytes {
		return fmt.Errorf("max_state_bytes must be between 256 and %d", maximumStateBytes)
	}
	if cfg.TimeoutMS < 100 || cfg.TimeoutMS > maximumTimeoutMS {
		return fmt.Errorf("timeout_ms must be between 100 and %d", maximumTimeoutMS)
	}
	if cfg.Authentication != "none" && cfg.Authentication != "bearer" {
		return errors.New(`authentication must be "none" or "bearer"`)
	}
	return nil
}

func init() {
	sdk.OnBeforeRequest(func(ctx context.Context, req *pbv1.ChatRequest) (sdk.RequestResult, error) {
		cfg, policyHash, configured, err := loadConfig()
		if err != nil {
			return sdk.RequestResult{}, err
		}
		if !configured {
			return sdk.PassRequest(), nil
		}

		conversationID := conversationID(req)
		var stateKey string
		if sticky(cfg) {
			if conversationID == "" {
				return fallback("missing_conversation"), nil
			}
			stateKey = decisionStateKey(conversationID)
			prior, found, err := readDecision(stateKey)
			if err != nil {
				return fallback("state_read_failed"), nil
			}
			if found && prior.PolicyHash == policyHash && routeMatches(cfg, prior) {
				if err := sdk.RouteRequest(prior.Provider, prior.Model); err != nil {
					return fallback("route_failed"), nil
				}
				emit("sticky", prior.Choice)
				return sdk.PassRequest(), nil
			}
		}

		state, ok := boundedState(req, cfg.MaxStateBytes)
		if !ok {
			return fallback("no_user_turn"), nil
		}
		choice, confidence, ok := decide(cfg, state)
		if !ok {
			return sdk.PassRequest(), nil
		}
		target, exists := cfg.Routes[choice]
		if !exists { // defense in depth: decide also performs this exact lookup
			return fallback("unknown_choice"), nil
		}
		if confidence < cfg.MinimumConfidence {
			return fallback("low_confidence"), nil
		}

		if sticky(cfg) {
			// Persist before staging the verdict: a state write failure must
			// leave this request unchanged. The host may later reject the route;
			// replaying the same choice is intentional, and host telemetry (not
			// this record) is authoritative for whether routing actually applied.
			record := storedDecision{PolicyHash: policyHash, Choice: choice, Provider: target.Provider, Model: target.Model}
			if err := sdk.StateSetJSON(stateKey, record); err != nil {
				return fallback("state_write_failed"), nil
			}
		}
		if err := sdk.RouteRequest(target.Provider, target.Model); err != nil {
			return fallback("route_failed"), nil
		}
		emit("selected", choice)
		return sdk.PassRequest(), nil
	})
}

func decide(cfg config, state requestState) (string, float64, bool) {
	criteria := make(map[string]string, len(cfg.Routes))
	for id, target := range cfg.Routes {
		criteria[id] = target.Description
	}
	body, err := json.Marshal(systemOneRequest{
		Model:     cfg.DecisionModel,
		State:     state,
		Questions: map[string]systemOneQuestion{"route": {Type: "choice", Instructions: cfg.Question, Criteria: criteria}},
	})
	if err != nil {
		fallback("request_encode_failed")
		return "", 0, false
	}
	headers := []*pbv1.HTTPHeader{{Name: "Content-Type", Values: []string{"application/json"}}}
	if cfg.Authentication == "bearer" {
		secret, err := sdk.GetCredential(credentialSlot)
		if err != nil || len(secret) == 0 || containsHeaderByte(secret) {
			fallback("credential_unavailable")
			return "", 0, false
		}
		headers = append(headers, &pbv1.HTTPHeader{Name: "Authorization", Values: []string{"Bearer " + string(secret)}})
	}
	response, err := sdk.HTTPRequest(&pbv1.OutboundHTTPRequestArgs{
		Endpoint: endpointSlot, Method: "POST", Path: decisionPath, Headers: headers, Body: body, TimeoutMs: cfg.TimeoutMS,
	})
	if err != nil || response == nil {
		fallback("endpoint_failed")
		return "", 0, false
	}
	if response.Status < 200 || response.Status >= 300 {
		fallback("endpoint_status")
		return "", 0, false
	}
	if len(response.Body) == 0 || len(response.Body) > maxResponseBytes {
		fallback("malformed_response")
		return "", 0, false
	}
	var envelope systemOneResponse
	if err := json.Unmarshal(response.Body, &envelope); err != nil || envelope.Answers == nil {
		fallback("malformed_response")
		return "", 0, false
	}
	raw, exists := envelope.Answers["route"]
	if !exists {
		fallback("missing_answer")
		return "", 0, false
	}
	var answer choiceAnswer
	// The service may add response metadata. Decode only fields that control
	// routing, then validate their type, exact configured choice and confidence.
	if err := json.Unmarshal(raw, &answer); err != nil || answer.Type != "choice" || answer.Confidence == nil ||
		math.IsNaN(*answer.Confidence) || math.IsInf(*answer.Confidence, 0) || *answer.Confidence < 0 || *answer.Confidence > 1 {
		fallback("malformed_answer")
		return "", 0, false
	}
	if _, exists := cfg.Routes[answer.Choice]; !exists {
		fallback("unknown_choice")
		return "", 0, false
	}
	return answer.Choice, *answer.Confidence, true
}

func routeMatches(cfg config, prior storedDecision) bool {
	target, ok := cfg.Routes[prior.Choice]
	return ok && target.Provider == prior.Provider && target.Model == prior.Model
}

func readDecision(key string) (storedDecision, bool, error) {
	var decision storedDecision
	found, err := sdk.StateGetJSON(key, &decision)
	return decision, found, err
}

func conversationID(req *pbv1.ChatRequest) string {
	if req == nil || len(req.ToranaMetaJson) == 0 {
		return ""
	}
	var meta struct {
		ConversationID string `json:"_conversation_id"`
	}
	if json.Unmarshal(req.ToranaMetaJson, &meta) != nil {
		return ""
	}
	return meta.ConversationID
}

func decisionStateKey(conversationID string) string {
	sum := sha256.Sum256([]byte(conversationID))
	return "decision/v1/" + hex.EncodeToString(sum[:])
}

func boundedState(req *pbv1.ChatRequest, maxBytes int) (requestState, bool) {
	text := latestUserText(req)
	if strings.TrimSpace(text) == "" {
		return requestState{}, false
	}
	state := requestState{LatestUserTurn: truncateUTF8(text, maxBytes)}
	state.Request.Model = truncateUTF8(req.Model, maximumModelBytes)
	seen := map[string]bool{}
	for _, tool := range req.Tools {
		if tool == nil || tool.Name == "" || seen[tool.Name] || len(state.Request.Tools) >= maximumTools {
			continue
		}
		seen[tool.Name] = true
		state.Request.Tools = append(state.Request.Tools, truncateUTF8(tool.Name, maximumNameBytes))
	}
	sort.Strings(state.Request.Tools)
	return state, true
}

func latestUserText(req *pbv1.ChatRequest) string {
	if req == nil {
		return ""
	}
	for i := len(req.Messages) - 1; i >= 0; i-- {
		message := req.Messages[i]
		if message == nil || message.Role != "user" {
			continue
		}
		var parts []string
		for _, block := range message.Blocks {
			if block != nil && block.GetText() != nil {
				parts = append(parts, block.GetText().Text)
			}
		}
		if joined := strings.Join(parts, "\n"); strings.TrimSpace(joined) != "" {
			return joined
		}
	}
	return ""
}

func truncateUTF8(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func containsHeaderByte(value []byte) bool {
	for _, b := range value {
		if b <= 0x20 || b == 0x7f {
			return true
		}
	}
	return false
}

func fallback(reason string) sdk.RequestResult {
	emit(reason, "")
	sdk.Log("decision_router: kept the current route ("+reason+")", sdk.LogLevelInfo)
	return sdk.PassRequest()
}

func emit(outcome, choice string) {
	labels := map[string]string{"outcome": outcome}
	if choice != "" {
		labels["choice"] = choice
	}
	sdk.EmitMetric(metricDecisionName, sdk.MetricCounter, 1, labels)
}
