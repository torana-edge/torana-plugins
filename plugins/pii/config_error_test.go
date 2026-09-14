package main

import (
	"errors"
	sdk "github.com/torana-edge/torana-plugin-sdk"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
	"testing"
)

func TestPluginConfigRefusalIsRetryableAndDoesNotInstallDefaults(t *testing.T) {
	h := newHarness(t)
	calls := 0
	h.StubHostCall("env.plugin_config", func(string) (string, error) {
		calls++
		if calls == 1 {
			return sdktest.HostResultError(pbv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "denied"), nil
		}
		return sdktest.HostResultValue([]byte(`{"on_error":"allow","max_scan_bytes":17}`)), nil
	})
	h.Run(func() {
		err := loadConfig()
		var refusal *sdk.HostCallRefusalError
		if !errors.As(err, &refusal) || refusal.Code != pbv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED {
			t.Fatalf("first load error = %v", err)
		}
	})
	if cfgLoaded || cfg.OnError != "" || cfg.MaxScanBytes != 0 {
		t.Fatalf("refused config installed state: loaded=%v cfg=%+v", cfgLoaded, cfg)
	}
	h.Run(func() {
		if err := loadConfig(); err != nil {
			t.Fatalf("retry load: %v", err)
		}
	})
	if calls != 2 || !cfgLoaded || cfg.OnError != "allow" || cfg.MaxScanBytes != 17 {
		t.Fatalf("retry state: calls=%d loaded=%v cfg=%+v", calls, cfgLoaded, cfg)
	}
}

func TestPluginConfigRefusalStopsHookBeforeScannerSideEffects(t *testing.T) {
	h := newHarness(t)
	calls := 0
	h.StubHostCall("env.plugin_config", func(string) (string, error) {
		calls++
		if calls == 1 {
			return sdktest.HostResultError(pbv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "denied"), nil
		}
		return sdktest.HostResultValue([]byte(`{"on_error":"allow"}`)), nil
	})
	first := h.BeforeRequest(&pbv1.ChatRequest{})
	if first.Err == nil {
		t.Fatal("refused config returned hook success")
	}
	if got := h.Calls(); len(got) != 1 || got[0].Command != "env.plugin_config" {
		t.Fatalf("calls after refusal = %+v", got)
	}
	second := h.BeforeRequest(&pbv1.ChatRequest{})
	if second.Err != nil || !second.PassedThrough || calls != 2 {
		t.Fatalf("retry = %+v, calls=%d", second, calls)
	}
}
