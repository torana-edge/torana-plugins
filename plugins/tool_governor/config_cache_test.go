package main

import (
	"reflect"
	"testing"
)

func TestConfiguredPolicyTracksExactConfig(t *testing.T) {
	loadedPolicy.Lock()
	loadedPolicy.ready = false
	loadedPolicy.raw = ""
	loadedPolicy.Unlock()
	// The empty first input exercises ready; equal-length valid inputs catch
	// length-only keys, and different-length inputs exercise normal replacement.
	for _, raw := range []string{"", `{"deny":["shell"]}`, `{"allow":["read"]}`, `{"allow":["read","search"]}`, `{"unknown":true}`, `{"deny":["shell"]}`} {
		want, wantErr := parsePolicy([]byte(raw))
		first, err := configuredPolicy(raw)
		second, again := configuredPolicy(raw)
		if (err == nil) != (again == nil) {
			t.Fatal("cached error changed")
		}
		if (err == nil) != (wantErr == nil) || !reflect.DeepEqual(first, want) || !reflect.DeepEqual(second, want) {
			t.Fatalf("policy does not match requested raw config %q", raw)
		}
		if raw == `{"unknown":true}` && err == nil {
			t.Fatal("invalid config accepted")
		}
	}
}

func TestConfiguredPolicyHitAllocations(t *testing.T) {
	const raw = `{"deny":["shell"],"allow":["read"]}`
	if _, err := configuredPolicy(raw); err != nil {
		t.Fatal(err)
	}
	allocs := testing.AllocsPerRun(100, func() {
		if _, err := configuredPolicy(raw); err != nil {
			panic(err)
		}
	})
	if allocs != 0 {
		t.Fatalf("cache hit allocated %g times; parsing may have returned to the hit path", allocs)
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
