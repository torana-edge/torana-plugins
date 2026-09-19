# Block recognizable sensitive data without a model

`pii_guard` checks tool-result text for high-confidence PII and secret patterns
before the next request reaches the configured model provider. It runs entirely
inside Torana: there is no scanner model, network destination, credential, or
clean-result cache to configure.

[All plugins](../../README.md#choose-a-plugin) · [Source](main.go) ·
[Manifest](plugin.json) · [Settings schema](schema.json)

## Install

Start [Torana](https://github.com/torana-edge/torana-edge/blob/main/docs/QUICKSTART.md)
first. Commands use `torana` on PATH; use `./torana` from a source checkout.
Run installation from the host checkout or supply its configured plugin
directory with `--dir`.

```bash
torana plugin install https://github.com/torana-edge/torana-plugins/tree/main/plugins/pii_guard
torana plugin inspect pii_guard
```

Installation builds the source locally and never approves or enables it.
Open Torana's local control plane, select **pii_guard**, review the inspected
digest and its two permissions, then choose **Approve and enable**. It requests
only permission to read its tool allowlist and block a request.

## Try it safely

Create a file containing an obviously synthetic credential:

```text
PAYMENT_API_KEY=sk_test_torana_demo_not_a_real_key_123
```

Ask your coding harness to read that file. The harness executes the read
locally; when it prepares the follow-up containing the tool result, Torana
returns a value-free `sensitive_data_detected` block before the configured
provider receives that result.

The message reports only a category and line number. It never repeats the
matched value. Remove or replace the value before asking the harness to retry.

## Coverage and boundary

The deterministic guard recognizes a deliberately conservative set of common
shapes: email addresses, US SSNs, AWS access-key IDs, PEM private-key headers,
selected `sk-`/`sk_` API keys, and GitHub access tokens. Unmatched text is not a
claim that the result is clean. The plugin scans tool-result text, not arbitrary
files, user prompts, tool arguments, headers, images, or every possible secret.

For names, addresses, prose, unfamiliar credentials, and other contextual
cases, use the model-backed [`pii`](../pii/README.md) plugin instead. The two
plugins conflict because `pii` already includes this deterministic fast path
before its contextual scan; choose one.

## Optional tool selection

The default scans every tool result. To limit it, export the plugin snapshot,
set its `config.tools` array to exact tool names, preserve `revision`, and apply
it through the live host:

```bash
torana plugin config get pii_guard > plugin-settings.json
torana plugin config apply pii_guard --file plugin-settings.json --yes
```

Unknown tool names still err toward scanning. Disable the guard with
`torana plugin disable pii_guard --yes`. For errors, inspect
`torana plugin status`, `torana feed`, and the log path from `torana status`.
