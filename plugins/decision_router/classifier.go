package main

import (
	"encoding/json"
	"errors"
	"math"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

const (
	endpointSlot      = "decision-service"
	credentialSlot    = "api-key"
	decisionPath      = "/v1/systemone"
	maxResponseBytes  = 64 << 10
	defaultStateBytes = 8 << 10
	maximumStateBytes = 64 << 10
	maximumRoutes     = 32
	maximumModelBytes = 256
)

type route struct {
	Description string `json:"description"`
}
type config struct {
	DecisionModel  string
	Question       string
	Routes         map[string]route
	TimeoutMS      uint32
	Authentication string
}
type requestState struct {
	LatestUserTurn string             `json:"latest_user_turn"`
	Signals        *shadowSignalFacts `json:"signals,omitempty"`
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
	var refusal *sdk.HostCallRefusalError
	if errors.As(err, &refusal) && refusal.Code == pbv1.ErrorCode_ERROR_CODE_NOT_CONFIGURED {
		sdk.Debug("decision_router: bind the decision-service endpoint before enabling a classifier")
		emit("decision_service_not_configured", "")
		return "", 0, false
	}
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
	if json.Unmarshal(response.Body, &envelope) != nil || envelope.Answers == nil {
		fallback("malformed_response")
		return "", 0, false
	}
	raw, exists := envelope.Answers["route"]
	if !exists {
		fallback("missing_answer")
		return "", 0, false
	}
	var answer choiceAnswer
	if json.Unmarshal(raw, &answer) != nil || answer.Type != "choice" || answer.Confidence == nil ||
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

func containsHeaderByte(value []byte) bool {
	for _, b := range value {
		if b <= 0x20 || b == 0x7f {
			return true
		}
	}
	return false
}
