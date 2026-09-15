#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

"$root/scripts/check-go-toolchain-source.sh"

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
workspace_dir=$(mktemp -d)
trap 'rm -rf "$workspace_dir"' EXIT

sdk_dir="${TORANA_SDK_DIR:-$root/../torana-plugin-sdk}"
if [[ -n "${TORANA_SDK_DIR:-}" && ( ! -d "$sdk_dir" || ! -f "$sdk_dir/go.mod" ) ]]; then
  echo "SDK directory is missing or has no go.mod: $sdk_dir" >&2
  exit 1
fi
if [[ -d "$sdk_dir" && -f "$sdk_dir/go.mod" ]]; then
  sdk_dir=$(cd "$sdk_dir" && pwd)
  modules=("$sdk_dir")
  for module in "$root"/plugins/*; do modules+=("$module"); done
  (cd "$workspace_dir" && go work init "${modules[@]}")
  export GOWORK="$workspace_dir/go.work"
  # Prove the workspace actually resolved the sibling SDK. A version-pinned
  # `go work edit -replace` used to sit here and had silently stopped matching
  # at v0.1.1 -- the build kept working via the module list above, so nothing
  # ever reported that half the setup was dead. Assert the outcome instead of
  # trusting the plumbing.
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
go test "$root/scripts/doccheck.go" "$root/scripts/doccheck_test.go"
go run "$root/scripts/doccheck.go" "$root/plugins"
"$root/scripts/check-workflow-shell-inputs.sh"
"$root/scripts/check-setup-go-cache-paths.sh"
"$root/scripts/test-workflow-shell-inputs.sh"
"$root/scripts/test-behaviour-skip-policy.sh"
"$root/scripts/test-behaviour-fixture-diagnostics.sh"
"$root/scripts/test_manifest_contract_negatives.sh"
"$root/scripts/test_capability_sync.sh"
"$root/scripts/test_capability_sync_negatives.sh"
"$root/scripts/test_sdk_ref_checkout.sh"
"$root/scripts/test_build_without_sibling.sh"
"$root/scripts/test_bundle_digest.sh"
for module in "$root"/plugins/*; do
  (cd "$module" && go test ./...)
done
