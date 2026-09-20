# Keep one conversation's cache warm for a bounded gap

Opt a conversation into periodic refresh requests, with a deadline and break-even budget. Useful only for explicit cache markers whose lifetime refreshes on a read.

[All plugins](../../README.md#choose-a-plugin) · [Source](main.go) · [Manifest](plugin.json) · [Settings schema](schema.json)

[When warming is worth it](ECONOMICS.md)

## Install and inspect

Start [Torana](https://github.com/torana-edge/torana-edge/blob/main/docs/QUICKSTART.md)
first. Commands use `torana` on PATH; use `./torana` from a source checkout.
Run installation from the host checkout or supply its configured plugin
directory with `--dir`. Keep the same `TORANA_DATA_DIR` for local file commands.

```bash
torana plugin install https://github.com/torana-edge/torana-plugins/tree/main/plugins/cache_warmer
torana plugin inspect cache_warmer
```

Installation builds source locally; it does not approve or enable the bundle.
Review its source, digest and complete permission set. These examples describe
the manifest beside this guide; if your installed revision differs, inspect
and review that revision before proceeding.

## Configure

```bash
torana plugin config get cache_warmer > plugin-settings.json
```

Set the snapshot's `config` object to the following, preserving its `revision`:

```json
{
  "conversations": "",
  "warm_for_minutes": 15,
  "interval_seconds_override": 0
}
```

```bash
torana plugin config apply cache_warmer --file plugin-settings.json --yes
```

Bind required `warm-cache` to the warmed provider/model, prices, lifetimes and refresh semantics. The route needs its own host-managed credential (or auth `none` for a compatible local service); a background tick cannot borrow a caller key. Also set a tick interval and per-plugin egress budget as shown below. The example rates are illustrative, not current provider prices.

## Approve and enable

Save this as `approval.json`. Replace the digest with the exact one you reviewed
and replace any provider/model placeholders. Do not paste real secrets here;
model-service authentication belongs to the host provider's credential binding.
Permissions must equal the manifest's requested set; budgets can be lower.

```json
{
  "digest": "sha256:REPLACE_WITH_YOUR_INSPECTED_DIGEST",
  "permissions": [
    "env.background_tick",
    "env.cache_policy",
    "env.host_call.torana_send_request",
    "env.now",
    "env.plugin_config",
    "env.state_get",
    "env.state_keys",
    "env.state_set"
  ],
  "failure_mode": "pass",
  "prompt_cache_policies": {
    "warm-cache": {
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
torana plugin approve cache_warmer --file approval.json --yes
torana plugin enable cache_warmer --yes
torana plugin status
```

A missing required binding or stale digest prevents activation. Status should
show the intended bundle loaded, not merely installed. The local UI offers the
same inspect/configure/approve/enable flow. Rebuilds need a new digest approval.

## Try it and check the result

First use the empty conversation list to observe without warming. Run `torana conversations --json`, select one ID, set it in `conversations`, then send another real turn so its eligible prefix is captured. During an idle gap, look for attributed `plugin-egress` refreshes. No explicit marker, unsupported refresh semantics, missing state or exhausted budgets should produce no refresh.

If an opted-in conversation is never refreshed, read the plugin's durable
entry for that conversation (`plugin-state.json` beside the host
configuration, under key `warm/<conversation-id>`). Its `stopped` field says
why — including `replay artifact exceeds the prefix budget` and `durable state
refused the replay artifact`, the two ways a conversation can be too large to
store. The plugin holds no logging grant, so the entry is where those reasons
are recorded.

## Data and failure behavior

Stores opted-in prefixes in durable private state and sends them again to the configured provider, including while you are idle. It can spend money. Stops at the deadline, break-even count, or a refresh reporting a cache write. It does not extend Gemini cache-resource TTLs or manage automatic OpenAI/DeepSeek prefix caches. Default failure mode is pass; a tick error is logged, not proof of successful warming.

The stored prefix is the whole replayable request, so it is bounded: Torana
caps one durable value at 256 KiB by default, and this plugin splits the
encoded request across at most eight such values — roughly a megabyte, which
covers conversations well past the size where caching pays for itself. A
conversation above that ceiling, or one the store refuses for its own
reasons, is **not warmed**: no partial prefix is kept, nothing is sent, and
the entry records the reason. Superseded prefixes are deleted on the next
tick, so a warmed conversation does not grow the store turn after turn. The
durable store is shared with every other plugin and has a total budget of its
own; warm a handful of conversations, never everything.

## Combine or disable

Optional alongside `cache_tier_selector`; verify the chosen marker and bound policy agree. Never enable globally for every conversation.

```bash
torana plugin disable cache_warmer --yes
```

Disabling keeps configuration, approval and private data. `plugin revoke`
also removes approval; neither deletes private files. For errors, start with
`torana plugin status`, `torana feed` and the log path in `torana status`.
See the [CLI reference](https://github.com/torana-edge/torana-edge/blob/main/docs/CLI.md)
for instance selection, revision conflicts and pipeline order.

### Background runtime settings

Tick cadence and egress budgets are startup runtime settings, outside the
live pipeline API. Follow the host's [runtime settings procedure](https://github.com/torana-edge/torana-edge/blob/main/docs/PLUGINS.md#runtime-settings):
inspect status, stop Torana, edit the reported managed configuration while
stopped, preserve existing settings, then start it again.

Merge this fragment into the existing `plugins.runtime` object:

```json
{
  "tick_interval_seconds": 60,
  "egress": {
    "cache_warmer": {
      "max_calls_per_minute": 4,
      "max_tokens_per_hour": 200000
    }
  }
}
```

This grants neither permissions nor a cache policy; approval is still required.
After restarting, recheck status and the attributed egress feed. Do not set
`conversations` until you have reviewed the destination and spending limits.
