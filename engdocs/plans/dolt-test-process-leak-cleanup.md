# Plan: dolt-test-process-leak — stop the test-suite from leaking `dolt sql-server` children

> **Status:** decomposing — 2026-05-12
> **Source bead:** `ga-n29qet` (P1, BUG) — *[CRITICAL] gc/beads tests leak
> dolt sql-server processes — 900+ accumulated, twice crashed city dolt +
> caused OOM.*
> **Decomposed into:** 2 builder beads (see Children below)
> **HQ tracking:** `gm-uw3orn` (close once both children land).

## Context

Twice in the last 24 hours the gas-city dolt sql-server crashed because the
test suite leaked ~900 `dolt sql-server` child processes over a single
working day. Each leaked process holds a small chunk of memory and a few
file descriptors; at 900+ the system load avg hit 21 and available memory
collapsed to single-digit GB with swap pegged at 100%. The crashed dolt
took ~13 minutes of `gc dolt-state recover-managed` retries to come back
because the recover-managed callers themselves dogpiled (~14 concurrent
attempts holding the same lock).

The leaked processes are children of test runs under `/tmp/`. By config-
file path:

| Test family root config path                         | Process count |
|------------------------------------------------------|---------------|
| `/tmp/TestCityRuntime*/001/.gc/runtime/packs/dolt/dolt-config.yaml` | **831** |
| `/tmp/gc-reload-invalid-*/.gc/runtime/packs/dolt/dolt-config.yaml`  | ~30    |
| `/tmp/gc-stop-*/.gc/runtime/packs/dolt/dolt-config.yaml`            | small  |
| `/tmp/TestDoBeadsHealth*/`                                           | small  |
| `/tmp/TestControllerState*/`                                          | small  |
| `/tmp/gc-margin-*/`                                                  | small  |
| `/tmp/gc-force-stop-*/`                                              | small  |
| bare `dolt sql-server --host=0.0.0.0 --port=3306`                    | small  |

The single overwhelming source is **`TestCityRuntime*`** at 831 of 900+
processes (≈92%). Fixing that one test family yields the most resource
relief by far.

Separately, the existing `mol-dog-stale-db` order (fires every 4h) only
enumerates orphan dolt *databases* via `gc dolt cleanup --probe`; it does
not enumerate stray `dolt sql-server` *processes*. So even when an
operator-side cleanup is wanted, the cleanup tooling currently can't see
or stop the leaks — they accumulate under `/tmp/` until the city tips
over.

### Why this is now P1

- Recurred overnight 2026-05-12: 900 stray processes killed at 07:42 PDT
  after the city was within hours of OOM.
- "Wait until tomorrow" was the wrong call; leak rate is fast enough to
  take the city down repeatedly.

## Strategy

Two independent fixes, both `ready-to-build`. They can ship in either
order, but Fix #1 cures the root cause; Fix #2 is defense-in-depth.

### Fix #1 — Test cleanup audit (root cause)

Audit every `gascity` test that can spawn a `dolt sql-server` child and
ensure each one registers a `t.Cleanup` (or equivalent teardown) that
kills the spawned process when the test exits. The dominant entry point
is `startManagedDoltProcess` in
`cmd/gc/dolt_start_managed.go:22-71`, which invokes
`exec.Command("dolt", "sql-server", "--config", layout.ConfigFile)`.
Any test that constructs a `CityRuntime` or otherwise triggers
`startManagedDoltProcess` is a candidate. The leak fingerprints under
`/tmp/` are the search heuristic.

Highest yield is the `TestCityRuntime*` family in
`cmd/gc/city_runtime_test.go` (>50 subtests; the file already uses
`t.Cleanup` in places but apparently not for the dolt process); fix that
helper and the lion's share of the leak is gone.

### Fix #2 — Defense-in-depth janitor

Extend the `mol-dog-stale-db` formula (or add a sibling janitor order)
so it also enumerates and kills stray `dolt sql-server` *processes*
rooted under known test-only path prefixes:

- `/tmp/Test*` (Go `t.TempDir()`)
- `/tmp/gc-reload-invalid-*`
- `/tmp/gc-stop-*`
- `/tmp/gc-margin-*`
- `/tmp/gc-force-stop-*`
- Other `/tmp/gc-*-<random>` prefixes used by the suite

A `dolt sql-server --config <path>` is "test-only" iff the config path is
under `/tmp/` AND matches one of the above stems AND the parent test
process is no longer alive. The current city's managed dolt (under
`<cityPath>/.gc/runtime/packs/dolt/dolt-config.yaml`) MUST NOT match
this rule. The janitor must fail-safe: if it can't classify a process,
leave it alone.

## Out of scope for this slice

- **Upstream Dolt issue.** Dolt sql-server failing silently on startup
  scan when orphan DBs are present (no error logged even at
  `log_level=debug`) is a real Dolt-side bug. The fix to file an upstream
  issue with a minimal repro is tracked as a follow-up (see "Follow-ups"
  below) — it is NOT part of either builder bead in this slice. We can
  ship #1 and #2 without an upstream fix; the upstream fix only improves
  diagnosability for the next time something else trips startup scan.
- **One-time cleanup of any currently-running stray processes.** Operator
  task, not a code change. The bug body has the recovery procedure and it
  has already been run once today.

## Children

