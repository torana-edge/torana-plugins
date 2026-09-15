package main

import (
	"strings"
	"testing"
)

func TestGuideExamples(t *testing.T) {
	manifest := []byte(`{"name":"sample","failure_mode":"pass","permissions":[{"name":"env.file_append"}],"files":[{"path":"usage.jsonl","required":true,"max_bytes":10,"retained_files":1}]}`)
	guide := "```json\n{}\n```\n\n```json\n" + `{"digest":"sha256:reviewed","permissions":["env.file_append"],"failure_mode":"pass","files":{"usage.jsonl":{"max_bytes":10,"retained_files":1}}}` + "\n```\n"
	for _, command := range []string{"inspect", "approve", "enable", "disable"} {
		guide += "torana plugin " + command + " sample\n"
	}
	if err := checkGuide(manifest, []byte(guide)); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		strings.Replace(guide, `["env.file_append"]`, `[]`, 1),
		strings.Replace(guide, `"usage.jsonl"`, `"other.jsonl"`, 1),
		strings.Replace(guide, `"max_bytes":10`, `"max_bytes":11`, 1),
		strings.Replace(guide, "torana plugin disable sample", "", 1),
		strings.Replace(guide, `"failure_mode":"pass"`, `"failure_mode":"block"`, 1),
	} {
		if err := checkGuide(manifest, []byte(bad)); err == nil {
			t.Fatal("invalid guide passed")
		}
	}
}
