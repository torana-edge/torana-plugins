# Keep recognizable sensitive data out of model requests

`pii_guard` checks tool-result text for high-confidence PII and secret patterns
before the next request reaches the configured model provider. It runs entirely
inside Torana: there is no scanner model, network destination, credential, or
clean-result cache to configure.

Use this as your first plugin when you do not already have a local model
endpoint. If you do, the model-backed [`pii`](../pii/README.md) guard adds
contextual checks while keeping the same deterministic fast path.

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
digest and permissions, then choose **Approve and enable**. It requests
only the permissions needed to read its allowlist, replace affected tool
results, and remember those replacements across restarts.

## Try it safely

Create a file containing an obviously synthetic credential:

```text
PAYMENT_API_KEY=sk_test_torana_demo_not_a_real_key_123
```

Ask your coding harness to read that file. The harness executes the read
locally; when it prepares the follow-up containing the tool result, Torana
replaces that result with a value-free tool error before the configured
provider receives it. The model can acknowledge the error, skip the affected
lines, or ask for a narrower read instead of losing the whole conversation.

The message reports only a category and line number. It never repeats the
matched value. Torana persists the safe replacement—not the original
content—so later turns, resumed conversations, and restarts remain protected.
Only the newest tool-result batch is scanned; historical output is changed
only when replaying a decision the plugin already made.

## Coverage and boundary

The deterministic guard recognizes a deliberately conservative set of common
shapes: US SSNs, AWS access-key IDs, PEM private-key headers, selected
`sk-`/`sk_` API keys, and GitHub access tokens. Email addresses are allowed by
default because a pattern alone cannot distinguish public Git metadata from a
private address. Unmatched text is not a claim that the result is clean. The
plugin scans tool-result text, not arbitrary files, user prompts, tool
arguments, headers, images, or every possible secret.

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
