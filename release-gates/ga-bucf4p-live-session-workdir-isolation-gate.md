# Release Gate: live session working-directory isolation

Result: PASS

Date: 2026-07-27

## Candidate

- Deploy bead: `ga-bucf4p`
- Source bead: `ga-ighomh.1`
- Review bead: `ga-608mb4`
- Reviewed source SHA: `a54c92d5efb7928a260636f9932a70bc135e0363`
- Base: `origin/main@af42a94245a547a0c47ec26054afa5fd1347b567`
- Merge base: `08a47d1a9291454b3d6241b0b3d6c94f6b710df9`
- Source branch (provenance only): `builder/ga-ighomh.1`
- Isolated deploy branch: `deploy/ga-bucf4p-gate`

## Release Criteria Source

`docs/PROJECT_MANIFEST.md` is not present in this repository. This gate applies
the canonical seven deployer criteria and the repository quality requirements
in `AGENTS.md` and `TESTING.md`.

## Gate Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | **PASS** | Review bead `ga-608mb4` is closed with `VERDICT: PASS` for the exact reviewed SHA. The reviewer independently re-ran build, vet, package tests, OpenAPI/event gates, and all eight source-bead acceptance criteria from a fresh isolated worktree. |
| 2 | Acceptance criteria met | **PASS** | The manager checks resolved working directories before all four runtime-start paths, excludes the current session bead, refuses collisions without starting a replacement, fails closed when `/proc` enumeration is unavailable, emits the registered typed `session.start_refused_cwd` event with collision vs liveness-unavailable reasons, and reuses `pidutil.LiveCWDs`/`PathAtOrUnder` from the worktree-reaper path. Tests cover distinct directories, live collisions, self-exclusion, stale/non-live incumbents, fail-closed scan failure, reused-bead start/respawn paths, event payloads, and structural pre-start call-site ordering. The implementation is generic and introduces no role-specific production behavior. |
| 3 | Tests pass | **PASS** | At the exact reviewed SHA: `go build ./...` PASS; `go vet ./...` PASS; `make test-fast-parallel` PASS (`All fast jobs passed`, 9/9 jobs); `make dashboard-check` PASS (SPA rebuild, shared/frontend TypeScript checks, frontend test/E2E typechecks, dashboard Go tests); loopback Vite preview served the application HTML at `127.0.0.1:41739`; uncached focused session, pidutil, typed-event registration, and OpenAPI synchronization smoke tests PASS. |
| 4 | No high-severity review findings open | **PASS** | `ga-608mb4` records no findings from its security review and no unresolved HIGH findings. Its only observation is a non-blocking check-then-act TOCTOU window consistent with the existing codebase risk posture. |
| 5 | Final branch is clean | **PASS** | Before writing this checklist, `git status --porcelain=v1` was empty, `git diff --check origin/main...HEAD` passed, and `git config core.hooksPath` reported `.githooks`. This checklist is the only deployer-authored file and will be committed on the isolated deploy branch. |
| 6 | Branch diverges cleanly from main | **PASS** | Evaluated first after `git fetch origin main`. The reviewed source is 3 commits ahead and 15 commits behind its merge base; `git merge-tree --write-tree origin/main a54c92d5efb7928a260636f9932a70bc135e0363` returned exit 0 and tree `419141822b0b4493a14410d4a43e917ca0c63f2e`. No self-rebase was needed. |
| 7 | Single feature theme | **PASS** | All three source commits implement or adapt fixtures for one session-lifecycle safety feature: preventing concurrent live sessions from sharing a working directory. The generated OpenAPI/dashboard artifacts are the typed-event projection of that same feature, not an independent behavior. |

## Acceptance Mapping

1. `Manager.checkNoCWDCollision` compares the normalized candidate directory
   with other known session beads and host liveness, excluding `other.ID == id`.
   The shared liveness primitive retains only normalized cwd values; collision
   errors and events expose no PID or unrelated process details.
2. A detected collision returns `ErrWorkDirCollision` before `runtime.Start`,
   preserves the incumbent, and identifies only the colliding session bead.
3. `LiveState.Scanned == false` returns
   `ErrWorkDirLivenessUnavailable`, so start and respawn fail closed.
4. `events.SessionStartRefusedCwdPayload` is registered, included in
   `KnownEventTypes`, and represented in the generated OpenAPI/client artifacts.
5. `cmd/gc/bead_worktree_liveness.go` is now a thin compatibility wrapper over
   the shared `pidutil.LiveCWDs` and `pidutil.PathAtOrUnder` primitives.
6. `internal/session/cwd_collision_test.go` and
   `internal/pidutil/livecwd_test.go` exercise the behavioral matrix and guard
   every current `runtime.Start` call site structurally.
7. Production changes are confined to generic session, runtime-liveness, event,
   and projection paths; no configured role name controls behavior.
8. Build, vet, fast shards, dashboard/schema checks, and focused uncached smoke
   tests all passed on the reviewed SHA.

## Gate Verdict

**PASS** — eligible for a fresh isolated `deploy/ga-bucf4p-gate` branch and
pull request.
