# Plugins for your workflow

Add one useful behavior to Torana, then make it yours. These are the sources
for eleven maintained plugin examples: telemetry, tool policy, checks and
optional context experiments.

[Get Torana running](https://github.com/torana-edge/torana-edge/blob/main/docs/QUICKSTART.md) ·
[Browse plugin listings](https://torana.sh/plugins/) ·
[Write a plugin](https://github.com/torana-edge/torana-plugin-sdk/blob/main/docs/FIRST_PLUGIN.md)

## Choose a plugin

| Plugin | Use it to… |
| --- | --- |
| [`pii`](plugins/pii/README.md) | Use a local model for contextual checks of tool output |
| [`pii_guard`](plugins/pii_guard/README.md) | Block recognizable PII and secrets without a model |
| [`usage_logger`](plugins/usage_logger/README.md) | See usage without saving prompts |
| [`tool_governor`](plugins/tool_governor/README.md) | Choose the tools your model sees |
| [`otel`](plugins/otel/README.md) | Add request metrics |
| [`schema_translator`](plugins/schema_translator/README.md) | Adapt map-shaped tool schemas |
| [`intent`](plugins/intent/README.md) | Carry the reason for a tool call |
| [`keyword_compactor`](plugins/keyword_compactor/README.md) | Trim repeatable tool output without another model |
| [`compactor`](plugins/compactor/README.md) | Summarize selected historical tool results |
| [`cache_tier_selector`](plugins/cache_tier_selector/README.md) | Choose a cache lifetime for a conversation |
| [`cache_warmer`](plugins/cache_warmer/README.md) | Keep one conversation's cache warm for a bounded gap |

Already running Ollama or another OpenAI-compatible local model? Start with
`pii` and bind its scanner to that endpoint. It checks obvious patterns first,
then uses your local model for contextual cases. If you do not have a local
model ready, start with the deterministic `pii_guard` instead. Install only one;
their manifests declare them as conflicting.

Every guide includes settings, exact permissions, required resource bindings,
and a way to check the result. Install your choice from the Torana checkout
where the proxy is running (use `./torana` for a source build):

```bash
torana plugin install https://github.com/torana-edge/torana-plugins/tree/main/plugins/pii
```

Or, without a local model:

```bash
torana plugin install https://github.com/torana-edge/torana-plugins/tree/main/plugins/pii_guard
```

Installation compiles source locally. It never approves or enables a plugin.
Open Torana's local control plane and select the plugin you installed. For
`pii`, configure the required scanner binding before reviewing its permissions
and model-call limits. `pii_guard` makes no model or network calls; review its
requested tool-result and state permissions before enabling it. Follow the
[PII guide](plugins/pii/README.md) or the deterministic guard's
[safe walkthrough](plugins/pii_guard/README.md#try-it-safely).

Prefer terminal or agent automation? Each plugin guide covers its configuration
and lifecycle commands. Other plugins may need their own settings or resource
bindings; each listing links to its owning guide.

## Combine deliberately

Put `tool_governor` before `intent` and `schema_translator`. Put optional
`intent` before a compactor. Run only one of `keyword_compactor` or
`compactor`; the host enforces their conflict. Individual guides explain
data flow and other ordering considerations.

Torana runs locally, but plugins with approved model or network resources may
send data to those destinations. `pii_guard` uses neither. The model-backed PII
scanner and model compactor use the model-service bindings you choose, which
may be local or remote.

## Share your own

Your plugin can live in its own repository. You do not need a website listing
to install it. Go plugins can use Git URLs; Rust projects are cloned and reviewed
locally before building, including their dependencies and build scripts.

Use the [Go/Rust SDK](https://github.com/torana-edge/torana-plugin-sdk), then
[request a website listing](https://torana.sh/plugins/submit/) if you would
like other users to find it. Contributions to this maintained example set are
welcome;
see [CONTRIBUTING.md](CONTRIBUTING.md).

## Build and test this repository

```bash
./scripts/test.sh
./scripts/build.sh usage_logger
```

Use a sibling SDK checkout or `TORANA_SDK_DIR` for coordinated local development.
Release builds use the exact SDK pin in `SDK_REF` and each module. Current
maintained sources target ABI v1, contract revision 1. Build output stays in
`dist/`, not source control.

## The auth reference is not a listed plugin

`plugins/auth` is a twelfth, reference-only capability example. It is
excluded from the public listing and is not an authentication boundary:
explicit verifier rejection blocks, but unavailable verification and its
`failure_mode: pass` policy can allow traffic. Do not deploy it as access
control. The reference remains in the executable release inventory.
Its [README](plugins/auth/README.md) states what it does today, and why none
of it is protection.

## Evidence

[PII scan memory measurements](plugins/pii/PERFORMANCE.md) retain the measured limits
of that experiment. Full proxy/plugin-chain CPU and memory results remain
[public in Edge](https://github.com/torana-edge/torana-edge/blob/main/benchmarks/README.md).
