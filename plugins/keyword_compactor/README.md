# Trim repeatable tool output without another model

Apply explicit deterministic or keyword policies to selected tool results. Useful for long searches and listings that can be rerun, while keeping source reads exact.

[All plugins](../../README.md#choose-a-plugin) · [Source](main.go) · [Manifest](plugin.json) · [Settings schema](schema.json)

[Shared tool-output policies](../compactor/COMPACTION.md#tool-result-policies) · [DeepSeek experiment](../compactor/DEEPSEEK_RESULTS.md)

## Install and inspect

Start [Torana](https://github.com/torana-edge/torana-edge/blob/main/docs/QUICKSTART.md)
first. Commands use `torana` on PATH; use `./torana` from a source checkout.
Run installation from the host checkout or supply its configured plugin
directory with `--dir`. Keep the same `TORANA_DATA_DIR` for local file commands.

```bash
torana plugin install https://github.com/torana-edge/torana-plugins/tree/main/plugins/keyword_compactor
torana plugin inspect keyword_compactor
```

Installation builds source locally; it does not approve or enable the bundle.
Review its source, digest and complete permission set. These examples describe
the manifest beside this guide; if your installed revision differs, inspect
and review that revision before proceeding.

## Configure

```bash
torana plugin config get keyword_compactor > plugin-settings.json
```

Set the snapshot's `config` object to the following, preserving its `revision`:

```json
{
  "tool_policies": [
    {
      "match": "read*",
      "mode": "exact"
    },
    {
      "match": "web_search",
      "mode": "deterministic",
      "first_pass": true,
      "rerun": "Repeat the search for the complete result."
    },
    {
      "match": "grep*",
      "mode": "keyword"
    }
  ]
}
```

```bash
torana plugin config apply keyword_compactor --file plugin-settings.json --yes
```

The `target` pricing resource is required for savings attribution, even though this plugin makes no model calls. Bind every routed provider/model you intend it to price. Rates below are illustrative, not current provider prices; replace them before approval.

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
    "env.model_pricing",
    "env.host_call.torana_record_savings",
    "env.plugin_config",
    "ir.tool_results.write"
  ],
  "failure_mode": "pass",
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
    }
  }
}
```

```bash
torana plugin approve keyword_compactor --file approval.json --yes
torana plugin enable keyword_compactor --yes
torana plugin status
```

A missing required binding or stale digest prevents activation. Status should
show the intended bundle loaded, not merely installed. The local UI offers the
same inspect/configure/approve/enable flow. Rebuilds need a new digest approval.

## Try it and check the result

Send a sufficiently large successful search result with the matching tool call. Deterministic mode retains bounded head/tail evidence and a recovery marker. Keyword mode waits for at least one exact exposure. Check the feed and `torana stats` for applied changes; a small, unmatched or ineligible result can correctly remain unchanged.

## Data and failure behavior

Unknown tools default to exact; mutation and explicit failure evidence is preserved. Explicit `read*` exact rules protect source reads—there is no automatic age-based source policy. Cache reuse keeps replacements stable. Estimated removed tokens or attributed dollars are not verified workload savings. Default failure mode is pass.

## Combine or disable

Conflicts with `compactor`; disable it before enabling this plugin. Put optional `intent` earlier for captured guidance.

```bash
torana plugin disable keyword_compactor --yes
```

Disabling keeps configuration, approval and private data. `plugin revoke`
also removes approval; neither deletes private files. For errors, start with
`torana plugin status`, `torana feed` and the log path in `torana status`.
See the [CLI reference](https://github.com/torana-edge/torana-edge/blob/main/docs/CLI.md)
for instance selection, revision conflicts and pipeline order.
