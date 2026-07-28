# Contributing to the official plugins

**Most plugins should not live here.** Plugins belong in their own repositories:
there is no index to register with, nothing to publish, and
`torana plugin install github.com/you/your-plugin` works against any git path.
That is the normal way to write and share one, and
[WRITING_A_PLUGIN.md](https://github.com/torana-edge/torana-plugin-sdk/blob/main/docs/WRITING_A_PLUGIN.md)
is the whole story.

This file is for the narrower case: proposing a plugin for the first-party set,
or changing one that is already in it. The bar is higher here because these ship
as examples people copy and as defaults people trust.

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
- **A declared hook must actually be exported** by the WASM binary. The host
  treats a missing export as silent success, so `ValidateHooks` rejects it at
  load rather than letting the plugin do nothing forever.

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

**`schema.json` renders scalars only** — string, number, boolean, enum. A list
has to be a comma-separated string. Values are type-checked against what you
declare before they reach the plugin, so an array where you said string is
rejected at save time.

## Testing

Plugin behaviour is tested from torana-edge, where the WASM runtime lives — but
against bundles built **here**, by this repository's CI:

```bash
# build every bundle, then run torana-edge's plugin-behaviour suite against them
for d in plugins/*/; do ./scripts/build.sh "$(basename "$d")"; done
./scripts/verify-behaviour.sh ../torana-edge dist
```

Expect `62 gated tests ran, 0 skipped`. A *skip* fails that script deliberately:
this is the only place plugin behaviour runs, so a silently-skipped suite is
indistinguishable from one that passes.

**Do not copy your plugin into `torana-edge/plugins/`.** That directory no
longer exists. It used to hold a hand-synced mirror of this repository, and
keeping two trees in step by hand failed exactly once, silently, in the plugin
where it mattered most: the copy there shipped a `pii` cache key bound to
nothing but a `tool_call_id`, so a tool result was skipped **without being
scanned** whenever that id had been cleared before. torana-edge#206.

torana-edge now tests the *host* with purpose-built fixtures in
`examples/plugins/` — is a hook dispatched, is a missing grant refused — and
keeps no copy of these plugins at all. Assertions about what a plugin *does*
live behind `TORANA_PLUGIN_BUNDLES_DIR` and run from here, against bundles
built from the source that owns them.

A test that passes whether or not the code works is worse than no test. Two
examples from this repo's own history: a stickiness test that ran in a mode
where the plugin correctly did nothing, and a warming test whose timestamps
tripped a deadline so every send-path assertion passed for the wrong reason.
Disable your logic, watch the test fail, put it back.

## Releasing

```bash
./scripts/build.sh your_plugin
./scripts/package.sh your_plugin 0.1.0
```

Artifacts go to `dist/` and are not committed. Publishing a new artifact changes
its digest, which invalidates every operator's existing approval — that is the
point of digest-bound approvals, so mention it in the release notes when a
plugin's capabilities change.
