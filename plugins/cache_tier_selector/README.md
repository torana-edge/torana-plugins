# Choose a cache lifetime for a conversation

Choose between configured prompt-cache tiers using observed idle gaps, while keeping a live cached prefix's choice stable.

[All plugins](../../README.md#choose-a-plugin) · [Source](main.go) · [Manifest](plugin.json) · [Settings schema](schema.json)

[Tier selection versus warming](../cache_warmer/ECONOMICS.md)

## Install and inspect

Start [Torana](https://github.com/torana-edge/torana-edge/blob/main/docs/QUICKSTART.md)
first. Commands use `torana` on PATH; use `./torana` from a source checkout.
Run installation from the host checkout or supply its configured plugin
directory with `--dir`. Keep the same `TORANA_DATA_DIR` for local file commands.

```bash
torana plugin install https://github.com/torana-edge/torana-plugins/tree/main/plugins/cache_tier_selector
torana plugin inspect cache_tier_selector
```

Installation builds source locally; it does not approve or enable the bundle.
Review its source, digest and complete permission set. These examples describe
the manifest beside this guide; if your installed revision differs, inspect
and review that revision before proceeding.

## Configure

```bash
torana plugin config get cache_tier_selector > plugin-settings.json
```

Set the snapshot's `config` object to the following, preserving its `revision`:

```json
{
  "mode": "auto"
}
```

```bash
torana plugin config apply cache_tier_selector --file plugin-settings.json --yes
```

Bind the required `request-cache` policy to an actual provider/model with explicit breakpoint tiers. The example uses illustrative Anthropic-shaped markers and prices; verify your endpoint's semantics. Automatic prefix caching alone is not enough.

## Approve and enable

Save this as `approval.json`. Replace the digest with the exact one you reviewed
and replace any provider/model placeholders. Do not paste real secrets here;
model-service authentication belongs to the host provider's credential binding.
Permissions must equal the manifest's requested set; budgets can be lower.

```json
{
  "digest": "sha256:REPLACE_WITH_YOUR_INSPECTED_DIGEST",
  "permissions": [
    "env.cache_policy",
    "env.host_call.torana_plugin_counter",
    "env.log",
    "env.now",
    "env.plugin_config",
    "env.state_get",
    "env.state_keys",
    "env.state_set",
    "ir.cache_control.write"
  ],
  "failure_mode": "pass",
  "prompt_cache_policies": {
    "request-cache": {
      "models": [
        {
          "provider": "your-cache-provider",
          "model": "your-cache-model",
          "cache_read_usd_per_mtok": 0.1,
          "cache_write_usd_per_mtok": 1.25,
          "refresh_on_read": true,
          "warm_interval_seconds": 240,
          "tiers": [
            {
              "ttl_seconds": 300,
              "write_multiplier": 1.25,
              "marker": {
                "type": "ephemeral"
              }
            },
            {
              "ttl_seconds": 3600,
              "write_multiplier": 2,
              "marker": {
                "type": "ephemeral",
                "ttl": "1h"
              }
            }
          ]
        }
      ]
    }
  }
}
```

```bash
torana plugin approve cache_tier_selector --file approval.json --yes
torana plugin enable cache_tier_selector --yes
torana plugin status
```

A missing required binding or stale digest prevents activation. Status should
show the intended bundle loaded, not merely installed. The local UI offers the
same inspect/configure/approve/enable flow. Rebuilds need a new digest approval.

## Try it and check the result

Use a request carrying a supported explicit breakpoint. `mode: "long"` selects the longer tier for a new eligible prefix; `auto` uses observed gaps, `short` leaves the harness default, and `off` makes no changes. A request without an explicit marker is a correct no-op. Watch cache usage and tier-decision counters; do not infer savings from a decision alone.

## Data and failure behavior

Sends no additional inference requests, but may buy a more expensive cache write on the request you already make. It stores prefix decisions and activity privately. Refresh-on-read checkpoints occur at most once per TTL/10; choices may remain sticky up to that much longer, never expire early between checkpoints. Policy changes create a new state scope. Default failure mode is pass.

## Combine or disable

Review other plugins that rewrite the cached prefix: changing content or markers affects its identity. No compactor is required.

```bash
torana plugin disable cache_tier_selector --yes
```

Disabling keeps configuration, approval and private data. `plugin revoke`
also removes approval; neither deletes private files. For errors, start with
`torana plugin status`, `torana feed` and the log path in `torana status`.
See the [CLI reference](https://github.com/torana-edge/torana-edge/blob/main/docs/CLI.md)
for instance selection, revision conflicts and pipeline order.
