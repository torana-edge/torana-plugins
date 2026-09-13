package main

import (
	"errors"
	"testing"

	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
	"google.golang.org/protobuf/proto"
)

func TestConfigFetchFailuresDoNotAdvertiseTools(t *testing.T) {
	for _, failure := range []string{"transport", "host", "empty", "malformed"} {
		t.Run(failure, func(t *testing.T) {
			h := sdktest.New(t)
			mode := "valid"
			h.StubHostCall("env.plugin_config", func(string) (string, error) {
				switch mode {
				case "transport":
					return "", errors.New("unavailable")
				case "host":
					return sdktest.HostResultError(pbv1.ErrorCode_ERROR_CODE_UNAVAILABLE, "unavailable"), nil
				case "empty":
					return sdktest.HostResultValue(nil), nil
				case "malformed":
					return "not a host reply", nil
				case "explicit-empty":
					return sdktest.HostResultValue([]byte(`{}`)), nil
				default:
					return sdktest.HostResultValue([]byte(`{"deny":["shell"]}`)), nil
				}
			})
			input := requestWithTools(tool("read", "", `{}`, false, ""), tool("shell", "", `{}`, false, ""))
			before := proto.Clone(input)
			for _, next := range []string{failure, "valid", failure, "valid", "explicit-empty"} {
				mode = next
				result := h.BeforeRequest(input)
				if next == failure {
					if result.Err == nil || result.PassedThrough || result.Request != nil {
						t.Fatalf("failure advertised tools: %+v", result)
					}
				} else if next == "explicit-empty" {
					if result.Err != nil || !result.PassedThrough {
						t.Fatalf("explicit empty config rejected: %+v", result)
					}
				} else if result.Err != nil || result.Request == nil || len(result.Request.Tools) != 1 || result.Request.Tools[0].Name != "read" {
					t.Fatalf("valid policy not recovered: %+v", result)
				}
				if !proto.Equal(input, before) {
					t.Fatal("input mutated")
				}
			}
		})
	}
}
