#!/usr/bin/env bash
# check-census-owner-liveness.sh
#
# Order wrapper for the "census-owner-liveness" gc doctor check (ga-kr3glv.1,
# decision doc ga-kr3glv secs 2, 13). The resource-census ledger
# (test/test-resources.toml) anchors every row on an owner_bead, but nothing
# else in the pipeline notices when that bead stops resolving -- it happened
# once already (ga-c1slhq, same-day, <24h).
#
# Runs `gc doctor --json`, looks for dangling owner_bead findings from the
# census-owner-liveness check, and files one alert bead per distinct
# dangling owner_bead -- deduped against existing open alerts so a
# persistent condition doesn't spam a fresh bead on every cron tick.
#
# Detection only: this script never repairs the ledger or the bead store.
# Intended trigger: a cron order running every few hours (see the close-out
# notes on ga-kr3glv.1 for the order.toml to deploy).
#
# Security note: bead IDs and row text read from the ledger/doctor output
# are untrusted data, not trusted shell fragments. Every bd/jq invocation
# below passes that data as a quoted argv element or through `jq -n --arg`
# / a heredoc -- never through `sh -c`/`eval` string interpolation.

set -euo pipefail

routed_to="${CENSUS_OWNER_LIVENESS_ROUTED_TO:-gascity/architect}"
alert_label="source:census-owner-liveness-patrol"

# gc doctor exits nonzero when unrelated BLOCKING checks fail; the
# census-owner-liveness check is advisory-only, so capture the JSON
# regardless of exit code and validate it parses before trusting it.
# A bare `doctor_json=$(...)` under `set -e` would abort the patrol here.
set +e
doctor_json=$(gc doctor --json)
set -e

if ! printf '%s' "$doctor_json" | jq -e . >/dev/null 2>&1; then
    echo "check-census-owner-liveness: gc doctor --json did not return valid JSON" >&2
    exit 1
fi

check_status=$(printf '%s' "$doctor_json" | jq -r '
  .results[] | select(.name == "census-owner-liveness") | .status // empty
')

if [ -z "$check_status" ]; then
    echo "check-census-owner-liveness: census-owner-liveness check not present in gc doctor output" >&2
    exit 1
fi

if [ "$check_status" != "warning" ]; then
    echo "check-census-owner-liveness: status=$check_status, nothing to do"
    exit 0
fi

dangling_lines=$(printf '%s' "$doctor_json" | jq -r '
  .results[]
  | select(.name == "census-owner-liveness")
  | .details[]?
  | select(test("dangling owner_bead="))
')

if [ -z "$dangling_lines" ]; then
    echo "check-census-owner-liveness: status=warning but no dangling owner_bead findings (skip-only warning); nothing to alert on"
    exit 0
fi

owner_beads=$(printf '%s\n' "$dangling_lines" | sed -n 's/.*dangling owner_bead=\([^ ]*\).*/\1/p' | sort -u)

created=0
while IFS= read -r owner_bead; do
    [ -z "$owner_bead" ] && continue

    existing_count=$(bd list --json --label "$alert_label" --status open \
        --metadata-field "census.owner_bead=${owner_bead}" | jq 'length')

    if [ "${existing_count:-0}" -gt 0 ]; then
        echo "check-census-owner-liveness: owner_bead=$owner_bead already has an open alert (${existing_count}), skipping"
        continue
    fi

    matching_lines=$(printf '%s\n' "$dangling_lines" | grep -F "dangling owner_bead=${owner_bead} ")

    metadata=$(jq -n --arg routed_to "$routed_to" --arg owner_bead "$owner_bead" \
        '{"gc.routed_to": $routed_to, "census.owner_bead": $owner_bead}')

    description=$(cat <<EOF
gc doctor census-owner-liveness detected a resource-census ledger row
whose owner_bead no longer resolves in the bead store.

owner_bead: ${owner_bead}

Affected rows:
${matching_lines}

Detection only -- no auto-repair. Re-point the ledger row's owner_bead
through council review (see TESTING.md).
EOF
)

    new_id=$(bd create \
        --title "resource-census ledger references dangling owner_bead ${owner_bead}" \
        --type task \
        --label "$alert_label" \
        --metadata "$metadata" \
        --description "$description" \
        --silent)

    created=$((created + 1))
    echo "check-census-owner-liveness: filed ${new_id} for dangling owner_bead=${owner_bead}"
done <<< "$owner_beads"

echo "check-census-owner-liveness: done (${created} alert(s) created)"
exit 0
