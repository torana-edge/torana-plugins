#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
repo="$tmp/sdk"
ref_file="$tmp/SDK_REF"

git init -q -b main "$repo"
git -C "$repo" -c user.name=Test -c user.email=test@example.invalid commit --allow-empty -qm base
base=$(git -C "$repo" rev-parse HEAD)
git -C "$repo" update-ref refs/remotes/origin/main HEAD
git -C "$repo" -c user.name=Test -c user.email=test@example.invalid commit --allow-empty -qm merged
merged=$(git -C "$repo" rev-parse HEAD)
git -C "$repo" update-ref refs/remotes/origin/main HEAD

printf '%s\n' "$merged" > "$ref_file"
SDK_REF_FILE="$ref_file" "$root/scripts/checkout_sdk_ref.sh" "$repo" >/dev/null
if [[ $(git -C "$repo" rev-parse HEAD) != "$merged" ]]; then
  echo "SDK ref checkout test: merged revision was not checked out" >&2
  exit 1
fi

git -C "$repo" checkout -q --detach "$base"
git -C "$repo" -c user.name=Test -c user.email=test@example.invalid commit --allow-empty -qm divergent
divergent=$(git -C "$repo" rev-parse HEAD)
git -C "$repo" checkout -q main
printf '%s\n' "$divergent" > "$ref_file"
if SDK_REF_FILE="$ref_file" "$root/scripts/checkout_sdk_ref.sh" "$repo" >/dev/null 2>&1; then
  echo "SDK ref checkout test: divergent revision was accepted" >&2
  exit 1
fi
if [[ $(git -C "$repo" rev-parse HEAD) != "$merged" ]]; then
  echo "SDK ref checkout test: failed validation mutated the checkout" >&2
  exit 1
fi
