#!/usr/bin/env bash
set -euo pipefail

if [[ $# != 2 ]]; then
  echo "usage: $0 <plugin> <version>" >&2
  exit 2
fi

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
plugin=$1
version=$2
go run "$root/scripts/validate_release.go" "$root/plugins/$plugin/plugin.json" "$plugin" "$version"
"$root/scripts/build.sh" "$plugin" >/dev/null
staging=$(mktemp -d)
trap 'rm -rf "$staging"' EXIT
cp "$root/dist/$plugin/plugin.wasm" "$staging/plugin.wasm"
cp "$root/plugins/$plugin/plugin.json" "$staging/plugin.json"
cp "$root/plugins/$plugin/schema.json" "$staging/schema.json"
bundle_files=(plugin.wasm plugin.json schema.json)
digest_args=("$staging/plugin.json" "$staging/plugin.wasm" "$staging/schema.json")
if [[ -f "$root/plugins/$plugin/agent.json" ]]; then
  cp "$root/plugins/$plugin/agent.json" "$staging/agent.json"
  bundle_files+=(agent.json)
  digest_args+=("$staging/agent.json")
fi
cp "$root/LICENSE" "$staging/LICENSE"
cp "$root/README.md" "$staging/README.md"
(cd "$staging" && sha256sum "${bundle_files[@]}" > SHA256SUMS)
go run "$root/scripts/bundle_digest.go" "${digest_args[@]}" > "$staging/BUNDLE_DIGEST"
archive="$root/dist/$plugin/$plugin-$version.tar.gz"
(cd "$staging" && tar --sort=name --mtime='@0' --owner=0 --group=0 --numeric-owner -cf - "${bundle_files[@]}" LICENSE README.md SHA256SUMS BUNDLE_DIGEST | gzip -n > "$archive")
(cd "$(dirname "$archive")" && sha256sum "$(basename "$archive")" > "$(basename "$archive").sha256")
cat "$archive.sha256"
