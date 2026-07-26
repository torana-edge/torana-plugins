# Official Torana Plugins

This repository contains the source for the first-party plugins distributed
through the curated Torana registry. Plugins are separate from the proxy so
they can be followed, audited, released, and used as authoring examples.

Each plugin requests capabilities in `plugin.json`; users approve those
requests for the exact installed artifact. A request in a manifest is never a
grant.

## Build locally

```bash
./scripts/test.sh
./scripts/build.sh pii
./scripts/package.sh pii 0.1.0
```

The workspace resolves `../torana-plugin-sdk` during local development. An
external plugin should depend on the released SDK module instead. Build
artifacts are written to `dist/` and are deliberately not committed.

## Official plugins

- `auth` — virtual-key and request-header identity normalization.
- `compactor` — economically gated cheap-model tool-result compaction.
- `intent` — captures tool-call intent for compaction policies.
- `keyword_compactor` — deterministic intent-guided compaction.
- `otel` — request metrics and a minimal plugin HTTP endpoint.
- `pii` — local-model and regex PII request guard.
- `schema_translator` — translates map schemas for constrained providers.

## A note on `auth`

`plugins/auth` ships in this repository but is **deliberately excluded from the public
registry** at torana.sh. It is a reference for the capability surface — how a plugin
requests `env.host_call.verify_virtual_key` and `env.request_headers` — not a
general-purpose authentication plugin, and it should not be deployed as an access
control.

An earlier iteration of this plugin shipped hardcoded stubs that returned a dummy
tenant for every request. Those were removed (torana-edge#130) precisely because a
security stub that returns success is worse than no stub at all.
