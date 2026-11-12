#!/usr/bin/env bash
# jsonl-archive-settle — re-chain the oversized deltas in the local JSONL
# archive without losing any object.
#
# A delta that takes as many bytes on disk as the file it encodes is a
# whole-file copy dressed as a delta: git picked a base from another file
# because the delta window was too small to hold the right one. Repacking with
# a window sized for the largest blob (jsonl_archive_common.sh) stops new ones;
# this script repairs the ones already in the archive's packs.
#
# Usage: jsonl-archive-settle.sh [--archive PATH] [--apply]
#
# Without --apply it only reports what an apply would do and writes nothing
# under the archive's .git. --apply is the only mutating flag; there is no
# force. The archive is --archive, else $GC_JSONL_ARCHIVE_REPO, else
# $GC_PACK_STATE_DIR/jsonl-archive.
#
# One --apply is one transaction:
#   1. preflight: take the archive lock, refuse on a .keep pack or too little
#      free disk;
#   2. select: F = the flagged deltas; C = F, their bases and every object that
#      is a delta (transitively) on one of those; P = the packs holding any
#      object of C;
#   3. stage N1 = C packed afresh (--no-reuse-delta, a window sized for C's
#      largest blob);
#   4. stage R = every object of P that is not in C, packed with its deltas
#      reused as they are (C is closed under "is a delta on", so R's bases are
#      all in R and its chains keep their shape);
#   5. verify both packs and that every object of P is in N1, R or a pack that
#      stays;
#   6. promote N1 and R into objects/pack (.rev, .pack, then .idx last);
#   7. retire each pack of P after checking its objects are all held elsewhere
#      (.idx first, then the rest);
#   8. verify again: fsck and the object-id completeness check.
# A failure before step 6 deletes the staging directory and changes nothing.
# The script never runs gc or repack, commits, pushes, fetches, touches a ref
# or writes the archive's config.
#
# Exit: 0 done (or dry run), 1 refused or failed, 2 bad invocation.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck disable=SC1091
. "$SCRIPT_DIR/jsonl_archive_common.sh"

usage() {
    cat >&2 <<'EOF'
usage: jsonl-archive-settle.sh [--archive PATH] [--apply]

  --archive PATH  the archive repository (default: $GC_JSONL_ARCHIVE_REPO, then
                  $GC_PACK_STATE_DIR/jsonl-archive)
  --apply         rewrite the packs; without it, report only
EOF
}

die_usage() {
    echo "jsonl-archive-settle: $*" >&2
    usage
    exit 2
}

ARCHIVE_ARG=""
APPLY=0
while [ "$#" -gt 0 ]; do
    case "$1" in
        --archive)
            [ "$#" -ge 2 ] || die_usage "--archive needs a path"
            ARCHIVE_ARG="$2"
            shift 2
            ;;
        --archive=*)
            ARCHIVE_ARG="${1#--archive=}"
            shift
            ;;
        --apply)
            APPLY=1
            shift
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            die_usage "unknown argument: $1"
            ;;
    esac
done

MIN_DISK_KB="${GC_JSONL_SETTLE_MIN_DISK_KB:-256}"
MIN_RATIO_PCT="${GC_JSONL_SETTLE_MIN_RATIO_PCT:-1}"
MIN_FREE_KB="${GC_JSONL_GC_MIN_FREE_KB:-512000}"
BIG_FILE_THRESHOLD="${GC_JSONL_BIG_FILE_THRESHOLD:-4g}"
FAULT="${GC_JSONL_SETTLE_FAULT:-}"
MEMINFO="${GC_JSONL_SETTLE_MEMINFO:-/proc/meminfo}"

require_non_negative_integer() {
    local name="$1"
    local value="$2"

    case "$value" in
        ''|*[!0-9]*) die_usage "$name must be a non-negative integer, got '$value'" ;;
    esac
    # [ ] rejects a value beyond the 64-bit range with an error.
    [ "$value" -ge 0 ] 2>/dev/null || die_usage "$name must be at most 9223372036854775807"
}

