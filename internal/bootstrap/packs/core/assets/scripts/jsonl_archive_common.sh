#!/usr/bin/env bash
# jsonl_archive_common — helpers the JSONL export and the archive settle tool
# share. Source it; it defines functions only and runs nothing.
#
# Everything here reads the archive and writes nothing under its .git.

# archive_window_memory LARGEST_BLOB_BYTES
#
# The pack.windowMemory a repack of the archive runs under, as git's "<n>m":
# max(256 MiB, 6 x the largest blob), rounded up to whole MiB. Git keeps every
# object in the delta window in memory and stops trying a base once the window
# holds more than pack.windowMemory; a blob larger than a quarter of the
# window leaves no room to compare it with its predecessors, so it is stored
# whole or against a far worse base. 6 x leaves the window room for the blob
# and its neighbours.
archive_window_memory() {
    local largest="$1"
    local mib

    case "$largest" in
        ''|*[!0-9]*)
            echo "archive_window_memory: largest blob size must be a non-negative integer, got '$largest'" >&2
            return 1
            ;;
    esac
    mib=$(((6 * largest + 1048575) / 1048576))
    if [ "$mib" -lt 256 ]; then
        mib=256
    fi
    printf '%sm\n' "$mib"
}

# archive_window_bytes VALUE
#
# VALUE (git's pack.windowMemory spelling: a byte count with an optional k, m
# or g suffix in either case, k = 1024) in bytes.
archive_window_bytes() {
    local value="$1"
    local number="${value%[kKmMgG]}"
    local unit=1

    case "$number" in
        ''|*[!0-9]*)
            echo "archive_window_bytes: '$value' is not a byte count with an optional k, m or g suffix" >&2
            return 1
            ;;
    esac
    case "${value#"$number"}" in
        k|K) unit=1024 ;;
        m|M) unit=1048576 ;;
        g|G) unit=1073741824 ;;
    esac
    # A leading 0 would read as octal; strip them.
    number="${number#"${number%%[!0]*}"}"
    printf '%s\n' "$(( ${number:-0} * unit ))"
}

# archive_window_guard VALUE LARGEST_BLOB_BYTES
#
# Fails, naming VALUE, when VALUE is under 4 x the largest blob.
archive_window_guard() {
    local value="$1"
    local largest="$2"
    local bytes

    bytes=$(archive_window_bytes "$value") || return 1
    if [ "$bytes" -lt "$((4 * largest))" ]; then
        echo "pack.windowMemory=$value is $bytes bytes, under 4 x the largest blob ($largest bytes = $((4 * largest)) bytes): the delta window would collapse around that blob" >&2
        return 1
    fi
}

# archive_largest_blob_bytes REPO
#
# Prints the size in bytes of the largest blob in REPO. Fails when git fails,
# when its output does not parse, or when REPO holds no blob, so the caller
# never packs under a window it did not measure.
archive_largest_blob_bytes() {
    local repo="$1"
    local sizes

    sizes=$(git -C "$repo" cat-file --batch-all-objects --batch-check='%(objecttype) %(objectsize)') || return 1
    printf '%s\n' "$sizes" | awk '
        $1 == "blob" {
            if ($2 !~ /^[0-9]+$/ || NF != 2) { bad = 1; exit }
            if ($2 + 0 > max) { max = $2 + 0 }
            n++
        }
        END {
            if (bad || n == 0) { exit 1 }
            printf "%.0f\n", max
        }
    '
}

# archive_flagged_deltas REPO MIN_DISK_KB MIN_RATIO_PCT [TIMEOUT_SECS]
#
# Prints "<oid> <logical bytes> <disk bytes> <delta base>" for every object
# stored as a delta whose on-disk size is at least
# max(MIN_DISK_KB KiB, MIN_RATIO_PCT percent of its logical size): a delta
# that large is a whole-file-sized copy, not a saving. Read-only. Stops
# after TIMEOUT_SECS (0 or no timeout command: no limit), exit status 124.
archive_flagged_deltas() {
    local repo="$1"
    local min_kb="$2"
    local min_pct="$3"
    local secs="${4:-0}"
    local -a limit=()

    if [ "$secs" -gt 0 ] && command -v timeout >/dev/null 2>&1; then
        limit=(timeout "$secs")
    fi
    ${limit[@]+"${limit[@]}"} git -C "$repo" cat-file --batch-all-objects \
        --batch-check='%(objectname) %(objectsize) %(objectsize:disk) %(deltabase)' \
        | awk -v min_bytes="$((min_kb * 1024))" -v pct="$min_pct" '
            NF == 4 && $4 !~ /^0+$/ {
                floor = $2 * pct / 100
                if (floor < min_bytes) { floor = min_bytes }
                if ($3 + 0 >= floor) { print }
            }
        '
}
