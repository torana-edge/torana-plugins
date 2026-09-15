# Summarize selected historical tool results

Use an operator-bound model for eligible older tool output, only when the estimated benefit clears the cache-rewrite and summarizer-cost gate.

[All plugins](../../README.md#choose-a-plugin) · [Source](main.go) · [Manifest](plugin.json) · [Settings schema](schema.json)

## Install and inspect

Start [Torana](https://github.com/torana-edge/torana-edge/blob/main/docs/QUICKSTART.md)
first. Commands use `torana` on PATH; use `./torana` from a source checkout.
Run installation from the host checkout or supply its configured plugin
directory with `--dir`. Keep the same `TORANA_DATA_DIR` for local file commands.

```bash
torana plugin install https://github.com/torana-edge/torana-plugins/tree/main/plugins/compactor
torana plugin inspect compactor
```

Installation builds source locally; it does not approve or enable the bundle.
Review its source, digest and complete permission set. These examples describe
the manifest beside this guide; if your installed revision differs, inspect
and review that revision before proceeding.

## Configure

```bash
torana plugin config get compactor > plugin-settings.json
```

Set the snapshot's `config` object to the following, preserving its `revision`:

```json
{
  "expected_applications": 6,
  "tool_policies": [
    {
      "match": "read*",
      "mode": "exact"
    },
    {
      "match": "web_search",
      "mode": "model"
    }
  ],
  "max_summarizer_input_bytes": 32768
}
```

```bash
torana plugin config apply compactor --file plugin-settings.json --yes
```

Bind the required `summarizer` model service and both pricing resources: `target` for routed requests and `summarizer` for that exact bound service model. The example assumes a local OpenAI-compatible server. Add its `local-summarizer` provider first (URL `http://127.0.0.1:11434`, format `openai`, auth mode `none`). Replace model names and illustrative target rates. Zero summarizer rates mean explicitly no API charge, not unknown cost.

## Approve and enable

Save this as `approval.json`. Replace the digest with the exact one you reviewed
and replace any provider/model placeholders. Do not paste real secrets here;
model-service authentication belongs to the host provider's credential binding.
Permissions must equal the manifest's requested set; budgets can be lower.

```json
{
  "digest": "sha256:REPLACE_WITH_YOUR_INSPECTED_DIGEST",
  "permissions": [
    "env.cache_get",
    "env.cache_set",
    "env.shared_cache_get",
    "env.emit_metric",
    "env.host_call.torana_evaluate_compaction",
    "env.model_complete",
    "env.model_pricing",
    "env.host_call.torana_record_savings",
    "env.plugin_config",
    "ir.tool_results.write"
  ],
  "failure_mode": "pass",
  "model_services": {
    "summarizer": {
      "provider": "local-summarizer",
      "model": "your-loaded-model",
      "path": "/v1/chat/completions",
      "timeout_ms": 30000,
      "max_tokens": 512,
      "max_input_bytes": 65536,
      "max_calls_per_minute": 4,
      "max_tokens_per_hour": 20000
    }
  },
  "pricing_resources": {
    "target": {
      "models": [
        {
          "provider": "your-provider",
          "model": "your-model",
          "input_usd_per_mtok": 1,
          "output_usd_per_mtok": 4,
          "cache_read_usd_per_mtok": 0.1,
          "cache_write_usd_per_mtok": 1.25
        }
      ]
    },
    "summarizer": {
      "models": [
        {
          "provider": "local-summarizer",
          "model": "your-loaded-model",
          "input_usd_per_mtok": 0,
          "output_usd_per_mtok": 0,
          "cache_read_usd_per_mtok": 0,
          "cache_write_usd_per_mtok": 0
        }
      ]
    }
  }
}
```

```bash
torana plugin approve compactor --file approval.json --yes
torana plugin enable compactor --yes
torana plugin status
```

A missing required binding or stale digest prevents activation. Status should
show the intended bundle loaded, not merely installed. The local UI offers the
same inspect/configure/approve/enable flow. Rebuilds need a new digest approval.

## Try it and check the result

Use a long successful historical search result that has already appeared unchanged to the model, and a realistic positive `expected_applications`. Observe model calls as `plugin-egress` in `torana feed`. The gate may correctly decline before or after a call; missing price/usage is not treated as free. Check stats and compare task outcomes, not just bytes removed.

## Data and failure behavior

Selected output and guidance go to the bound summarizer, which can be local or remote. Remote summarization exposes that content to another provider and can incur charges. Mutation/failure evidence stays exact; model mode never reduces the first exposure. Cached replacements remain stable. This is optional and does not promise cheaper or equally good sessions. Default failure mode is pass.

## Combine or disable

Conflicts with `keyword_compactor`; disable it first. Put optional `intent` and any route-changing plugin before compaction.

```bash
torana plugin disable compactor --yes
```

Disabling keeps configuration, approval and private data. `plugin revoke`
also removes approval; neither deletes private files. For errors, start with
`torana plugin status`, `torana feed` and the log path in `torana status`.
See the [CLI reference](https://github.com/torana-edge/torana-edge/blob/main/docs/CLI.md)
for instance selection, revision conflicts and pipeline order.

## Count the whole experiment

Every successful summarizer response costs something when the bound service is
paid, even if its content is unusable or the batch never applies. Applied
savings reports alone exclude that discarded work:

```text
workload net = applied gross savings - applied rewrite premiums
               - all summarizer egress costs
```

Do not subtract all egress from applied **net** savings: that double-counts
successful summaries. Reconcile every attempt's provider/model/usage and rates
within the same workload. Missing usage, prices or event retention makes the
total unknown. The bounded feed is not a durable accounting ledger. The
[public experiment](https://torana.sh/blog/context-compaction-negative-result/)
shows why fewer tokens need not mean cheaper sessions.