require_non_negative_integer GC_JSONL_SETTLE_MIN_DISK_KB "$MIN_DISK_KB"
require_non_negative_integer GC_JSONL_SETTLE_MIN_RATIO_PCT "$MIN_RATIO_PCT"
require_non_negative_integer GC_JSONL_GC_MIN_FREE_KB "$MIN_FREE_KB"
# Git reads core.bigFileThreshold as a byte count with an optional unit suffix,
# takes 0 to mean no limit and a leading 0 as octal.
case "${BIG_FILE_THRESHOLD%[kKmMgG]}" in
    ''|0*|*[!0-9]*) die_usage "GC_JSONL_BIG_FILE_THRESHOLD must be a positive byte count, without leading zeros, with an optional k, m or g suffix (e.g. 4g)" ;;
esac
case "$FAULT" in
    ''|after-stage|after-verify|after-promote|mid-retire) ;;
    *) die_usage "GC_JSONL_SETTLE_FAULT must be one of after-stage, after-verify, after-promote, mid-retire, got '$FAULT'" ;;
esac

ARCHIVE_REPO="${ARCHIVE_ARG:-${GC_JSONL_ARCHIVE_REPO:-${GC_PACK_STATE_DIR:+$GC_PACK_STATE_DIR/jsonl-archive}}}"
[ -n "$ARCHIVE_REPO" ] || die_usage "no archive: pass --archive PATH or set GC_JSONL_ARCHIVE_REPO or GC_PACK_STATE_DIR"
[ -d "$ARCHIVE_REPO" ] || die_usage "$ARCHIVE_REPO is not a directory"
[ -d "$ARCHIVE_REPO/.git/objects" ] || die_usage "$ARCHIVE_REPO is not a git repository"
GIT_DIR_PATH="$ARCHIVE_REPO/.git"
PACK_DIR="$GIT_DIR_PATH/objects/pack"
STAGE_DIR="$GIT_DIR_PATH/jsonl-archive-settle.stage"
LOCK_FILE="$GIT_DIR_PATH/jsonl-archive.lock"

MODE=dry-run
[ "$APPLY" -eq 1 ] && MODE=apply

# LC_ALL=C: sort, comm and awk must agree on one byte order.
export LC_ALL=C

FLAGGED=0
GROUP_COUNT=0
BYTES_BEFORE=0
BYTES_AFTER=0
STATUS=failed
WORK=""
STAGE_OWNED=0
CRASHED=0

emit_telemetry() {
    printf 'jsonl-archive-settle: mode=%s flagged=%s groups=%s bytes_before=%s bytes_after=%s wall=%ss status=%s\n' \
        "$MODE" "$FLAGGED" "$GROUP_COUNT" "$BYTES_BEFORE" "$BYTES_AFTER" "$SECONDS" "$STATUS"
}

cleanup() {
    # A fault stands in for a kill: it leaves the staging directory behind, as
    # a kill would, for the next run to clear.
    if [ "$STAGE_OWNED" -eq 1 ] && [ "$CRASHED" -eq 0 ]; then
        rm -rf "$STAGE_DIR"
    fi
    if [ -n "$WORK" ]; then
        rm -rf "$WORK"
    fi
}
trap cleanup EXIT

refuse() {
    echo "jsonl-archive-settle: refused: $*" >&2
    STATUS=refused
    emit_telemetry
    exit 1
}

fail() {
    echo "jsonl-archive-settle: failed: $*" >&2
    STATUS=failed
    emit_telemetry
    exit 1
}

# fault_point NAME: exits 1 without cleaning up when GC_JSONL_SETTLE_FAULT
# names this boundary.
fault_point() {
    if [ "$FAULT" = "$1" ]; then
        CRASHED=1
        echo "jsonl-archive-settle: fault injected at $1" >&2
        exit 1
    fi
}

archive_pack_bytes() {
    git -C "$ARCHIVE_REPO" count-objects -v | awk '$1 == "size-pack:" { printf "%.0f\n", $2 * 1024; found = 1 } END { if (!found) print 0 }'
}

file_bytes() {
    wc -c < "$1" | tr -d ' '
}

# pack_ids IDX OUT: the sorted object ids of the pack index IDX, one per line.
pack_ids() {
    local idx="$1"
    local out="$2"

    git -C "$ARCHIVE_REPO" show-index < "$idx" | awk '{ print $2 }' | sort > "$out"
}

# Packing runs at the lowest CPU and I/O priority the host allows. With -t,
# ionice still runs git when it cannot set the I/O class; the probe leaves out
# an ionice that cannot start a command here at all.
PRIO=()
if command -v nice >/dev/null 2>&1; then
    PRIO=(nice -n 19)
