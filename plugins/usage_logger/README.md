# See usage without saving prompts

Write one content-free JSONL record for each completed response: provider, model, status, latency and reported token counts.

[All plugins](../../README.md#choose-a-plugin) · [Source](main.go) · [Manifest](plugin.json) · [Settings schema](schema.json)

## Install and inspect

Start [Torana](https://github.com/torana-edge/torana-edge/blob/main/docs/QUICKSTART.md)
first. Commands use `torana` on PATH; use `./torana` from a source checkout.
Run installation from the host checkout or supply its configured plugin
directory with `--dir`. Keep the same `TORANA_DATA_DIR` for local file commands.

```bash
torana plugin install https://github.com/torana-edge/torana-plugins/tree/main/plugins/usage_logger
torana plugin inspect usage_logger
```

Installation builds source locally; it does not approve or enable the bundle.
Review its source, digest and complete permission set. These examples describe
the manifest beside this guide; if your installed revision differs, inspect
and review that revision before proceeding.

## Configure

For the quickest setup, open Torana’s local control plane, select
**usage_logger**, review the requested file access and budget, and choose
**Approve and enable**. The default plugin configuration is `{}`. Send another
request and follow [the output](#try-it-and-check-the-result).

Prefer a terminal or an agent-driven workflow? The CLI steps follow below.

```bash
torana plugin config get usage_logger > plugin-settings.json
```

Set the snapshot's `config` object to the following, preserving its `revision`:

```json
{}
```

```bash
torana plugin config apply usage_logger --file plugin-settings.json --yes
```

Approve the required private file `usage.jsonl`. The example permits 16 MiB per generation and five retained rotations. You may lower these limits; the guest cannot select an OS path.

## Approve and enable

Save this as `approval.json`. Replace the digest with the exact one you reviewed.
The usage logger needs only the private file grant shown here; it has no model
service or provider-key setup of its own.
Permissions must equal the manifest's requested set; budgets can be lower.

```json
{
  "digest": "sha256:REPLACE_WITH_YOUR_INSPECTED_DIGEST",
  "permissions": [
    "env.file_append"
  ],
  "failure_mode": "pass",
  "files": {
    "usage.jsonl": {
      "max_bytes": 16777216,
      "retained_files": 5
    }
  }
}
```

```bash
torana plugin approve usage_logger --file approval.json --yes
torana plugin enable usage_logger --yes
torana plugin status
```

A missing required binding or stale digest prevents activation. Status should
show the intended bundle loaded, not merely installed. The local UI offers the
same inspect/configure/approve/enable flow. Rebuilds need a new digest approval.

## Try it and check the result

Send a request from your connected harness, then follow the output with your shell:

```bash
tail -F "$(torana plugin file path usage_logger usage.jsonl)"
```

In PowerShell, use `Get-Content -Wait (torana plugin file path usage_logger usage.jsonl)`.
Torana resolves the running plugin’s absolute file path; your shell reads it.
Use the same `TORANA_DATA_DIR` as the running instance. Its recorded listener
is discovered automatically, including a non-default port. To select an instance
explicitly, use `torana plugin file path --addr 127.0.0.1:9090 usage_logger usage.jsonl`.
Expect a JSON record with `status`, `duration_ms` and `usage_reported`.
Missing usage is not zero usage.

## Data and failure behavior

No prompts, response bodies or headers are written. The file still contains operational identifiers and timing; keep it private. Append failures follow the approved failure policy (default pass), so this is not a guaranteed audit ledger.

## Combine or disable

No special order is required. It observes completed responses; earlier vetoes may prevent its hook from running.

```bash
torana plugin disable usage_logger --yes
```

Disabling keeps configuration, approval and private data. `plugin revoke`
also removes approval; neither deletes private files. For errors, start with
`torana plugin status`, `torana feed` and the log path in `torana status`.
See the [CLI reference](https://github.com/torana-edge/torana-edge/blob/main/docs/CLI.md)
for instance selection, revision conflicts and pipeline order.
