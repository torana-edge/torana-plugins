package main

import (
	"encoding/json"
	"os"
	"sort"
	"testing"
)

func TestManifestPermissionSetExact(t *testing.T) {
	raw, err := os.ReadFile("plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		ABI         string `json:"abi_version"`
		FailureMode string `json:"failure_mode"`
		Hooks       []struct {
			Name string `json:"name"`
		} `json:"hooks"`
		Permissions []struct {
			Name string `json:"name"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.ABI != "v2" || manifest.FailureMode != "block" {
		t.Fatalf("abi/failure_mode = %q/%q", manifest.ABI, manifest.FailureMode)
	}
	if len(manifest.Hooks) != 1 || manifest.Hooks[0].Name != "run_before_request" {
		t.Fatalf("hooks = %+v", manifest.Hooks)
	}
	got := make([]string, 0, len(manifest.Permissions))
	seen := map[string]bool{}
	for _, permission := range manifest.Permissions {
		if seen[permission.Name] {
			t.Fatalf("duplicate permission %q", permission.Name)
		}
		seen[permission.Name] = true
		got = append(got, permission.Name)
	}
	sort.Strings(got)
	want := []string{"env.plugin_config", "ir.cache_control.write", "ir.tools.write"}
	if len(got) != len(want) {
		t.Fatalf("permissions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("permissions = %v, want %v", got, want)
		}
	}
}
