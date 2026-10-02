package main

import (
	"context"
	"encoding/json"
	"strings"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func init() {
	sdk.OnHTTPRequest(func(_ context.Context, req *pb.HttpRequest) (sdk.HTTPResult, error) {
		if req == nil || req.Method != "GET" || req.Path != "/agent/status" {
			return sdk.PassHTTP(), nil
		}
		cfg, err := loadConfig()
		if err != nil {
			return sdk.PassHTTP(), err
		}
		optedIn := map[string]bool{}
		for _, id := range strings.Split(cfg.Conversations, ",") {
			if id = strings.TrimSpace(id); id != "" {
				optedIn[id] = true
			}
		}
		body, err := json.Marshal(struct {
			Plugin       string `json:"plugin"`
			OptedInCount int    `json:"opted_in_count"`
			Minutes      int    `json:"warm_for_minutes"`
			Interval     int    `json:"interval_seconds_override"`
		}{"cache_warmer", len(optedIn), cfg.WarmForMinutes, cfg.IntervalSecondsOverride})
		if err != nil {
			return sdk.PassHTTP(), err
		}
		return sdk.ServeHTTP(&pb.HttpResponse{Status: 200, HeadersJson: []byte(`{"Content-Type":["application/json"]}`), Body: body}), nil
	})
}
