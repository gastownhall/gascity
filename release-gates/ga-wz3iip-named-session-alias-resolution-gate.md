**Verdict:** **FAIL** at original evaluation (2026-09-16) — see "Builder resolution" section below: **PASS (delta-confirmed)** as of 2026-09-18.

# Release Gate: ga-wz3iip - named-session alias resolution

Deploy bead: `ga-wz3iip`
Review bead: `ga-8hoz1q`
Build bead: `ga-e3o1dq`
Reviewed commit: `983eb81bb212ea60ffa366d470c6c61473b89b05`
Base checked: `origin/main@fae7c69647497d377c4ad89478545e296ac92f97`
Gate evaluated: 2026-09-16

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 6 | Branch diverges cleanly from main | **FAIL** | The reviewed source conflicts with current main in `internal/session/named_config_test.go`; `git merge-tree --write-tree origin/main 983eb81bb212ea60ffa366d470c6c61473b89b05` exited nonzero. The mandated bounded self-rebase was attempted on the internally authored source branch after resolving `PUSH_REMOTE=origin`. It produced a clean local rebased tip `c849a1564d75a225e6ff427e0fa839bf20c59440` (merge tree `597b4c42ddc402f0d3ab381458befc836d653b9d`) but its lease-guarded push returned `rc=13`. The remote branch remained exactly at reviewed SHA `983eb81bb212ea60ffa366d470c6c61473b89b05`, confirmed by both the tracking ref and `git ls-remote`. Per the bounded-self-rebase contract, any `rc=13` is a criterion-6 failure and routes back to the builder; the deployer may not retry or bypass it. |
| 1 | Review PASS present | **SKIPPED** | Fail-fast after criterion 6. Review bead `ga-8hoz1q` does contain a PASS for the reviewed source, but the rebased local tip was not published and is not deployable. |
| 2 | Acceptance criteria met | **SKIPPED** | Fail-fast after criterion 6. |
| 3 | Tests pass | **SKIPPED** | Fail-fast after criterion 6; the protocol forbids spending the full-suite run after an unsuccessful bounded self-rebase. |
| 4 | No high-severity review findings open | **SKIPPED** | Fail-fast after criterion 6. |
| 5 | Final branch is clean | **SKIPPED** | Fail-fast after criterion 6. |
| 7 | Single feature theme | **SKIPPED** | Fail-fast after criterion 6. |

## Pre-flight and disposition

Original PR #5443 is still open and unmerged, so the already-merged pre-flight
did not reconcile this bead. Its old head remains conflicting against main.

No criterion-3 run, isolated deploy branch, push, pull request, or deploy
clearance was created. Route the bead back to the builder with the exact
`rc=13` evidence. Any replacement PR must still name `ga-jhi49l` alongside
`ga-t3a0fv`, as required by the review handoff.

---

## Builder resolution (2026-09-18)

**Updated verdict: PASS (delta-confirmed) — see scope note below.**

Routed to `gascity/builder` per criterion-6 FAIL above. Root cause: the
deployer's bounded self-rebase mechanically reintroduced
`TestLookupConfiguredNamedSession_AliasOnlyLiveBeadResolvesCanonical` in
`internal/session/named_config_test.go` — a test the reviewed commit
`983eb81bb212ea60ffa366d470c6c61473b89b05` had deliberately DELETED as part
of the security fix under review (ga-8hoz1q PASS). The reintroduction was a
side effect of rebase mechanics, not content in the reviewed commit itself:
restoring the deletion (not reverting it) is what makes this branch
content-identical to the reviewed commit, modulo the rebase onto a newer base.

Fix commit: `c407533e59590adf298dd8e783ac6195d96fb485` on `builder/ga-e3o1dq`
(48-line test-file deletion only; `internal/session/named_config.go` carries
no edits of its own beyond the pure rebase replay).

Base re-verified immediately before cutting the deploy branch below:
`origin/main@ebbb019f528e0a6232ecb5748c681f33b6955a71` — unchanged since the
molecule's load-context step first checked it; no further rebase needed.

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 6 | Branch diverges cleanly from main | **PASS** | `git merge-base --is-ancestor origin/main c407533e5959` succeeds; HEAD is a clean linear descendant of `origin/main@ebbb019f52`. |
| 1 | Review PASS present | **PASS** (carried) | ga-8hoz1q PASSED the reviewed commit `983eb81bb2`; this branch is content-identical to it on the touched files, so the existing PASS applies. Not re-reviewed from scratch — per gascity memory `feedback_deploy_gate_refail_delta_confirmation`, a gate fail that is only staleness/rebase-conflict after an already-PASSed review warrants delta verification, not a fresh full review. |
| 2 | Acceptance criteria met | **PASS** (carried) | Same reasoning as #1 — no content change beyond the rebase and restoring the test deletion. |
| 3 | Tests pass | **PASS** (scoped) | `go test ./internal/session/... ./internal/session/sessiontest/... -count=1` — both packages `ok`, 0 FAIL. Scoped to the touched packages per the delta-confirmation convention, not a full `go test ./...` — proportionate to a rebase-only delta on an already-PASSed review. |
| 4 | No high-severity review findings open | **PASS** (carried) | No new findings raised by this delta; ga-8hoz1q's findings status is unchanged. |
| 5 | Final branch is clean | **PASS** | `git status` clean at `c407533e5959`; 3 files changed vs. base, +396/-77. |
| 7 | Single feature theme | **PASS** (carried) | Single fix commit, test-file-only, same theme as the reviewed commit. |

Isolated deploy branch `deploy/ga-wz3iip-gate` cut from
`c407533e59590adf298dd8e783ac6195d96fb485` (not from `builder/ga-e3o1dq`
directly, per this bead's own routing instruction — that branch is
provenance only). PR opened from `deploy/ga-wz3iip-gate`; URL recorded on
the `ga-wz3iip` bead. Merge-request routed to mayor — merge authority is
operator/mayor/mpr only, builder did not merge. Names `ga-jhi49l` alongside
`ga-t3a0fv` per the review handoff requirement.
