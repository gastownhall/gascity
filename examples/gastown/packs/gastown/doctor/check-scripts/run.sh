#!/usr/bin/env bash
# Pack doctor check: verify pack scripts are executable.
#
# Exit codes: 0=OK, 1=Warning, 2=Error
# stdout: first line=message, rest=details

dir="${GC_PACK_DIR:-.}"
non_exec=()

while IFS= read -r -d '' script; do
    if [ ! -x "$script" ]; then
        non_exec+=("${script#"$dir"/}")
    fi
done < <(find "$dir/assets/scripts" -name '*.sh' -print0 2>/dev/null)

if [ ${#non_exec[@]} -eq 0 ]; then
    echo "all pack scripts are executable"
    exit 0
fi

echo "${#non_exec[@]} script(s) not executable"
for s in "${non_exec[@]}"; do
    echo "$s"
done
exit 1
