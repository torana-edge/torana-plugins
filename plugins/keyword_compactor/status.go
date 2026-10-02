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
		body, err := json.Marshal(struct {
			Plugin          string `json:"plugin"`
			PolicyCount     int    `json:"policy_count"`
			MinContentBytes int    `json:"min_content_bytes"`
			MaxResultBytes  int    `json:"max_result_bytes"`
			MaxKeepLines    int    `json:"max_keep_lines"`
		}{"keyword_compactor", len(parseConfig(raw).ToolPolicies), minContentLength, maxResultBytes, maxKeepLines})
		if err != nil {
			return sdk.PassHTTP(), err
		}
		return sdk.ServeHTTP(&pb.HttpResponse{Status: 200, HeadersJson: []byte(`{"Content-Type":["application/json"]}`), Body: body}), nil
	})
}
