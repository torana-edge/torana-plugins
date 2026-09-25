// decision_router measures adaptive ladder decisions in shadow mode. It never
// changes the harness's route or effort until a later, consent-aware phase.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/pb/v1/jsontext"
)

func main() {}

const metricDecisionName = "torana_decision_router_total"

func init() {
	sdk.OnBeforeRequest(func(_ context.Context, req *pbv1.ChatRequest) (sdk.RequestResult, error) {
		raw, err := sdk.PluginConfig()
		if err != nil {
			return sdk.RequestResult{}, err
		}
		if strings.TrimSpace(raw) == "" || strings.TrimSpace(raw) == "{}" {
			return sdk.PassRequest(), nil
		}
		if err := jsontext.Validate([]byte(raw)); err != nil {
			return sdk.RequestResult{}, fmt.Errorf("decision_router: invalid config: %w", err)
		}
		var header struct {
			Mode string `json:"mode"`
		}
		if err := json.Unmarshal([]byte(raw), &header); err != nil {
			return sdk.RequestResult{}, fmt.Errorf("decision_router: invalid config: %w", err)
		}
		if header.Mode == "" {
			return sdk.RequestResult{}, fmt.Errorf("decision_router: use v2 config, for example {\"mode\":\"shadow\",\"ladders\":{\"anthropic\":{\"start\":\"fast\",\"steps\":[{\"id\":\"fast\",\"model\":\"model-a\",\"description\":\"Routine work\"},{\"id\":\"strong\",\"model\":\"model-b\",\"description\":\"Hard work\"}]}}}")
		}
		if header.Mode == "off" {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal([]byte(raw), &fields); err != nil || len(fields) != 1 {
				return sdk.RequestResult{}, fmt.Errorf("decision_router: off mode accepts only the mode field")
			}
			return sdk.PassRequest(), nil
		}
		return runAdaptiveShadow(req, raw)
	})
}

func emit(outcome, choice string) {
	labels := map[string]string{"outcome": outcome}
	if choice != "" {
		labels["choice"] = choice
	}
	sdk.EmitMetric(metricDecisionName, sdk.MetricCounter, 1, labels)
}

func fallback(reason string) sdk.RequestResult {
	emit(reason, "")
	sdk.Log("decision_router: kept the current route ("+reason+")", sdk.LogLevelInfo)
	return sdk.PassRequest()
}
