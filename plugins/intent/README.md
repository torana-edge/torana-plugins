# Carry the reason for a tool call

Ask the model to include why it is making a tool call, strip the added field before the harness sees the call, and make captured intent available to a compactor.

[All plugins](../../README.md#choose-a-plugin) · [Source](main.go) · [Manifest](plugin.json) · [Settings schema](schema.json)

## Install and inspect

Start [Torana](https://github.com/torana-edge/torana-edge/blob/main/docs/QUICKSTART.md)
first. Commands use `torana` on PATH; use `./torana` from a source checkout.
Run installation from the host checkout or supply its configured plugin
directory with `--dir`. Keep the same `TORANA_DATA_DIR` for local file commands.

```bash
torana plugin install https://github.com/torana-edge/torana-plugins/tree/main/plugins/intent
torana plugin inspect intent
```

Installation builds source locally; it does not approve or enable the bundle.
Review its source, digest and complete permission set. These examples describe
the manifest beside this guide; if your installed revision differs, inspect
and review that revision before proceeding.

## Configure

```bash
torana plugin config get intent > plugin-settings.json
```

Set the snapshot's `config` object to the following, preserving its `revision`:

```json
{
  "fill": "heuristic"
}
```

```bash
torana plugin config apply intent --file plugin-settings.json --yes
```

No bound resource slots are required. It requests private cache and separately approved shared-cache writes. The shared cache is visible to other plugins with the corresponding shared-cache grant.

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
    "env.shared_cache_set",
    "env.emit_metric",
    "env.log",
    "env.meta_get",
    "env.meta_set",
    "env.plugin_config",
    "ir.cache_control.write",
    "ir.messages.write.assistant",
    "ir.messages.write.developer",
    "ir.messages.write.other",
    "ir.messages.write.system",
    "ir.messages.write.tool",
    "ir.messages.write.user",
    "ir.stream.write",
    "ir.tool_results.write",
    "ir.tools.write"
  ],
  "failure_mode": "pass"
}
```

```bash
torana plugin approve intent --file approval.json --yes
torana plugin enable intent --yes
torana plugin status
```

A missing required binding or stale digest prevents activation. Status should
show the intended bundle loaded, not merely installed. The local UI offers the
same inspect/configure/approve/enable flow. Rebuilds need a new digest approval.

## Try it and check the result

Inspect the provider-facing tool schema for the added `i` field, then confirm that the harness receives the original tool arguments without it — on a streamed workflow and on a non-streamed one; both response paths capture and strip. Captured intent is restored only for the same host conversation ID, tool-call ID, tool name and inputs. Remapped IDs use heuristic fill, or remain unchanged with `fill: "off"`.

## Data and failure behavior

Adds a model-facing convention and changes tool schemas/history; it is not a guarantee of intent quality. Captured intent and tool-derived cache data can contain workflow context, so they go to the cache a compactor reads and never to the plugin log: diagnostics report that an intent was captured or filled and how long it was, never the text. A tool that declares its own `i` keeps it, value and signature untouched. Heuristic fill uses stable call information, not newer turns. Missing identity/cache entries cannot recover the original intent. Default failure mode is pass.

## Combine or disable

Place after `tool_governor` and before one compactor. Compactors also work without it using bounded local guidance.

```bash
torana plugin disable intent --yes
```

Disabling keeps configuration, approval and private data. `plugin revoke`
also removes approval; neither deletes private files. For errors, start with
`torana plugin status`, `torana feed` and the log path in `torana status`.
See the [CLI reference](https://github.com/torana-edge/torana-edge/blob/main/docs/CLI.md)
for instance selection, revision conflicts and pipeline order.
