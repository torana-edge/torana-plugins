#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cache=$(mktemp -d)
trap 'rm -rf "$cache"' EXIT

go run "$root/scripts/validate_manifests.go" "$root/plugins"
for module in "$root"/plugins/*; do
  GOCACHE="$cache" go test "$module"
done
