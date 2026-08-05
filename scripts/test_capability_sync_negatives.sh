#!/usr/bin/env bash
# Executable negatives for the capability-sync gate: the unmodified pin
# passes, and EACH of removing one env permission, removing one write
# permission, and adding one validator-only permission FAILS the sync. A
# nonfunctional gate (silent pass on drift) fails here.
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
sync="$root/scripts/test_capability_sync.sh"
validator="$root/scripts/validate_manifests.go"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

expect() { # want-fail label file
  if VALIDATOR="$3" PLUGINS_DIR="$4" bash "$sync" >/dev/null 2>&1; then
    if [[ "$1" == "fail" ]]; then
      echo "capability sync negative: expected FAILURE for \"$2\", but the sync passed" >&2
      exit 1
    fi
  else
    if [[ "$1" == "pass" ]]; then
      echo "capability sync negative: expected PASS for \"$2\", but the sync failed" >&2
      exit 1
    fi
  fi
}

# one_token swaps EXACTLY ONE quoted permission token and asserts the
# fixture changed exactly once (a whole-line delete or a no-op mutation
# fails here).
one_token() { # file pattern replacement
  local f="$1" pat="$2" rep="$3"
  sed "s|$pat|$rep|" "$f" > "$f.tmp" && mv "$f.tmp" "$f"
  if [[ $(grep -c "$rep" "$f") -ne 1 ]]; then
    echo "capability sync negative: fixture mutation did not apply exactly once: $pat -> $rep" >&2
    exit 1
  fi
}

expect pass "unmodified pin" "$validator" "$root/plugins"

cp "$validator" "$tmp/v-env.go"
one_token "$tmp/v-env.go" '"env.now": true' '"env.forged_drift": true'
expect fail "an env permission token swapped" "$tmp/v-env.go" "$root/plugins"

cp "$validator" "$tmp/v-write.go"
one_token "$tmp/v-write.go" '"ir.tools.write": true' '"ir.toolz.write": true'
expect fail "a write permission token swapped" "$tmp/v-write.go" "$root/plugins"

cp "$validator" "$tmp/v-extra.go"
one_token "$tmp/v-extra.go" '"ir.tools.write": true' '"ir.tools.write": true, "env.bogus_drift": true'
expect fail "a validator-only permission added" "$tmp/v-extra.go" "$root/plugins"

# A MISLEADING SUBSTRING SDK_REF (a value contained in the pin but not the
# exact revision) must fail: substring matching would let 0.2.1 or the date
# pass.
# The REAL SDK_REF is never written: the negative uses an overridden file
# path, so an interrupted or concurrent ordinary gate can never expose a
# truncated/partial tracked SDK_REF.
echo "20260804" > "$tmp/SDK_REF"
if SDK_REF_FILE="$tmp/SDK_REF" bash "$sync" >/dev/null 2>&1; then
  echo "capability sync negative: expected FAILURE for a misleading substring SDK_REF, but the sync passed" >&2
  exit 1
fi

# One-MODULE pin drift: a plugins dir where ONLY schema_translator's go.mod
# requires a different SDK revision must fail the sync (all-nine agreement).
mkdir -p "$tmp/plugins"
for dir in "$root"/plugins/*/; do
  mkdir -p "$tmp/plugins/$(basename "$dir")"
  cp "$dir/go.mod" "$tmp/plugins/$(basename "$dir")/go.mod"
done
drifted="$tmp/plugins/schema_translator/go.mod"
sed 's|995c0bd40baa|deadbeef0000|' "$drifted" > "$drifted.tmp" && mv "$drifted.tmp" "$drifted"
if [[ $(grep -c 'deadbeef0000' "$drifted") -ne 1 ]]; then
  echo "capability sync negative: the one-module drift did not apply exactly once" >&2
  exit 1
fi
expect fail "a one-module SDK pin drift" "$validator" "$tmp/plugins"

echo "capability sync negatives: all six cases pass"
