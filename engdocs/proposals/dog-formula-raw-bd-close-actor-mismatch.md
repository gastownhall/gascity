---
title: "Dog-Formula Raw bd-Close Actor/Assignee Guard Mismatch"
---

Source bead: `ga-j3lkma`
Generated: 2026-09-11 by `gascity/planner`
Type: internal reliability/infra PRD (no end-user UI surface)

## Problem Statement

`gc hook --claim` writes a claimed bead's `assignee` as the session's
**session ID** (e.g. `gm-wisp-mmj4dz`). `gc bd close` resolves the closing
actor correctly against that convention and closes without complaint. A raw
`bd close` subprocess call, by contrast, resolves its actor from the session
**name** (`gc.session_name`, e.g. `bd__dog-1-pool`) — a different string
denoting the very same session — and its assignee/actor guard rejects the
mismatch:

```
cannot close gm-nerjd0: assignee is "gm-wisp-mmj4dz", actor is "bd__dog-1-pool"; reclaim or use --force to override
```

`examples/bd/dolt/formulas/mol-dog-stale-db.toml` (phase `vapor`; live copy
symlinked as `.beads/formulas/mol-dog-stale-db.formula.toml`) ends its
`cleanup` step with exactly this raw call, confirmed at the file's last three
lines:

```bash
bd close "$WORK_BEAD" --reason "Stale DB scan complete (orphans=${ORPHAN_TOTAL}, applied=${APPLIED}, escalated=${ESCALATED})"
drain_ack_once # drain-ack before normal exit
exit
```

Under `set -euo pipefail` the non-zero exit propagates: the script dies, the
`EXIT` trap still fires `gc runtime drain-ack`, and the shell exits 1 — **on
every run that reaches this line**, i.e. every no-op, soft-escalation, and
apply outcome, regardless of whether the underlying scan and cleanup decision
were entirely correct. Worked example, 2026-09-11, gc-management, work bead
`gm-nerjd0` (session `bd__dog-1-pool`, wisp assignee `gm-wisp-mmj4dz`): the
scan was clean (`dropped.count=0`, `force_blockers=[]`,
`summary.errors_total=0`), the JSON report attached correctly via
`bd update --append-notes` (line 108 — no assignee/actor guard on that verb),
and only the terminal `bd close` failed. `gc bd close gm-nerjd0 --reason
"..."` against the same bead succeeds immediately, confirming the diagnosis.

