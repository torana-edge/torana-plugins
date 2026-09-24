# See when a model switch might help

`decision_router` watches an ordered model ladder for each Torana provider. In
**shadow mode** it records when a stronger model might help, without switching
models, changing effort, or modifying a request. It is a way to learn from real
workflows before enabling routing decisions.

It can use repeated tool failures as a local signal. Optionally, an
operator-bound System One-compatible endpoint can classify the latest user turn
against the ladder's step descriptions. Hosted
[TypeSafe Jev](https://typesafe.ai/) and local servers such as
[Von](https://github.com/wfzyx/von) and
[Simple Jev](https://github.com/featherless-ai/simple-jev) can supply that
answer. The classifier is off by default, so no decision-service call is made
unless you enable it.

[All plugins](../../README.md#choose-a-plugin) · [Source](main.go) · [Manifest](plugin.json) · [Settings schema](schema.json)

## Try it

```bash
torana plugin install https://github.com/torana-edge/torana-plugins/tree/main/plugins/decision_router
torana plugin inspect decision_router
```

Installation builds source locally; it does not enable the plugin. Open the
control plane to configure, approve, and enable it. Start with this `config`
object in its settings:

```json
{
  "mode": "shadow",
  "ladders": {
    "anthropic": {
      "start": "sonnet",
      "steps": [
        {"id": "haiku", "description": "Routine mechanical work", "model": "claude-haiku-4-5", "pricing": {"input": 1, "output": 5, "cache_read": 0.1, "cache_write": 1.25}},
        {"id": "sonnet", "description": "Normal coding work", "model": "claude-sonnet-5", "pricing": {"input": 2, "output": 10, "cache_read": 0.2, "cache_write": 2.5}},
        {"id": "opus", "description": "Difficult debugging or architecture", "model": "claude-opus-5", "pricing": {"input": 5, "output": 25, "cache_read": 0.5, "cache_write": 6.25}}
      ]
    }
  }
}
```

The prices are illustrative USD per million tokens, **not live provider
prices**. Replace them with current rates for your models before trusting a
cost-based suggestion. Missing prices produce a `pricing_unavailable` shadow
metric instead of pretending a switch is free. Change the provider and models
to match your Torana configuration. The steps
are ordered from lighter to stronger; `start` is the baseline when a
conversation has not already selected a matching model. An empty `{}` config
does nothing, and `{"mode":"off"}` explicitly turns measurement off.

The policy evaluates at most one step per new user turn. It considers new tool
errors, repeated attempts, long turns, and responses that hit the output limit.
An after-response observation updates the context-token and average-output
estimates used for the one-time cache rebuild cost. By default it considers at
most one model switch and blocks suggestions whose estimated rebuild exceeds
$0.50. The `triggers` and `escalation` settings let you tune those limits.
An effort step requires `"manage_effort": true`; without that explicit choice,
the harness's effort setting remains authoritative. Even with it, shadow mode
still only measures—it does not change effort.

Watch the `torana_decision_router_total` metric with outcome `would_suggest`.
It includes the provider and the current and proposed step IDs. No actual
route call is made. The plugin records durable state per conversation thread,
so restarting Torana does not make old tool failures appear new.

## Optional classifier

Add a `classifier` block when you have a bound decision-service endpoint:

```json
"classifier": {
  "enabled": true,
  "decision_model": "jev-latest",
  "question": "Which model tier fits this coding task?",
  "minimum_confidence": 0.8,
  "timeout_ms": 1500,
  "authentication": "none"
}
```

Bind the `decision-service` endpoint to a local or hosted System
One-compatible server. For bearer authentication, select `"bearer"` and bind
the separately approved `api-key` credential. The classifier receives the
latest textual user turn (bounded by `max_state_bytes`) and bounded signal
counts. It does **not** receive the system prompt, earlier conversation, tool
definitions, tool arguments, or tool results. A hosted endpoint still receives
that latest turn; use a local endpoint if it should stay on your machine.

The answer is a closed choice among your step IDs or `hold`. Low confidence,
timeouts, malformed answers, and endpoint failures leave the current model
untouched. Even a valid answer only records what shadow mode *would* suggest.

## What the signals mean

The plugin distinguishes a new user turn from a tool-result continuation,
including a tool result accompanied by reminder text. It counts only new
explicit tool-error results rather than replayed history. If compaction removes
the replay anchor, it conservatively starts a new baseline. Error bits survive
canonicalization on some API shapes better than others, so `would_suggest` is
an observation, not proof that switching models would fix a broken tool.

Editing the policy starts a new measurement baseline. The plugin never mutates
provider-visible content, preserving the prompt prefix and cache markers.

The earlier fixed-routing configuration (`decision_model`, `question`,
`routes`, `sticky`) has been removed before Torana's first release. Use the
shadow ladder above; a v1-shaped config is rejected with a v2 example rather
than silently interpreted as another policy.
