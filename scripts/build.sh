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
  (cd "$workspace_dir" && go work edit -replace "github.com/torana-edge/torana-plugin-sdk@v0.1.0=$root/../torana-plugin-sdk")
  export GOWORK="$workspace_dir/go.work"
else
  export GOWORK=off
fi
GOCACHE="$cache" GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -buildvcs=false -o "$root/dist/$plugin/plugin.wasm" "$source"
cp "$source/plugin.json" "$root/dist/$plugin/plugin.json"
cp "$source/schema.json" "$root/dist/$plugin/schema.json"
rm -f "$root/dist/$plugin/agent.json"
if [[ -f "$source/agent.json" ]]; then
  cp "$source/agent.json" "$root/dist/$plugin/agent.json"
fi
echo "$root/dist/$plugin/plugin.wasm"
