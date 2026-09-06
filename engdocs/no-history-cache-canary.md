# Cached native-Dolt no-history canary

## Purpose and limits

This manifest is the release receipt for `b2763cc2e` (`fix(beads): preserve
storage policy through cache`).  It proves the production-shaped cached
native-Dolt create path preserves the selected `no_history` class.  It does
not erase existing Dolt history, remediate rows, resume the fleet, or authorize
publisher, schedule, credential, or producer changes.

Run this only while admission remains frozen, with one operator and one
baseline-bounded canary at a time.  If any precondition or observation is
indeterminate, stop admission and preserve the evidence.  Do not retry by
creating a competing canary.

## Preconditions

- The candidate binary's checksum and `gc version` identify `b2763cc2e`.
- The previously checksummed `20c257b4fa5bb795ddbd448a5127a7ff0a158007`
  binary is available for rollback.
- Runtime admission is frozen except for this canary.  Publishers, cron
  producers, Mac workers, credentials, and schedules are unchanged.
- Record `BASELINE_UTC` immediately before the first create, in UTC with
  nanosecond precision when the backing store supports it.
- Create a unique `CANARY_TAG` and record every created bead/run ID before
  allowing it to close.

```sh
BASELINE_UTC="$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)"
CANARY_TAG="no-history-cache-$(date -u +%Y%m%dT%H%M%SZ)"
printf '%s %s %s\n' "$BASELINE_UTC" "$CANARY_TAG" "b2763cc2e" \
  | tee no-history-cache-canary.receipt
```

## Canary procedure

1. Start exactly one cached native-Dolt session using the normal controlled
   admission path.  Capture its session bead ID as `SESSION_ID`.
2. Create exactly one order through the same controlled path.  Capture its
   order-tracking bead ID as `ORDER_ID`.
3. Immediately query the backing store using the production read path for
   those two IDs.  Both rows must show `no_history=1` and `ephemeral=0`.
4. Close both controlled objects.  Query the same IDs again.  The two flags
   must remain unchanged after close.
5. Run this null-safe native-Dolt query for the recorded IDs and
   `created_at >= BASELINE_UTC`.  It must return zero rows.  Do not replace the
   null-safe comparisons with `<>`: SQL treats `NULL <> 1` as unknown, which
   would hide the legacy state this canary is designed to detect.

   ```sql
   SELECT id, no_history, ephemeral
   FROM issues
   WHERE created_at >= :baseline_utc
     AND id IN (:session_id, :order_id)
     AND (NOT (no_history <=> 1) OR NOT (ephemeral <=> 0));
   ```

   Do not use an unbounded query: the known older durable session rows are
   outside this proof.
6. Separately query runtime rows updated after `BASELINE_UTC` but created
   before it.  Name every returned ID in the receipt; this is monitoring for
   legacy state, not a failure of the create canary.  Any returned row with
   `no_history <> 1` stops rollout and requires an explicit allowlist or a
   guarded remediation decision outside this manifest.  Use
   `NOT (no_history <=> 1)` for this check as well so `NULL` is a failure.
7. Reconcile the HQ/Dolt commit delta during the window to the named runtime
   rows.  An unexplained commit is a failed canary.

Record `BASELINE_UTC`, `CANARY_TAG`, `SESSION_ID`, `ORDER_ID`, immediate and
post-close flag observations, the bounded-query result, the legacy-update
list, and the commit reconciliation in the receipt.

## Decision rules

- **Pass:** both IDs remain `no_history=1, ephemeral=0`, the bounded bad-row
  count is zero, and every window commit is reconciled.
- **Fail:** any canary row has the wrong flags, an observation cannot be read
  back, an unexpected runtime row is found, or commit reconciliation fails.
  Re-freeze admission and restore the known previous binary.  Preserve the
  receipt and row IDs; do not rewrite data or delete history.
- **Out of scope:** the two already closed incident rows (`sc-pae5u` and
  `sc-us7aa`) may only be remediated later through a separately authorized,
  guarded `bd update --no-history --if-status closed`.  That action cannot
  remove their prior Dolt history and is not part of this canary.

## Evidence gap

This source lane verifies the Go cache adapter and its policy composition, but
does not verify SQL-server transport behavior.  The operator must record the
actual native-Dolt query and result in the live receipt.  Until that readback
is completed, SQL-server transport remains explicitly unverified.
