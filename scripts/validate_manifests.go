package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type manifest struct {
	SchemaVersion        int    `json:"schema_version"`
	ID                   string `json:"id"`
	Name                 string `json:"name"`
	Version              string `json:"version"`
	ABIVersion           string `json:"abi_version"`
	MinimumToranaVersion string `json:"minimum_torana_version"`
	FailureMode          string `json:"failure_mode"`
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
		if m.SchemaVersion != 1 || m.ID != "torana/"+entry.Name() || m.Name != entry.Name() || m.Version == "" || m.ABIVersion != "v1" || m.MinimumToranaVersion == "" {
			panic(fmt.Sprintf("%s: incomplete v1 manifest", entry.Name()))
		}
		if m.FailureMode != "pass" && m.FailureMode != "block" {
			panic(fmt.Sprintf("%s: invalid failure_mode %q", entry.Name(), m.FailureMode))
		}
		var schema map[string]any
		readJSON(filepath.Join(dir, "schema.json"), &schema)
		if schema["$schema"] == "" || schema["type"] != "object" {
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
