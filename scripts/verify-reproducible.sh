#!/usr/bin/env bash
#
# Build every plugin from two DIFFERENT directories and require both builds to
# produce the same digest.
#
# Torana binds an operator's approval to a SHA-256 over plugin.json,
# plugin.wasm, schema.json and agent.json. If the same source can produce two
# different .wasm files, that approval means nothing: reinstalling identical
# code invalidates it, and an operator who reapproves out of habit is trained to
# click through the one control that protects them.
#
# The build DIRECTORY is the variable that matters, not the run. Go bakes
# absolute paths into a binary unless -trimpath is passed, and `torana plugin
# install` stages source into os.MkdirTemp("", "torana-build-*") — a randomized
# directory — so two installs of byte-identical source produced different
# digests. Building twice from one fixed path, as scripts/build.sh does, agrees
# with itself either way and proves nothing; this script deliberately varies the
# path so it can fail.
#
# A source-level guard (every build invocation carries -trimpath) lives in
# torana-edge's plugincmd tests.
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
sdk_dir=""
if [[ -d "$root/../torana-plugin-sdk" ]]; then
  sdk_dir=$(cd "$root/../torana-plugin-sdk" && pwd)
fi

plugins=()
if [[ $# -gt 0 ]]; then
  plugins=("$@")
else
  for dir in "$root"/plugins/*/; do
    plugins+=("$(basename "$dir")")
  done
fi

# Build one plugin from a copy at $2, print the digest of the artifact.
build_at() {
  local plugin=$1 stage=$2
  cp -r "$root/plugins/$plugin/." "$stage/"
  local cache
  cache=$(mktemp -d)
  (
    cd "$stage"
    if [[ -n "$sdk_dir" ]]; then
      local ws
      ws=$(mktemp -d)
      (cd "$ws" && go work init "$sdk_dir" "$stage" >/dev/null)
      export GOWORK="$ws/go.work"
    else
      export GOWORK=off
    fi
    GOCACHE="$cache" GOOS=wasip1 GOARCH=wasm \
      go build -trimpath -buildmode=c-shared -buildvcs=false -o "$stage/plugin.wasm" .
  )
  rm -rf "$cache"
  sha256sum "$stage/plugin.wasm" | cut -d' ' -f1
}

status=0
for plugin in "${plugins[@]}"; do
  # Two stage directories with different names AND different lengths, so a path
  # baked into the binary cannot coincidentally match.
  a=$(mktemp -d "${TMPDIR:-/tmp}/torana-repro-a-XXXXXX")
  b=$(mktemp -d "${TMPDIR:-/tmp}/torana-repro-bbbbbbbbbb-XXXXXX")
  trap 'rm -rf "$a" "$b"' RETURN

  first=$(build_at "$plugin" "$a")
  second=$(build_at "$plugin" "$b")
  rm -rf "$a" "$b"

  if [[ "$first" != "$second" ]]; then
    echo "NOT REPRODUCIBLE: $plugin" >&2
    echo "  built in $a: $first" >&2
    echo "  built in $b: $second" >&2
    status=1
  else
    echo "ok  $plugin  $first"
  fi
done

if [[ $status -ne 0 ]]; then
  echo >&2
  echo "Plugin builds must be reproducible or approval-by-digest is theatre:" >&2
  echo "reinstalling identical source would invalidate the operator's approval." >&2
  echo "Check that every 'go build' still passes -trimpath." >&2
fi
exit $status
