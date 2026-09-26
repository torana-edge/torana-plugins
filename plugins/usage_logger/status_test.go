package main

import (
	"testing"

	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
)

func TestStatusDoesNotReadUsageRecords(t *testing.T) {
	h := sdktest.New(t)
	got := h.HTTPRequest(&pb.HttpRequest{Method: "GET", Path: "/agent/status"})
	if got.Err != nil || got.Response == nil || string(got.Response.Body) != `{"plugin":"usage_logger","log":"usage.jsonl","content_logged":false}` || len(h.Calls()) != 0 {
		t.Fatalf("status=%+v calls=%v", got, h.Calls())
	}
	if got := h.HTTPRequest(&pb.HttpRequest{Method: "GET", Path: "/elsewhere"}); !got.PassedThrough {
		t.Fatal("status handled an unrelated path")
	}
}
