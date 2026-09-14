#!/usr/bin/env bash
#
# Run torana-edge's official-plugin behaviour suite against the bundles this
# repository just built, and prove it actually ran.
#
# torana-edge tests the *host* with purpose-built fixtures and owns no copy of
# these plugins. Assertions about plugin behaviour — does pii detect PII, does
# the warmer stop at break-even, does the tier selector stay sticky — are gated
# on TORANA_PLUGIN_BUNDLES_DIR and skip there. This is the only place they run,
# so "green" is not enough: a gate that silently skipped everywhere looks
# exactly like a passing run.
#
# Hence the two assertions below. They are self-maintaining — no list of test
# names to keep in step with the suite.
#
# Usage: scripts/verify-behaviour.sh <path-to-torana-edge> <path-to-dist>

set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
edge_dir=${1:?usage: verify-behaviour.sh <torana-edge dir> <bundles dir>}
bundles=${2:?usage: verify-behaviour.sh <torana-edge dir> <bundles dir>}

edge_dir=$(cd "$edge_dir" && pwd)
bundles=$(cd "$bundles" && pwd)

if ! ls "$bundles"/*/plugin.wasm >/dev/null 2>&1; then
  echo "no built bundles in $bundles — did the build step run?" >&2
  exit 1
fi
echo "built bundles: $(ls -d "$bundles"/*/ | wc -l)"

# Exported from the argument so this script is the single source of truth for
# the variable, and so running it by hand works. Guard 1 below then catches a
# real drift — torana-edge renaming the variable its helper reads — rather than
# a caller who simply forgot to set it.
export TORANA_PLUGIN_BUNDLES_DIR="$bundles"
export TORANA_PLUGIN_SOURCE_DIR="$root/plugins"

log=$(mktemp)
fixture_log=$(mktemp)
suite_tmp=$(mktemp -d)
trap 'rm -f "$log" "$fixture_log"; rm -rf "$suite_tmp"' EXIT

# Reuse compiled wazero modules across the broad suite. Callers may provide a
# durable CI cache; standalone runs get a request-local cache with the same
# semantics instead of recompiling every guest for every test process.
export TORANA_CI_CACHE=${TORANA_CI_CACHE:-"$suite_tmp/wazero-cache"}
mkdir -p "$TORANA_CI_CACHE"
export TORANA_E2E=1

# torana-edge's OWN fixtures are build artifacts and are deliberately not
# committed, so they have to be built before its suite will run. Without this
# the fixture-backed tests skip — around 37 of them — and the skip guard below
# fails the build for skips this script caused itself.
echo "building torana-edge test fixtures"
(cd "$edge_dir" && make testdata) >"$fixture_log" 2>&1 || {
  echo "failed to build torana-edge test fixtures" >&2
  # Keep successful jobs quiet, but never discard the evidence needed to
  # diagnose a cross-repository fixture failure. Bound the output so one noisy
  # compiler cannot flood the workflow log.
  tail -80 "$fixture_log" >&2
  exit 1
}

# Resolve the exact SDK source Edge is compiled against. An explicit checkout
# is accepted only when its HEAD is that same module revision; otherwise use
# the immutable extracted module directory returned by the Go tool.
sdk_module=github.com/torana-edge/torana-plugin-sdk
sdk_version=$(cd "$edge_dir" && GOWORK=off go list -m -f '{{.Version}}' "$sdk_module")
sdk_metadata=$(cd "$edge_dir" && GOWORK=off go mod download -json "$sdk_module@$sdk_version")
resolved_sdk_dir=$(printf '%s' "$sdk_metadata" | jq -r '.Dir // empty')
sdk_revision=$(printf '%s' "$sdk_metadata" | jq -r '.Origin.Hash // empty')
if [[ -z "$resolved_sdk_dir" || -z "$sdk_revision" ]]; then
  echo "resolved SDK metadata lacks its source directory or VCS revision" >&2
  exit 1
fi
if [[ -n "${TORANA_SDK_DIR:-}" ]]; then
  provided_sdk_dir=$(cd "$TORANA_SDK_DIR" && pwd)
  provided_revision=$(git -C "$provided_sdk_dir" rev-parse HEAD)
  if [[ "$provided_revision" != "$sdk_revision" ]]; then
    echo "TORANA_SDK_DIR is $provided_revision, but Edge resolves SDK $sdk_revision" >&2
    exit 1
  fi
  export TORANA_SDK_DIR="$provided_sdk_dir"
else
  export TORANA_SDK_DIR="$resolved_sdk_dir"
fi

# The behaviour suite contains the production Edge roundtrip and both Rust
# scaffold acceptance paths. Build the exact SDK guest rather than allowing
# those tests to skip because no artifact happened to be present.
rust_target="$suite_tmp/rust-target"
PROTOC=${PROTOC:-$(command -v protoc)} \
  CARGO_TARGET_DIR="$rust_target" \
  cargo build --locked --target wasm32-wasip1 \
    --manifest-path "$TORANA_SDK_DIR/conformance/guests/rust-allhooks/Cargo.toml" \
    >"$fixture_log" 2>&1 || {
      echo "failed to build the exact SDK Rust conformance guest" >&2
      tail -80 "$fixture_log" >&2
      exit 1
    }
