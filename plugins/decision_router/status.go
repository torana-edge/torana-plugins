package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func init() {
	sdk.OnHTTPRequest(func(_ context.Context, req *pb.HttpRequest) (sdk.HTTPResult, error) {
		if req == nil || req.Method != "GET" || req.Path != "/agent/status" {
			return sdk.PassHTTP(), nil
		}
		raw, err := sdk.PluginConfig()
		if err != nil {
			return sdk.PassHTTP(), err
		}
		mode, ladders := "off", 0
		if trimmed := strings.TrimSpace(raw); trimmed != "" && trimmed != "{}" {
			var fields map[string]json.RawMessage
			if json.Unmarshal([]byte(raw), &fields) != nil || fields == nil {
				return sdk.PassHTTP(), fmt.Errorf("decision_router: invalid configuration")
			}
			var configuredMode string
			if json.Unmarshal(fields["mode"], &configuredMode) != nil {
				return sdk.PassHTTP(), fmt.Errorf("decision_router: configuration needs a mode")
			}
			if configuredMode == "off" {
				if len(fields) != 1 {
					return sdk.PassHTTP(), fmt.Errorf("decision_router: off mode accepts only mode")
				}
			} else {
				policy, _, err := loadShadowPolicy(raw)
				if err != nil {
					return sdk.PassHTTP(), fmt.Errorf("decision_router: invalid routing policy")
				}
				mode, ladders = policy.Mode, len(policy.Ladders)
			}
		}
		body, _ := json.Marshal(map[string]any{"plugin": "decision_router", "mode": mode, "configured_ladders": ladders})
		return sdk.ServeHTTP(&pb.HttpResponse{Status: 200, HeadersJson: []byte(`{"Content-Type":["application/json"]}`), Body: body}), nil
	})
}