This is the **only** call site of its kind in the repository. A repo-wide
search of every `*.formula.toml` under `examples/**/formulas` and
`internal/bootstrap/packs/**/formulas` (2026-09-11, by this PRD's author)
found four other formulas closing or heartbeating their own work bead — all
four already use the correct `gc bd` form: `mol-polecat-commit.toml:162`,
`mol-polecat-report.toml:171`, and `mol-prompt-synth.toml:147` (`gc bd close
"$WORK_BEAD_ID"`), and `mol-do-work.toml:65` (`gc bd heartbeat
"$WORK_BEAD_ID"`). The fix pattern is therefore not new — it is the
already-established convention everywhere else in the codebase — and this is
a single-file, single-line regression rather than a class of bugs needing a
multi-file sweep.

**Impact:**
- Work beads accumulate open/`in_progress` after every successful clean run
  of this formula, indistinguishable from the outside from a genuine cleanup
  failure — training operators to ignore this formula's exit code.
- Lease expiry on the un-closed bead can cause the same maintenance order to
  re-run, redoing work that already completed correctly.
- The dog's own report/telemetry (`bd update --append-notes`, `gc event emit
  mol-dog-stale-db.*`) is unaffected and correct — only the terminal close
  fails, which makes the failure look like a cleanup defect rather than the
  identity-plumbing issue it actually is.

**A note on this document's location.** Per established precedent
(`engdocs/proposals/executor-identity-stamp-lifecycle.md`), this rig's
`docs/` tree is Mintlify-published and gated by `TestEveryDocsPageIsPublished`
(`test/docsync/docsync_test.go`, added 2026-08-24, commit `e736d74d0a`), which
rejects internal engineering PRDs placed there. This PRD is filed under
`engdocs/proposals/` accordingly. Separately: this rig has no
`docs/PROJECT_MANIFEST.md` (unlike a typical downstream Actual-managed
product rig) — `AGENTS.md` substitutes as the source of technical
constraints and conventions below.

## Goals

1. Every `mol-dog-stale-db` run that reaches its success tail (no-op,
   soft-escalation, or apply outcome) successfully closes its own work bead —
   no spurious exit-1 on a run that completed correctly.
2. The fix is the minimal, already-established `bd close` → `gc bd close`
   substitution — no new mechanism invented, no change to the probe/decide/
   escalate/apply decision logic itself.
3. The intentional hard-abort paths (`fail_open_after_drain`, used when the
   dry-run scan itself fails or returns invalid JSON) continue to leave the
   work bead open exactly as today — that is deliberate, commented-as-such
   design for a genuinely unresolved scan, not part of this bug.
4. Recurrence of this exact class of regression (a formula script using a
   raw `bd` mutation verb instead of the `gc bd` wrapper) is caught before
   merge, not discovered again in production months later.

## Non-Goals

- Changing the probe/decide/escalate/apply decision logic, the
  `gc.dolt.cleanup.v1` schema, or the `max_orphans_for_sql`/`warn_threshold`
  thresholds — out of scope; this PRD is a close-mechanism fix only.
- Changing `fail_open_after_drain`'s intentional exit-without-close behavior
  for genuine scan failures (dry-run failure, invalid JSON, and any other
  hard-abort call sites using that helper) — that design is correct and
  unrelated to this bug.
- Reconciling `gc hook --claim`'s assignee-as-session-ID convention with raw
  `bd`'s actor-as-session-name resolution at the `bd`/Go-storage layer
  itself. Grepping `internal/workrecord/gate.go`, `cmd/gc/work_record_gate.go`,
  and `cmd/gc/cmd_bd_by_id.go` during authoring found an existing "closed
  front door" concept for `gc bd close`/`gc bd reopen`, but it addresses a
  different problem — routing by-ID reads/writes to the correct storage
  binding (class store vs. work store) on a split/relocated city — not actor
  identity reconciliation. It is not relevant to this fix and is not touched.
- Auditing formula directories outside `examples/**/formulas` and
  `internal/bootstrap/packs/**/formulas` (e.g. any pack a downstream city
  supplies) — out of this PRD's search scope; FR-4's guard, once merged,
  covers new call sites going forward regardless of location within this
  repo.

## User Stories

1. **As the dog formula itself**, I want my own successful, clean run to
   close my work bead, so a completed maintenance pass doesn't masquerade as
   a failure.
2. **As an operator or on-call role**, I want this formula's exit code to be
   a trustworthy signal — non-zero means something actually went wrong with
   the scan or cleanup, not that the bookkeeping at the very end tripped over
   an identity mismatch.
3. **As a crash-recovery/stale-bead audit** (Tier 1 of any role's work-
   discovery query), I want `mol-dog-stale-db` work beads to actually reach
   `closed` on success, so they don't accumulate as false-positive
   "abandoned in-progress work" for another session to investigate.
4. **As a future contributor** writing or editing a formula step script, I
   want a guard that fails fast if I write a raw `bd close`/`bd update
   --status closed`/`bd heartbeat`/`bd unclaim` instead of the `gc bd`
   equivalent, so I don't reintroduce this exact bug in a different formula.

## Functional Requirements

| ID | Requirement | Priority | Acceptance Criteria |
|----|-------------|----------|---------------------|
| FR-1 | **Fix the single regressed call site.** In `examples/bd/dolt/formulas/mol-dog-stale-db.toml`'s `cleanup` step, change the terminal `bd close "$WORK_BEAD" --reason "..."` to `gc bd close "$WORK_BEAD" --reason "..."`. | Must | A clean/no-op/soft-escalation/apply run of the formula exits 0 and `bd show "$WORK_BEAD"` reports `status: closed` with the expected reason string attached. |
| FR-2 | **No regression to intentional hard-abort behavior.** `fail_open_after_drain`'s call sites (dry-run scan failure, invalid JSON, and any other existing hard-abort condition in this step) continue to `drain_ack_once` then `exit 1` **without** attempting a close, exactly as today. | Must | The six table-driven cases in `TestStaleDBFormulaFailurePathsDrainAck` (`examples/bd/dolt/stale_db_formula_test.go`) still pass unchanged: dry run command failure, invalid scan JSON, apply command failure, invalid apply JSON, apply misses dry-run reclaimable bytes, and invalid identifier skipped in scan. Each must still exit 1 with the work bead left open. |
| FR-3 | **Harden the adjacent raw-`bd` call for consistency.** Change `append_report_note`'s `bd update "$WORK_BEAD" --append-notes ...` (line 108) to `gc bd update "$WORK_BEAD" --append-notes ...`. Not currently broken (no assignee/actor guard fires on this verb today), but flagged by the bug report as the same call family; fixing it alongside FR-1 pre-empts the same class of failure if a future `bd`/`gc` version tightens `update`'s actor guard the way `close`'s is already guarded. | Should | Line 108 uses `gc bd update`; append-notes content/format and all call sites that invoke `append_report_note` (including hard-abort paths) are otherwise unchanged. |
| FR-4 | **Add a merge-time guard against this regression class.** A test (or lint, per architect's choice — see Open Questions) fails the build if any `*.formula.toml` step script under `examples/**/formulas` or `internal/bootstrap/packs/**/formulas` contains a raw (non-`gc`-prefixed) `bd close`, `bd update ... --status closed`, `bd heartbeat`, or `bd unclaim` invocation, mirroring the convention already followed by every other shipped formula (see Problem Statement). The guard must parse the TOML and inspect only executable step-script content inside the relevant string fields — not grep whole files — because prose and comments live in the same file as the scripts. | Should | Guard fails against a fixture/test formula containing a raw `bd close` line in a step script; passes against the corrected formula set (post FR-1/FR-3). Must not false-positive on prose mentions of `bd` verbs in a formula's own header/doc text (line 22 of this file is such a `bd show` prose mention). |

## Non-Functional Requirements

| ID | Requirement | Metric |
|----|-------------|--------|
| NFR-1 | **Scoped, mechanical change.** The fix touches only the identified `bd close`/`bd update` invocation lines (FR-1, FR-3) plus new guard/test code (FR-4) — no change to the scan/decide/escalate/apply decision branches. | Diff review: no lines changed outside the identified call sites, the stub-binary allowlists and literal-string assertions in `examples/bd/dolt/stale_db_formula_test.go` that FR-1 and FR-3 force to change, and new guard code. |
| NFR-2 | **No hardcoded role names.** Per `AGENTS.md`'s "ZERO hardcoded roles" invariant, `gc bd close`/`gc bd update` resolve actor identity generically from the running session, never from a role-name literal. | Code review / grep for role-name literals in the diff. |
| NFR-3 | **SDK self-sufficiency.** Dog/vapor formulas are infrastructure-maintenance formulas that must keep functioning with only the controller running, independent of any specific user-configured agent role. The fix must not introduce a dependency on a named role. | Test: the fixed formula runs correctly under a generic/unnamed actor, not just the `bd__dog-1-pool` session named in the bug report. |
| NFR-4 | **Failure stays a real signal.** The corrected `gc bd close` call (FR-1) must not be wrapped in `\|\| true` or `run_or_warn` — unlike the script's non-critical calls (e.g. `maintenance_notice`), a close failure is the terminal, load-bearing action of the step and must still propagate as a real error if it recurs for a different reason. | Code review confirms the fixed close call is not swallowed. |

## Technical Constraints

Derived from `AGENTS.md` (no `docs/PROJECT_MANIFEST.md` exists on this rig —
see Problem Statement):

- **No upward dependencies / Layer 0 confinement** — formula step scripts are
  already shell-level, Layer-0 side-effecting code; this fix changes no
  layering.
- **Beads is the persistence substrate; no status files** — unaffected; the
  fix does not add any file-based state tracking.
- **Zero hardcoded roles** — `gc bd close`/`gc bd update` already satisfy
  this; do not reintroduce a role literal while fixing.
- **No panics in library code; atomic writes** — applies if FR-4's guard is
  implemented as Go rather than a shell lint.
- **TDD** — write a red test first for FR-4's guard (a fixture formula
  containing a raw `bd close` line should fail the new check before the
  fixture is corrected or the check is implemented), matching this
  codebase's established convention.
- **Do not touch `fail_open_after_drain`'s exit-without-close semantics**
  (explicitly commented as intentional at the call sites) — reserved as
  out of scope per Non-Goals.
- The canonical fix location is this repo's
  `examples/bd/dolt/formulas/mol-dog-stale-db.toml`. The bug report's
  evidence trail shows the failing run's symlink resolving through a cached
  clone (`~/.gc/cache/repos/<hash>/.../examples/bd/dolt/formulas/mol-dog-stale-db.toml`)
  rather than directly against this checkout; that cache refreshes
  independently and is not something this PRD needs to orchestrate.

## Dependencies

- `examples/bd/dolt/formulas/mol-dog-stale-db.toml` — the file to change
  (FR-1, FR-3); live-symlinked as
  `.beads/formulas/mol-dog-stale-db.formula.toml`.
- `gc bd close` / `gc bd update` (`cmd/gc/cmd_bd_by_id.go` and the `gc bd`
  actor-resolution path) — the already-correct, already-shipped mechanism
  this fix adopts. No changes needed to this mechanism itself.
- `examples/bd/dolt/stale_db_formula_test.go` — the test that renders this
  formula's step script and executes it against stub binaries on `PATH`. Its
  eight fake `gc` implementations `exit 64` on any subcommand outside an
  allowlist (`dolt-cleanup`, `event emit`, `session nudge`, `runtime
  drain-ack`, `mail send`) that does **not** include `bd`, and sixteen
  assertions match the literal strings `bd close bead-1` and `bd update
  bead-1 --append-notes`. Under the script's `set -euo pipefail`, FR-1 and
  FR-3 turn every one of those into a failure unless this harness is updated
  in the same change. Treat it as part of the change, not a follow-up.
- Reference implementations already following the correct convention:
  `internal/bootstrap/packs/core/formulas/mol-polecat-commit.toml:162`,
  `mol-polecat-report.toml:171`, `mol-prompt-synth.toml:147`,
  `mol-do-work.toml:65`.
- Source bead: `ga-j3lkma`.

## Open Questions

### For the architect

1. **FR-4's guard shape.** A Go test that greps the formula-script source
   tree (matching `TestGCNonTestFilesStayOnWorkerBoundary`'s existing
   grep-the-tree style, `cmd/gc/worker_boundary_import_test.go`) vs. a
   `scripts/check-*.sh` shell lint (matching `scripts/check-core-boundary.sh`,
   referenced in `AGENTS.md`). Recommend the Go-test shape for consistency
   with the nearest existing precedent, but defer to whichever convention
   the architect judges most maintainable.
2. **FR-4's directory scope.** Fleet-wide across every current and future
   formula directory in this repo, or narrower (e.g. only
   `internal/bootstrap/packs/**`, the shipped/bootstrapped pack, treating
   `examples/**` as reference material with lower guarantees)? This bug
   lived in `examples/**`, so a narrower scope would not have caught it —
   recommend fleet-wide (within this repo), flagged because it changes the
   guard's false-positive surface: formula files legitimately mention
   `bd` verbs in prose within header/doc text (line 22 of this very file is a
   `bd show` prose mention), so the guard must inspect executable step-script
   content inside the TOML string fields, not comment or documentation text.
3. **FR-3 bundling.** Land the `append_report_note` hardening (FR-3) in the
   same commit as FR-1 (one file, adjacent lines, same session), or file it
   as a separate lower-priority follow-up since it is not currently broken?
   Recommend bundling; architect may split if preferred.
4. **Does `gc bd close` actually resolve the actor differently here?**
   This PRD's remedy assumes it does, but the mechanism is not yet confirmed
   on a city without a relocated class binding. The `gc bd` wrapper passes
   `BEADS_ACTOR` through to `bd` unchanged, and the in-process routed arm
   that could resolve identity differently engages only for class-owned
   beads. Issue #5814 reports the wrapper failing identically, which is
   evidence against the assumption. **The experiment that settles it:** on a
   default single-binding city, freshly claim a bead, then run both `bd
   close` and `gc bd close` against it with `BEADS_ACTOR` set exactly as the
   formula's step script sees it, and capture both exit codes. If the
   wrapper fails the same way, FR-1 is not the fix and this PRD needs
   rework. **Answer this before implementing FR-1.**

### For the designer

Not applicable — this is a backend/formula-tooling reliability fix with no
UI/UX surface. No dashboard panel or CLI UX is added or changed; the only
consumer-visible change is that a previously-failing exit code becomes 0 on
success.

## References

- Source bead: `ga-j3lkma`
- Reference implementations (already-correct convention):
  `internal/bootstrap/packs/core/formulas/mol-polecat-commit.toml`,
  `mol-polecat-report.toml`, `mol-prompt-synth.toml`, `mol-do-work.toml`.
  Note that `mol-do-work.toml:65` is prompt prose instructing an agent to
  run `gc bd heartbeat`, not an executable step-script line; the other three
  citations are executable calls.
- Related but distinct identity-lifecycle PRD, different mechanism, no
  overlap: `engdocs/proposals/executor-identity-stamp-lifecycle.md`
  (`ga-cm2o5t`) — covers `gc.session_name`/`gc.work_dir` stamp clearing on
  bead re-route, not `bd close`'s actor/assignee guard.
