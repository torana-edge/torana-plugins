package main

import (
	"testing"

	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
)

func TestAgentStatus(t *testing.T) {
	for _, tc := range []struct{ config, want string }{
		{`{}`, `{"plugin":"cache_warmer","opted_in_count":0,"warm_for_minutes":45,"interval_seconds_override":0}`},
		{`{"conversations":" private-chat, private-chat, ,other-chat ","warm_for_minutes":0,"interval_seconds_override":30}`, `{"plugin":"cache_warmer","opted_in_count":2,"warm_for_minutes":0,"interval_seconds_override":30}`},
	} {
		h := sdktest.New(t).SetConfig(tc.config)
		res := h.HTTPRequest(&pb.HttpRequest{Method: "GET", Path: "/agent/status"})
		if res.Err != nil || res.Response == nil || res.Response.Status != 200 || string(res.Response.Body) != tc.want {
			t.Fatalf("status=%+v", res)
		}
		for _, call := range h.Calls() {
			if call.Command != "env.plugin_config" {
				t.Fatalf("status performed warming or read private state: %s", call.Command)
			}
		}
		for _, req := range []*pb.HttpRequest{{Method: "POST", Path: "/agent/status"}, {Method: "GET", Path: "/unrelated"}} {
			if !h.HTTPRequest(req).PassedThrough {
				t.Fatal("handled unrelated request")
			}
		}
	}
}
