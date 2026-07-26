#!/usr/bin/env bash
# The manifest validator restates the SDK's capability vocabulary because it
# runs as a single-file `go run`, including when the SDK is not checked out
# alongside. Restating means it can drift, and it has: capabilities valid to the
# host were rejected here, so a correct plugin failed CI for a reason no
# contributor could act on.
#
# This compares the two lists whenever the SDK is available, and is a no-op
# otherwise rather than a failure, since the validator must keep working
# without it.
set -euo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
sdk="$root/../torana-plugin-sdk/capabilities.go"
validator="$root/scripts/validate_manifests.go"

if [[ ! -f "$sdk" ]]; then
  echo "capability sync: skipped (torana-plugin-sdk not checked out alongside)"
  exit 0
fi

# Print the quoted strings between a declaration line and its closing brace.
# Comment lines are dropped first: a commented-out entry still contains a quoted
# string, and counting it would let the two lists drift while reporting a match.
block() {
  sed -n "/^var $2 = /,/^}/p" "$1" \
    | sed 's|//.*||' \
    | grep -o '"[^"]*"' | tr -d '"' | sort -u
}

fail=0
compare() { # sdk-var validator-var
  local a b
  a=$(block "$sdk" "$1")
  b=$(block "$validator" "$2")
  if [[ -z "$a" || -z "$b" ]]; then
    echo "capability sync: could not read $1 or $2 — the declaration shape changed" >&2
    fail=1
    return
  fi
  if ! diff <(echo "$a") <(echo "$b") >/dev/null; then
    echo "capability sync: $2 differs from the SDK's $1" >&2
    diff <(echo "$a") <(echo "$b") | sed 's/^/  /' >&2
    fail=1
  fi
}

compare Hooks knownHooks
compare Permissions knownPermissions
[[ $fail -eq 0 ]] && echo "capability sync: validator matches the SDK"
exit $fail
