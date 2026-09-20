# Adapt map-shaped tool schemas

Convert supported open-map tool parameters into key/value arrays for providers that need a constrained schema, then reverse the recorded conversion on the model's tool calls, streamed or not.

[All plugins](../../README.md#choose-a-plugin) · [Source](main.go) · [Manifest](plugin.json) · [Settings schema](schema.json)

## Install and inspect

Start [Torana](https://github.com/torana-edge/torana-edge/blob/main/docs/QUICKSTART.md)
first. Commands use `torana` on PATH; use `./torana` from a source checkout.
Run installation from the host checkout or supply its configured plugin
directory with `--dir`. Keep the same `TORANA_DATA_DIR` for local file commands.

```bash
torana plugin install https://github.com/torana-edge/torana-plugins/tree/main/plugins/schema_translator
torana plugin inspect schema_translator
```

Installation builds source locally; it does not approve or enable the bundle.
Review its source, digest and complete permission set. These examples describe
the manifest beside this guide; if your installed revision differs, inspect
and review that revision before proceeding.

## Configure

```bash
torana plugin config get schema_translator > plugin-settings.json
```

Set the snapshot's `config` object to the following, preserving its `revision`:

```json
{}
```

```bash
torana plugin config apply schema_translator --file plugin-settings.json --yes
```

No resource bindings or custom settings are required. The plugin keeps its conversion map in request-scoped metadata.

## Approve and enable

Save this as `approval.json`. Replace the digest with the exact one you reviewed
and replace any provider/model placeholders. Do not paste real secrets here;
model-service authentication belongs to the host provider's credential binding.
Permissions must equal the manifest's requested set; budgets can be lower.

```json
{
  "digest": "sha256:REPLACE_WITH_YOUR_INSPECTED_DIGEST",
  "permissions": [
    "env.meta_get",
    "env.meta_set",
    "ir.messages.write.assistant",
    "ir.stream.write",
    "ir.tools.write"
  ],
  "failure_mode": "pass"
}
```

```bash
torana plugin approve schema_translator --file approval.json --yes
torana plugin enable schema_translator --yes
torana plugin status
```

A missing required binding or stale digest prevents activation. Status should
show the intended bundle loaded, not merely installed. The local UI offers the
same inspect/configure/approve/enable flow. Rebuilds need a new digest approval.

## Try it and check the result

Use a test tool with an `additionalProperties` map and inspect the provider-facing definition: eligible maps become arrays of key/value entries. Verify the harness receives the original map shape, on a streamed call and on a non-streamed one. Test the exact nested schemas and response paths you use; this is not arbitrary schema conversion.

## Data and failure behavior

This plugin adapts tool schemas. It is not the protocol bridge and does not change the provider API. Reversal runs on both response paths — the stream hook for streamed tool calls, the after-response hook for non-streamed ones — and only for the conversions this request recorded; tool calls it did not translate are passed through byte for byte, signature included. A tool call whose arguments it does change loses its provider signature, which is the prescribed response to changing the content that signature covers. Missing or malformed conversion state is terminal rather than guessed, and errors under the approved failure policy (default pass); verify actual outputs before enabling broadly.

## Combine or disable

Place after `tool_governor`. If using `intent`, keep its injected field inside the schema you test with the translator.

```bash
torana plugin disable schema_translator --yes
```

Disabling keeps configuration, approval and private data. `plugin revoke`
also removes approval; neither deletes private files. For errors, start with
`torana plugin status`, `torana feed` and the log path in `torana status`.
See the [CLI reference](https://github.com/torana-edge/torana-edge/blob/main/docs/CLI.md)
for instance selection, revision conflicts and pipeline order.
