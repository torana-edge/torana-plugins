#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

# No native build output in the tree.
#
# `go build ./...` inside a plugin module writes an executable named after the
# directory, and `git add -A` picks it up: ~7MB of ELF per plugin. That happened
# three times on three branches, because .gitignore only protects the branches
# carrying it — a branch cut from an older main does not — and gitignore does
# not untrack an already-tracked file, so once it lands it is permanent and
# churns on every contributor's build.
#
# Checked here because this runs on every branch regardless of its .gitignore.
stray=$(git -C "$root" ls-files 'plugins/*' | awk -F/ 'NF==3 && $NF !~ /\./' || true)
if [ -n "$stray" ]; then
  echo "tracked files with no extension under plugins/ — native build output?" >&2
  echo "$stray" | sed 's/^/  /' >&2
  echo "The only artifact this repo ships is dist/<plugin>/plugin.wasm;" >&2
  echo "remove with: git rm --cached <path>" >&2
  exit 1
fi
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
  # Any plugin module will do — the workspace resolves the SDK the same way for
  # all of them. Naming one made the check fail with "did not resolve the
  # sibling SDK" if that plugin were ever renamed, which is not what went wrong.
  probe=$(find "$root/plugins" -mindepth 2 -maxdepth 2 -name go.mod -print -quit)
  if [ -z "$probe" ]; then
    echo "no plugin modules found under $root/plugins" >&2
    exit 1
  fi
  resolved=$(cd "$(dirname "$probe")" && go list -m -f '{{.Dir}}' github.com/torana-edge/torana-plugin-sdk 2>/dev/null || true)
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
"$root/scripts/test_manifest_contract_negatives.sh"
"$root/scripts/test_capability_sync.sh"
"$root/scripts/test_capability_sync_negatives.sh"
"$root/scripts/test_bundle_digest.sh"
for module in "$root"/plugins/*; do
  GOCACHE="$cache" go test "$module"
done
