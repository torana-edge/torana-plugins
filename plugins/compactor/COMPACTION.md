# Compact tool output without losing the evidence

This guide covers the shared tool-output policies used by [the model compactor](README.md)
and [the keyword compactor](../keyword_compactor/README.md), followed by the
model compactor's economic gate. Start with either setup guide for the complete
configuration and approval workflow; run only one compactor.

These plugins can rewrite only visible history that the harness resends. They
cannot rewrite history held by a provider behind a response ID. The proxy's
[OpenAI Responses compaction option](https://github.com/torana-edge/torana-edge/blob/main/docs/RESPONSES_COMPACTION.md)
is a separate provider feature, not a plugin mode.

Neither plugin is enabled implicitly. Unmatched tool results remain exact.

## Tool-result policies

Policies are evaluated in order using case-insensitive shell-style matches; the
first match wins. Torana recovers a result's tool name from the preceding tool
call when a provider only supplies a call ID.

The supported modes are:

| Mode | Behavior |
| --- | --- |
| `exact` | Never alter the output. This is also the default for unknown tools. |
| `deterministic` | Retain bounded head/tail evidence plus size, SHA-256, omitted-byte count, and a rerun instruction. Set `first_pass` to compact the first model exposure. |
| `keyword` | `keyword_compactor` only: retain lines matching cached intent or bounded guidance derived from the historical user request and tool call, after at least one exact exposure. |
| `model` | `compactor` only: create a summary through an operator-bound model service, guided by cached intent or the same bounded historical fallback, after at least one exact exposure, then apply it only when the economic gate passes. |

Mutation tools, diffs, failed commands, errors, stack traces, and similar
safety-sensitive outputs remain exact even if a broad rule matches them.
Model-generated compaction can never run on a fresh output.

Example deterministic coding-agent policy (settings only; approvals are separate):

```json
{
  "plugins": {
    "order": ["schema_translator", "intent", "keyword_compactor"],
    "config": {
      "keyword_compactor": {
        "tool_policies": [
          {
            "match": "read*",
            "mode": "exact"
          },
          {
            "match": "web_search",
            "mode": "deterministic",
            "first_pass": true,
            "rerun": "Repeat the search to recover every result."
          },
          {"match": "grep*", "mode": "keyword"}
        ]
      }
    }
  }
}
```

`intent` is optional for both compactors. When it runs earlier, its non-empty
cached value is the higher-quality signal. When it is absent or the cache entry
is empty, the compactor derives deterministic bounded guidance from user text
at or before the exact result position, the tool name, and the exact arguments.
Later conversation text cannot rewrite that historical signal. A missing intent
is still counted in plugin metrics so operators can measure the relevance path
in use.

`first_pass` is intentionally honored only by `deterministic`. This is useful
for large, reproducible listings, searches, and repetitive successful logs: the
model sees the same compact representation on every turn, so no later prompt
prefix rewrite is required. Do not enable it for reads that require exact
fidelity or for exact records. Keep source-reading tools `exact`: merely making
their output recoverable does not bound the number or cost of recovery calls an
agent may make.

### Install and approve the resources

Even deterministic `keyword_compactor` requires the `target` pricing slot for
attribution. It does not call a model service. Install and inspect the bundle,
bind that slot to the routed provider/models, approve the exact permission set,
then enable it. Missing required bindings prevent activation.

Use the complete [keyword compactor setup](../keyword_compactor/README.md)
or [model compactor setup](README.md)
for CLI approval examples. Run only one compactor. Plugin settings above are
not a complete approval and cannot grant resources.

## Model compaction economics

Model compaction is fail-closed. The plugin declares three logical resources:
the `summarizer` model service, `target` pricing, and `summarizer` pricing.
During approval, the operator binds those names to concrete providers, models,
credentials, budgets, and rates. The guest never sees provider configuration or
credentials. Torana ships no price table because rates and cache semantics
change.

```json
{
  "providers": {
    "primary": {
      "url": "https://api.example.com",
      "format": "openai"
    },
    "local-summarizer": {
      "url": "http://localhost:11434",
      "format": "openai",
      "auth": {"mode": "none"}
    }
  },
  "plugins": {
    "order": ["intent", "compactor"],
    "config": {
      "compactor": {
        "expected_applications": 6,
        "tool_policies": [
          {"match": "web_search", "mode": "model"},
          {"match": "read*", "mode": "exact"}
        ]
      }
    }
  }
}
```

The [setup guide](README.md#approve-and-enable) supplies the approval JSON
for all three declared bindings. Bind `summarizer` to `local-summarizer` / `qwen2.5:3b`; bind the
same-named pricing resource to that service with explicit zero rates; and bind
`target` to every provider/model combination the compactor may evaluate. For
example, the target binding for `primary` / `my-model` might use input 1.0,
output 4.0, cache read 0.1, and cache write 1.25 USD per million tokens. Those
rates are illustrative, not built-in defaults. An explicit zero is valid for a
local model; an omitted rate is unknown.

The compactor first tests whether even a best-case reduction could repay the
cache rewrite. Only then does it call the summarizer service. Generated candidates
are evaluated as one batch using:

- estimated tokens removed;
- the prompt span rewritten from the earliest changed result;
- expected future applications;
- target cache-read and cache-write rates; and
- reported summarizer input, output, and cache usage.

The batch is applied only when estimated net savings are positive. Token counts
derived from bytes are labeled as estimates. Routing plugins must run before an
economically gated compactor so the decision uses the final provider and model.

`/stats` separates transformations, cache reuse, and applications, and exposes
estimated removed tokens, cache rewrite tokens, gross savings, net savings, and
reasons a dollar estimate was unavailable. These numbers support workload-
specific A/B comparisons; they are not a universal percentage of the total API
bill. It also exposes successful summarizer input, output, cache-read, and
cache-write tokens so an A/B test can include the summarizer's actual cost.
OpenAI-compatible DeepSeek responses are accounted using DeepSeek's
`prompt_cache_hit_tokens` fields when standard OpenAI cache details are absent.
