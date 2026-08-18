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
	SchemaVersion        int      `json:"schema_version"`
	ID                   string   `json:"id"`
	Name                 string   `json:"name"`
	Version              string   `json:"version"`
	ABIVersion           string   `json:"abi_version"`
	MinimumToranaVersion string   `json:"minimum_torana_version"`
	MaximumToranaVersion string   `json:"maximum_torana_version"`
	FailureMode          string   `json:"failure_mode"`
	Repository           string   `json:"repository"`
	RequiresUpstream     []string `json:"requires_upstream"`
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
	"env.shared_cache_get": true, "env.shared_cache_set": true,
	"env.emit_metric":                          true,
	"env.host_call.torana_cache_pricing":       true,
	"env.host_call.torana_db_query":            true,
	"env.host_call.torana_evaluate_compaction": true,
	"env.host_call.torana_kms_decrypt":         true,
	"env.host_call.torana_offload_completion":  true,
	"env.host_call.torana_plugin_counter":      true,
	"env.host_call.torana_record_savings":      true,
	"env.host_call.torana_send_request":        true,
	"env.host_call.verify_virtual_key":         true,
	"env.log":                                  true, "env.meta_get": true, "env.meta_set": true,
	"env.now": true, "env.original_request": true, "env.original_response": true,
	"env.plugin_config": true, "env.request_headers": true,
	"env.respond_request": true, "env.route_request": true, "env.serve_http": true, "env.set_identity": true,
	"env.state_get": true, "env.state_keys": true, "env.state_set": true,
	"ir.cache_control.write":      true,
	"ir.messages.write.assistant": true, "ir.messages.write.developer": true,
	"ir.messages.write.other": true, "ir.messages.write.system": true,
	"ir.messages.write.tool": true, "ir.messages.write.user": true,
	"ir.model.write": true, "ir.params.write": true,
	"ir.stream.write": true, "ir.tool_results.write": true, "ir.tools.write": true,
}

// pluginContract pins one plugin's EXACT approved contract.
type pluginContract struct {
	hooks            []string
	permissions      []string
	requiresUpstream []string
}

// pluginContracts is the executable nine-plugin release contract. Every
// manifest must match its row exactly.
var pluginContracts = map[string]pluginContract{
	"auth": {hooks: []string{"run_before_request"},
		permissions: []string{"env.block_request", "env.host_call.verify_virtual_key", "env.request_headers", "env.set_identity"}},
	"cache_tier_selector": {hooks: []string{"run_before_request"},
		permissions: []string{"env.host_call.torana_cache_pricing", "env.host_call.torana_plugin_counter", "env.log", "env.now", "env.plugin_config", "env.state_get", "env.state_keys", "env.state_set", "ir.cache_control.write"}},
	"cache_warmer": {hooks: []string{"run_before_request", "run_on_tick"},
		permissions: []string{"env.background_tick", "env.host_call.torana_cache_pricing", "env.host_call.torana_send_request", "env.now", "env.plugin_config", "env.state_get", "env.state_keys", "env.state_set"}},
	"compactor": {hooks: []string{"run_before_request"},
		permissions: []string{"env.cache_get", "env.cache_set", "env.emit_metric", "env.host_call.torana_evaluate_compaction", "env.host_call.torana_offload_completion", "env.host_call.torana_record_savings", "env.plugin_config", "env.shared_cache_get", "ir.tool_results.write"}},
	"intent": {hooks: []string{"run_before_request", "run_on_stream_chunk"},
		permissions: []string{"env.cache_get", "env.cache_set", "env.emit_metric", "env.log", "env.meta_get", "env.meta_set", "env.plugin_config", "env.shared_cache_set", "ir.cache_control.write", "ir.messages.write.assistant", "ir.messages.write.developer", "ir.messages.write.other", "ir.messages.write.system", "ir.messages.write.tool", "ir.messages.write.user", "ir.stream.write", "ir.tool_results.write", "ir.tools.write"}},
	"keyword_compactor": {hooks: []string{"run_before_request"},
		permissions: []string{"env.cache_get", "env.cache_set", "env.emit_metric", "env.host_call.torana_record_savings", "env.plugin_config", "env.shared_cache_get", "ir.tool_results.write"}},
	"otel": {hooks: []string{"run_before_request", "run_after_response", "run_on_http_request"},
		permissions: []string{"env.emit_metric", "env.serve_http"}},
	"pii": {hooks: []string{"run_before_request"},
		permissions: []string{"env.block_request", "env.cache_get", "env.cache_set", "env.host_call.torana_offload_completion", "env.plugin_config"}},
	"schema_translator": {hooks: []string{"run_before_request", "run_on_stream_chunk"},
		permissions: []string{"env.meta_get", "env.meta_set", "ir.messages.write.assistant", "ir.stream.write", "ir.tools.write"}},
}

