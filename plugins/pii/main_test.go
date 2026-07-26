package main

import "testing"

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
