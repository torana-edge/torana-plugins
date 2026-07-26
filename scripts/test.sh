#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cache=$(mktemp -d)
workspace_dir=$(mktemp -d)
trap 'rm -rf "$cache" "$workspace_dir"' EXIT

if [[ -d "$root/../torana-plugin-sdk" ]]; then
  modules=("$root/../torana-plugin-sdk")
  for module in "$root"/plugins/*; do modules+=("$module"); done
  (cd "$workspace_dir" && go work init "${modules[@]}")
  export GOWORK="$workspace_dir/go.work"
  # Prove the workspace actually resolved the sibling SDK. A version-pinned
  # `go work edit -replace` used to sit here and had silently stopped matching
  # at v0.1.1 -- the build kept working via the module list above, so nothing
  # ever reported that half the setup was dead. Assert the outcome instead of
  # trusting the plumbing.
  sdk_dir=$(cd "$root/../torana-plugin-sdk" && pwd)
  resolved=$(cd "$root/plugins/pii" && go list -m -f '{{.Dir}}' github.com/torana-edge/torana-plugin-sdk 2>/dev/null || true)
  case "$resolved" in
    "$sdk_dir"*) ;;
    *)
      echo "workspace did not resolve the sibling SDK (got '${resolved:-nothing}')" >&2
      exit 1
      ;;
  esac
else
  export GOWORK=off
fi

go run "$root/scripts/validate_manifests.go" "$root/plugins"
"$root/scripts/test_capability_sync.sh"
"$root/scripts/test_bundle_digest.sh"
for module in "$root"/plugins/*; do
  GOCACHE="$cache" go test "$module"
done
