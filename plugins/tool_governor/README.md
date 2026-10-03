# Choose the tools your model sees

Restrict or replace model-visible tool definitions. For example, expose search and read tools while omitting deployment tools during a review.

[All plugins](../../README.md#choose-a-plugin) · [Source](main.go) · [Manifest](plugin.json) · [Settings schema](schema.json)

## Install and inspect

Start [Torana](https://github.com/torana-edge/torana-edge/blob/main/docs/QUICKSTART.md)
first. Commands use `torana` on PATH; use `./torana` from a source checkout.
The CLI discovers your running instance and its plugin directory. Use `--dir`
only to target a different directory. Keep the same `TORANA_DATA_DIR` if you
explicitly set it.

```bash
torana plugin install tool_governor
torana plugin inspect tool_governor
```

Installation downloads a verified release; no Git or Go needed. It never
approves or enables the bundle.
Review its source, digest and complete permission set. These examples describe
the manifest beside this guide; if your installed revision differs, inspect
and review that revision before proceeding.

## Configure

```bash
torana plugin config get tool_governor > plugin-settings.json
```

Set the snapshot's `config` object to the following, preserving its `revision`:

```json
{
  "allow": [
    "read_file",
    "web_search"
  ]
}
```

```bash
torana plugin config apply tool_governor --file plugin-settings.json --yes
```

No resource bindings are required. Review the configuration, session-state,
agent-operation, and tool/cache-marker permissions.

## Approve and enable

Save this as `approval.json`. Replace the digest with the exact one you reviewed
and replace any provider/model placeholders. Do not paste real secrets here;
model-service authentication belongs to the host provider's credential binding.
Permissions must equal the manifest's requested set; budgets can be lower.

```json
{
  "digest": "sha256:REPLACE_WITH_YOUR_INSPECTED_DIGEST",
  "permissions": [
    "env.plugin_config",
    "env.serve_http",
    "env.state_get",
    "env.state_set",
    "ir.cache_control.write",
    "ir.tools.write"
  ],
  "failure_mode": "block"
}
```

```bash
torana plugin approve tool_governor --file approval.json --yes
torana plugin enable tool_governor --yes
torana plugin status
```

A missing required binding or stale digest prevents activation. Status should
show the intended bundle loaded, not merely installed. The local UI offers the
same inspect/configure/approve/enable flow. Rebuilds need a new digest approval.

## Try it and check the result

Send a request containing `read_file`, `web_search` and `deploy` tool definitions to a test backend. Only the first two should reach it. `allow: []` removes all definitions; omitting `allow` leaves them eligible. `deny` must not overlap `allow`. `replace` changes a retained tool's description, parameters or strict flag; it does not add a missing tool.

With Torana's MCP connected, the model can ask to allow one tool omitted by the
operator's allowlist for the verified session. An explicit entry in `deny`
cannot be overridden. Torana shows the host-generated change for your confirmation;
the plugin cannot apply it from an unbound call. The allowance is local to that
session and durable across restarts. Undo it from Torana's change history. If a
later allowance has replaced the change, undo refuses instead of overwriting
the newer state. Allowing or undoing a tool changes the model-visible tool list,
so the next request can rebuild that conversation's prompt cache. This changes
only what the model sees; your harness still owns tool execution and its own
approval prompts. Disabling the plugin suspends the policy but keeps its local
state, including session allowances.

An operator configuration change invalidates existing session allowances. This
keeps a newly reviewed global policy authoritative instead of silently carrying
older exceptions into it.

## Data and failure behavior

This controls advertised definitions, not execution. Your harness still executes tools and owns its approvals; a model can propose a call it was not shown. Config is read on every request. Session allowances are read only when the base policy restricts a tool. A valid unset/empty value or `{}` leaves tools unchanged; malformed/whitespace policy, corrupt session state and host-call failures error rather than reuse an old policy. Default failure mode is block.

## Combine or disable

Place before `intent` and `schema_translator` so policy is applied to the harness's original definitions. Preserve existing pipeline entries when reordering.

```bash
torana plugin disable tool_governor --yes
```

Disabling keeps configuration, approval and private data. `plugin revoke`
also removes approval; neither deletes private files. For errors, start with
`torana plugin status`, `torana feed` and the log path in `torana status`.
See the [CLI reference](https://github.com/torana-edge/torana-edge/blob/main/docs/CLI.md)
for instance selection, revision conflicts and pipeline order.
