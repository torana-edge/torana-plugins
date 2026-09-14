# Official Torana Plugins

This repository contains the source for the first-party plugins distributed
through the curated Torana registry. Plugins are separate from the proxy so
they can be followed, audited, released, and used as authoring examples.

Each plugin requests capabilities in `plugin.json`; users approve those
requests for the exact installed artifact. A request in a manifest is never a
grant.

All eleven official plugins use Torana's ABI v1 and pin the same SDK revision. Their
manifest ABI, hook, permission, and upstream contracts are checked as one
executable release inventory.

## Build locally

```bash
./scripts/test.sh
./scripts/build.sh pii
./scripts/package.sh pii 0.1.0
```

The workspace resolves `../torana-plugin-sdk` during local development. An
external plugin should pin the SDK matching its target host:

```bash
go get github.com/torana-edge/torana-plugin-sdk@v0.4.3-0.20260914113223-3eed3394e409
```

This foundation uses ABI v1 contract revision 1. Package versions and ABI
revisions are distinct; older bundles must be rebuilt against this SDK for
the coordinated Edge upgrade. `torana plugin new` generates a project pinned
to the SDK used by that host.

Build artifacts are written to `dist/` and are deliberately not committed.

## Writing your own plugin

**You do not need this repository.** Plugins live in their own repos — there is
no index to register with and nothing to publish. Put yours anywhere and users
install it by path:

```bash
torana plugin install https://github.com/you/your-plugin
```

Torana fetches the source, builds it locally, and prints the digest of what it
built, so nobody runs a binary they could not have read. Start at
[WRITING_A_PLUGIN.md](https://github.com/torana-edge/torana-plugin-sdk/blob/main/docs/WRITING_A_PLUGIN.md).

This repository is only the first-party set — see
[CONTRIBUTING.md](CONTRIBUTING.md) if you want to propose one for it.

## Official plugins

- `usage_logger` — the recommended first plugin: writes content-free request, latency, and token usage to a private rotating JSONL file.
- `auth` — virtual-key and request-header identity normalization.
- `cache_tier_selector` — buys the cheapest prompt-cache lifetime per conversation.
- `cache_warmer` — keeps a chosen conversation's cache alive across an idle gap.
- `compactor` — economically gated cheap-model tool-result compaction; cached intent improves relevance but is optional.
- `intent` — captures tool-call intent for compaction policies.
- `keyword_compactor` — deterministic keyword compaction with cached-intent or bounded local guidance.
- `otel` — request metrics and a minimal plugin HTTP endpoint.
- `pii` — local-model and regex PII request guard.
- `schema_translator` — translates map schemas for constrained providers.
- `tool_governor` — restricts or replaces model-visible tool definitions; it is policy, not an execution sandbox.

When combining them, put `tool_governor` before `intent` and
`schema_translator`: governance applies to the harness's original definitions,
then the later plugins may add intent fields or translate an approved schema
for the provider. Run only one of the two compactors; their manifests declare
that conflict and the host enforces it before loading either guest.

Intent history restoration requires the same host conversation ID, tool-call ID,
tool name, and arguments as the captured call. Harnesses that remap call IDs, or
requests without a conversation identity, use the configured heuristic fill (or
leave history unchanged with `fill: off`). Equal arguments alone cannot identify
why an earlier call was made; old arguments-only cache entries are ignored.

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
tenant for every request. Those were removed precisely because a security stub
that returns success is worse than no stub at all.

### Local SDK override

For development against a checked out SDK, set `TORANA_SDK_DIR` to its
absolute path. The scripts validate the module before using it:

```bash
TORANA_SDK_DIR=/path/to/torana-plugin-sdk ./scripts/test.sh
TORANA_SDK_DIR=/path/to/torana-plugin-sdk ./scripts/build.sh pii
```

Release builds continue to use the SDK revision pinned by each plugin's
`go.mod`.

### Coordinated SDK and host reviews

Every module pins the same SDK revision recorded in `SDK_REF`. For coordinated
PRs, the Go pseudo-version and full SDK commit make that revision fetchable
before a release. PR CI checks out that exact commit with `--review`; default
and release checkouts still require the SDK revision to be reachable from main.

`EDGE_REVIEW_REF`, when present, pins the corresponding Edge commit for PR
integration tests. CI on main always uses Edge main. Update or remove the review
pin when rebasing the coordinated change. To reproduce the PR locally, check out
those two revisions as `../torana-plugin-sdk` and `../torana-edge`, then run
`./scripts/test.sh`, build every bundle with `./scripts/build.sh`, and run
`./scripts/verify-behaviour.sh ../torana-edge dist`.
