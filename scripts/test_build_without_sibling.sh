#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
fixture="$tmp/torana-plugins"

mkdir -p "$fixture/scripts" "$fixture/plugins"
cp "$root/scripts/build.sh" "$fixture/scripts/build.sh"
cp -R "$root/plugins/auth" "$fixture/plugins/auth"

# The fixture deliberately has no ../torana-plugin-sdk sibling. This is the
# documented contributor/release fallback and must build from the module pin,
# not accidentally run `go build` from a directory with no go.mod.
"$fixture/scripts/build.sh" auth >/dev/null
if [[ ! -s "$fixture/dist/auth/plugin.wasm" ]]; then
  echo "no-sibling build test: auth WASM was not produced" >&2
  exit 1
fi