fi
if command -v ionice >/dev/null 2>&1 && ionice -t -c 3 true >/dev/null 2>&1; then
    PRIO=(ionice -t -c 3 ${PRIO[@]+"${PRIO[@]}"})
fi

WORK="$(mktemp -d "${TMPDIR:-/tmp}/jsonl-archive-settle.XXXXXX")" || {
    echo "jsonl-archive-settle: could not create a scratch directory under ${TMPDIR:-/tmp}" >&2
    exit 1
}

BYTES_BEFORE=$(archive_pack_bytes) || fail "git count-objects failed in $ARCHIVE_REPO"
BYTES_AFTER="$BYTES_BEFORE"

# --- preflight: the lock ----------------------------------------------------

if [ "$APPLY" -eq 1 ]; then
    if ! command -v flock >/dev/null 2>&1; then
        refuse "flock is not installed, so the archive lock cannot be taken; --apply needs it (a dry run does not)"
    fi
    # fd 9 stays open for the whole run, so the git children inherit the lock.
    exec 9>>"$LOCK_FILE"
    if ! flock -n 9; then
        refuse "the archive lock ($LOCK_FILE) is held by a running export or settle; try again when it finishes"
    fi
    # Under the lock, nothing else is writing the archive's packs: a staging
    # directory left by an interrupted run is stale.
    if [ -e "$STAGE_DIR" ]; then
        echo "jsonl-archive-settle: clearing a stale staging directory ($STAGE_DIR)" >&2
        rm -rf "$STAGE_DIR"
    fi
fi

# --- select ------------------------------------------------------------------

OBJECTS="$WORK/objects"
FLAGGED_FILE="$WORK/flagged"
git -C "$ARCHIVE_REPO" cat-file --batch-all-objects \
    --batch-check='%(objectname) %(objecttype) %(objectsize) %(objectsize:disk) %(deltabase)' > "$OBJECTS" \
    || fail "git cat-file could not list the objects of $ARCHIVE_REPO"
archive_flagged_deltas "$ARCHIVE_REPO" "$MIN_DISK_KB" "$MIN_RATIO_PCT" > "$FLAGGED_FILE" \
    || fail "the oversized-delta detector failed in $ARCHIVE_REPO"
FLAGGED=$(wc -l < "$FLAGGED_FILE" | tr -d ' ')

C_IDS="$WORK/c.ids"
C_PATHS="$WORK/c.paths"
: > "$C_IDS"
: > "$C_PATHS"
if [ "$FLAGGED" -gt 0 ]; then
    # C0 = the flagged deltas and their bases; C adds every object that is a
    # delta, directly or through a chain, on a member.
    awk '{ print $1; print $4 }' "$FLAGGED_FILE" | sort -u > "$WORK/c0.ids"
    awk '
        function resolve(o,    top, stack, r, k) {
            top = 0
            while (!(o in memo) && (o in base)) {
                stack[++top] = o
                o = base[o]
            }
            if (o in memo) {
                r = memo[o]
            } else {
                r = (o in c0) ? 1 : 0
                memo[o] = r
            }
            while (top > 0) {
                k = stack[top--]
                if (k in c0) { r = 1 }
                memo[k] = r
            }
            return r
        }
        FILENAME == ARGV[1] { c0[$1] = 1; next }
        {
            n++
            id[n] = $1
            if ($5 !~ /^0+$/) { base[$1] = $5 }
        }
        END {
            for (i = 1; i <= n; i++) {
                if (resolve(id[i])) { print id[i] }
            }
        }
    ' "$WORK/c0.ids" "$OBJECTS" | sort -u > "$C_IDS" || fail "could not compute the closure of the flagged deltas"

    # The path each object of C has in the history, for the report and as the
    # name hint that lets git group versions of one file in its delta window.
    git -C "$ARCHIVE_REPO" rev-list --objects --all | awk '
        FILENAME == ARGV[1] { c[$1] = 1; next }
        ($1 in c) && !($1 in seen) {
            seen[$1] = 1
            i = index($0, " ")
            if (i > 0) { print $1, substr($0, i + 1) }
        }
    ' "$C_IDS" - > "$C_PATHS" || fail "git rev-list could not list the paths of the flagged objects"
fi
CLOSURE_OBJECTS=$(wc -l < "$C_IDS" | tr -d ' ')

