package main

import (
	"strings"
	"testing"

	pb "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
)

func TestStatusExposesCountsNotToolPolicyContents(t *testing.T) {
	h := sdktest.New(t).SetConfig(`{"allow":["private-tool"],"deny":["secret-tool"]}`)
	got := h.HTTPRequest(&pb.HttpRequest{Method: "GET", Path: "/agent/status"})
	if got.Err != nil || got.Response == nil {
		t.Fatalf("status=%+v", got)
	}
	body := string(got.Response.Body)
	if strings.Contains(body, "private-tool") || strings.Contains(body, "secret-tool") || !strings.Contains(body, `"allow_restricted":true`) || !strings.Contains(body, `"denied_count":1`) {
		t.Fatal(body)
	}
	if got := h.HTTPRequest(&pb.HttpRequest{Method: "POST", Path: "/agent/status"}); !got.PassedThrough {
		t.Fatal("status handled a mutation")
	}
}
