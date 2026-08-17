#!/bin/sh
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
fixtures=$(mktemp -d)
trap 'rm -rf "$fixtures"' EXIT

cat >"$fixtures/safe.yml" <<'EOF'
jobs:
  release:
    steps:
      - env:
          RELEASE_VERSION: ${{ inputs.version }}
        run: ./package.sh "$RELEASE_VERSION"
EOF
"$root/scripts/check-workflow-shell-inputs.sh" "$fixtures"

cat >"$fixtures/unsafe-inline.yml" <<'EOF'
jobs:
  release:
    steps:
      - run: ./package.sh '${{ inputs.version }}'
EOF
if "$root/scripts/check-workflow-shell-inputs.sh" "$fixtures" >/dev/null 2>&1; then
	echo "direct input interpolation on an inline run was accepted" >&2
	exit 1
fi
rm "$fixtures/unsafe-inline.yml"

cat >"$fixtures/unsafe-block.yml" <<'EOF'
jobs:
  release:
    steps:
      - run: |
          ./package.sh "${{ inputs.version }}"
EOF
if "$root/scripts/check-workflow-shell-inputs.sh" "$fixtures" >/dev/null 2>&1; then
	echo "direct input interpolation in a run block was accepted" >&2
	exit 1
fi
