package main

import (
	"encoding/json"
	"os"
	"sort"
	"testing"
)

// TestManifestPermissionSetExact — the exact release permission set,
// order-independently with duplicate rejection (env.log is intentionally
// absent).
func TestManifestPermissionSetExact(t *testing.T) {
	raw, err := os.ReadFile("plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Permissions []struct {
			Name string `json:"name"`
		} `json:"permissions"`
		ModelServices []struct {
			Name              string `json:"name"`
			Required          bool   `json:"required"`
			TimeoutMS         int    `json:"timeout_ms"`
			MaxTokens         int    `json:"max_tokens"`
			MaxInputBytes     int    `json:"max_input_bytes"`
			MaxCallsPerMinute int    `json:"max_calls_per_minute"`
			MaxTokensPerHour  int    `json:"max_tokens_per_hour"`
		} `json:"model_services"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(m.Permissions))
	seen := map[string]bool{}
	for _, p := range m.Permissions {
		if seen[p.Name] {
			t.Fatalf("duplicate permission %q", p.Name)
		}
		seen[p.Name] = true
		got = append(got, p.Name)
	}
	sort.Strings(got)
	want := []string{
		"env.block_request",
		"env.cache_get",
		"env.cache_set",
		"env.model_complete",
		"env.plugin_config",
	}
	if len(got) != len(want) {
		t.Fatalf("permissions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("permissions = %v, want %v", got, want)
		}
	}
	if len(m.ModelServices) != 1 {
		t.Fatalf("model services = %+v, want exactly scanner", m.ModelServices)
	}
	scanner := m.ModelServices[0]
	if scanner.Name != "scanner" || !scanner.Required || scanner.TimeoutMS != 30000 ||
		scanner.MaxTokens != 512 || scanner.MaxInputBytes != 1048576 ||
		scanner.MaxCallsPerMinute != 60 || scanner.MaxTokensPerHour != 100000 {
		t.Fatalf("scanner model-service contract = %+v", scanner)
	}
}
