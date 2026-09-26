package main

import (
	"encoding/json"
	"os"
	"sort"
	"testing"
)

func TestManifestContract(t *testing.T) {
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
		Credentials []struct {
			Slot     string `json:"slot"`
			Required bool   `json:"required"`
		} `json:"credentials"`
		Endpoints []struct {
			Name     string   `json:"name"`
			Required bool     `json:"required"`
			Methods  []string `json:"methods"`
		} `json:"http_endpoints"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.ABI != "v1" || manifest.FailureMode != "pass" {
		t.Fatalf("abi/failure = %s/%s", manifest.ABI, manifest.FailureMode)
	}
	if len(manifest.Hooks) != 2 || manifest.Hooks[0].Name != "run_before_request" || manifest.Hooks[1].Name != "run_after_response" {
		t.Fatalf("hooks = %+v", manifest.Hooks)
	}
	var got []string
	seen := map[string]bool{}
	for _, permission := range manifest.Permissions {
		if seen[permission.Name] {
			t.Fatalf("duplicate permission %q", permission.Name)
		}
		seen[permission.Name] = true
		got = append(got, permission.Name)
	}
	sort.Strings(got)
	want := []string{"env.credential_get", "env.emit_metric", "env.http_request", "env.log", "env.meta_get", "env.meta_set", "env.model_capabilities", "env.plugin_config", "env.route_request", "env.route_request.effort", "env.state_get", "env.state_set", "env.suggest"}
	if len(got) != len(want) {
		t.Fatalf("permissions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("permissions = %v, want %v", got, want)
		}
	}
	if len(manifest.Credentials) != 1 || manifest.Credentials[0].Slot != credentialSlot || manifest.Credentials[0].Required {
		t.Fatalf("credentials = %+v", manifest.Credentials)
	}
	if len(manifest.Endpoints) != 1 || manifest.Endpoints[0].Name != endpointSlot || manifest.Endpoints[0].Required || len(manifest.Endpoints[0].Methods) != 1 || manifest.Endpoints[0].Methods[0] != "POST" {
		t.Fatalf("endpoints = %+v", manifest.Endpoints)
	}
}
