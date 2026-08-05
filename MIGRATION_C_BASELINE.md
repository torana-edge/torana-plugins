# Official plugin ABI v2 baseline

Migration C completed on 2026-08-05 at merged revision
`0e8a1014af138588e26ed2d1b9abfdaf65560bf6`.

The nine official modules use ABI v2 and SDK pseudo-version
`v0.2.1-0.20260804120604-995c0bd40baa`. Their manifests are checked against an
exact executable contract table, all bundles rebuild reproducibly, and the
cross-repository behavior gate ran 92 registered rows with zero skips and zero
failures against Edge revision
`5727793c79f1ff1fead3c983624773cd23c931ef`.

No compatibility path for the pre-release flat ABI is retained.
