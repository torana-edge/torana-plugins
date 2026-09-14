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
		return sdktest.HostResultValue([]byte(`{"max_summarizer_input_bytes":4096,"expected_applications":7}`)), nil
	})
	h.Run(func() {
		err := loadConfig()
		var refusal *sdk.HostCallRefusalError
		if !errors.As(err, &refusal) || refusal.Code != pbv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED {
			t.Fatalf("first load error = %v", err)
		}
	})
	if cfgLoaded || maxSummarizerInputBytes != 0 || expectedApplications != 0 || toolPolicies != nil {
		t.Fatalf("refused config installed state: loaded=%v max=%d expected=%d policies=%v", cfgLoaded, maxSummarizerInputBytes, expectedApplications, toolPolicies)
	}
	h.Run(func() {
		if err := loadConfig(); err != nil {
			t.Fatalf("retry load: %v", err)
		}
	})
	if calls != 2 || !cfgLoaded || maxSummarizerInputBytes != 4096 || expectedApplications != 7 {
		t.Fatalf("retry state: calls=%d loaded=%v max=%d expected=%d", calls, cfgLoaded, maxSummarizerInputBytes, expectedApplications)
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
