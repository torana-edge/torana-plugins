package main

import (
	"strings"
	"testing"

	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
)

func TestStatusReadsSettingsWithoutCallingAModel(t *testing.T) {
	h := sdktest.New(t).SetConfig(`{"expected_applications":3,"max_summarizer_input_bytes":1000,"tool_policies":[{"match":"private-tool-name","mode":"model"}]}`)
	res := h.HTTPRequest(&pb.HttpRequest{Method: "GET", Path: "/agent/status"})
	if res.Err != nil || res.Response == nil || res.Response.Status != 200 || string(res.Response.Body) != `{"plugin":"compactor","expected_applications":3,"max_summarizer_input_bytes":1000,"policy_count":1}` {
		t.Fatalf("response=%+v", res)
	}
	for _, call := range h.Calls() {
		if call.Command != "env.plugin_config" {
			t.Fatalf("status made an unnecessary host call: %s", call.Command)
		}
	}
	if strings.Contains(string(res.Response.Body), "private-tool-name") {
		t.Fatal("status exposed tool policies")
	}
	if res := h.HTTPRequest(&pb.HttpRequest{Method: "POST", Path: "/agent/status"}); !res.PassedThrough {
		t.Fatal("status accepted a write method")
	}
}
