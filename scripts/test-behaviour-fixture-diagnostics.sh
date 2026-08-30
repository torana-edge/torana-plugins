#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

mkdir -p "$tmp/edge" "$tmp/bundles/example" "$tmp/bin"
: >"$tmp/bundles/example/plugin.wasm"

cat >"$tmp/bin/make" <<'EOF'
#!/bin/sh
i=1
while [ "$i" -le 100 ]; do
	printf 'fixture-line-%03d\n' "$i" >&2
	i=$((i + 1))
done
exit 1
EOF
chmod +x "$tmp/bin/make"

set +e
output=$(PATH="$tmp/bin:$PATH" "$root/scripts/verify-behaviour.sh" "$tmp/edge" "$tmp/bundles" 2>&1)
status=$?
set -e

if [ "$status" -eq 0 ]; then
	echo "fixture-build failure unexpectedly passed" >&2
	exit 1
fi
printf '%s\n' "$output" | grep -F 'failed to build torana-edge test fixtures' >/dev/null
printf '%s\n' "$output" | grep -F 'fixture-line-021' >/dev/null
printf '%s\n' "$output" | grep -F 'fixture-line-100' >/dev/null
if printf '%s\n' "$output" | grep -F 'fixture-line-001' >/dev/null; then
	echo "fixture diagnostic was not bounded to the last 80 lines" >&2
	exit 1
fi

echo "behaviour fixture diagnostics: bounded failure output passes"
