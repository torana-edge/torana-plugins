# Read OTel status from an agent

After [installing, approving and enabling OTel](README.md), discover its
advertised operations:

```bash
curl --fail-with-body http://127.0.0.1:8080/_torana/api/v1/
curl --fail-with-body http://127.0.0.1:8080/_torana/api/v1/agent/plugins/otel/status
```

Use the local instance's actual port. The public path dispatches to
`/agent/status` inside this plugin. [agent.json](agent.json) declares the
operation and output schema; [main.go](main.go) implements it. Torana validates
the JSON response against that descriptor.

Changing `agent.json` changes the bundle digest and needs approval again.
For creating operations in your own plugin, use the
[SDK authoring guide](https://github.com/torana-edge/torana-plugin-sdk/blob/main/docs/AGENT_OPERATIONS.md).
