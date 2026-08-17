#!/bin/sh
set -eu

workflow_dir=${1:-.github/workflows}
failed=0

for workflow in "$workflow_dir"/*.yml "$workflow_dir"/*.yaml; do
	[ -f "$workflow" ] || continue
	if ! awk '
		function indentation(line) {
			match(line, /[^ ]/)
			return RSTART == 0 ? length(line) : RSTART - 1
		}
		FNR == 1 { in_run = 0 }
		{
			level = indentation($0)
			if (in_run && $0 !~ /^[[:space:]]*$/ && level <= run_level) {
				in_run = 0
			}
			if ($0 ~ /^[[:space:]]*-[[:space:]]+run:[[:space:]]*/) {
				in_run = 1
				run_level = level
			}
			if (in_run && $0 ~ /\$\{\{[[:space:]]*inputs\./) {
				printf "%s:%d: workflow input is interpolated directly into shell: %s\n", FILENAME, FNR, $0 > "/dev/stderr"
				bad = 1
			}
		}
		END { exit bad }
	' "$workflow"; then
		failed=1
	fi
done

exit "$failed"
