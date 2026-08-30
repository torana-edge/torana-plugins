# Plugin agent operations

Torana plugins can contribute machine-readable JSON operations to the local
control plane in addition to browser pages. The `otel` plugin is the reference
implementation:

- [`plugins/otel/agent.json`](../plugins/otel/agent.json) declares the stable
  operation contract.
- [`plugins/otel/main.go`](../plugins/otel/main.go) handles the corresponding
  guest path, `/agent/status`.

After the plugin is installed, approved, and enabled, Torana includes its
operations in:

```sh
curl http://127.0.0.1:8080/_torana/api/v1/
```

An agent can then call the advertised public path:

```sh
curl http://127.0.0.1:8080/_torana/api/v1/agent/plugins/otel/status
```

## Contract

`agent.json` is optional. When present it has this shape:

```json
{
  "schema_version": 1,
  "description": "Machine-readable plugin operations.",
  "operations": [
    {
      "id": "status",
      "method": "GET",
      "path": "/status",
      "description": "Read plugin status.",
      "risk": "read",
      "idempotent": true,
      "output_schema": {
        "type": "object"
      }
    }
  ]
}
```

Each operation must use JSON input and output. `input_schema` is optional;
`output_schema` is required. Supported methods are `GET`, `POST`, `PUT`,
`PATCH`, and `DELETE`. Risk must be `read`, `write`, or `destructive`, and it
must agree with the method. Schemas describe the contract to callers; plugin
bundles are rejected if they use schema keywords the host cannot enforce, and
Torana validates both request and response values at dispatch.

The supported subset is `type`, `properties`, `required`,
`additionalProperties` (boolean), `items`, `const`, `enum`, `$schema`, `title`,
and `description`. Without `input_schema`, an operation accepts no body.

The public operation path is:

```text
/_torana/api/v1/agent/plugins/<plugin-name><operation-path>
```

Torana rewrites it before dispatch, so the plugin receives:

```text
/agent<operation-path>
```

Mutating calls made without a browser `Origin` must include:

```text
X-Torana-Local-Request: 1
```

The existing control-plane loopback, host, and request-origin protections still
apply. Plugin HTTP handling also requires the `run_on_http_request` hook and an
approved `env.serve_http` grant.

## Packaging and approval

`agent.json` is included in the bundle digest. Changing an operation, schema,
or description therefore creates a new digest that the operator must approve,
just like changing `plugin.wasm`, `plugin.json`, or `schema.json`.

The repository build and packaging scripts automatically copy and package
`agent.json` when it exists. Run:

```sh
./scripts/test.sh
./scripts/package.sh otel 0.5.0
```

The generated archive includes `agent.json`, and `SHA256SUMS` and
`BUNDLE_DIGEST` cover it.
