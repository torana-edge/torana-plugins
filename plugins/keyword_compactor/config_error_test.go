package main

import (
	"errors"
	sdk "github.com/torana-edge/torana-plugin-sdk"
	pbv1 "github.com/torana-edge/torana-plugin-sdk/pb/v1"
	"github.com/torana-edge/torana-plugin-sdk/sdktest"
	"testing"
)

func TestPluginConfigRefusalIsRetryableAndDoesNotInstallPolicy(t *testing.T) {
	h := newHarness(t)
	calls := 0
	h.StubHostCall("env.plugin_config", func(string) (string, error) {
		calls++
		if calls == 1 {
			return sdktest.HostResultError(pbv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "denied"), nil
		}
		return sdktest.HostResultValue([]byte(`{"tool_policies":[{"match":"read","mode":"deterministic"}]}`)), nil
	})
	h.Run(func() {
		err := loadConfig()
		var refusal *sdk.HostCallRefusalError
		if !errors.As(err, &refusal) || refusal.Code != pbv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED {
			t.Fatalf("first load error = %v", err)
		}
	})
	if cfgLoaded || toolPolicies != nil {
		t.Fatalf("refused config installed policy: loaded=%v policies=%v", cfgLoaded, toolPolicies)
	}
	h.Run(func() {
		if err := loadConfig(); err != nil {
			t.Fatalf("retry load: %v", err)
		}
	})
	if calls != 2 || !cfgLoaded || len(toolPolicies) != 1 || toolPolicies[0].Match != "read" {
		t.Fatalf("retry state: calls=%d loaded=%v policies=%+v", calls, cfgLoaded, toolPolicies)
	}
}

func TestPluginConfigRefusalStopsHookBeforeCompactionSideEffects(t *testing.T) {
	h := newHarness(t)
	calls := 0
	h.StubHostCall("env.plugin_config", func(string) (string, error) {
		calls++
		if calls == 1 {
			return sdktest.HostResultError(pbv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "denied"), nil
		}
		return sdktest.HostResultValue([]byte(`{}`)), nil
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
