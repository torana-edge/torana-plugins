# PII clean-scan memory profile — 2026-08-18

Edge's per-instance probe found that `pii` was the only official plugin whose
linear memory grew by more than 512 KiB after one clean 16 KiB coding-agent
request. This follow-up isolates and removes that extra growth without changing
the regular expressions or their fail-closed behavior.

## Root cause

The deterministic scanner called all four regular expressions for every line,
even when the line lacked a literal required by every accepting path. The first
clean 16 KiB scan therefore initialized four regexp execution engines:

- first scan: 1,265,408 allocated bytes in 29 allocations;
- warm scan: allocation-free, approximately 1.09 ms on this host.

Each pattern now has a necessary literal prefilter: `@` for email, `-` for US
SSN, `AKIA` for AWS access keys, and `-----BEGIN ` for private keys. Absence of
that literal makes a match impossible; when it is present, the unchanged regex
remains authoritative. A test-local unfiltered reference implementation and
fuzz seeds require byte-for-byte-equivalent findings, ordering, line numbers,
deduplication, and overflow behavior.

## Result

On the same host and current SDK pin:

| Measurement | Before | After |
|---|---:|---:|
| clean 16 KiB first scan | 1,265,408 B, 29 allocs | 0 B, 0 allocs |
| clean 16 KiB scan time | ~3.31 ms median first scan; ~1.09 ms warm | ~3.3 µs |
| PII instance after init | 7.00 MiB | 7.00 MiB |
| PII instance after one real hook | 8.50 MiB | 7.50 MiB |

The rebuilt WASM guest was exercised through Edge's real `run_before_request`
dispatch on each of four deterministically targeted pool instances. The change
removes 1.00 MiB of linear growth per warm PII instance (4.00 MiB at pool size
four) and brings PII in line with the other eight official standard-Go plugins.

The retained records are in
[`pii-scan-memory-2026-08-18.jsonl`](./pii-scan-memory-2026-08-18.jsonl).
These are single-machine engineering measurements, not universal latency
claims. Triggered PII still runs the unchanged regex and retains the exact
security behavior.
