# Plugins for your workflow

Add one useful behavior to Torana, then make it yours. These are the sources
for the ten official catalogue plugins: telemetry, tool policy, checks and
optional context experiments.

[Get Torana running](https://github.com/torana-edge/torana-edge/blob/main/docs/QUICKSTART.md) ·
[Browse the website catalogue](https://torana.sh/plugins/) ·
[Write a plugin](https://github.com/torana-edge/torana-plugin-sdk/blob/main/docs/FIRST_PLUGIN.md)

## Choose a plugin

| Plugin | Use it to… |
| --- | --- |
| [`usage_logger`](plugins/usage_logger/README.md) | See usage without saving prompts |
| [`tool_governor`](plugins/tool_governor/README.md) | Choose the tools your model sees |
| [`otel`](plugins/otel/README.md) | Add request metrics |
| [`schema_translator`](plugins/schema_translator/README.md) | Adapt map-shaped tool schemas |
| [`intent`](plugins/intent/README.md) | Carry the reason for a tool call |
| [`keyword_compactor`](plugins/keyword_compactor/README.md) | Trim repeatable tool output without another model |
| [`compactor`](plugins/compactor/README.md) | Summarize selected historical tool results |
| [`pii`](plugins/pii/README.md) | Check tool output before forwarding it |
| [`cache_tier_selector`](plugins/cache_tier_selector/README.md) | Choose a cache lifetime for a conversation |
| [`cache_warmer`](plugins/cache_warmer/README.md) | Keep one conversation's cache warm for a bounded gap |

Start with `usage_logger` if you want a visible result without changing
payloads. Every guide includes settings, exact permissions, required resource
bindings, a CLI setup path and a way to check the result.

```bash
torana plugin install https://github.com/torana-edge/torana-plugins/tree/main/plugins/usage_logger
torana plugin inspect usage_logger
```

Installation compiles source locally. It never approves or enables a plugin.
Follow the guide for your installed revision, inspect the digest, approve the
complete requested permission set and bounded resources, then enable it.

## Combine deliberately

Put `tool_governor` before `intent` and `schema_translator`. Put optional
`intent` before a compactor. Run only one of `keyword_compactor` or
`compactor`; the host enforces their conflict. Individual guides explain
data flow and other ordering considerations.

Torana runs locally, but plugins with approved model or network resources may
send data to those destinations. The PII scanner and model compactor are not
necessarily local: you choose their model-service bindings.

## Share your own

Your plugin can live in its own repository. You do not need a registry listing
to install it. Go plugins can use Git URLs; Rust projects are cloned and reviewed
locally before building, including their dependencies and build scripts.

Use the [Go/Rust SDK](https://github.com/torana-edge/torana-plugin-sdk), then
[request a catalogue listing](https://torana.sh/plugins/submit/) if you would
like other users to find it. Contributions to the official set are welcome;
see [CONTRIBUTING.md](CONTRIBUTING.md).

## Build and test this repository

```bash
./scripts/test.sh
./scripts/build.sh usage_logger
```

Use a sibling SDK checkout or `TORANA_SDK_DIR` for coordinated local development.
Release builds use the exact SDK pin in `SDK_REF` and each module. Current
official sources target ABI v1, contract revision 1. Build output stays in
`dist/`, not source control.

## The auth reference is not a catalogue plugin

`plugins/auth` is an eleventh, reference-only capability example. It is
excluded from the public registry and is not an authentication boundary:
explicit verifier rejection blocks, but unavailable verification and its
`failure_mode: pass` policy can allow traffic. Do not deploy it as access
control. The reference remains in the executable release inventory.

## Evidence

[PII scan memory measurements](docs/PERFORMANCE.md) retain the measured limits
of that experiment. Full proxy/plugin-chain CPU and memory results remain
[public in Edge](https://github.com/torana-edge/torana-edge/blob/main/benchmarks/README.md).
