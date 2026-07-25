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
trap 'rm -rf "$cache"' EXIT
GOCACHE="$cache" GOOS=wasip1 GOARCH=wasm go build -buildvcs=false -o "$root/dist/$plugin/plugin.wasm" "$source"
echo "$root/dist/$plugin/plugin.wasm"
