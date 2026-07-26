#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cache=$(mktemp -d)
workspace_dir=$(mktemp -d)
trap 'rm -rf "$cache" "$workspace_dir"' EXIT

if [[ -d "$root/../torana-plugin-sdk" ]]; then
  modules=("$root/../torana-plugin-sdk")
  for module in "$root"/plugins/*; do modules+=("$module"); done
  (cd "$workspace_dir" && go work init "${modules[@]}")
  (cd "$workspace_dir" && go work edit -replace "github.com/torana-edge/torana-plugin-sdk@v0.1.0=$root/../torana-plugin-sdk")
  export GOWORK="$workspace_dir/go.work"
else
  export GOWORK=off
fi

go run "$root/scripts/validate_manifests.go" "$root/plugins"
for module in "$root"/plugins/*; do
  GOCACHE="$cache" go test "$module"
done
