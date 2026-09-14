package main

import (
	"errors"
	sdk "github.com/torana-edge/torana-plugin-sdk"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
	"testing"
)

func TestPluginConfigRefusalIsRetryableAndDoesNotInstallDefaultFill(t *testing.T) {
	h := newHarness(t)
	fillMode = ""
	calls := 0
	h.StubHostCall("env.plugin_config", func(string) (string, error) {
		calls++
		if calls == 1 {
			return sdktest.HostResultError(pbv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "denied"), nil
		}
		return sdktest.HostResultValue([]byte(`{"fill":"off"}`)), nil
	})
	h.Run(func() {
		err := loadConfig()
		var refusal *sdk.HostCallRefusalError
		if !errors.As(err, &refusal) || refusal.Code != pbv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED {
			t.Fatalf("first load error = %v", err)
		}
	})
	if cfgLoaded || fillMode != "" {
		t.Fatalf("refused config installed default: loaded=%v fill=%q", cfgLoaded, fillMode)
	}
	h.Run(func() {
		if err := loadConfig(); err != nil {
			t.Fatalf("retry load: %v", err)
		}
	})
	if calls != 2 || !cfgLoaded || fillMode != "off" {
		t.Fatalf("retry state: calls=%d loaded=%v fill=%q", calls, cfgLoaded, fillMode)
	}
}

func TestPluginConfigRefusalStopsHookBeforeIntentSideEffects(t *testing.T) {
	h := newHarness(t)
	calls := 0
	h.StubHostCall("env.plugin_config", func(string) (string, error) {
		calls++
		if calls == 1 {
			return sdktest.HostResultError(pbv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "denied"), nil
		}
		return sdktest.HostResultValue([]byte(`{"fill":"off"}`)), nil
	})
	req := &pbv1.ChatRequest{Tools: []*pbv1.ToolDef{{Name: "read", ParametersJson: []byte(`{"type":"object","properties":{}}`)}}}
	first := h.BeforeRequest(req)
	if first.Err == nil {
		t.Fatal("refused config returned hook success")
	}
	if got := h.Calls(); len(got) != 1 || got[0].Command != "env.plugin_config" {
		t.Fatalf("calls after refusal = %+v", got)
	}
	second := h.BeforeRequest(req)
	if second.Err != nil || second.Request == nil || calls != 2 {
		t.Fatalf("retry = %+v, calls=%d", second, calls)
	}
}
