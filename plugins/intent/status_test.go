package main

import (
	"testing"

	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
)

func TestAgentStatus(t *testing.T) {
	for _, tc := range []struct{ config, want string }{
		{`{}`, `{"plugin":"intent","fill":"heuristic","intent_field":"i"}`},
		{`{"fill":"off"}`, `{"plugin":"intent","fill":"off","intent_field":"i"}`},
	} {
		h := sdktest.New(t).SetConfig(tc.config)
		res := h.HTTPRequest(&pb.HttpRequest{Method: "GET", Path: "/agent/status"})
		if res.Err != nil || res.Response == nil || res.Response.Status != 200 || string(res.Response.Body) != tc.want {
			t.Fatalf("status=%+v", res)
		}
		for _, call := range h.Calls() {
			if call.Command != "env.plugin_config" {
				t.Fatalf("unexpected call %s", call.Command)
			}
		}
		for _, req := range []*pb.HttpRequest{{Method: "POST", Path: "/agent/status"}, {Method: "GET", Path: "/unrelated"}} {
			if !h.HTTPRequest(req).PassedThrough {
				t.Fatal("handled unrelated request")
			}
		}
	}
}
