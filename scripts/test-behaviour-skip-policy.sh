#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
filter="$root/scripts/filter-behaviour-skips.sh"

allowed=$(printf '%s\n' \
  TestGuestIdleRetirementProfile \
  TestGuestHostMemoryProfile/go \
  TestGuestHostMemoryProfile/rust \
  TestGuestLinearMemoryProfile/go \
  TestGuestLinearMemoryProfile/rust \
  TestGuestLinearMemoryRepeatedProfile/go \
  TestGuestLinearMemoryRepeatedProfile/rust)

if unexpected=$(printf '%s\n' "$allowed" | "$filter"); then
  if [[ -n "$unexpected" ]]; then
    echo "allowed profile skips produced output: $unexpected" >&2
    exit 1
  fi
else
  echo "allowed profile skips were rejected" >&2
  exit 1
fi

set +e
unexpected=$(printf '%s\n' TestRealOfficialPluginSchemas | "$filter")
status=$?
set -e
if [[ "$status" -eq 0 || "$unexpected" != TestRealOfficialPluginSchemas ]]; then
  echo "an official-plugin skip was not rejected exactly" >&2
  exit 1
fi

set +e
unexpected=$(printf '%s\n' TestGuestFutureProfile/go | "$filter")
status=$?
set -e
if [[ "$status" -eq 0 || "$unexpected" != TestGuestFutureProfile/go ]]; then
  echo "a future profiling skip was silently accepted" >&2
  exit 1
fi

echo "behaviour skip policy: exact allowlist passes"
