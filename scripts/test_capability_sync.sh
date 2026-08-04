#!/usr/bin/env bash
# The manifest validator restates the SDK's capability vocabulary because it
# runs as a single-file `go run`, including when the SDK is not checked out
# alongside. Restating means it can drift, and it has: capabilities valid to
# the host were rejected here, so a correct plugin failed CI for a reason no
# contributor could act on.
#
# This compares the two lists whenever the SDK is resolvable. The SDK module
# is resolved DETERMINISTICALLY from the plugin go.mod pins in GOMODCACHE —
# there is no optional sibling-checkout skip; an unresolvable pin or an
# unreadable declaration is a failure, never a silent pass.
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
validator="${VALIDATOR:-$root/scripts/validate_manifests.go}"

# The pinned SDK module: every plugin go.mod requires the same revision
# (the atomic Migration-C contract); take the first pin.
pin=$(grep -h 'github.com/torana-edge/torana-plugin-sdk' "$root"/plugins/*/go.mod | head -1 | awk '{print $3}')
if [[ -z "$pin" ]]; then
  echo "capability sync: no torana-plugin-sdk pin found in plugins/*/go.mod" >&2
  exit 1
fi
gomodcache=$(cd "$root" && go env GOMODCACHE)
sdk="$gomodcache/github.com/torana-edge/torana-plugin-sdk@$pin"
if [[ ! -f "$sdk/capabilities.go" || ! -f "$sdk/capabilities_write.go" ]]; then
  echo "capability sync: pinned SDK $pin is not in the module cache ($sdk)" >&2
  exit 1
fi

# block prints the quoted strings between a declaration line and its closing
# brace. Comment lines are dropped first: a commented-out entry still
# contains a quoted string, and counting it would let the two lists drift
# while reporting a match.
block() { # file var
  sed -n "/^var $2 = /,/^}/p" "$1" \
    | sed 's|//.*||' \
    | grep -o '"[^"]*"' | tr -d '"' | sort -u
}

# The SDK's Permissions is `var Permissions = append([]string{...},
# WritePermissions...)`: the inner list closes with `}, WritePermissions...)`
# and the write grants live in capabilities_write.go. Extract the direct
# Permissions block (the env half) separately from the WritePermissions var,
# require BOTH non-empty, and union them.
env_block() { # file
  awk '/^var Permissions = append\(\[\]string\{/{f=1;next} f&&/^}, WritePermissions\.\.\.\)/{f=0} f' "$1" \
    | sed 's|//.*||' \
    | grep -o '"[^"]*"' | tr -d '"' | sort -u
}

env_perms=$(env_block "$sdk/capabilities.go")
write_perms=$(block "$sdk/capabilities_write.go" WritePermissions)
if [[ -z "$env_perms" || -z "$write_perms" ]]; then
  echo "capability sync: could not read the SDK's Permissions or WritePermissions — the declaration shape changed" >&2
  exit 1
fi
sdk_perms=$( { echo "$env_perms"; echo "$write_perms"; } | sort -u )
validator_perms=$(block "$validator" knownPermissions | sort -u)

if ! diff <(echo "$sdk_perms") <(echo "$validator_perms") >/dev/null; then
  echo "capability sync: knownPermissions differs from the SDK's Permissions+WritePermissions" >&2
  diff <(echo "$sdk_perms") <(echo "$validator_perms") | sed 's/^/  /' >&2
  exit 1
fi

hooks_sdk=$(block "$sdk/capabilities.go" Hooks)
hooks_validator=$(block "$validator" knownHooks)
if [[ -z "$hooks_sdk" || -z "$hooks_validator" ]]; then
  echo "capability sync: could not read Hooks or knownHooks — the declaration shape changed" >&2
  exit 1
fi
if ! diff <(echo "$hooks_sdk") <(echo "$hooks_validator") >/dev/null; then
  echo "capability sync: knownHooks differs from the SDK's Hooks" >&2
  diff <(echo "$hooks_sdk") <(echo "$hooks_validator") | sed 's/^/  /' >&2
  exit 1
fi

echo "capability sync: validator matches the SDK ($pin)"
