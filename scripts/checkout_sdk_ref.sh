#!/usr/bin/env bash
set -euo pipefail

if [[ $# != 1 ]]; then
  echo "usage: $0 <sdk-checkout>" >&2
  exit 2
fi

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
sdk=$1
ref_file=${SDK_REF_FILE:-"$root/SDK_REF"}
ref=$(cat "$ref_file" 2>/dev/null || true)

if [[ -z "$ref" ]]; then
  echo "SDK_REF is empty or missing: $ref_file" >&2
  exit 1
fi
if ! git -C "$sdk" rev-parse --git-dir >/dev/null 2>&1; then
  echo "SDK checkout is not a Git repository: $sdk" >&2
  exit 1
fi
if ! git -C "$sdk" cat-file -e "$ref^{commit}" 2>/dev/null; then
  echo "SDK_REF $ref is not present in the SDK checkout" >&2
  exit 1
fi
if ! git -C "$sdk" merge-base --is-ancestor "$ref" refs/remotes/origin/main; then
  echo "SDK_REF $ref is not reachable from torana-plugin-sdk main" >&2
  exit 1
fi

git -C "$sdk" checkout --detach "$ref"
