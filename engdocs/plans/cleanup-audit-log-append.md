# Plan: cleanup-audit.log append + tests (ga-nw4z6 slice 4/5)

> **Status:** decomposing — 2026-05-11
> **Parent architecture:** `ga-nw4z6` (closed) — P0 DATA LOSS:
> `gc dolt cleanup` classifier marks the live `beads` DB as orphan.
> **Designer spec:** `ga-endmgy` (~272-line design body) pins:
> `cleanupAuditLogRelPath = "packs/dolt/cleanup-audit.log"`,
> `appendCleanupAuditLine` signature, 5-tab-separated line format,
> file mode 0644 on create, `O_APPEND|O_CREATE|O_WRONLY`, no
> escaping, sort-stable serialization, defer-at-top call site, full
> 6-subtest `TestCleanupAuditLineAppended` + 1-subtest negative test.
> Architect builder estimate: ~50 LOC source + ~40 LOC tests, 1 PR.
> **Designer handoff mail:** `gm-s2961n` (2026-05-11).
> **Branch:** `local/integration-2026-04-30` (slice 1 confirmed Go
> runner on this branch — no cherry-pick).
> **Sibling slices:** `ga-0txff0` (1, CLOSED), `ga-9h05hk` (2, in
> flight), `ga-set4vz` (3, in flight), `ga-qg23tv` (5, design in
> flight).
> **Decomposed into:** 1 builder bead — `ga-w6assk`.

## Context

Slice 4 ships the only durable post-condition of a `gc dolt-cleanup`
invocation other than the JSON report on stdout: an append-only audit
log under `<runtimeDir>/packs/dolt/cleanup-audit.log`. The stdout
report disappears when the operator closes the terminal; the audit log
survives. Operators can `awk` / `cut` / `grep` it post-incident.

Every invocation writes **one** line (force AND dry-run; success AND
failure). The line is tab-separated and machine-parseable. The design
deliberately accepts a small concurrency risk past ~4 KB (two
overlapping invocations can interleave) because two cleanups racing is
already a class-A operational bug — `live-session` exists to prevent
exactly that — and a lock file would violate the project's "no status
files" rule.

## Plan

One builder bead. The whole slice is a single PR shape: one new helper,
one constant, two test functions, and a runner edit that converts every
`return N` to `auditExitCode = N; return N` plus a `defer` at the head.

| Builder bead    | PR shape | Files                                                  |
|-----------------|----------|--------------------------------------------------------|
| **Audit append**| PR       | `cmd/gc/cmd_dolt_cleanup.go`, `cmd/gc/cmd_dolt_cleanup_test.go` |

### `ga-w6assk` — audit log append + tests (P2, `ready-to-build`)

Implements NFR-05 from `ga-nw4z6`. Routed to `gascity/builder` via
`gc.routed_to` metadata; `gc.design_parent=ga-endmgy` records the
back-link.

**Acceptance criteria (verbatim from `ga-endmgy` §"Acceptance criteria"):**

- `cleanupAuditLogRelPath` constant exists and equals
  `"packs/dolt/cleanup-audit.log"` (unexported, in `cmd/gc`, declared
  immediately above `appendCleanupAuditLine`).
- `appendCleanupAuditLine(fs, runtimeDir, now, schema, exitCode,
  report)` has the design §2 signature (no `os.Getenv` calls inside;
  `runtimeDir` threaded in as argument).
- `runDoltCleanup` calls it via `defer` exactly once per invocation;
  every `return N` is converted to `auditExitCode = N; return N`.
- Line format matches design §3 exactly:
  `<rfc3339-utc>\t<schema>\t<exit>\t<dropped:csv>\t<skipped:csv>\n`
  (UTF-8, no BOM, no trailing whitespace, no escaping).
- Field 1 is `time.RFC3339` on `now.UTC()` (second precision, ends in
  `Z`, no fractional seconds).
- Field 4 is sorted `Dropped.Names` (empty if none).
- Field 5 is sorted `reason=name` tokens from `Dropped.Skipped`
  (sort by reason then name; empty if none).
