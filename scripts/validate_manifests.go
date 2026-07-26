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
var agentOperationID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
var agentOperationPath = regexp.MustCompile(`^/(?:[A-Za-z0-9._~-]+/?)*$`)
var agentSchemaKeywords = map[string]bool{
	"$schema": true, "title": true, "description": true, "type": true,
	"properties": true, "required": true, "additionalProperties": true,
	"items": true, "const": true, "enum": true,
}

type agentDescriptor struct {
	SchemaVersion int              `json:"schema_version"`
	Operations    []agentOperation `json:"operations"`
}

type agentOperation struct {
	ID           string          `json:"id"`
	Method       string          `json:"method"`
	Path         string          `json:"path"`
	Description  string          `json:"description"`
	Risk         string          `json:"risk"`
	InputSchema  json.RawMessage `json:"input_schema"`
	OutputSchema json.RawMessage `json:"output_schema"`
}

// knownHooks and knownPermissions mirror supportedHooks and supportedPermissions
// in torana-edge's internal/plugin/discovery.go. The host is authoritative; this
// copy exists so a bad manifest fails here rather than at load time in someone's
// proxy.
//
// Because it is a copy it can drift, and it did: every capability added for the
// cache plugins was rejected here while being perfectly valid to the host.
// Adding a capability upstream means updating this list in the same change.
var knownHooks = map[string]bool{
	"run_before_request": true, "run_after_response": true,
	"run_on_stream_chunk": true, "run_on_http_request": true,
	"run_on_tick": true,
}

var knownPermissions = map[string]bool{
	"env.background_tick": true,
	"env.block_request":   true, "env.cache_get": true, "env.cache_set": true,
	"env.emit_metric":                          true,
	"env.host_call.torana_cache_pricing":       true,
	"env.host_call.torana_evaluate_compaction": true,
	"env.host_call.torana_offload_completion":  true,
	"env.host_call.torana_plugin_counter":      true,
	"env.host_call.torana_record_savings":      true,
	"env.host_call.torana_send_request":        true,
	"env.host_call.verify_virtual_key":         true,
	"env.log":                                  true, "env.meta_get": true, "env.meta_set": true,
	"env.now": true, "env.original_request": true, "env.original_response": true,
	"env.plugin_config": true, "env.request_headers": true,
	"env.respond_request": true, "env.route_request": true, "env.serve_http": true,
	"env.state_get": true, "env.state_keys": true, "env.state_set": true,
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
		agentPath := filepath.Join(dir, "agent.json")
		if _, err := os.Stat(agentPath); err == nil {
			validateAgentDescriptor(entry.Name(), agentPath, m)
		} else if !os.IsNotExist(err) {
			panic(err)
		}
	}
}

func validateAgentDescriptor(pluginName, path string, m manifest) {
	var descriptor agentDescriptor
	readJSON(path, &descriptor)
	if descriptor.SchemaVersion != 1 || len(descriptor.Operations) == 0 || len(descriptor.Operations) > 64 {
		panic(fmt.Sprintf("%s: invalid agent descriptor header", pluginName))
	}
	hasHook, hasPermission := false, false
	for _, hook := range m.Hooks {
		hasHook = hasHook || hook.Name == "run_on_http_request"
	}
	for _, permission := range m.Permissions {
		hasPermission = hasPermission || permission.Name == "env.serve_http"
	}
	if !hasHook || !hasPermission {
		panic(fmt.Sprintf("%s: agent descriptor requires run_on_http_request and env.serve_http", pluginName))
	}
	seen := map[string]bool{}
	for _, operation := range descriptor.Operations {
		operation.Method = strings.ToUpper(strings.TrimSpace(operation.Method))
		if !agentOperationID.MatchString(operation.ID) || seen["id:"+operation.ID] {
			panic(fmt.Sprintf("%s: invalid or duplicate agent operation id %q", pluginName, operation.ID))
		}
		seen["id:"+operation.ID] = true
		switch operation.Method {
		case "GET", "POST", "PUT", "PATCH", "DELETE":
		default:
			panic(fmt.Sprintf("%s: unsupported agent method %q", pluginName, operation.Method))
		}
		if operation.Path == "" || !strings.HasPrefix(operation.Path, "/") ||
			!agentOperationPath.MatchString(operation.Path) ||
			strings.HasSuffix(operation.Path, "/.") || strings.Contains(operation.Path, "/./") ||
			strings.HasPrefix(operation.Path, "//") || strings.Contains(operation.Path, "..") ||
			strings.ContainsAny(operation.Path, "?#") || seen["route:"+operation.Method+" "+operation.Path] {
			panic(fmt.Sprintf("%s: invalid or duplicate agent route %s %s", pluginName, operation.Method, operation.Path))
		}
		seen["route:"+operation.Method+" "+operation.Path] = true
		if strings.TrimSpace(operation.Description) == "" {
			panic(fmt.Sprintf("%s: agent operation %q requires description", pluginName, operation.ID))
		}
		if operation.Risk == "read" && operation.Method != "GET" {
			panic(fmt.Sprintf("%s: mutating operation %q cannot use read risk", pluginName, operation.ID))
		}
		if operation.Risk != "read" && operation.Risk != "write" && operation.Risk != "destructive" {
			panic(fmt.Sprintf("%s: agent operation %q has invalid risk", pluginName, operation.ID))
		}
		if operation.Risk == "write" && (operation.Method == "GET" || operation.Method == "DELETE") {
			panic(fmt.Sprintf("%s: agent operation %q has inconsistent write risk", pluginName, operation.ID))
		}
		if operation.Risk == "destructive" && operation.Method == "GET" {
			panic(fmt.Sprintf("%s: agent operation %q has inconsistent destructive risk", pluginName, operation.ID))
		}
		validateSchemaObject(pluginName, operation.ID, "input_schema", operation.InputSchema, true)
		validateSchemaObject(pluginName, operation.ID, "output_schema", operation.OutputSchema, false)
	}
}

