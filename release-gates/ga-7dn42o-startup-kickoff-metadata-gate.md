# Release gate - startup kickoff metadata binding (ga-7dn42o)

**Verdict:** PASS

- Bead: `ga-7dn42o`
- Source branch: `gc-builder-3-cfd0e6a74ec1`
- Reviewed HEAD: `4583684ebbe698747eea879db98256fb5cb89124`
- Base checked: `origin/main` at `9ddbea5c0b4b3cebf09fc36c0f88a8c52f9dd991`
- Manifest note: `docs/PROJECT_MANIFEST.md` is not present in the Gas City worktree; this gate applies the deployer prompt's seven release criteria.

## Criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 6 | Branch diverges cleanly from main | PASS | Evaluated first. `git merge-base origin/main origin/gc-builder-3-cfd0e6a74ec1` equals `origin/main`; branch is 0 behind / 2 ahead. `git merge-tree --write-tree origin/main origin/gc-builder-3-cfd0e6a74ec1` returned tree `ad1937b99566906776bc12718098bea8f36ca2e0` with exit 0. |
| 1 | Review PASS present | PASS | Review bead `ga-al803f` is closed with re-review verdict PASS for `4583684ebbe698747eea879db98256fb5cb89124`. |
| 2 | Acceptance criteria met | PASS | Named/direct bound sessions seed `gc.bound_step_id` plus `startup_kickoff_state=pending`, `startup_kickoff_started_at`, and `startup_kickoff_attempts=0`; unbound named sessions skip/clear kickoff metadata; pool-managed sessions remain ineligible; reopen path reseeds or clears stale kickoff metadata atomically through `extraMeta`; no runtime provider interface change and no `GC_STARTUP_PROMPT_DELIVERED` behavior change. |
| 3 | Tests pass | PASS | `go build ./...` passed. `go vet ./...` passed. `go test ./cmd/gc/... -run 'TestReopenClosedConfiguredNamedSessionBead|StartupKickoff' -v` passed all 9 targeted tests. `make test-fast-parallel` passed all 8 fast jobs. |
| 4 | No high-severity review findings open | PASS | Initial REQUEST-CHANGES finding was fixed by `startupKickoffReopenMetadata`; reviewer re-check reports the blocking finding fully resolved and no new issues found. |
| 5 | Final branch is clean | PASS | Scratch worktree at reviewed HEAD was clean before gate-file creation (`git status --short --branch` reported only detached HEAD). The gate file is the only deployer-authored delta and is committed as the release-gate commit. |
| 7 | Single feature theme | PASS | The commit set touches one subsystem/theme: startup-kickoff metadata binding for named/direct session beads. Diff paths are `cmd/gc/build_desired_state.go`, `cmd/gc/session_beads.go`, `cmd/gc/session_beads_test.go`, `cmd/gc/startup_kickoff.go`, `cmd/gc/template_resolve.go`, and `internal/beadmeta/keys.go`. |

## Validation

- `git fetch origin refs/heads/gc-builder-3-cfd0e6a74ec1:refs/remotes/origin/gc-builder-3-cfd0e6a74ec1` - PASS
- `git ls-remote origin refs/heads/gc-builder-3-cfd0e6a74ec1 refs/heads/main` - confirmed remote branch at `4583684ebbe698747eea879db98256fb5cb89124` and main at `9ddbea5c0b4b3cebf09fc36c0f88a8c52f9dd991`
- `git merge-tree --write-tree origin/main origin/gc-builder-3-cfd0e6a74ec1` - PASS, no conflicts
- `go build ./...` - PASS
- `go vet ./...` - PASS
- `go test ./cmd/gc/... -run 'TestReopenClosedConfiguredNamedSessionBead|StartupKickoff' -v` - PASS
- `make test-fast-parallel` - PASS, all fast jobs passed

## Review Notes

The PR should call out that this is a metadata contract change for named/direct startup delivery tracking. The important review surface is the reopen path: stale `startup_kickoff_*` and `gc.bound_step_id` metadata is replaced for a newly-bound step and cleared for unbound reopens.
