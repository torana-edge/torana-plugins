package main

import "testing"

func TestConfiguredPolicyTracksExactConfig(t *testing.T) {
	for _, raw := range []string{`{"deny":["shell"]}`, `{"allow":["read"]}`, `{"unknown":true}`, `{"deny":["shell"]}`} {
		first, err := configuredPolicy(raw)
		second, again := configuredPolicy(raw)
		if (err == nil) != (again == nil) {
			t.Fatal("cached error changed")
		}
		if err == nil && (len(first.allow) != len(second.allow) || len(first.deny) != len(second.deny)) {
			t.Fatal("cached policy changed")
		}
		if raw == `{"unknown":true}` && err == nil {
			t.Fatal("invalid config accepted")
		}
	}
}

func BenchmarkConfiguredPolicyReuse(b *testing.B) {
	const raw = `{"deny":["shell"],"replace":{"read":{"parameters":{"type":"object","properties":{}}}}}`
	if _, err := configuredPolicy(raw); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := configuredPolicy(raw); err != nil {
			b.Fatal(err)
		}
	}
}
