#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
ci=$root/.github/workflows/ci.yml
release=$root/.github/workflows/release.yml

require_once() {
	file=$1
	line=$2
	count=$(grep -Fxc "$line" "$file" || true)
	if [ "$count" -ne 1 ]; then
		echo "$file: expected exactly one: $line" >&2
		exit 1
	fi
}

require_once "$ci" '          go-version-file: torana-edge/go.mod'
require_once "$release" '          go-version-file: torana-plugins/.go-version'

version=$(sed -n '1p' "$root/.go-version")
if ! printf '%s\n' "$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$'; then
	echo ".go-version: expected an exact Go patch version, got: $version" >&2
	exit 1
fi
if [ "$(wc -l < "$root/.go-version")" -ne 1 ]; then
	echo '.go-version: expected exactly one line' >&2
	exit 1
fi

echo "Go toolchain sources: CI follows Edge go.mod; releases use $version"
