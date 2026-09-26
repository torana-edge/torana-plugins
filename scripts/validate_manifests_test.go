package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentV2ContractAdmission(t *testing.T) {
	valid := `{"schema_version":2,"namespace":{"title":"Example","summary":"Read example status","categories":["policy"]},"operations":[{"id":"status","method":"GET","path":"/status","description":"Read status","risk":"read","model_access":"read","conversation_binding":"none","output_schema":{"type":"object"}}]}`
	for _, tc := range []struct {
		name, raw string
		fail      bool
	}{
		{"valid", valid, false},
		{"bad version", strings.Replace(valid, `"schema_version":2`, `"schema_version":3`, 1), true},
		{"missing namespace", strings.Replace(valid, `"namespace":`, `"ignored_namespace":`, 1), true},
		{"binding", strings.Replace(valid, `"none"`, `"guess"`, 1), true},
		{"access", strings.Replace(valid, `"model_access":"read"`, `"model_access":"anything"`, 1), true},
		{"v1 fields", strings.Replace(valid, `"schema_version":2`, `"schema_version":1`, 1), true},
		{"removed alias", strings.Replace(valid, `"title":"Example"`, `"title":"Example","alias":"ex"`, 1), true},
		{"removed directive", strings.Replace(valid, `"id":"status"`, `"id":"status","directive":{}`, 1), true},
		{"removed user direct", strings.Replace(valid, `"id":"status"`, `"id":"status","user_direct":false`, 1), true},
		{"unsupported schema", strings.Replace(valid, `"type":"object"`, `"type":"object","minimum":0`, 1), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agent.json")
			if err := os.WriteFile(path, []byte(tc.raw), 0o600); err != nil {
				t.Fatal(err)
			}
			m := manifest{}
			m.Hooks = append(m.Hooks, struct {
				Name string `json:"name"`
			}{Name: "run_on_http_request"})
			m.Permissions = append(m.Permissions, struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			}{Name: "env.serve_http"})
			defer func() {
				if panicked := recover() != nil; panicked != tc.fail {
					t.Fatalf("rejected=%v want=%v", panicked, tc.fail)
				}
			}()
			validateAgentDescriptor("example", path, m)
		})
	}
}
