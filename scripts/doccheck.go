// Check the actual JSON examples in each public plugin guide against its
// manifest. This is a documentation gate, not a replacement for host validation.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
)

var jsonBlocks = regexp.MustCompile("(?s)" + "```" + "json\n(.*?)\n" + "```")

func checkGuide(manifestBytes, guide []byte) error {
	var manifest map[string]json.RawMessage
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return err
	}
	var name string
	if err := json.Unmarshal(manifest["name"], &name); err != nil {
		return err
	}
	var approval map[string]json.RawMessage
	blocks := jsonBlocks.FindAllSubmatch(guide, -1)
	if len(blocks) < 2 {
		return fmt.Errorf("%s: need settings and approval examples", name)
	}
	for _, block := range blocks {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(block[1], &object); err != nil {
			return err
		}
		if _, ok := object["digest"]; ok {
			if approval != nil {
				return fmt.Errorf("%s: ambiguous approval example", name)
			}
			approval = object
		}
	}
	if approval == nil {
		return fmt.Errorf("%s: missing approval example", name)
	}
	var requested []struct{ Name string }
	var granted []string
	if err := json.Unmarshal(manifest["permissions"], &requested); err != nil {
		return err
	}
	if err := json.Unmarshal(approval["permissions"], &granted); err != nil {
		return err
	}
	names := make([]string, 0, len(requested))
	for _, permission := range requested {
		names = append(names, permission.Name)
	}
	slices.Sort(names)
	slices.Sort(granted)
	if !reflect.DeepEqual(names, granted) {
		return fmt.Errorf("%s: approval permissions differ from manifest", name)
	}
	if string(approval["failure_mode"]) != string(manifest["failure_mode"]) {
		return fmt.Errorf("%s: example failure policy differs from manifest", name)
	}
	for _, kind := range []string{"files", "model_services", "pricing_resources", "prompt_cache_policies", "credentials", "http_endpoints"} {
		var declarations []map[string]json.RawMessage
		if raw := manifest[kind]; len(raw) > 0 {
			if err := json.Unmarshal(raw, &declarations); err != nil {
				return err
			}
		}
		var bindings map[string]json.RawMessage
		if raw := approval[kind]; len(raw) > 0 {
			if err := json.Unmarshal(raw, &bindings); err != nil {
				return err
			}
		}
		known := map[string]bool{}
		for _, declaration := range declarations {
			key := "name"
			if kind == "files" {
				key = "path"
			}
			var slot string
			if err := json.Unmarshal(declaration[key], &slot); err != nil {
				return err
			}
			known[slot] = true
			var required bool
			if err := json.Unmarshal(declaration["required"], &required); err != nil {
				return err
			}
			if required && len(bindings[slot]) == 0 {
				return fmt.Errorf("%s: missing required %s.%s", name, kind, slot)
			}
			if kind == "files" || kind == "model_services" {
				// Model bindings also contain string coordinates, so decode
				// limits individually instead of accepting a lossy partial map.
				var fields map[string]json.RawMessage
				if raw := bindings[slot]; len(raw) > 0 {
					if err := json.Unmarshal(raw, &fields); err != nil {
						return err
					}
				}
				for _, limit := range []string{"max_bytes", "retained_files", "timeout_ms", "max_tokens", "max_input_bytes", "max_calls_per_minute", "max_tokens_per_hour"} {
					if len(fields[limit]) == 0 || len(declaration[limit]) == 0 {
						continue
					}
					var actual, ceiling float64
					if err := json.Unmarshal(fields[limit], &actual); err != nil {
						return err
					}
					if err := json.Unmarshal(declaration[limit], &ceiling); err != nil {
						return err
					}
					if actual < 0 || actual > ceiling {
						return fmt.Errorf("%s: %s.%s exceeds manifest", name, slot, limit)
					}
				}
			}
		}
		for slot := range bindings {
			if !known[slot] {
				return fmt.Errorf("%s: undeclared %s.%s", name, kind, slot)
			}
		}
	}
	for _, command := range []string{"inspect", "approve", "enable", "disable"} {
		if !strings.Contains(string(guide), "torana plugin "+command+" "+name) {
			return fmt.Errorf("%s: missing %s command", name, command)
		}
	}
	return nil
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: doccheck <plugins directory>")
		os.Exit(2)
	}
	paths, err := filepath.Glob(filepath.Join(os.Args[1], "*", "plugin.json"))
	if err != nil {
		panic(err)
	}
	count := 0
	for _, path := range paths {
		if filepath.Base(filepath.Dir(path)) == "auth" {
			continue
		}
		manifest, err := os.ReadFile(path)
		if err != nil {
			panic(err)
		}
		guide, err := os.ReadFile(filepath.Join(filepath.Dir(path), "README.md"))
		if err != nil {
			panic(err)
		}
		if err := checkGuide(manifest, guide); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		count++
	}
	if count == 0 {
		panic("no public plugin guides found")
	}
	fmt.Printf("Checked %d plugin guides: JSON, permissions, required slots and resource ceilings.\n", count)
}
