package main

import (
	"context"

	sdk "github.com/torana-edge/torana-plugin-sdk"
	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
)

func init() {
	sdk.OnHTTPRequest(func(_ context.Context, req *pb.HttpRequest) (sdk.HTTPResult, error) {
		if req == nil || req.Method != "GET" || req.Path != "/agent/status" {
			return sdk.PassHTTP(), nil
		}
		return sdk.ServeHTTP(&pb.HttpResponse{Status: 200, HeadersJson: []byte(`{"Content-Type":["application/json"]}`), Body: []byte(`{"plugin":"usage_logger","log":"usage.jsonl","content_logged":false}`)}), nil
	})
}