- File mode 0644 on create (tested via `os.Chmod` + re-open round-trip
  per design §7 subtest 4).
- Open flags exactly `os.O_APPEND | os.O_CREATE | os.O_WRONLY`. One
  `Write([]byte(line))` per call (line built in memory first).
- Write failure logs to stderr but does NOT change the runner's exit
  code.
- `TestCleanupAuditLineAppended` exists with the 6 subtests in design
  §7 (`happy_path_dropped`, `happy_path_skipped`, `exit_nonzero`,
  `mode_0644`, `appends_not_truncates`, `sort_stable`). Uses
  `fsys.NewMemFS()`.
- `TestCleanupAuditLineAppendedRefusesMissingDir` exists with the
  single subtest in design §8 (asserts a wrapped `fs.ErrNotExist`).
- Existing tests for `runDoltCleanup` still pass (slice 1's tests).
- `go test ./...` passes; `go vet ./...` clean.

**HARD RULES carried from design:**

- No log rotation, archival, or pruning — operator owns the file.
- No JSON output, no `slog` integration.
- No `cleanupReportSchema` string literal anywhere else; pass the
  existing constant through.
- No defensive escaping; the constraint `validDoltDatabaseIdentifier`
  makes tabs/commas/equals/newlines unreachable. If a future
  contributor widens the predicate, this design must be re-pinned
  (drop a `// TODO(audit-format)` comment above the constant per
  design §3).
- No file-existence preflight inside `appendCleanupAuditLine`. The
  cleanup runner already creates `<runtimeDir>/packs/dolt/` earlier in
  its lifecycle (slice 1 work). A missing dir at append time is a
  programming error; surface it via `fs.ErrNotExist`.
- No lock file or `flock`. Concurrent-invocation interleaving past
  ~4 KB is accepted as a separate operational bug.

## Sequence

No `bd dep add` edge between `ga-w6assk` and the open sibling
builder beads (`ga-9h05hk`, `ga-set4vz`). The audit append touches
only `cmd_dolt_cleanup.go` and the runner's exit-code plumbing —
no shared file with the live-session probe slice or the forwarder
slice. Three independent PRs can land in any order; merge-conflict
risk is contained to the small set of `return N` sites in
`runDoltCleanup`, all of which the designs explicitly call out
("Both touch `runDoltCleanup`" — slice 2 returns early on probe
failure; slice 4 wraps every return). Builder reconciles in the
last-to-land PR.

## Out of scope (separate beads)

- Slice 5 regression `TestDoltCleanupRefusesLiveBeadsDatabase`
  (`ga-qg23tv` — designer is finishing).
- Log rotation / archival.
- JSON-shaped audit records (file a follow-on if needed).
- Structured-logging integration via `slog`.
- Concurrency serialization (no flock; accepted trade-off).
- Operator playbook for the accumulated 272-DB drain
  (`ga-0ipc.4.2`).

## Verification (after PR lands)

```bash
# Smoke check — every invocation writes one line.
TS=$(date -u +%Y%m%dT%H%M%SZ)
gc dolt-cleanup --probe --json > /dev/null
tail -1 "$GC_CITY_RUNTIME_DIR/packs/dolt/cleanup-audit.log"
# Expect one tab-separated line. Field 1 ≈ now. Field 2 = gc.dolt.cleanup.v1.
# Field 3 = 0. Fields 4 and 5 = empty (probe didn't drop anything).

gc dolt-cleanup --probe --json > /dev/null
wc -l "$GC_CITY_RUNTIME_DIR/packs/dolt/cleanup-audit.log"
# Expect line count incremented by exactly 1.

stat -c '%a' "$GC_CITY_RUNTIME_DIR/packs/dolt/cleanup-audit.log"
# Expect 644 on the create path.
```

## Builder bead

- **`ga-w6assk`** — audit log append + tests (P2, `ready-to-build`,
  `source:actual-pm`, `backend:dolt`). Routed to `gascity/builder`.