# S(C): the largest blob in the closure (any object when it holds no blob),
# and the closure's logical bytes.
read -r LARGEST_BLOB_BYTES CLOSURE_BYTES < <(awk '
    FILENAME == ARGV[1] { c[$1] = 1; next }
    ($1 in c) {
        if ($2 == "blob" && $3 + 0 > blob) { blob = $3 + 0 }
        if ($3 + 0 > any) { any = $3 + 0 }
        total += $3
    }
    END { printf "%.0f %.0f\n", (blob > 0 ? blob : any), total }
' "$C_IDS" "$OBJECTS")
WINDOW_MEMORY=$(archive_window_memory "$LARGEST_BLOB_BYTES") || fail "could not derive the delta window from the largest blob"
archive_window_guard "$WINDOW_MEMORY" "$LARGEST_BLOB_BYTES" || fail "the derived delta window is under 4 x the largest blob"
LARGEST_BLOB_KB=$(((LARGEST_BLOB_BYTES + 1023) / 1024))
PEAK_RSS_KB=$(((9 * LARGEST_BLOB_BYTES + 1023) / 1024))

MEM_AVAILABLE_KB=unknown
if [ -r "$MEMINFO" ]; then
    mem=$(awk '$1 == "MemAvailable:" && $2 ~ /^[0-9]+$/ { print $2; exit }' "$MEMINFO" 2>/dev/null) || mem=""
    if [ -n "$mem" ]; then
        MEM_AVAILABLE_KB="$mem"
    fi
fi

# Per-pack object lists, then P (the packs holding an object of C) and the
# packs that are redundant outright (every object also lives in another pack,
# which is what a run interrupted after its promote leaves behind).
PACKS=()
for idx in "$PACK_DIR"/pack-*.idx; do
    [ -e "$idx" ] || continue
    name=$(basename "$idx" .idx)
    PACKS+=("$name")
    pack_ids "$idx" "$WORK/ids.$name" || fail "git show-index could not read $idx"
done
RETIRE=()
IN_P=()
if [ "${#PACKS[@]}" -gt 0 ]; then
    id_files=()
    for name in "${PACKS[@]}"; do
        id_files+=("$WORK/ids.$name")
    done
    sort -m "${id_files[@]}" | uniq -d > "$WORK/dup.ids"
    for name in "${PACKS[@]}"; do
        comm -12 "$C_IDS" "$WORK/ids.$name" > "$WORK/hit"
        if [ -s "$WORK/hit" ]; then
            IN_P+=("$name")
            RETIRE+=("$name")
            continue
        fi
        comm -23 "$WORK/ids.$name" "$WORK/dup.ids" > "$WORK/unique"
        if [ -s "$WORK/ids.$name" ] && [ ! -s "$WORK/unique" ]; then
            RETIRE+=("$name")
        fi
    done
fi
PACKS_TO_RETIRE="${#RETIRE[@]}"

# What the rewrite reads and writes, to the nearest KiB up: the flagged deltas'
# on-disk bytes bound what it reclaims; the packs of P are copied (R) and C is
# packed afresh (N1, at most its logical size) before anything is deleted.
FLAGGED_DISK_BYTES=$(awk '{ total += $3 } END { printf "%.0f\n", total }' "$FLAGGED_FILE")
ESTIMATED_RECLAIM_KB=$(((FLAGGED_DISK_BYTES + 1023) / 1024))
P_BYTES=0
for name in ${IN_P[@]+"${IN_P[@]}"}; do
    P_BYTES=$((P_BYTES + $(file_bytes "$PACK_DIR/$name.pack")))
done
TRANSIENT_NEED_KB=$(((P_BYTES + CLOSURE_BYTES + 1023) / 1024))

ORPHANS=()
for file in "$PACK_DIR"/pack-*.pack "$PACK_DIR"/pack-*.rev "$PACK_DIR"/pack-*.bitmap "$PACK_DIR"/pack-*.mtimes; do
    [ -e "$file" ] || continue
    [ -e "${file%.*}.idx" ] || ORPHANS+=("$file")
done

# --- report ------------------------------------------------------------------

echo "jsonl-archive-settle: archive=$ARCHIVE_REPO mode=$MODE"
awk '
    FILENAME == ARGV[1] { path[$1] = substr($0, index($0, " ") + 1); next }
    {
        p = ($1 in path) ? path[$1] : "(no path)"
        n[p]++
        bytes[p] += $3
    }
    END { for (p in n) { printf "group %s objects=%d bytes=%.0f\n", p, n[p], bytes[p] } }
' "$C_PATHS" "$FLAGGED_FILE" | sort > "$WORK/groups"
cat "$WORK/groups"
GROUP_COUNT=$(wc -l < "$WORK/groups" | tr -d ' ')
echo "closure_objects=$CLOSURE_OBJECTS"
echo "packs_to_retire=$PACKS_TO_RETIRE"
echo "orphan_pack_files=${#ORPHANS[@]}"
echo "largest_blob_kb=$LARGEST_BLOB_KB"
echo "window_memory=$WINDOW_MEMORY"
echo "expected_peak_rss_kb=$PEAK_RSS_KB"
echo "mem_available_kb=$MEM_AVAILABLE_KB"
echo "estimated_reclaim_kb=$ESTIMATED_RECLAIM_KB"
echo "transient_need_kb=$TRANSIENT_NEED_KB"
if [ "$MEM_AVAILABLE_KB" != unknown ] && [ "$PEAK_RSS_KB" -gt "$MEM_AVAILABLE_KB" ]; then
    echo "WARNING: the re-chaining pack-objects is expected to peak at ${PEAK_RSS_KB} KiB of memory and only ${MEM_AVAILABLE_KB} KiB is available; it may be killed (nothing is written to the archive until it finishes)"
fi

if [ "$APPLY" -ne 1 ]; then
    STATUS=ok
    emit_telemetry
    exit 0
fi

# --- preflight: packs and disk ----------------------------------------------

for name in ${RETIRE[@]+"${RETIRE[@]}"}; do
    if [ -e "$PACK_DIR/$name.keep" ]; then
        refuse "$name.pack has a .keep file, so it is not retired; remove the .keep (or settle another archive) and rerun"
    fi
done

if [ "$FLAGGED" -eq 0 ] && [ "$PACKS_TO_RETIRE" -eq 0 ] && [ "${#ORPHANS[@]}" -eq 0 ]; then
    echo "jsonl-archive-settle: nothing to settle"
    STATUS=ok
    emit_telemetry
    exit 0
fi

if [ "$FLAGGED" -gt 0 ]; then
    free_kb=$(df -Pk "$PACK_DIR" 2>/dev/null | awk 'NR == 2 { print $4 }') || free_kb=""
    case "$free_kb" in
        ''|*[!0-9]*)
            echo "jsonl-archive-settle: could not read the free disk space of $PACK_DIR; going on without the free-space check" >&2
            ;;
        *)
            if [ "$free_kb" -lt "$((TRANSIENT_NEED_KB + MIN_FREE_KB))" ]; then
                refuse "only ${free_kb} KiB free on the archive's filesystem; the rewrite needs ${TRANSIENT_NEED_KB} KiB while both the old and new packs exist, plus GC_JSONL_GC_MIN_FREE_KB=${MIN_FREE_KB} KiB; free disk space first"
            fi
            ;;
    esac
