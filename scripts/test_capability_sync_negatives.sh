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
  if VALIDATOR="$3" bash "$sync" >/dev/null 2>&1; then
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

expect pass "unmodified pin" "$validator"

sed '/"env.now": true/d' "$validator" > "$tmp/v-env.go"
expect fail "an env permission removed" "$tmp/v-env.go"

sed '/"ir.tools.write": true/d' "$validator" > "$tmp/v-write.go"
expect fail "a write permission removed" "$tmp/v-write.go"

sed 's|"ir.tools.write": true|"ir.tools.write": true, "env.bogus_drift": true|' "$validator" > "$tmp/v-extra.go"
expect fail "a validator-only permission added" "$tmp/v-extra.go"

echo "capability sync negatives: all four cases pass"
