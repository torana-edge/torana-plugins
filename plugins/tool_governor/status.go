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
		policy, err := configuredPolicy(raw)
		if err != nil {
			return sdk.PassHTTP(), err
		}
		body, _ := json.Marshal(map[string]any{"plugin": "tool_governor", "allow_restricted": policy.allowPresent, "allowed_count": len(policy.allow), "denied_count": len(policy.deny), "replaced_count": len(policy.replace)})
		return sdk.ServeHTTP(&pb.HttpResponse{Status: 200, HeadersJson: []byte(`{"Content-Type":["application/json"]}`), Body: body}), nil
	})
}