fi

# --- stage -------------------------------------------------------------------

# Pack files with no index are not part of the repository (a promote or retire
# was interrupted); holding the lock, nothing is writing one.
for file in ${ORPHANS[@]+"${ORPHANS[@]}"}; do
    echo "jsonl-archive-settle: removing $file, a pack file with no index left by an interrupted run" >&2
    rm -f "$file"
done

NEW=()
if [ "$FLAGGED" -gt 0 ]; then
    mkdir "$STAGE_DIR" || fail "could not create the staging directory $STAGE_DIR"
    STAGE_OWNED=1
    # The staging directory must be on the pack directory's filesystem, or the
    # promote would copy instead of renaming.
    stage_fs=$(df -P "$STAGE_DIR" | awk 'NR == 2 { print $1 }')
    pack_fs=$(df -P "$PACK_DIR" | awk 'NR == 2 { print $1 }')
    if [ "$stage_fs" != "$pack_fs" ]; then
        fail "the staging directory ($stage_fs) is not on the pack directory's filesystem ($pack_fs)"
    fi

    # N1: C packed afresh, each blob against its neighbours of the same path.
    awk '
        FILENAME == ARGV[1] { path[$1] = substr($0, index($0, " ") + 1); next }
        { if ($1 in path) { print $1, path[$1] } else { print $1 } }
    ' "$C_PATHS" "$C_IDS" > "$WORK/n1.input"
    "${PRIO[@]+"${PRIO[@]}"}" git -C "$ARCHIVE_REPO" \
        -c "core.bigFileThreshold=$BIG_FILE_THRESHOLD" \
        -c "pack.windowMemory=$WINDOW_MEMORY" \
        -c pack.threads=1 \
        pack-objects --no-reuse-delta --window=3 --depth=250 --delta-base-offset -q "$STAGE_DIR/pack" \
        < "$WORK/n1.input" > "$WORK/n1.name" \
        || fail "git pack-objects could not re-chain the flagged objects"
    N1_NAME="pack-$(tr -d ' \n' < "$WORK/n1.name")"
    [ -e "$STAGE_DIR/$N1_NAME.pack" ] && [ -e "$STAGE_DIR/$N1_NAME.idx" ] \
        || fail "git pack-objects did not write $N1_NAME"
    NEW+=("$N1_NAME")

    # R: the rest of P, deltas reused as they are. Its chains are intact
    # because C is closed under "is a delta on".
    for name in "${IN_P[@]}"; do
        cat "$WORK/ids.$name"
    done | sort -u | comm -23 - "$C_IDS" > "$WORK/r.ids"
    if [ -s "$WORK/r.ids" ]; then
        "${PRIO[@]+"${PRIO[@]}"}" git -C "$ARCHIVE_REPO" \
            -c pack.threads=1 \
            pack-objects --window=0 --depth=4095 --delta-base-offset -q "$STAGE_DIR/pack" \
            < "$WORK/r.ids" > "$WORK/r.name" \
            || fail "git pack-objects could not repack the untouched objects"
        R_NAME="pack-$(tr -d ' \n' < "$WORK/r.name")"
        [ -e "$STAGE_DIR/$R_NAME.pack" ] && [ -e "$STAGE_DIR/$R_NAME.idx" ] \
            || fail "git pack-objects did not write $R_NAME"
        NEW+=("$R_NAME")
    fi
    fault_point after-stage

    # --- verify --------------------------------------------------------------

    : > "$WORK/new.ids"
    for name in "${NEW[@]}"; do
        git -C "$ARCHIVE_REPO" verify-pack "$STAGE_DIR/$name.idx" > /dev/null \
            || fail "git verify-pack rejected the staged pack $name"
        pack_ids "$STAGE_DIR/$name.idx" "$WORK/ids.$name" || fail "git show-index could not read the staged pack $name"
        sort -mu "$WORK/new.ids" "$WORK/ids.$name" > "$WORK/new.ids.next"
        mv -f "$WORK/new.ids.next" "$WORK/new.ids"
    done
    # Every object of C is in N1 ...
    comm -23 "$C_IDS" "$WORK/ids.$N1_NAME" > "$WORK/missing"
    [ ! -s "$WORK/missing" ] || fail "the staged pack $N1_NAME lacks $(wc -l < "$WORK/missing" | tr -d ' ') flagged objects"
    # ... and every object of P is in the new packs or in a pack that stays.
    : > "$WORK/stay.ids"
    for name in "${PACKS[@]}"; do
        in_p=0
        for p in "${IN_P[@]}"; do
            [ "$p" = "$name" ] && in_p=1
        done
        [ "$in_p" -eq 1 ] && continue
        sort -mu "$WORK/stay.ids" "$WORK/ids.$name" > "$WORK/stay.ids.next"
        mv -f "$WORK/stay.ids.next" "$WORK/stay.ids"
    done
    for name in "${IN_P[@]}"; do
        cat "$WORK/ids.$name"
    done | sort -u > "$WORK/p.ids"
    sort -mu "$WORK/new.ids" "$WORK/stay.ids" > "$WORK/have.ids"
    comm -23 "$WORK/p.ids" "$WORK/have.ids" > "$WORK/missing"
    [ ! -s "$WORK/missing" ] || fail "$(wc -l < "$WORK/missing" | tr -d ' ') objects of the packs to retire are in neither the staged packs nor a pack that stays"
    fault_point after-verify

    # --- promote -------------------------------------------------------------

    # From here a failure leaves the old packs in place beside the new ones.
    STAGE_OWNED=0
    for name in "${NEW[@]}"; do
        for ext in rev pack idx; do
            if [ -e "$STAGE_DIR/$name.$ext" ]; then
                mv -f "$STAGE_DIR/$name.$ext" "$PACK_DIR/$name.$ext" \
                    || fail "could not move $name.$ext into $PACK_DIR; the old packs are untouched, rerun to clear the leftovers"
            fi
        done
    done
    rm -rf "$STAGE_DIR"
    git -C "$ARCHIVE_REPO" fsck --connectivity-only > /dev/null 2> "$WORK/fsck.err" \
        || fail "git fsck failed after the promote; the old packs are kept ($(head -n 1 "$WORK/fsck.err"))"
    fault_point after-promote
