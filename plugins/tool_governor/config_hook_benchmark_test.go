package main

import (
	"testing"

	"github.com/torana-edge/torana-plugin-sdk/sdktest"
)

// Measures the complete native SDK hook path, including config host dispatch,
// protobuf input/output and policy application. This is not WASM ABI timing or
// an end-to-end proxy benchmark. The forced-miss variant models reparsing every
// request; invalidation itself is excluded from the timed region.
func BenchmarkBeforeRequestConfigCache(b *testing.B) {
	for _, cached := range []bool{false, true} {
		name := "parse-every-request"
		if cached {
			name = "cache-hit"
		}
		b.Run(name, func(b *testing.B) {
			h := sdktest.New(b)
			h.SetConfig(`{"deny":["shell"],"replace":{"read":{"description":"approved","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}}`)
			input := requestWithTools(tool("read", "", `{}`, false, ""), tool("shell", "", `{}`, false, ""))
			if result := h.BeforeRequest(input); result.Err != nil {
				b.Fatal(result.Err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if !cached {
					b.StopTimer()
					loadedPolicy.Lock()
					loadedPolicy.ready = false
					loadedPolicy.Unlock()
					b.StartTimer()
				}
				result := h.BeforeRequest(input)
				if result.Err != nil || result.Request == nil || len(result.Request.Tools) != 1 {
					b.Fatalf("unexpected result: %+v", result)
				}
			}
		})
	}
}
