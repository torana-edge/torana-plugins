package main

import (
	"strings"
	"testing"

	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
)

func TestStatusReportsOnlyRoutingModeAndCounts(t *testing.T) {
	h := sdktest.New(t).SetConfig(shadowConfigJSON)
	result := h.HTTPRequest(&pb.HttpRequest{Method: "GET", Path: "/agent/status"})
	if result.Err != nil || result.Response == nil || result.Response.Status != 200 {
		t.Fatalf("status=%+v", result)
	}
	body := string(result.Response.Body)
	if !strings.Contains(body, `"mode":"shadow"`) || !strings.Contains(body, `"configured_ladders":1`) {
		t.Fatal(body)
	}
	for _, private := range []string{"fast-model", "strong-model", "ladders", "pricing", "endpoints"} {
		if strings.Contains(body, `"`+private+`"`) {
			t.Fatal("status exposed routing configuration")
		}
	}
	if got := h.HTTPRequest(&pb.HttpRequest{Method: "POST", Path: "/agent/status"}); !got.PassedThrough {
		t.Fatal("status handled a mutation method")
	}
	if got := sdktest.New(t).SetConfig(`{"mode":"shadow"}`).HTTPRequest(&pb.HttpRequest{Method: "GET", Path: "/agent/status"}); got.Err == nil {
		t.Fatal("invalid configuration reported healthy")
	}
}
