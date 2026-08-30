#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)

if [ -f "$root/go.mod" ]; then
	echo "root go.mod now exists; reconsider the multi-module setup-go cache paths" >&2
	exit 1
fi

for module in "$root"/plugins/*/go.mod; do
	[ -f "$module" ] || {
		echo "no plugin modules found" >&2
		exit 1
	}
	lock=${module%/go.mod}/go.sum
	[ -f "$lock" ] || {
		echo "plugin module has no cache dependency file: $module" >&2
		exit 1
	}
done

require_once() {
	file=$1
	needle=$2
	count=$(grep -Fxc "$needle" "$file" || true)
	if [ "$count" -ne 1 ]; then
		echo "$file: expected exactly one line '$needle', got $count" >&2
		exit 1
	fi
}

# Every repository is checked out below GITHUB_WORKSPACE. CI compiles plugin,
# SDK, and Edge packages; release compiles plugins and the SDK only.
ci=$root/.github/workflows/ci.yml
release=$root/.github/workflows/release.yml
require_once "$ci" '          cache-dependency-path: |'
require_once "$release" '          cache-dependency-path: |'

assert_paths() {
	file=$1
	expected=$2
	actual=$(grep -E '^            [^ ]+go\.sum$' "$file" || true)
	if [ "$actual" != "$expected" ]; then
		echo "$file: setup-go cache dependency inventory differs" >&2
		echo 'expected:' >&2
		printf '%s\n' "$expected" >&2
		echo 'actual:' >&2
		printf '%s\n' "$actual" >&2
		exit 1
	fi
}

ci_paths='            torana-plugins/plugins/*/go.sum
            torana-plugin-sdk/go.sum
            torana-edge/go.sum'
release_paths='            torana-plugins/plugins/*/go.sum
            torana-plugin-sdk/go.sum'
assert_paths "$ci" "$ci_paths"
assert_paths "$release" "$release_paths"

echo 'setup-go cache paths cover every checked-out dependency graph'
