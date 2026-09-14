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
# An explicit initially cold cache exercises the caller override as well as
# reuse. Both builds must produce the same release artifact.
export GOCACHE="$tmp/go-cache"
export GOWORK=off
"$fixture/scripts/build.sh" auth >/dev/null
if [[ ! -s "$fixture/dist/auth/plugin.wasm" ]]; then
  echo "no-sibling build test: auth WASM was not produced" >&2
  exit 1
fi


cp "$fixture/dist/auth/plugin.wasm" "$tmp/first.wasm"
if [[ ! -d "$GOCACHE" ]]; then
  echo "build ignored the explicit GOCACHE" >&2
  exit 1
fi
"$fixture/scripts/build.sh" auth >/dev/null
if ! cmp -s "$tmp/first.wasm" "$fixture/dist/auth/plugin.wasm"; then
  echo "auth WASM differs between cold and warm cache builds" >&2
  exit 1
fi
echo "auth WASM is identical with cold and warm build caches"
