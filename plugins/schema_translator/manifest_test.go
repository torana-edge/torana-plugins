package main

import (
	"encoding/json"
	"os"
	"sort"
	"testing"
)

// TestManifestPermissionSetExact — the exact release permission set,
// order-independently with duplicate rejection; stale grants fail the pin.
func TestManifestPermissionSetExact(t *testing.T) {
	raw, err := os.ReadFile("plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Permissions []struct {
			Name string `json:"name"`
		} `json:"permissions"`
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
	want := []string{"env.meta_get", "env.meta_set", "ir.messages.write.assistant", "ir.stream.write", "ir.tools.write"}
	if len(got) != len(want) {
		t.Fatalf("permissions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("permissions = %v, want %v", got, want)
		}
	}
}
