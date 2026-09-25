package main

import (
	"strings"
	"testing"

	"github.com/torana-edge/torana-plugin-sdk/sdktest"
	"google.golang.org/protobuf/proto"
)

func TestOnlyShadowOrOffCanRun(t *testing.T) {
	for _, tc := range []struct {
		name      string
		config    string
		wantError bool
	}{
		{"empty", `{}`, false},
		{"off", `{"mode":"off"}`, false},
		{"off extra", `{"mode":"off","ladders":{}}`, true},
		{"old fixed", `{"decision_model":"jev","question":"which?","routes":{}}`, true},
		{"unknown", `{"mode":"auto"}`, true},
		{"shadow", shadowConfigJSON, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := sdktest.New(t).SetConfig(tc.config)
			result := h.BeforeRequest(request("session", "Fix this"))
			if (result.Err != nil) != tc.wantError {
				t.Fatalf("error = %v, wantError %v", result.Err, tc.wantError)
			}
			if len(routes(h)) != 0 {
				t.Fatal("shadow/off policy staged a route")
			}
			if tc.name == "old fixed" && !strings.Contains(result.Err.Error(), `"mode":"shadow"`) {
				t.Fatalf("v1 error lacks v2 example: %v", result.Err)
			}
		})
	}
}

func TestShadowNeverMutatesRequest(t *testing.T) {
	h := sdktest.New(t).SetConfig(shadowConfigJSON)
	req := request("session", "Fix this")
	before := proto.Clone(req)
	result := h.BeforeRequest(req)
	if result.Err != nil {
		t.Fatal(result.Err)
	}
	if !proto.Equal(req, before) {
		t.Fatal("shadow policy changed canonical request")
	}
	for _, call := range h.Calls() {
		if call.Command == "env.route_request" {
			t.Fatal("shadow policy called route_request")
		}
	}
}
