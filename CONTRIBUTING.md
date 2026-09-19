# Contributing to Torana's plugin examples

Fix a plugin, improve a guide, or propose another maintained example.
Include a concrete use case and a small reproducible test.

You can also keep a plugin in your own repository and share its install URL.
Start with the [first-plugin tutorial](https://github.com/torana-edge/torana-plugin-sdk/blob/main/docs/FIRST_PLUGIN.md).
A website listing is optional and never grants permissions.

Plugins in this repository target ABI v1. New or changed plugins must use the
repository's pinned Go SDK, declare the exact grants they exercise, and remain consistent
with the executable twelve-module contract table. Eleven modules are public
examples; `auth` remains a reference-only integration but is still part of the
release inventory.

## Local setup

```bash
git clone https://github.com/torana-edge/torana-plugins
git clone https://github.com/torana-edge/torana-plugin-sdk   # sibling directory
cd torana-plugins
./scripts/test.sh
./scripts/build.sh pii
```

`scripts/build.sh` builds each plugin in a throwaway Go workspace that points at
`../torana-plugin-sdk` when it exists, so the two repos must be siblings. There
is deliberately no committed `go.work`: a checked-in workspace would make the
local SDK path part of the repo, and released plugins depend on the published
SDK module.

Routine builds and tests use Go's persistent build cache, including any
`GOCACHE` you explicitly set. The temporary workspace is still removed after
each invocation. For an intentionally cold reproducibility check, supply a
temporary `GOCACHE` yourself; normal builds should reuse dependency compilation
across plugins and invocations.

## Adding a plugin

```
plugins/your_plugin/
  go.mod          module github.com/torana-edge/torana-plugins/plugins/your_plugin
  main.go         package main, hooks registered in init(), empty func main()
  plugin.json     manifest — hooks and requested capabilities
  schema.json     optional — the config fields the control plane renders
  agent.json      optional — JSON operations for the agent API
```

Two things are easy to miss and both stop a plugin loading:

- **`"schema_version": 1` is required** in `plugin.json`. Without it the
  manifest fails validation and the plugin is skipped.
- **The manifest hooks must exactly match the exported bitmap.** ABI v1 uses
  one `run_hook` dispatcher plus `supported_hooks`, not separate exports for
  each hook. The host rejects a mismatch or incompatible ABI at load time.

## What the review looks for

**Determinism.** Anything a plugin writes into a request must be a pure function
of its input. Injecting a timestamp, a random value, or a request ID changes the
cacheable prefix, which invalidates the provider's prompt cache and multiplies
the operator's token spend on *every subsequent turn*. Torana enforces this: new
request-mutating plugins are added to `cache_compliance_test.go` in torana-edge,
which runs the plugin twice over an identical request and compares bytes.

**Failing closed when it costs money.** A plugin that cannot price its action
should decline to take it. Unknown pricing, an unconfigured provider, or a
missing capability are all cases where doing nothing is correct and guessing is
expensive. `cache_warmer` and `cache_tier_selector` are the worked examples —
most of their code is about refusing.

**Least privilege in the manifest.** Every capability you request is one the
operator has to approve, and the description you write is what they read while
deciding. Say what you do with it, not what it is.

**Use JSON Schema for settings.** The host validates the complete document,
including nested objects, arrays and `additionalProperties`. The UI renders
scalar controls and uses raw JSON for structured settings; a list need not be
encoded as a comma-separated string. Keep each plugin's README examples in
sync with its schema and required approval resources.

## Testing

Plugin behaviour is tested from torana-edge, where the WASM runtime lives — but
against bundles built **here**, by this repository's CI:

```bash
# build every bundle, then run torana-edge's plugin-behaviour suite against them
for d in plugins/*/; do ./scripts/build.sh "$(basename "$d")"; done
./scripts/verify-behaviour.sh ../torana-edge dist
```

Expect every currently registered gated row to run with zero skips. A *skip*
fails that script deliberately:
this is the only place plugin behaviour runs, so a silently-skipped suite is
indistinguishable from one that passes.

Keep plugin source here, not duplicated inside Edge. Edge's purpose-built
fixtures test the host; behavior tests use `TORANA_PLUGIN_BUNDLES_DIR` to
exercise bundles built from their owning source.

Test a case where the plugin acts, one where it should leave input unchanged,
and a failure. Temporarily disabling the behavior should make its action test
fail; setup that accidentally exercises a no-op is not coverage.

### Coordinated SDK and host changes

All plugin modules and `SDK_REF` pin the same SDK revision. A review pin must be
fetchable; release builds require it to be reachable from SDK main.
`EDGE_REVIEW_REF`, when present, pins the Edge commit for PR integration;
main CI uses Edge main. Reproduce with those sibling checkouts, then run the
behavior suite above. Land owning SDK changes before their consumers.

## Releasing

```bash
./scripts/build.sh your_plugin
./scripts/package.sh your_plugin 0.1.0
```

Artifacts go to `dist/` and are not committed. Publishing a new artifact changes
its digest, which invalidates every operator's existing approval — that is the
point of digest-bound approvals, so mention it in the release notes when a
plugin's capabilities change.
