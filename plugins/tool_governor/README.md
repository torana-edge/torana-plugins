# Tool governor

Restrict or replace the tool definitions advertised to the model. This does not
control tool execution: your coding harness still owns executing tools and its
own approvals.

Configuration is fetched on every request, so changes are observed without
relying on a new guest instance. The last exact configuration is cached after
parsing and validation; this avoids repeated parsing, not the host call or its
guest-memory copy. Native test harnesses share the cache slot, while WASM guests
each have their own instance.

If fetching configuration fails, returns an empty response, or yields invalid
configuration, the hook returns an error. The bundled `failure_mode: block`
prevents forwarding that request with an unrestricted tool list. Restore access
to `env.plugin_config` or correct the reported configuration problem, then retry.
An explicitly configured `{}` is valid and leaves tools unchanged; a failed
fetch is not treated as `{}`. No prior policy is substituted silently.

## Verification

```sh
go test -race -shuffle=on ./...
go test -run '^$' -bench BenchmarkBeforeRequestConfigCache -benchtime=1000x -count=3
```

The benchmark compares full native SDK hook executions with cached parsing and
forced reparsing. It includes native host dispatch and request handling, but
does not measure WASM boundary overhead, proxy latency, or provider calls.
Cache-hit allocation assertions separately guard against accidental reparsing.
