# Add request metrics

Emit request shape, latency, observed status classes and provider-reported token usage through Torana's metrics host.

[All plugins](../../README.md#choose-a-plugin) · [Source](main.go) · [Manifest](plugin.json) · [Settings schema](schema.json)

[Read status from an agent](AGENT_OPERATIONS.md)

## Install and inspect

Start [Torana](https://github.com/torana-edge/torana-edge/blob/main/docs/QUICKSTART.md)
first. Commands use `torana` on PATH; use `./torana` from a source checkout.
Run installation from the host checkout or supply its configured plugin
directory with `--dir`. Keep the same `TORANA_DATA_DIR` for local file commands.

```bash
torana plugin install https://github.com/torana-edge/torana-plugins/tree/main/plugins/otel
torana plugin inspect otel
```

Installation builds source locally; it does not approve or enable the bundle.
Review its source, digest and complete permission set. These examples describe
the manifest beside this guide; if your installed revision differs, inspect
and review that revision before proceeding.

## Configure

```bash
torana plugin config get otel > plugin-settings.json
```

Set the snapshot's `config` object to the following, preserving its `revision`:

```json
{}
```

```bash
torana plugin config apply otel --file plugin-settings.json --yes
```

No outbound endpoint binding is requested. The host owns metric export; the plugin does not connect directly to an OpenTelemetry collector.

## Approve and enable

Save this as `approval.json`. Replace the digest with the exact one you reviewed
and replace any provider/model placeholders. Do not paste real secrets here;
model-service authentication belongs to the host provider's credential binding.
Permissions must equal the manifest's requested set; budgets can be lower.

```json
{
  "digest": "sha256:REPLACE_WITH_YOUR_INSPECTED_DIGEST",
  "permissions": [
    "env.emit_metric",
    "env.serve_http"
  ],
  "failure_mode": "pass"
}
```

```bash
torana plugin approve otel --file approval.json --yes
torana plugin enable otel --yes
torana plugin status
```

A missing required binding or stale digest prevents activation. Status should
show the intended bundle loaded, not merely installed. The local UI offers the
same inspect/configure/approve/enable flow. Rebuilds need a new digest approval.

## Try it and check the result

Send an inference request, then open `http://127.0.0.1:8080/_torana/plugin/otel/` for its minimal status page. `torana agent discover` lists its declared status operation. The page is not a metrics dashboard: inspect your configured host metrics exporter for the `torana_plugin_requests_total` and response series.

## Data and failure behavior

No payload rewrite or content export. Labels describe request/model shape. Missing status or usage is not invented. Logging/metrics imports are best effort; a status page alone does not prove collector ingestion. Default failure mode is pass.

## Combine or disable

No special order is required. Host metrics remain the source for outcomes such as vetoes that may bypass this plugin.

```bash
torana plugin disable otel --yes
```

Disabling keeps configuration, approval and private data. `plugin revoke`
also removes approval; neither deletes private files. For errors, start with
`torana plugin status`, `torana feed` and the log path in `torana status`.
See the [CLI reference](https://github.com/torana-edge/torana-edge/blob/main/docs/CLI.md)
for instance selection, revision conflicts and pipeline order.
