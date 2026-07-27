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

# -v so the marker and skip reasons reach the log; -count=1 to defeat caching,
# which would otherwise let a stale pass stand in for a run that never happened.
status=0
(cd "$edge_dir" && go test ./internal/plugin ./internal/proxy ./internal/wasm \
  -count=1 -v -timeout 900s) >"$log" 2>&1 || status=$?

# Show failures without dumping several thousand lines of -v output.
if [ "$status" -ne 0 ]; then
  echo "--- test failures ---"
  grep -E '^(---|\s+---) (FAIL|ERROR)|^(FAIL|ok|panic)' "$log" | head -80
fi

ran=$(grep -c 'official-plugin behaviour: bundles from' "$log" || true)
unset_skips=$(grep -c 'TORANA_PLUGIN_BUNDLES_DIR unset' "$log" || true)
missing_skips=$(grep -cE 'bundle "[^"]+" not present in' "$log" || true)

echo "--- behaviour suite ---"
echo "gated tests that ran:      $ran"
echo "skipped, bundles dir unset: $unset_skips"
echo "skipped, bundle missing:    $missing_skips"

# 1. The environment reached the tests. If it did not, every gated test skipped
#    and plugin behaviour is untested everywhere.
if [ "$unset_skips" -ne 0 ]; then
  echo "FAIL: $unset_skips test(s) skipped because TORANA_PLUGIN_BUNDLES_DIR was unset." >&2
  echo "      The variable is not reaching the test process." >&2
  status=1
fi

# 2. Every plugin the suite asks for was built. A rename or a build that quietly
#    produced nothing turns those tests into skips rather than failures.
if [ "$missing_skips" -ne 0 ]; then
  echo "FAIL: $missing_skips test(s) skipped for a missing bundle." >&2
  grep -oE 'bundle "[^"]+" not present in .*' "$log" | sort -u | head >&2
  status=1
fi

# 3. The gated tests still exist. Guards 1 and 2 are both vacuously satisfied by
#    a suite that has none left.
if [ "$ran" -eq 0 ]; then
  echo "FAIL: no gated test ran. Either they were deleted, or the marker in" >&2
  echo "      torana-edge's officialBundlesDir helper changed." >&2
  status=1
fi

exit "$status"
