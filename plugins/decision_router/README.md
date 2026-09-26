# See when a model switch might help

`decision_router` watches an ordered model ladder for each Torana provider. In
**shadow mode** it records when a stronger model might help, without switching
models, changing effort, or modifying a request. It is a way to learn from real
workflows before enabling routing decisions.

Use **advise mode** (`"mode":"advise"`) with the same ladder to receive
optional suggestions, including estimated cache-rebuild cost and payback for
cheaper models. Switch models in your harness if you want to follow the advice.
Torana does not change your route or effort in this mode. Approve the
`env.suggest` permission when upgrading the plugin to enable these notices.
Use a Torana release with the suggestion service enabled to display advice.
The `suggest_failed` metric distinguishes `not_configured`, `denied`,
and other failures. Repeated advice refreshes the same live suggestion rather
than replacing its confirmation code. If a harness uses short model names,
put its target name first in the step's `aliases`; otherwise Torana uses `model`.

**Confirm mode** (`"mode":"confirm"`) offers the same suggestions with
accept/dismiss actions. Accept through Torana to apply the switch on your next
user turn. Your harness's model picker stays unchanged; you can always switch
there instead to take control back.

**Auto mode** (`"mode":"auto"`) is an explicit opt-in for unattended work:
upward moves obey the switch cap, declared prices, and cache-rebuild cost guard.
Downward moves still need acceptance. No mode moves to a different step during
a tool continuation; an already applied route is kept until you change models
in your harness. Approve `env.route_request` for these modes. Effort changes
also require `manage_effort: true` and `env.route_request.effort`; otherwise
Torana leaves the harness's effort alone.

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

Torana's declared model capabilities are the preferred source of pricing:
omit each step's `pricing` to use the rates configured for that provider/model.
The example's explicit prices are illustrative USD per million tokens, **not
live provider prices**. Explicit step prices override individual declared rates;
omitted fields use Torana's declared rates. Overrides
emit a `deprecated_pricing_override` metric; migrate them into your Torana
model configuration. Missing prices produce a `pricing_unavailable` shadow
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
  "inputs": "user_turn+signals",
  "minimum_confidence": 0.8,
  "timeout_ms": 1500,
  "authentication": "none"
}
```

Bind the `decision-service` endpoint to a local or hosted System
One-compatible server. For bearer authentication, select `"bearer"` and bind
the separately approved `api-key` credential. The classifier receives the
latest textual user turn (bounded by `max_state_bytes`) and bounded signal
counts by default. Set `"inputs": "signals"` to send only those counts—no user
text. It does **not** receive the system prompt, earlier conversation, tool
definitions, tool arguments, or tool results. A hosted endpoint still receives
that latest turn; use a local endpoint if it should stay on your machine.

In signals-only mode, the service receives these fields: recent tool-error
count, number of user turns, the previous turn's request, retry, and max-token
counts, average requests per turn, and a coarse context-size bucket (`0-2k`,
`2k-8k`, `8k-32k`, `32k-128k`, `128k+`, or `unknown`). Counts are capped at
1,000. It receives no conversation text.

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

Request retries, requests per user turn, and max-token finishes are collected
through the whole turn and considered when the next user turn begins. A
non-max-token finish does not erase earlier max-token finishes in that turn.
The per-turn cost comparison uses the observed average requests per user turn;
it remains an estimate, not a provider quote.

If the harness is using a model outside the configured ladder, shadow mode
records `off_ladder` and does not pretend that model is the ladder's starting
step. An unprompted harness switch is recorded separately; it does not consume
Torana's future automatic-switch allowance. `would_suggest` also records the
estimated one-time cache-rebuild cost and escalation depth as histograms.

Editing the policy starts a new measurement baseline. The plugin never mutates
provider-visible content, preserving the prompt prefix and cache markers.

The earlier fixed-routing configuration (`decision_model`, `question`,
`routes`, `sticky`) has been removed before Torana's first release. Use the
shadow ladder above; a v1-shaped config is rejected with a v2 example rather
than silently interpreted as another policy.