| ID            | Title                                                                                          | Routing label    | Routes to         | Depends on |
|---------------|------------------------------------------------------------------------------------------------|------------------|-------------------|------------|
| `ga-tpzo5e`   | test(gascity): register t.Cleanup teardown for every test that spawns `dolt sql-server` (ga-n29qet Fix #1) | `ready-to-build` | `gascity/builder` | (none)     |
| `ga-1eg8wl`   | feat(orders): extend mol-dog-stale-db to kill stray `dolt sql-server` processes under /tmp test paths (ga-n29qet Fix #2) | `ready-to-build` | `gascity/builder` | (none — parallel-safe with bead-A) |

(IDs filled in by `bd create`; see the matching commit for the final
bead-A and bead-B values.)

## Acceptance for the parent (ga-n29qet)

Met when **both** child builder beads land and all of the following hold.

### Fix #1 acceptance

- [ ] Every `*_test.go` under `cmd/gc/` and `internal/` that spawns
      `dolt sql-server` (directly via `exec.Command` or transitively via
      `startManagedDoltProcess`, `newCityRuntime`, or similar) registers
      a `t.Cleanup` that kills the process. Search heuristic for the
      builder: `git grep -l 'startManagedDoltProcess\|newCityRuntime\|dolt sql-server' -- '*_test.go'`.
- [ ] A new test (e.g. `TestNoLeakedDoltSqlServersAfterCityRuntimeShutdown`)
      asserts no `dolt sql-server` child of the current process is alive
      after a representative `TestCityRuntime*` subtest finishes.
      Mechanism: `ps -eo pid,ppid,comm` filtered to children of `os.Getpid()`.
- [ ] `go test ./cmd/gc/... -run TestCityRuntime -count=5` produces
      zero leaked `dolt sql-server` processes (manual verification by
      builder before commit; record in the builder bead notes).
- [ ] `go test ./...` still passes.

### Fix #2 acceptance

- [ ] `mol-dog-stale-db` (or a sibling janitor order — builder's call) now
      enumerates `dolt sql-server` processes by `--config <path>` and
      kills those rooted under any of: `/tmp/Test*`, `/tmp/gc-reload-invalid-*`,
      `/tmp/gc-stop-*`, `/tmp/gc-margin-*`, `/tmp/gc-force-stop-*`,
      `/tmp/gc-*-` (general `t.TempDir()` style).
- [ ] The city's own managed dolt (rooted at
      `<cityPath>/.gc/runtime/packs/dolt/dolt-config.yaml`) is **never**
      classified as stale. Add a positive unit test that constructs a
      mock process list containing the live city dolt and asserts the
      janitor returns zero kills.
- [ ] Add a positive unit test that constructs a mock process list with
      a `/tmp/TestCityRuntime*/001/.gc/runtime/packs/dolt/dolt-config.yaml`
      entry whose parent PID is unreachable, and asserts the janitor
      returns exactly one kill.
- [ ] Janitor logs each kill with the killed PID, the matched config
      path, and the reason (parent dead / path matches test prefix).
      Logs go to the existing `mol-dog-*` log stream; the order tracking
      bead notes record the count (depends on `ga-2uizkj` capturing
      stdout, but the janitor MUST write a deterministic summary line
      regardless so it's recoverable from raw logs).

## Follow-ups (NOT in this slice)

1. **Upstream Dolt issue** — file with a minimal repro: orphan DBs under
   `.beads/dolt/` cause `dolt sql-server` to exit silently with `EOF` on
   stdout and no log entry at any level. Owner: builder once Fix #1 +
   Fix #2 land; file a new bead `ga-*` with `--label needs-architecture`
   so the architect can capture the upstream contact protocol.
2. **Quarantine cleanup.** 278 quarantined dirs at `/tmp/dolt-quarantine`
   are still there from the 2026-05-11 recovery. Operator task; not a
   code change. Document in mayor handoff if not already.

## Notes for the builder

- **Read `ga-n29qet` in full before any edit.** The body has the exact
  process-table fingerprints used to triage the incident; you will need
  them to write the janitor's matching rules.
- The leak is reproducible by running `go test ./cmd/gc/... -run TestCityRuntime`
  and then `pgrep -f 'dolt sql-server.*--config /tmp/Test'` immediately
  after. If you don't get a non-zero count, the test family may have
  already been fixed in-flight — double-check before assuming the bug is
  gone (it was filed and recurred twice in 24h).
- **`t.Cleanup` order matters.** Register the dolt-kill cleanup BEFORE
  any `t.TempDir()` cleanup; otherwise the temp dir holding
  `dolt-config.yaml` may be GC'd before the dolt process notices, and
  the process will exit dirty (or hang). Go's `t.Cleanup` runs in LIFO
  order — register dolt-kill first to ensure it runs last.
- **Idempotency for Fix #2.** Two concurrent `mol-dog-stale-db` runs
  must not double-kill or panic on a vanished PID. Use `os.FindProcess`
  + `signal.Signal(0)` to test liveness before sending SIGKILL.
- **Never run bare `pkill -9 dolt`**. Always match by `--config <path>`
  prefix. The user's personal dev shell may have a legitimate `dolt`
  process they care about.
- **`tmpx` invariant** (CLAUDE.md repo rule): never run `tmux
  kill-server` as cleanup. This is a separate process class but the
  principle applies: target the specific process, not the family.

## Verification (for the PM, post-merge)

1. `pgrep -fc 'dolt sql-server'` after a full `go test ./...` run returns
   the same count as before the run (i.e., zero leaks).
2. `mol-dog-stale-db` history (via `gc order history mol-dog-stale-db`)
   shows a non-empty run summary including any killed-stray count
   (depends on ga-2uizkj landing for the full text; the kill count must
   be visible in raw `mol-dog-*` logs regardless).
3. Close `gm-uw3orn` once both children close.
