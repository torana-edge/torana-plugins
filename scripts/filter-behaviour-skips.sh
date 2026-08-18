#!/usr/bin/env bash
# Print only skips that are not deliberate opt-in Edge profiling surfaces.
#
# The official-plugin behavior gate runs broad Edge packages because behavior
# rows share helpers with host conformance tests. Edge's retained WASM memory
# profiles live in one of those packages and intentionally skip unless their
# explicit guest-artifact environment is supplied. They are not bundle-gated
# behavior, so treating those exact skips as missing plugin coverage makes the
# release gate impossible to run. The allowlist is exact and fail-closed: any
# new test or subtest name remains an unexpected skip until reviewed here.
set -euo pipefail

status=0
while IFS= read -r name; do
  case "$name" in
    "") ;;
    TestGuestIdleRetirementProfile | \
      TestGuestHostMemoryProfile/go | \
      TestGuestHostMemoryProfile/rust | \
      TestGuestLinearMemoryProfile/go | \
      TestGuestLinearMemoryProfile/rust | \
      TestGuestLinearMemoryRepeatedProfile/go | \
      TestGuestLinearMemoryRepeatedProfile/rust) ;;
    *)
      printf '%s\n' "$name"
      status=1
      ;;
  esac
done
exit "$status"
