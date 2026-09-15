# Use a local summarizer

Use the `compactor` plugin to route explicitly eligible historical results to
a local model. The provider URL is the server origin or configured base path;
the `summarizer` model-service approval separately names the root-relative
inference path. For an OpenAI-compatible Ollama endpoint, bind that path to
`/v1/chat/completions`.

The `intent` plugin is optional. When enabled before `compactor`, its cached
signal improves summary guidance; without it, the compactor derives bounded
guidance from the historical request and tool call. This minimal example does
not enable `intent`:

```json
{
  "providers": {
    "ollama": {
      "url": "http://localhost:11434",
      "format": "openai",
      "auth": {"mode": "none"}
    }
  },
  "plugins": {
    "dir": "./plugins",
    "order": ["compactor"],
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

When approving `compactor`, bind its `summarizer` model-service slot to provider
`ollama`, model `qwen2.5:3b`, and path `/v1/chat/completions`. Bind the associated
`summarizer` pricing resource to explicit zero rates, and bind `target` to the
models that may carry the original request. The plugin receives only the
logical slot names; provider URLs, credentials, models, paths, and budgets
remain operator-owned.

A local summarizer may have no per-token API charge, but uses your machine's
compute and adds latency. The target resource still
needs operator-supplied cache-read/write pricing for the positive-net gate.
Start with the [complete setup and approval guide](README.md), then use
[the policy and economics reference](COMPACTION.md) to choose eligible tools.
The fragment above is not a complete approval.