fi

# --- retire ------------------------------------------------------------------

# A promoted pack with the name of a pack to retire is the same pack (git names
# a pack by its content): never delete it.
KEEP_NAMES=" ${NEW[*]+"${NEW[*]}"} "
CANDIDATES=()
for name in ${RETIRE[@]+"${RETIRE[@]}"}; do
    case "$KEEP_NAMES" in
        *" $name "*) ;;
        *) CANDIDATES+=("$name") ;;
    esac
done

PRESENT=("${PACKS[@]}")
for name in ${NEW[@]+"${NEW[@]}"}; do
    case " ${PRESENT[*]} " in
        *" $name "*) ;;
        *) PRESENT+=("$name") ;;
    esac
done

# A stale multi-pack-index would name packs that are about to go.
if [ "${#CANDIDATES[@]}" -gt 0 ]; then
    rm -rf "$PACK_DIR"/multi-pack-index "$PACK_DIR"/multi-pack-index-*.bitmap "$PACK_DIR"/multi-pack-index-*.rev "$PACK_DIR"/multi-pack-index.d
fi

# Biggest first: when packs duplicate one another, the oldest and largest is
# the one to drop.
ORDERED=()
if [ "${#CANDIDATES[@]}" -gt 0 ]; then
    while IFS= read -r file; do
        ORDERED+=("$(basename "$file" .pack)")
    done < <(for name in "${CANDIDATES[@]}"; do printf '%s\n' "$PACK_DIR/$name.pack"; done | xargs ls -S)
