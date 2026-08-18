#!/usr/bin/env bash
# Executable negatives for the manifest contract gate: the unmodified
# plugins tree passes, and EACH of a missing plugin, an extra plugin, a
# duplicate permission, an extra grant, a missing grant, a hook drift, and
# an upstream drift, a conflict drift, and a duplicate conflict FAIL the
# validator. A one-way inventory (rows without
# directories) or a silent drift fails here.
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
validator="$root/scripts/validate_manifests.go"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

expect() { # want-fail label plugins-dir
  if (cd "$root" && GOWORK=off go run "$validator" "$3") >/dev/null 2>&1; then
    if [[ "$1" == "fail" ]]; then
      echo "manifest contract negative: expected FAILURE for \"$2\", but validation passed" >&2
      exit 1
    fi
  else
    if [[ "$1" == "pass" ]]; then
      echo "manifest contract negative: expected PASS for \"$2\", but validation failed" >&2
      exit 1
    fi
  fi
}

cp_plugins() { # dest
  cp -r "$root/plugins" "$1"
}

# Unmodified tree passes.
expect pass "unmodified plugins tree" "$root/plugins"

# Missing plugin: otel removed.
cp_plugins "$tmp/minus-otel"
rm -rf "$tmp/minus-otel/otel"
expect fail "a missing plugin" "$tmp/minus-otel"

# Extra plugin: a full valid copy under a new name (id/name updated) has no
# contract-table row.
cp_plugins "$tmp/plus-extra"
cp -r "$tmp/plus-extra/otel" "$tmp/plus-extra/extraplugin"
python3 - "$tmp/plus-extra/extraplugin/plugin.json" << 'PYEOF'
import json, sys
f = sys.argv[1]
d = json.load(open(f))
d['id'] = 'torana/extraplugin'
d['name'] = 'extraplugin'
json.dump(d, open(f, 'w'), indent=2)
open(f, 'a').write('\n')
PYEOF
expect fail "an extra plugin" "$tmp/plus-extra"

# Duplicate permission in one manifest.
cp_plugins "$tmp/dup-perm"
python3 - "$tmp/dup-perm/pii/plugin.json" << 'PYEOF'
import json, sys
f = sys.argv[1]
d = json.load(open(f))
d['permissions'].append({'name': 'env.cache_get', 'description': 'duplicate'})
json.dump(d, open(f, 'w'), indent=2)
open(f, 'a').write('\n')
PYEOF
expect fail "a duplicate permission" "$tmp/dup-perm"

# Extra grant in one manifest.
cp_plugins "$tmp/extra-grant"
python3 - "$tmp/extra-grant/pii/plugin.json" << 'PYEOF'
import json, sys
f = sys.argv[1]
d = json.load(open(f))
d['permissions'].append({'name': 'env.log', 'description': 'stale'})
json.dump(d, open(f, 'w'), indent=2)
open(f, 'a').write('\n')
PYEOF
expect fail "an extra grant" "$tmp/extra-grant"

# Missing grant in one manifest.
cp_plugins "$tmp/missing-grant"
python3 - "$tmp/missing-grant/pii/plugin.json" << 'PYEOF'
import json, sys
f = sys.argv[1]
d = json.load(open(f))
d['permissions'] = [p for p in d['permissions'] if p['name'] != 'env.block_request']
json.dump(d, open(f, 'w'), indent=2)
open(f, 'a').write('\n')
PYEOF
expect fail "a missing grant" "$tmp/missing-grant"

# Hook drift in one manifest.
cp_plugins "$tmp/hook-drift"
python3 - "$tmp/hook-drift/pii/plugin.json" << 'PYEOF'
import json, sys
f = sys.argv[1]
d = json.load(open(f))
d['hooks'].append({'name': 'run_on_tick'})
json.dump(d, open(f, 'w'), indent=2)
open(f, 'a').write('\n')
PYEOF
expect fail "a hook drift" "$tmp/hook-drift"

# Upstream drift in one manifest.
cp_plugins "$tmp/upstream-drift"
python3 - "$tmp/upstream-drift/compactor/plugin.json" << 'PYEOF'
import json, sys
f = sys.argv[1]
d = json.load(open(f))
d['requires_upstream'] = ['torana/other']
json.dump(d, open(f, 'w'), indent=2)
open(f, 'a').write('\n')
PYEOF
expect fail "an upstream drift" "$tmp/upstream-drift"

# Conflict drift in one manifest.
cp_plugins "$tmp/conflict-drift"
python3 - "$tmp/conflict-drift/compactor/plugin.json" << 'PYEOF'
import json, sys
f = sys.argv[1]
d = json.load(open(f))
d['conflicts_with'] = ['torana/other']
json.dump(d, open(f, 'w'), indent=2)
open(f, 'a').write('\n')
PYEOF
expect fail "a conflict drift" "$tmp/conflict-drift"

# Duplicate conflict in one manifest.
cp_plugins "$tmp/duplicate-conflict"
python3 - "$tmp/duplicate-conflict/compactor/plugin.json" << 'PYEOF'
import json, sys
f = sys.argv[1]
d = json.load(open(f))
d['conflicts_with'].append(d['conflicts_with'][0])
json.dump(d, open(f, 'w'), indent=2)
open(f, 'a').write('\n')
PYEOF
expect fail "a duplicate conflict" "$tmp/duplicate-conflict"

echo "manifest contract negatives: all ten cases pass"
