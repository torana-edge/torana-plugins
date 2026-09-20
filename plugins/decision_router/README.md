# Route a conversation with one closed-set decision

Choose an operator-approved Torana provider and model from the latest user turn,
then keep that choice stable for the conversation. `decision_router` works with
System One-compatible `POST /v1/systemone` services, including hosted
[TypeSafe Jev](https://typesafe.ai/) and local servers such as
[Von](https://github.com/wfzyx/von) and
[Simple Jev](https://github.com/featherless-ai/simple-jev).

The plugin is named for what it does, not for one vendor. The endpoint and API
key are operator bindings; they are never arbitrary plugin configuration.

[All plugins](../../README.md#choose-a-plugin) · [Source](main.go) · [Manifest](plugin.json) · [Settings schema](schema.json)

## What leaves your machine

For a new conversation, the bound decision service receives:

- the latest textual user turn, capped by `max_state_bytes`;
- the request's current model name; and
- at most 32 available tool names.

It does **not** receive the system prompt, earlier messages, tool schemas, tool
arguments, tool results, provider extensions, or credentials. A remote binding
still sends that latest turn off-machine; use a local endpoint if it must remain
local.

The service chooses only among the route IDs you configure. Torana validates
the returned type, exact choice ID, confidence range and threshold before it
calls `route_request`. A timeout, refusal, non-2xx status, malformed answer,
unknown choice or low-confidence answer keeps the original route.

## Install

```bash
torana plugin install https://github.com/torana-edge/torana-plugins/tree/main/plugins/decision_router
torana plugin inspect decision_router
```

Installation builds source locally; it does not approve or enable the plugin.
The local control plane offers the same configure, approve and enable flow as
the CLI examples below.

## Configure routes

```bash
torana plugin config get decision_router > plugin-settings.json
```

Set the snapshot's `config` object, preserving its `revision`:

```json
{
  "decision_model": "jev-latest",
  "question": "What level of model does this coding task require?",
  "routes": {
    "fast": {
      "description": "Formatting, lookup, or straightforward mechanical work",
      "provider": "anthropic",
      "model": "claude-haiku"
    },
    "reasoning": {
      "description": "Architecture, debugging, ambiguity, or multi-step reasoning",
      "provider": "anthropic",
      "model": "claude-opus"
    }
  },
  "minimum_confidence": 0.82,
  "max_state_bytes": 8192,
  "timeout_ms": 5000,
  "authentication": "bearer",
  "sticky": true
}
```

```bash
torana plugin config apply decision_router --file plugin-settings.json --yes
```

Every route must set `provider`, `model`, or both. The names must match Torana
providers/models the operator has configured. A model-only route keeps the
current provider. Keep `sticky` enabled unless deliberate per-request switching
is worth changing the provider/model identity—and potentially its prompt cache—
on later turns.

## Bind TypeSafe Jev

Create a Torana credential such as `typesafe-jev`; do not put the token in the
plugin config or approval file. Bind `decision-service` to the hosted origin and
the optional `api-key` slot to that named credential:

```json
{
  "digest": "sha256:REPLACE_WITH_YOUR_INSPECTED_DIGEST",
  "permissions": [
    "env.credential_get",
    "env.emit_metric",
    "env.http_request",
    "env.log",
    "env.plugin_config",
    "env.route_request",
    "env.state_get",
    "env.state_set"
  ],
  "failure_mode": "pass",
  "credentials": {"api-key": "typesafe-jev"},
  "http_endpoints": {
    "decision-service": {
      "origin": "https://api.typesafe.ai",
      "methods": ["POST"],
      "timeout_ms": 5000,
      "max_request_bytes": 131072,
      "max_response_bytes": 65536,
      "max_calls_per_minute": 30
    }
  }
}
```

Use `authentication: "bearer"` and a Jev model ID supported by the service.
The plugin adds the `Authorization: Bearer …` header from the bound credential.

## Bind local Von

Start Von's server, then bind the endpoint to its loopback origin:

```bash
von serve --host 127.0.0.1 --port 8000
```

Use `authentication: "none"`, `decision_model: "von-1.0.0"`, no credential
binding, and this endpoint approval:

```json
"http_endpoints": {
  "decision-service": {
    "origin": "http://127.0.0.1:8000",
    "methods": ["POST"],
    "timeout_ms": 5000,
    "max_request_bytes": 131072,
    "max_response_bytes": 65536,
    "max_calls_per_minute": 60
  }
}
```

Von is an independent implementation. Its performance and calibration claims
belong to that project; evaluate its choices and your threshold on your own
traffic.

## Bind local Simple Jev

Start Simple Jev with a model that works on your machine. Its `/v1/systemone`
route is an alias of `/v1/classifier`. Bind the server's loopback origin as in
the Von example, use `authentication: "none"`, and set `decision_model` to the
exact model ID used to start the server—for example `Qwen/Qwen3.5-0.8B`.

Simple Jev uses an ordinary instruction model's label logits and does not
reproduce TypeSafe Jev's model architecture or guarantee equivalent accuracy,
calibration, or latency. Model compatibility also varies. Test it as a routing
policy, not as an unquestioned oracle.

## Approve, enable and observe

```bash
torana plugin approve decision_router --file approval.json --yes
torana plugin enable decision_router --yes
torana plugin status
torana feed
```

Metrics count `routed`, `sticky`, and value-free fallback reasons. Logs never
include prompts, response bodies or credentials. Route choice IDs may appear as
metric labels, so use short operational IDs rather than user data.

## Stickiness, restarts and policy changes

With the default `sticky: true`, the first accepted choice is stored in Torana's
plugin-private durable state under a hash of the stable conversation ID. Later
turns and resumed conversations reuse the provider/model without calling the
decision service again. Torana must have a data directory for durable state; if
stable conversation identity or state is unavailable, the plugin keeps the
current route instead of making a decision it cannot safely preserve.

The stored record includes a SHA-256 hash of the complete normalized policy.
Changing any route, description, question, threshold, model, authentication,
timeout, input bound, or stickiness invalidates the old record and causes a new
decision. The hash is local metadata and no prompt content is stored.

## Limits

- This is routing, not load balancing, health checking or a quality guarantee.
- Confidence is supplied by the chosen service; thresholds need evaluation on
  your own tasks and model version.
- One initial routing decision cannot anticipate needs that emerge much later.
  Disable stickiness only after considering prompt-cache and cost effects.
- The plugin intentionally ignores images, audio, tool results and historical
  context. That keeps disclosure and classification cost bounded, but a route
  may miss information present only in those fields.
- Updating the bundle requires a new digest approval. Changing the endpoint or
  credential binding is an operator action independent of plugin config.

Disable without deleting its configuration or durable decisions:

```bash
torana plugin disable decision_router --yes
```