fi

FIRST_RETIRED=1
for name in ${ORDERED[@]+"${ORDERED[@]}"}; do
    others=()
    for other in "${PRESENT[@]}"; do
        [ "$other" = "$name" ] || others+=("$WORK/ids.$other")
    done
    : > "$WORK/others.ids"
    if [ "${#others[@]}" -gt 0 ]; then
        sort -mu "${others[@]}" > "$WORK/others.ids"
    fi
    comm -23 "$WORK/ids.$name" "$WORK/others.ids" > "$WORK/missing"
    if [ -s "$WORK/missing" ]; then
        in_p=0
        for p in ${IN_P[@]+"${IN_P[@]}"}; do
            [ "$p" = "$name" ] && in_p=1
        done
        if [ "$in_p" -eq 1 ]; then
            fail "$name.pack holds $(wc -l < "$WORK/missing" | tr -d ' ') objects that no other pack has; it is kept"
        fi
        continue
    fi
    # The index first: without it git no longer sees the pack, and the rest is
    # garbage a rerun removes.
    rm -f "$PACK_DIR/$name.idx"
    if [ "$FIRST_RETIRED" -eq 1 ]; then
        FIRST_RETIRED=0
        fault_point mid-retire
    fi
    rm -f "$PACK_DIR/$name.pack" "$PACK_DIR/$name.rev" "$PACK_DIR/$name.bitmap" "$PACK_DIR/$name.mtimes"
    kept=()
    for other in "${PRESENT[@]}"; do
        [ "$other" = "$name" ] || kept+=("$other")
    done
    PRESENT=("${kept[@]+"${kept[@]}"}")
done

# --- verify after ------------------------------------------------------------

git -C "$ARCHIVE_REPO" fsck --connectivity-only > /dev/null 2> "$WORK/fsck.err" \
    || fail "git fsck failed after the retire ($(head -n 1 "$WORK/fsck.err"))"
awk '{ print $1 }' "$OBJECTS" | sort -u > "$WORK/before.ids"
git -C "$ARCHIVE_REPO" cat-file --batch-all-objects --batch-check='%(objectname)' | sort -u > "$WORK/after.ids" \
    || fail "git cat-file could not list the objects after the settle"
comm -23 "$WORK/before.ids" "$WORK/after.ids" > "$WORK/missing"
[ ! -s "$WORK/missing" ] || fail "$(wc -l < "$WORK/missing" | tr -d ' ') objects are missing after the settle"

BYTES_AFTER=$(archive_pack_bytes) || fail "git count-objects failed after the settle"
STATUS=ok
emit_telemetry
