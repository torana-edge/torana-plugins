package main

import (
	"errors"
	sdk "github.com/torana-edge/torana-plugin-sdk"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
	"testing"
)

func TestPluginConfigRefusalDoesNotApplyDefaultPolicyAndRetrySucceeds(t *testing.T) {
	h := newHarness(t)
	calls := 0
	h.StubHostCall("env.plugin_config", func(string) (string, error) {
		calls++
		if calls == 1 {
			return sdktest.HostResultError(pbv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "denied"), nil
		}
		return sdktest.HostResultValue([]byte(`{"mode":"long","activity_retention_days":9}`)), nil
	})
	h.Run(func() {
		cfg, err := loadConfig()
		var refusal *sdk.HostCallRefusalError
		if !errors.As(err, &refusal) || refusal.Code != pbv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED {
			t.Fatalf("first load error = %v", err)
		}
		if cfg.Mode != "" || cfg.ActivityRetentionDays != 0 {
			t.Fatalf("refusal applied defaults: %+v", cfg)
		}
	})
	h.Run(func() {
		cfg, err := loadConfig()
		if err != nil {
			t.Fatalf("retry load: %v", err)
		}
		if cfg.Mode != "long" || cfg.ActivityRetentionDays != 9 {
			t.Fatalf("retry config: %+v", cfg)
		}
	})
	if calls != 2 {
		t.Fatalf("config calls=%d, want 2", calls)
	}
}

func TestPluginConfigRefusalStopsHookBeforePolicySideEffects(t *testing.T) {
	h := newHarness(t)
	calls := 0
	h.StubHostCall("env.plugin_config", func(string) (string, error) {
		calls++
		if calls == 1 {
			return sdktest.HostResultError(pbv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "denied"), nil
		}
		return sdktest.HostResultValue([]byte(`{"mode":"off"}`)), nil
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
