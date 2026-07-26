package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type manifest struct {
	SchemaVersion        int    `json:"schema_version"`
	ID                   string `json:"id"`
	Name                 string `json:"name"`
	Version              string `json:"version"`
	ABIVersion           string `json:"abi_version"`
	MinimumToranaVersion string `json:"minimum_torana_version"`
	FailureMode          string `json:"failure_mode"`
	Repository           string `json:"repository"`
	Hooks                []struct {
		Name string `json:"name"`
	} `json:"hooks"`
	Permissions []struct {
		Name string `json:"name"`
	} `json:"permissions"`
}

var semver = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$`)

var knownHooks = map[string]bool{
	"run_before_request": true, "run_after_response": true,
	"run_on_stream_chunk": true, "run_on_http_request": true,
}

var knownPermissions = map[string]bool{
	"env.block_request": true, "env.cache_get": true, "env.cache_set": true,
	"env.emit_metric": true, "env.host_call.torana_evaluate_compaction": true,
	"env.host_call.torana_offload_completion": true,
	"env.host_call.torana_record_savings":     true, "env.host_call.verify_virtual_key": true,
	"env.log": true, "env.meta_get": true, "env.meta_set": true,
	"env.plugin_config": true, "env.request_headers": true, "env.serve_http": true,
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: validate_manifests <plugins-dir>")
		os.Exit(2)
	}
	entries, err := os.ReadDir(os.Args[1])
	if err != nil {
		panic(err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(os.Args[1], entry.Name())
		var m manifest
		readJSON(filepath.Join(dir, "plugin.json"), &m)
		if m.SchemaVersion != 1 || m.ID != "torana/"+entry.Name() || m.Name != entry.Name() || !semver.MatchString(m.Version) || m.ABIVersion != "v1" || !semver.MatchString(m.MinimumToranaVersion) {
			panic(fmt.Sprintf("%s: incomplete v1 manifest", entry.Name()))
		}
		if m.Repository != "https://github.com/torana-edge/torana-plugins" {
			panic(fmt.Sprintf("%s: invalid repository %q", entry.Name(), m.Repository))
		}
		if m.FailureMode != "pass" && m.FailureMode != "block" {
			panic(fmt.Sprintf("%s: invalid failure_mode %q", entry.Name(), m.FailureMode))
		}
		seen := map[string]bool{}
		for _, hook := range m.Hooks {
			if !knownHooks[hook.Name] || seen["hook:"+hook.Name] {
				panic(fmt.Sprintf("%s: invalid or duplicate hook %q", entry.Name(), hook.Name))
			}
			seen["hook:"+hook.Name] = true
		}
		for _, permission := range m.Permissions {
			if !knownPermissions[permission.Name] || seen["permission:"+permission.Name] {
				panic(fmt.Sprintf("%s: invalid or duplicate permission %q", entry.Name(), permission.Name))
			}
			seen["permission:"+permission.Name] = true
		}
		var schema map[string]any
		readJSON(filepath.Join(dir, "schema.json"), &schema)
		schemaURL, _ := schema["$schema"].(string)
		if !strings.HasPrefix(schemaURL, "https://json-schema.org/") || schema["type"] != "object" || schema["additionalProperties"] == nil || schema["properties"] == nil {
			panic(fmt.Sprintf("%s: schema is not a JSON Schema object", entry.Name()))
		}
	}
}

func readJSON(path string, into any) {
	b, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
	if err := json.Unmarshal(b, into); err != nil {
		panic(fmt.Errorf("%s: %w", path, err))
	}
}
