# PII scan performance evidence

Plugins add CPU and memory costs to the proxy. Measure your selected pipeline,
not just Torana without plugins. The [full plugin-chain report](https://github.com/torana-edge/torana-edge/blob/main/benchmarks/BENCHMARK_PLUGIN_CHAIN_RESULTS_2026-08-18.md)
and [Go/Rust memory report](https://github.com/torana-edge/torana-edge/blob/main/benchmarks/BENCHMARK_WASM_LINEAR_MEMORY_2026-08-18.md)
remain public.

## PII clean-scan experiment — 18 August 2026

A necessary-literal prefilter skipped regex execution only when a match was
impossible. When a literal was present, the original regex remained authoritative.
A test-local unfiltered reference and fuzz seeds checked findings, ordering,
line numbers, deduplication and overflow.

| Measurement on one host | Before | After |
| --- | ---: | ---: |
| Clean 16 KiB first scan allocation | 1,265,408 bytes / 29 allocations | 0 bytes / 0 allocations |
| Clean scan time | ~3.31 ms first; ~1.09 ms warm | ~3.3 µs |
| WASM instance after initialization | 7.00 MiB | 7.00 MiB |
| After one real hook | 8.50 MiB | 7.50 MiB |

The real hook was invoked on each of four targeted WASM instances. These are
historical clean-input measurements, not end-to-end scan latency, a benchmark
of today's ABI, or a guarantee of detection. Inputs triggering regex work or
contextual model calls have different costs.

[Raw records](pii-scan-memory-2026-08-18.jsonl) retain the measurement metadata.
The original investigation is also retrievable from this repository at commit
`fdca604551e586802c1852282da1f05d35ecb043`. No raw performance data was removed.
