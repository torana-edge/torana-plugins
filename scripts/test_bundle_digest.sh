#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

printf %s manifest > "$work/plugin.json"
printf %s wasm > "$work/plugin.wasm"
printf %s schema > "$work/schema.json"
printf %s agent > "$work/agent.json"

legacy_expected=sha256:8743ff3f6066c1859164f246e463e5dcff44f1a4e26cdc2193d62f51fd128ecb
agent_expected=sha256:4a00f9b037bbc9138735d6cbc7fdb54348cd1f54fe99302e73d06d6d3b36e708

legacy_actual=$(go run "$root/scripts/bundle_digest.go" \
  "$work/plugin.json" "$work/plugin.wasm" "$work/schema.json")
agent_actual=$(go run "$root/scripts/bundle_digest.go" \
  "$work/plugin.json" "$work/plugin.wasm" "$work/schema.json" "$work/agent.json")

[[ "$legacy_actual" == "$legacy_expected" ]] || {
  echo "legacy digest mismatch: got $legacy_actual want $legacy_expected" >&2
  exit 1
}
[[ "$agent_actual" == "$agent_expected" ]] || {
  echo "agent digest mismatch: got $agent_actual want $agent_expected" >&2
  exit 1
}