func validateSchemaObject(pluginName, operationID, field string, raw json.RawMessage, optional bool) {
	if len(raw) == 0 && optional {
		return
	}
	if err := validateAgentSchema(raw); err != nil {
		panic(fmt.Sprintf("%s: agent operation %q %s: %v", pluginName, operationID, field, err))
	}
}

func validateAgentSchema(raw json.RawMessage) error {
	var schema map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &schema) != nil || schema == nil {
		return fmt.Errorf("must be a JSON object")
	}
	for keyword := range schema {
		if !agentSchemaKeywords[keyword] {
			return fmt.Errorf("unsupported schema keyword %q", keyword)
		}
	}
	var schemaType string
	if rawType, ok := schema["type"]; ok {
		if json.Unmarshal(rawType, &schemaType) != nil {
			return fmt.Errorf("type must be a string")
		}
		switch schemaType {
		case "object", "array", "string", "number", "integer", "boolean", "null":
		default:
			return fmt.Errorf("unsupported type %q", schemaType)
		}
	}
	if schemaType == "" && schema["const"] == nil && schema["enum"] == nil {
		return fmt.Errorf("requires type, const, or enum")
	}
	var properties map[string]json.RawMessage
	if rawProperties, ok := schema["properties"]; ok {
		if schemaType != "object" || json.Unmarshal(rawProperties, &properties) != nil || properties == nil {
			return fmt.Errorf("properties requires object type and an object value")
		}
		for name, child := range properties {
			if name == "" {
				return fmt.Errorf("property name cannot be empty")
			}
			if err := validateAgentSchema(child); err != nil {
				return fmt.Errorf("property %q: %w", name, err)
			}
		}
	}
	if rawRequired, ok := schema["required"]; ok {
		var required []string
		if schemaType != "object" || json.Unmarshal(rawRequired, &required) != nil {
			return fmt.Errorf("required requires object type and an array of strings")
		}
		seen := map[string]bool{}
		for _, name := range required {
			if name == "" || seen[name] {
				return fmt.Errorf("required property %q is empty or duplicated", name)
			}
			seen[name] = true
			if properties != nil && properties[name] == nil {
				return fmt.Errorf("required property %q is not declared", name)
			}
		}
	}
	if rawAdditional, ok := schema["additionalProperties"]; ok {
		var allowed bool
		if schemaType != "object" || json.Unmarshal(rawAdditional, &allowed) != nil {
			return fmt.Errorf("additionalProperties requires object type and a boolean value")
		}
	}
	if rawItems, ok := schema["items"]; ok {
		if schemaType != "array" {
			return fmt.Errorf("items requires array type")
		}
		if err := validateAgentSchema(rawItems); err != nil {
			return fmt.Errorf("items: %w", err)
		}
	} else if schemaType == "array" {
		return fmt.Errorf("array type requires items")
	}
	if rawEnum, ok := schema["enum"]; ok {
		var values []json.RawMessage
		if json.Unmarshal(rawEnum, &values) != nil || len(values) == 0 {
			return fmt.Errorf("enum must be a non-empty array")
		}
	}
	for _, keyword := range []string{"$schema", "title", "description"} {
		if value, ok := schema[keyword]; ok {
			var text string
			if json.Unmarshal(value, &text) != nil {
				return fmt.Errorf("%s must be a string", keyword)
			}
		}
	}
	return nil
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
