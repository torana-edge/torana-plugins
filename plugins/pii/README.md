# Add a local check before tool output reaches your model

Use a local model to look for credentials and private data in new tool results.
When the scanner flags a result, Torana withholds it and sends a value-free tool
error instead, giving your coding agent a chance to continue without that output.
It is an extra check that can catch some accidental exposures—not complete
protection. Detection depends on the model: it can miss a secret or flag harmless
code. For a zero-model check of recognizable formats, use
[`pii_guard`](../pii_guard/README.md).

Already have a local model endpoint? Try it as the scanner and check both
sensitive and harmless examples from your workflow. Bind it locally so the
extra scan does not send tool output to another remote model service. Structured
JSON support makes the response readable; it does not establish detection quality.

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

The `pii` plugin is model-backed. Every eligible successful new tool result
goes to the required `scanner` model
service. Historical results are replayed from safe decisions instead of being
sent to the scanner again. The scanner must support JSON Schema structured
output; current llama.cpp servers support this. Torana supplies numbered lines
and requires a value-free JSON verdict. Reported lines refer to the tool output,
not necessarily the original file's line numbers. The example assumes a local OpenAI-compatible server.
Add provider `local-scanner` with a llama.cpp URL such as `http://127.0.0.1:8081/v1`, format `openai`,
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
    "env.serve_http",
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
      "timeout_ms": 90000,
      "max_tokens": 512,
      "max_input_bytes": 1048576,
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

Try a synthetic credential assignment such as `PAYMENT_API_KEY=sk_test_torana_demo_not_a_real_key_123` in a test configuration file. Ask your harness to read it. If your scanner flags it, Torana replaces that result before forwarding the request to the primary provider. Your agent can skip the result or request other content without the suspected value. Reported locations are lines of the returned tool output, not verified file positions; a model's finding does not establish that every other line is safe.

Also test clean source code, public support addresses, and syntactically redacted placeholders such as `<REDACTED>` or `YOUR_API_KEY`: these should remain readable. Labelling a realistic credential “example” or “not real” is not an exemption. Try a single-line read as well as the whole file: each new result gets its own decision, and a successful whole-file check does not prove narrower reads will be classified correctly. If your model misses the sample or repeatedly flags harmless code, choose another scanner or use the deterministic plugin for its supported formats.

A scanner failure is not a confirmed finding. Check the reported scanner settings, input limits or availability, or skip that result; repeatedly splitting the read does not fix an unreliable scanner. An unavailable scanner, oversized request or incomplete scan follows `on_error`, never a cached clean verdict. With the default `block`, the withheld result is remembered for historical replay. A result exceeding `max_scan_bytes` is rejected before any model call—no prefix scan is performed. The host's separate approved input limit counts the complete model request, including numbered lines and output schema, not just raw tool text. Only complete clean scans are cached; identical content already cleared by the same scan policy is not sent to the scanner again.

The policy does not withhold contact details that appear public, such as documentation or support addresses. This reduces noise in ordinary development, but the model can misjudge public versus private context; evaluate it on your own customer-data examples. For recognizable secret formats, run the independent [`pii_guard`](../pii_guard/README.md) before `pii`; neither plugin shares state with the other.

## Data and failure behavior

With Torana's MCP connection configured, your harness can call
`pii.redaction.explain_last`. It returns only normalized finding categories and
relative line numbers for the last newly protected tool result in the bound
session. A line of `0` means the contextual scanner identified a category but
did not provide a plausible source line. It never returns matched values,
filenames, tool names, scanner output, or original tool content. Historical
replay and transient scanner failures do not replace the last explanation.
`found: false` means no explanation was recorded; it is not a clean verdict.
Approve the new digest and HTTP permission when upgrading.

The scanner receives eligible tool-output text. If bound remotely, that text leaves your machine before the primary request is allowed. This is a tool-result guard, not a scanner for all user prompts, a comprehensive DLP system or a guarantee of detection. `on_error: allow` permits undecidable scans; the approval's failure mode separately controls hook failures. Defaults are block. Torana durably stores only hashes and safe replacement messages, never the original sensitive output, so the same decisions replay across later turns and restarts. Only the newest tool-result batch is scanned; historical output is changed only when replaying an earlier decision.

## Combine or disable

Place before plugins that reduce tool output so it sees the original content.
Torana rejects a pipeline that places a tool-result writer after a compaction
gate. For broader protection, place `pii_guard` immediately before `pii`:
recognizable values become value-free tool errors before the model scan, while
`pii` still scans other tool output—including failures that can contain secrets.
With `on_error: allow`, an unavailable scanner forwards undecided content, so
use the default `block` policy when preventing disclosure matters.

```bash
torana plugin disable pii --yes
```

Disabling keeps configuration, approval and private data. `plugin revoke`
also removes approval; neither deletes private files. For errors, start with
`torana plugin status`, `torana feed` and the log path in `torana status`.
See the [CLI reference](https://github.com/torana-edge/torana-edge/blob/main/docs/CLI.md)
for instance selection, revision conflicts and pipeline order.
