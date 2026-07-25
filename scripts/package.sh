#!/usr/bin/env bash
set -euo pipefail

if [[ $# != 2 ]]; then
  echo "usage: $0 <plugin> <version>" >&2
  exit 2
fi

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
plugin=$1
version=$2
"$root/scripts/build.sh" "$plugin" >/dev/null
staging=$(mktemp -d)
trap 'rm -rf "$staging"' EXIT
cp "$root/dist/$plugin/plugin.wasm" "$staging/plugin.wasm"
cp "$root/plugins/$plugin/plugin.json" "$staging/plugin.json"
cp "$root/plugins/$plugin/schema.json" "$staging/schema.json"
cp "$root/LICENSE" "$staging/LICENSE"
cp "$root/README.md" "$staging/README.md"
(cd "$staging" && sha256sum plugin.wasm > SHA256SUMS && tar -czf "$root/dist/$plugin/$plugin-$version.tar.gz" plugin.wasm plugin.json schema.json LICENSE README.md SHA256SUMS)
sha256sum "$root/dist/$plugin/$plugin-$version.tar.gz"
