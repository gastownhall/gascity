# Remote sync: one server-side fetch per database, and the kill on expiry

`gc dolt sync` and `gc dolt pull` run their remote operations INSIDE the managed
sql-server (`CALL DOLT_FETCH` / `CALL DOLT_PULL`), so a client that dies when its
wall-clock bound expires leaves the server-side procedure running. Before this
guard, a 15-minute patrol re-issuing `gc dolt sync` stacked one more fetch per
run on top of the abandoned ones (boomtown, 2026-09-11: 169 of 178 sql-server
connections in `CALL DOLT_FETCH`, the oldest two days, dolt at 180% CPU, that
store's sync dead for weeks). Both scripts now hold to one rule: before
issuing a fetch or pull they read `information_schema.processlist` and, when a
`DOLT_FETCH` / `DOLT_PULL` session is already in flight anywhere on the server
— the processlist `DB` column is the connection's `--use-db`, not the
statement's target, so no per-database filter is applied to it — they print one
line naming the oldest session's age and id and skip — sync skips without
pushing. A processlist read that fails, answers with anything that is not a
processlist, or carries a row that is not `digits,digits,…`, also skips (fail
closed). "In flight" means any server-side statement that names `DOLT_FETCH`
or `DOLT_PULL` as an identifier, however the call was spelled; a statement
that merely mentions the name in a literal or comment costs one skipped run
while it executes, which is the safe side. The processlist query itself is
constant text — no database name is interpolated into it — so a store named,
say, `dolt_fetch` cannot make the query match its own text. One limit, by design: the processlist shows
the statement text a session submitted, so a fetch run *indirectly* — through
a text-protocol prepared statement (`EXECUTE s`), a user stored procedure that
wraps the call, or an event — is invisible to the pre-check and to the health
count, and holds no pack lock. The pack's own runs never need that visibility
(they are serialized by the server lock below); an operator's indirect fetch is
theirs to `KILL` by hand.
The statement they do issue takes the server's own session lock for the
database (`GET_LOCK('gc_remote_op:<db>', 0)`) in the same batch as the CALL:
two runners that both read "nothing in flight" cannot both fetch, because the
server stops the second batch before its CALL, and a session whose client died
keeps the lock until it finishes or is killed. The gate also takes a lock named
for this run (`gc_remote_op_run:<db>:<pid>-<epoch>`), and the CALL's own first
argument re-checks that the session still holds it — Dolt evaluates expressions
in CALL arguments — so a client that reconnected between the gate and the CALL
fails before the procedure runs instead of fetching on a lockless session. The
statement also prints its own connection id first and runs with `--use-db`, so
when the client bound expires (exit 124, or 137 when GNU timeout had to escalate
to SIGKILL) the script `KILL`s exactly that server-side session — in one batch
that first checks the id still holds this run's lock, so a server that restarted
and handed the number to someone else is never asked to kill them — and proves
it gone with a second processlist read (a session still listed is reported as
NOT killed; a client that never learned its id kills nothing and names the
sessions for the operator). Database names are locked case-insensitively.
`gc dolt health` adds one `WARN` line, and a `fetch_sessions` block in its
JSON report, when more than `GC_DOLT_HEALTH_MAX_FETCH_SESSIONS` (default 2)
such sessions are in flight server-wide; leftovers from runs older than this
guard need one manual `KILL <Id>` each. Bounds: `GC_DOLT_SYNC_FETCH_TIMEOUT_SECS`
(sync's pre-push fetch, default 60), `GC_DOLT_PULL_TIMEOUT_SECS` (pull, default
120); each must be a positive integer, an all-zero value is rejected. Nothing
here changes the patrol order, the fast-forward classification, the push path,
or the CLI fallback (which has no server session to guard).
