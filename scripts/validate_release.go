package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

var semver = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$`)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: validate_release <plugin.json> <plugin-directory-name> <version>")
		os.Exit(2)
	}
	raw, err := os.ReadFile(os.Args[1])
	if err != nil {
		panic(err)
	}
	var manifest struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		panic(err)
	}
	directory := filepath.Base(os.Args[2])
	version := os.Args[3]
	if manifest.Name != directory || manifest.ID != "torana/"+directory {
		panic(fmt.Sprintf("manifest identity %q/%q does not match directory %q", manifest.ID, manifest.Name, directory))
	}
	if !semver.MatchString(version) || manifest.Version != version {
		panic(fmt.Sprintf("release version %q does not match manifest version %q", version, manifest.Version))
	}
}
