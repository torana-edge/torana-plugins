package main

import (
	"encoding/json"
	"os"
	"sort"
	"testing"
)

// TestManifestPermissionSetExact — the exact release permission set,
// order-independently with duplicate rejection. The table must not inherit a
// stale grant; ir.tool_results.write is the only IR write grant.
func TestManifestPermissionSetExact(t *testing.T) {
	raw, err := os.ReadFile("plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		RequiresUpstream []string `json:"requires_upstream"`
		Permissions      []struct {
			Name string `json:"name"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.RequiresUpstream) != 0 {
		t.Fatalf("requires_upstream = %v, want no hard intent dependency", m.RequiresUpstream)
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
		"env.cache_get",
		"env.cache_set",
		"env.emit_metric",
		"env.host_call.torana_record_savings",
		"env.plugin_config",
		"env.shared_cache_get",
		"ir.tool_results.write",
	}
	if len(got) != len(want) {
		t.Fatalf("permissions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("permissions = %v, want %v", got, want)
		}
	}
}
