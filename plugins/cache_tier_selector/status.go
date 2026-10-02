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
			Plugin        string `json:"plugin"`
			Mode          string `json:"mode"`
			GapSeconds    int    `json:"min_gap_seconds_for_long_tier"`
			RetentionDays int    `json:"activity_retention_days"`
		}{"cache_tier_selector", cfg.Mode, cfg.MinGapSecondsForLongTier, cfg.ActivityRetentionDays})
		if err != nil {
			return sdk.PassHTTP(), err
		}
		return sdk.ServeHTTP(&pb.HttpResponse{Status: 200, HeadersJson: []byte(`{"Content-Type":["application/json"]}`), Body: body}), nil
	})
}