func hookNames(hooks []struct {
	Name string `json:"name"`
}) []string {
	out := make([]string, 0, len(hooks))
	for _, h := range hooks {
		out = append(out, h.Name)
	}
	return out
}

func permissionNames(perms []struct {
	Name string `json:"name"`
}) []string {
	out := make([]string, 0, len(perms))
	for _, p := range perms {
		out = append(out, p.Name)
	}
	return out
}

// sameStringSet compares two lists as SETS: equal membership, no duplicates
// on either side, order-independent.
func sameStringSet(a, b []string) bool {
	set := func(items []string) map[string]int {
		m := map[string]int{}
		for _, it := range items {
			m[it]++
		}
		return m
	}
	ma, mb := set(a), set(b)
	if len(ma) != len(mb) {
		return false
	}
	for k, n := range ma {
		if n != 1 || mb[k] != 1 {
			return false
		}
	}
	return true
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
	dirs := map[string]bool{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dirs[entry.Name()] = true
		dir := filepath.Join(os.Args[1], entry.Name())
		var m manifest
		readJSON(filepath.Join(dir, "plugin.json"), &m)
		if m.SchemaVersion != 1 || m.ID != "torana/"+entry.Name() || m.Name != entry.Name() || !semver.MatchString(m.Version) || m.ABIVersion != "v2" {
			panic(fmt.Sprintf("%s: incomplete v2 manifest", entry.Name()))
		}
		if m.MinimumToranaVersion == "" || !semver.MatchString(m.MinimumToranaVersion) {
			panic(fmt.Sprintf("%s: invalid or missing minimum_torana_version %q", entry.Name(), m.MinimumToranaVersion))
		}
		if m.MaximumToranaVersion != "" && !semver.MatchString(m.MaximumToranaVersion) {
			panic(fmt.Sprintf("%s: invalid maximum_torana_version %q", entry.Name(), m.MaximumToranaVersion))
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
		for _, requiredID := range m.RequiresUpstream {
			if strings.TrimSpace(requiredID) == "" || requiredID == m.ID || seen["requires:"+requiredID] {
				panic(fmt.Sprintf("%s: invalid or duplicate requires_upstream %q", entry.Name(), requiredID))
			}
			seen["requires:"+requiredID] = true
		}
		// The release contract table: the exact approved hooks,
		// permissions, and requires_upstream per plugin, compared
		// order-independently with duplicate rejection on both sides. A
		// stale grant, a missing grant, a dropped hook, or a drifted
		// upstream dependency fails here.
		contract, ok := pluginContracts[entry.Name()]
		if !ok {
			panic(fmt.Sprintf("%s: no entry in the nine-plugin contract table", entry.Name()))
		}
		if !sameStringSet(contract.hooks, hookNames(m.Hooks)) {
			panic(fmt.Sprintf("%s: hooks %v do not match the contract %v", entry.Name(), hookNames(m.Hooks), contract.hooks))
		}
		if !sameStringSet(contract.permissions, permissionNames(m.Permissions)) {
			panic(fmt.Sprintf("%s: permissions %v do not match the contract %v", entry.Name(), permissionNames(m.Permissions), contract.permissions))
		}
		if !sameStringSet(contract.requiresUpstream, m.RequiresUpstream) {
			panic(fmt.Sprintf("%s: requires_upstream %v does not match the contract %v", entry.Name(), m.RequiresUpstream, contract.requiresUpstream))
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
	// BIDIRECTIONAL inventory: every contract row must have a directory
	// (a missing plugin passes nothing), and every directory must match a
	// row (an extra plugin is a contract drift).
	if len(dirs) != len(pluginContracts) {
		panic(fmt.Sprintf("plugin count %d does not match the contract table (%d)", len(dirs), len(pluginContracts)))
	}
	for name := range pluginContracts {
		if !dirs[name] {
			panic(fmt.Sprintf("plugin %s is in the contract table but has no directory", name))
		}
	}
	for name := range dirs {
		if _, ok := pluginContracts[name]; !ok {
			panic(fmt.Sprintf("directory %s has no contract-table row", name))
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
