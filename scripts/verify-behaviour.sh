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

log=$(mktemp)
trap 'rm -f "$log"' EXIT

# torana-edge's OWN fixtures are build artifacts and are deliberately not
# committed, so they have to be built before its suite will run. Without this
# the fixture-backed tests skip — around 37 of them — and the skip guard below
# fails the build for skips this script caused itself.
echo "building torana-edge test fixtures"
(cd "$edge_dir" && make testdata) >/dev/null 2>&1 || {
  echo "failed to build torana-edge test fixtures" >&2
  exit 1
}

# -v so the marker and skip reasons reach the log; -count=1 to defeat caching,
# which would otherwise let a stale pass stand in for a run that never happened.
status=0
# internal/plugincmd is included for TestCatalogMatchesThePluginRepository: it
# compares torana-edge's --official catalog against the plugins that actually
# exist HERE, and skips unless both repos are checked out. This job is the only
# place both are — so without it, the one guard against a shipped plugin being
# absent from the catalog runs nowhere at all.
(cd "$edge_dir" && go test ./internal/plugin ./internal/proxy ./internal/wasm ./internal/plugincmd \
  -count=1 -v -timeout 900s) >"$log" 2>&1 || status=$?

# Show failures without dumping several thousand lines of -v output.
if [ "$status" -ne 0 ]; then
  echo "--- test failures ---"
  # `|| true`: under `set -euo pipefail` a grep that matches nothing exits
  # non-zero and would terminate the script here — before the three guards
  # below print their counters, turning a red build into one with no
  # diagnostics at all.
  grep -E '^(---|\s+---) (FAIL|ERROR)|^[[:space:]]+[^[:space:]]+_test\.go:[0-9]+:|^(FAIL|ok|panic)' "$log" | head -80 || true
fi

# The marker is the one string this repository and torana-edge agree on by
# contract, and guard 3 fails if it disappears — so it is the only prose worth
# keying on.
ran=$(grep -c 'official-plugin behaviour: bundles from' "$log" || true)

# Skips are counted structurally instead. Matching a skip REASON meant
# hardcoding sentences owned by another repository: torana-edge already has a
# second wording ("TORANA_PLUGIN_BUNDLES_DIR is unset") that the previous
# pattern missed, so the guard was already partly blind. Any skip in these
# packages is suspect here, because the whole point of this job is that the
# bundles are present.
skipped=$(grep -cE '^\s*--- SKIP:' "$log" || true)
# `|| true` on the pipeline too, not just the count: with pipefail a grep that
# matches nothing fails the whole pipeline, and set -e then kills the script
# before a single guard reports. Which is exactly what happened the first time
# this ran green.
skipped_names=$(grep -E '^\s*--- SKIP:' "$log" | sed -E 's/^[[:space:]]*--- SKIP:[[:space:]]*//; s/ \(.*//' | sort -u || true)

echo "--- behaviour suite ---"
echo "gated tests that ran: $ran"
echo "tests skipped:        $skipped"

# 1 & 2. Nothing should skip. The bundles are built by the step before this one
#    and the directory is exported below, so a skip means the suite is not
#    actually exercising what this job exists to exercise — whatever reason the
#    test gave.
if [ "$skipped" -ne 0 ]; then
  echo "FAIL: $skipped test(s) skipped, but every official bundle was just built." >&2
  echo "      This job is the only place plugin behaviour runs, so a skip here is" >&2
  echo "      indistinguishable from that behaviour being untested." >&2
  echo "$skipped_names" | sed 's/^/        /' >&2
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
