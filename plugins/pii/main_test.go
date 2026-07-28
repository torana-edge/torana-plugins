package main

import (
	"encoding/json"
	"testing"
)

func TestPIICacheKeyChangesWithContentToolAndPolicy(t *testing.T) {
	cfg = piiConfig{Provider: "local", Model: "detector-v1", OnError: "block"}
	base := piiCleanCacheKey("call-1", "bash", "clean output")
	cases := []string{
		piiCleanCacheKey("call-1", "bash", "changed output"),
		piiCleanCacheKey("call-1", "grep", "clean output"),
		piiCleanCacheKey("call-2", "bash", "clean output"),
	}
	for _, changed := range cases {
		if changed == base {
			t.Fatal("cache key did not change with scan input")
		}
	}

	cfg.Model = "detector-v2"
	if changed := piiCleanCacheKey("call-1", "bash", "clean output"); changed == base {
		t.Fatal("cache key did not change with detector policy")
	}
}

func TestPIICacheKeyIsStableForIdenticalInput(t *testing.T) {
	cfg = piiConfig{Provider: "local", Model: "detector-v1", Tools: []string{"bash"}, OnError: "block"}
	left := piiCleanCacheKey("call-1", "bash", "clean output")
	right := piiCleanCacheKey("call-1", "bash", "clean output")
	if left != right {
		t.Fatalf("cache key is not deterministic: %q != %q", left, right)
	}
}

// extractJSON used to span the first "{" to the LAST "}" in the reply. A chatty
// model that added a sentence with a brace in it, or a second object, produced
// a string covering both — which does not parse, and the caller treats an
// unparseable verdict as a scan failure. On a PII detector, failing to read a
// verdict the model did produce is the worst direction to be wrong in.
func TestExtractJSONTakesTheFirstCompleteObject(t *testing.T) {
	const verdict = `{"pii":true,"findings":[{"type":"email","line":3}]}`

	for name, reply := range map[string]string{
		"bare":                    verdict,
		"code fence":              "```json\n" + verdict + "\n```",
		"leading prose":           "Here is the result:\n" + verdict,
		"trailing prose":          verdict + "\nI hope that helps.",
		"trailing prose w/ brace": verdict + "\nNote: check the {config} block too.",
		"trailing second object":  verdict + "\n{\"note\":\"extra\"}",
		"both sides":              "Sure!\n" + verdict + "\nLet me know if the {x} needs work.",
	} {
		t.Run(name, func(t *testing.T) {
			got := extractJSON(reply)

			var parsed struct {
				PII      bool `json:"pii"`
				Findings []struct {
					Type string `json:"type"`
					Line int    `json:"line"`
				} `json:"findings"`
			}
			if err := json.Unmarshal([]byte(got), &parsed); err != nil {
				t.Fatalf("extracted string does not parse: %v\n  got: %s", err, got)
			}
			if !parsed.PII || len(parsed.Findings) != 1 || parsed.Findings[0].Type != "email" {
				t.Errorf("verdict lost in extraction: %+v (from %q)", parsed, got)
			}
		})
	}
}

// A brace inside a string value must not shift the depth, and an escaped quote
// must not end the string early — either would truncate the object.
func TestExtractJSONHandlesBracesAndEscapesInStrings(t *testing.T) {
	for name, reply := range map[string]string{
		"brace in a value":         `{"pii":false,"note":"the {tricky} case","findings":[]}`,
		"escaped quote in a value": `{"pii":false,"note":"he said \"hi\"","findings":[]}`,
		"brace after an escape":    `{"pii":false,"note":"a\\","findings":[]} trailing }`,
	} {
		t.Run(name, func(t *testing.T) {
			var parsed map[string]any
			if err := json.Unmarshal([]byte(extractJSON(reply)), &parsed); err != nil {
				t.Fatalf("did not parse: %v\n  got: %s", err, extractJSON(reply))
			}
			if _, ok := parsed["findings"]; !ok {
				t.Errorf("object truncated: %v", parsed)
			}
		})
	}
}

func TestExtractJSONWithNoObject(t *testing.T) {
	// No brace at all: hand the reply back and let the caller report it.
	if got := extractJSON("I could not analyse that."); got != "I could not analyse that." {
		t.Errorf("got %q", got)
	}
}
