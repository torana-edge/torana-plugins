#!/usr/bin/env bash
set -euo pipefail

if [[ $# != 1 ]]; then
  echo "usage: $0 <plugin>" >&2
  exit 2
fi

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
plugin=$1
source="$root/plugins/$plugin"
[[ -d "$source" ]] || { echo "unknown plugin: $plugin" >&2; exit 2; }
mkdir -p "$root/dist/$plugin"
cache=$(mktemp -d)
workspace_dir=$(mktemp -d)
trap 'rm -rf "$cache" "$workspace_dir"' EXIT
if [[ -d "$root/../torana-plugin-sdk" ]]; then
  (cd "$workspace_dir" && go work init "$root/../torana-plugin-sdk" "$source")
  export GOWORK="$workspace_dir/go.work"
  # Prove the workspace actually resolved the sibling SDK. A version-pinned
  # `go work edit -replace` used to sit here and had silently stopped matching
  # at v0.1.1 -- the build kept working via the module list above, so nothing
  # ever reported that half the setup was dead. Assert the outcome instead of
  # trusting the plumbing.
  sdk_dir=$(cd "$root/../torana-plugin-sdk" && pwd)
  resolved=$(cd "$source" && go list -m -f '{{.Dir}}' github.com/torana-edge/torana-plugin-sdk 2>/dev/null || true)
  case "$resolved" in
    "$sdk_dir"*) ;;
    *)
      echo "workspace did not resolve the sibling SDK (got '${resolved:-nothing}')" >&2
      exit 1
      ;;
  esac
else
  # No sibling SDK: build against the published module. Say so, because a
  # silent fallback to whatever the proxy serves is the bug this script's
  # assertions exist to prevent — and a contributor who expected their local
  # SDK changes to be picked up otherwise learns nothing.
  echo "note: no sibling torana-plugin-sdk checkout; building $plugin against the published module" >&2
  export GOWORK=off
fi
(cd "$source" && GOCACHE="$cache" GOOS=wasip1 GOARCH=wasm \
  go build -trimpath -buildmode=c-shared -buildvcs=false \
  -o "$root/dist/$plugin/plugin.wasm" .)
cp "$source/plugin.json" "$root/dist/$plugin/plugin.json"
cp "$source/schema.json" "$root/dist/$plugin/schema.json"
rm -f "$root/dist/$plugin/agent.json"
if [[ -f "$source/agent.json" ]]; then
  cp "$source/agent.json" "$root/dist/$plugin/agent.json"
fi
echo "$root/dist/$plugin/plugin.wasm"
