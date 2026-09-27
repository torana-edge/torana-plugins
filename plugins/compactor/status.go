package main

import (
	"context"
	"encoding/json"

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
		cfg := parseConfig(raw)
		body, err := json.Marshal(struct {
			Plugin               string `json:"plugin"`
			ExpectedApplications int64  `json:"expected_applications"`
			MaxSourceBytes       int    `json:"max_summarizer_input_bytes"`
			PolicyCount          int    `json:"policy_count"`
		}{"compactor", cfg.ExpectedApplications, cfg.MaxSummarizerInputBytes, len(cfg.ToolPolicies)})
		if err != nil {
			return sdk.PassHTTP(), err
		}
		return sdk.ServeHTTP(&pb.HttpResponse{Status: 200, HeadersJson: []byte(`{"Content-Type":["application/json"]}`), Body: body}), nil
	})
}
