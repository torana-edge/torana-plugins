# Check tool output before forwarding it

Scan selected tool results with deterministic patterns and an operator-bound
contextual model. Replace a finding with a recoverable, value-free tool error
so the primary model can continue safely. If you want a
zero-model guard for recognizable values, use [`pii_guard`](../pii_guard/README.md).

This is the recommended first plugin when you already have a local model
endpoint. Bind the scanner locally so tool output does not make an additional
trip to a remote model service.

[All plugins](../../README.md#choose-a-plugin) · [Source](main.go) · [Manifest](plugin.json) · [Settings schema](schema.json)

[Measured clean-scan costs](PERFORMANCE.md)

## Install and inspect

Start [Torana](https://github.com/torana-edge/torana-edge/blob/main/docs/QUICKSTART.md)
first. Commands use `torana` on PATH; use `./torana` from a source checkout.
Run installation from the host checkout or supply its configured plugin
directory with `--dir`. Keep the same `TORANA_DATA_DIR` for local file commands.

```bash
torana plugin install https://github.com/torana-edge/torana-plugins/tree/main/plugins/pii
torana plugin inspect pii
```

Installation builds source locally; it does not approve or enable the bundle.
Review its source, digest and complete permission set. These examples describe
the manifest beside this guide; if your installed revision differs, inspect
and review that revision before proceeding.

## Configure

```bash
torana plugin config get pii > plugin-settings.json
```

Set the snapshot's `config` object to the following, preserving its `revision`:

```json
{
  "tools": [
    "*"
  ],
  "on_error": "block",
  "max_scan_bytes": 32768
}
```

```bash
torana plugin config apply pii --file plugin-settings.json --yes
```

The `scanner` model service is required even when a deterministic pattern may
detect a finding first. The example assumes a local OpenAI-compatible server.
Add provider `local-scanner` with URL `http://127.0.0.1:11434`, format `openai`,
auth mode `none`, then bind the model you actually loaded. Adjust limits within
the manifest ceilings. A remote binding sends eligible tool text to that
provider; call this setup local only when the bound endpoint and model are local.

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
    "env.model_complete",
    "env.plugin_config",
    "env.state_get",
    "env.state_set",
    "ir.cache_control.write",
    "ir.tool_result_content.write",
    "ir.tool_result_errors.write",
    "ir.tool_results.write"
  ],
  "failure_mode": "block",
  "model_services": {
    "scanner": {
      "provider": "local-scanner",
      "model": "your-loaded-model",
      "path": "/v1/chat/completions",
      "timeout_ms": 30000,
      "max_tokens": 512,
      "max_input_bytes": 65536,
      "max_calls_per_minute": 4,
      "max_tokens_per_hour": 20000
    }
  }
}
```

```bash
torana plugin approve pii --file approval.json --yes
torana plugin enable pii --yes
torana plugin status
```

A missing required binding or stale digest prevents activation. Status should
show the intended bundle loaded, not merely installed. The local UI offers the
same inspect/configure/approve/enable flow. Rebuilds need a new digest approval.

## Try it and check the result

Use synthetic tool output such as `contact: someone@example.com` with a matching tool-call ID. Expect that result to become a value-free tool error before the primary provider receives it. The model can then skip the affected lines or request a narrower read. Also test a clean result and an unavailable scanner. Only complete clean scans are cached; unsupported content or truncation is governed by `on_error`, not cached as clean.

## Data and failure behavior

The scanner receives eligible tool-output text. If bound remotely, that text leaves your machine before the primary request is allowed. This is a tool-result guard, not a scanner for all user prompts, a comprehensive DLP system or a guarantee of detection. `on_error: allow` permits undecidable scans; the approval's failure mode separately controls hook failures. Defaults are block. Torana durably stores only hashes and safe replacement messages, never the original sensitive output, so the same decisions replay across later turns and restarts. Only the newest tool-result batch is scanned; historical output is changed only when replaying an earlier decision.

## Combine or disable

Place before plugins that reduce tool output so it sees the original content.
Torana rejects a pipeline that places a tool-result writer after a compaction
gate. Do not enable this plugin with `pii_guard`: this plugin already performs
the deterministic check before its contextual scan, and the manifests declare
the pair as conflicting.

```bash
torana plugin disable pii --yes
```

Disabling keeps configuration, approval and private data. `plugin revoke`
also removes approval; neither deletes private files. For errors, start with
`torana plugin status`, `torana feed` and the log path in `torana status`.
See the [CLI reference](https://github.com/torana-edge/torana-edge/blob/main/docs/CLI.md)
for instance selection, revision conflicts and pipeline order.