export TORANA_RUST_CONFORMANCE=1
export TORANA_RUST_GUEST="$rust_target/wasm32-wasip1/debug/torana-rust-allhooks.wasm"

# -v so the marker and skip reasons reach the log; -count=1 to defeat caching,
# which would otherwise let a stale pass stand in for a run that never happened.
status=0
# internal/plugincmd is included for TestCatalogMatchesThePluginRepository: it
# compares torana-edge's --official catalog against the plugins that actually
# exist HERE, and skips unless both repos are checked out. This job is the only
# place both are — so without it, the one guard against a shipped plugin being
# absent from the catalog runs nowhere at all.
# -timeout matches torana-edge's own gate (1800s) rather than sitting below it.
# At 900s this was always marginal — internal/plugin used 876s of that budget on
# the run before this one — and it tipped over into `panic: test timed out after
# 15m0s` with no assertion having failed. A timeout under the suite's real
# runtime reports a green tree as broken, which is the most expensive kind of
# wrong: it costs a debugging session to discover nothing was.
(cd "$edge_dir" && go test ./internal/plugin ./internal/proxy ./internal/wasm ./internal/plugincmd \
  -count=1 -v -timeout 1800s) >"$log" 2>&1 || status=$?

# Show failures without dumping several thousand lines of -v output.
if [ "$status" -ne 0 ]; then
  echo "--- test failures ---"
  # `|| true`: under `set -euo pipefail` a grep that matches nothing exits
  # non-zero and would terminate the script here — before the three guards
  # below print their counters, turning a red build into one with no
  # diagnostics at all.
  grep -E '^(---|\s+---) (FAIL|ERROR)|^[[:space:]]+[^[:space:]]+_test\.go:[0-9]+:|^(FAIL|ok|panic)' "$log" |
    grep -v 'official-plugin behaviour: bundles from' | head -80 || true
fi

# The marker is the one string this repository and torana-edge agree on by
# contract, and guard 3 fails if it disappears — so it is the only prose worth
# keying on.
ran=$(grep -c 'official-plugin behaviour: bundles from' "$log" || true)

# Skips are counted structurally instead. Matching a skip REASON meant
# hardcoding sentences owned by another repository: torana-edge already has a
# second wording ("TORANA_PLUGIN_BUNDLES_DIR is unset") that the previous
# pattern missed, so the guard was already partly blind. The broad wasm package
# now also contains retained, explicitly opt-in memory profiles; their exact
# names are classified separately while every other skip still fails closed.
skipped=$(grep -cE '^\s*--- SKIP:' "$log" || true)
# `|| true` on the pipeline too, not just the count: with pipefail a grep that
# matches nothing fails the whole pipeline, and set -e then kills the script
# before a single guard reports. Which is exactly what happened the first time
# this ran green.
skipped_names=$(grep -E '^\s*--- SKIP:' "$log" | sed -E 's/^[[:space:]]*--- SKIP:[[:space:]]*//; s/ \(.*//' | sort -u || true)
skip_filter="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/filter-behaviour-skips.sh"
unexpected_skips=$(printf '%s\n' "$skipped_names" | "$skip_filter" || true)
unexpected_count=$(printf '%s\n' "$unexpected_skips" | sed '/^$/d' | wc -l)

echo "--- behaviour suite ---"
echo "gated tests that ran: $ran"
echo "tests skipped:        $skipped"
echo "unexpected skips:     $unexpected_count"

# 1 & 2. No bundle/conformance row may skip. The exact opt-in memory profiles
#    are unrelated to plugin behavior and are the only permitted skips.
if [ "$unexpected_count" -ne 0 ]; then
  echo "FAIL: $unexpected_count unexpected test(s) skipped, but every official bundle was just built." >&2
  echo "      This job is the only place plugin behaviour runs, so a skip here is" >&2
  echo "      indistinguishable from that behaviour being untested." >&2
  echo "$unexpected_skips" | sed 's/^/        /' >&2
  status=1
fi

# 3. The gated tests still exist. Guards 1 and 2 are both vacuously satisfied by
#    a suite that has none left.
if [ "$ran" -eq 0 ]; then
  if ! grep -rqs 'officialBundlesDir' "$edge_dir/internal"; then
    echo "FAIL: this torana-edge checkout has no bundle-gated tests at all." >&2
    echo "      They arrive with torana-edge#217, which must merge before this job" >&2
    echo "      can verify anything. Until then there is nothing here to run." >&2
  else
    echo "FAIL: no gated test ran, though the gating helper exists. Either the" >&2
    echo "      tests were deleted, or the marker string changed." >&2
  fi
  status=1
fi

exit "$status"
