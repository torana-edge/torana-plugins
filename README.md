# Official Torana Plugins

This repository contains the source for the first-party plugins distributed
through the curated Torana registry. Plugins are separate from the proxy so
they can be followed, audited, released, and used as authoring examples.

Each plugin requests capabilities in `plugin.json`; users approve those
requests for the exact installed artifact. A request in a manifest is never a
grant.

All nine official plugins use ABI v2 and pin the same SDK revision. Their
manifest ABI, hook, permission, and upstream contracts are checked as one
executable release inventory.

## Build locally

```bash
./scripts/test.sh
./scripts/build.sh pii
./scripts/package.sh pii 0.1.0
```

The workspace resolves `../torana-plugin-sdk` during local development. An
external plugin should depend on the released SDK module instead. Build
artifacts are written to `dist/` and are deliberately not committed.

## Writing your own plugin

**You do not need this repository.** Plugins live in their own repos — there is
no index to register with and nothing to publish. Put yours anywhere and users
install it by path:

```bash
torana plugin install github.com/you/your-plugin
```

Torana fetches the source, builds it locally, and prints the digest of what it
built, so nobody runs a binary they could not have read. Start at
[WRITING_A_PLUGIN.md](https://github.com/torana-edge/torana-plugin-sdk/blob/main/docs/WRITING_A_PLUGIN.md).

This repository is only the first-party set — see
[CONTRIBUTING.md](CONTRIBUTING.md) if you want to propose one for it.

## Official plugins

- `auth` — virtual-key and request-header identity normalization.
- `cache_tier_selector` — buys the cheapest prompt-cache lifetime per conversation.
- `cache_warmer` — keeps a chosen conversation's cache alive across an idle gap.
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
control. Its reference policy treats a verifier's explicit `rejected` answer as
authoritative and emits a value-free 401 block; an unwired or temporarily
unavailable verifier remains advisory and does not block. Its manifest also
deliberately uses `failure_mode: pass`: transport, protocol, and contract errors
fail open because this is a capability example, not an authentication boundary.
A production auth plugin must use a fail-closed policy instead.

An earlier iteration of this plugin shipped hardcoded stubs that returned a dummy
tenant for every request. Those were removed (torana-edge#130) precisely because a
security stub that returns success is worse than no stub at all.
