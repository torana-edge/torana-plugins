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
plugins_dir="${PLUGINS_DIR:-$root/plugins}"

# The pinned SDK module: EVERY plugin go.mod must require the SAME revision
# (the atomic Migration-C contract). Parse each module's direct SDK
# requirement; require exactly one unique non-empty version — a one-module
# drift fails here, never silently.
pin=""
for gomod in "$plugins_dir"/*/go.mod; do
  mod_pin=$(grep 'github.com/torana-edge/torana-plugin-sdk' "$gomod" | awk '{print $3}')
  if [[ -z "$mod_pin" ]]; then
    echo "capability sync: $gomod has no torana-plugin-sdk requirement" >&2
    exit 1
  fi
  if [[ -z "$pin" ]]; then
    pin="$mod_pin"
  elif [[ "$mod_pin" != "$pin" ]]; then
    echo "capability sync: plugin SDK pins disagree: $gomod requires $mod_pin, others require $pin" >&2
    exit 1
  fi
done
if [[ -z "$pin" ]]; then
  echo "capability sync: no torana-plugin-sdk pin found in $plugins_dir/*/go.mod" >&2
  exit 1
fi

# The checked-in SDK_REF must name the SAME revision every module pins (the
# atomic Migration-C contract): a drift between the reference and the pins
# fails here, never silently.
expected_ref=$(cat "$root/SDK_REF" 2>/dev/null || true)
if [[ -z "$expected_ref" ]]; then
  echo "capability sync: SDK_REF is empty or missing" >&2
  exit 1
fi
if [[ "$pin" != *"$expected_ref"* ]]; then
  echo "capability sync: SDK_REF $expected_ref does not match the pinned revision $pin" >&2
  exit 1
fi

# Resolve the agreed pin through Go module resolution (GOWORK=off) from a
# plugin module: a CLEAN checkout must work without a pre-warmed cache — the
# resolver downloads the declared dependency and returns its module directory.
first_module=$(echo "$plugins_dir"/*/go.mod | awk '{print $1}')
module_dir=$(dirname "$first_module")
# go mod download -json downloads the declared dependency (a clean
# GOMODCACHE works) and reports its module directory.
sdk=$(cd "$module_dir" && GOWORK=off go mod download -json github.com/torana-edge/torana-plugin-sdk 2>/dev/null | awk -F'"' '/"Dir"/{print $4; exit}')
if [[ -z "$sdk" || ! -f "$sdk/capabilities.go" || ! -f "$sdk/capabilities_write.go" ]]; then
  echo "capability sync: pinned SDK $pin could not be resolved (go mod download from $module_dir)" >&2
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

echo "capability sync: validator matches the SDK ($pin at $sdk)"
