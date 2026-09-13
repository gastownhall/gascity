# Release Gate: native-store hook removal and in-process autoclose re-review

- Deploy bead: `ga-ms9iyc`
- Source bead: `ga-2ri4x3`
- PR: https://github.com/gastownhall/gascity/pull/3303
- Branch: `builder/ga-frmdxd.2`
- Reviewed head: `7c95489881edb30c17006ed7a9e5bad31e8c0ffe`
- Base checked: `origin/main` at `60e402be98f7f1487c618a1f92590df88247299f`
- Gate date: 2026-06-12 UTC

`docs/PROJECT_MANIFEST.md` is not present in this checkout. This gate uses the
active deployer release criteria plus `TESTING.md` and the acceptance evidence
recorded on `ga-2ri4x3`.

## Release Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `ga-2ri4x3` notes record `Review Verdict: PASS (pass 4 - final)` for commit `7c9548988` on PR #3303 after the prior HIGH hook-removal finding was resolved. |
| 2 | Acceptance criteria met | PASS | `cmd/gc/hooks.go` now removes only gc-stamped hook files via `isGCManagedHook`; `cmd/gc/hooks_test.go` covers stamped removal and preservation of user-authored same-name hooks; `cmd/gc/api_state.go` dispatches bead-close autoclose in-process; `cmd/gc/api_state_test.go` covers convoy autoclose through the event path; `cmd/gc/city_discovery.go` ignores `/tmp/.gc` runtime roots. |
| 3 | Tests pass | PASS | Local gate commands passed on `builder/ga-frmdxd.2`: `make test-fast-parallel`; `go vet ./...`; `go build ./...`. |
| 4 | No high-severity review findings open | PASS | The final review notes on `ga-2ri4x3` state that the blocker is resolved and all findings from passes 1-3 are resolved. PR #3303 has no review/comment threads. |
| 5 | Final branch is clean | PASS | Clean before gate write: `git status --short --branch` reported only `## builder/ga-frmdxd.2...origin/builder/ga-frmdxd.2`. This gate file and the whitespace cleanup in the older gate file are the only deployer changes to commit. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` succeeded with tree `10ee78557fc8b3f17a483ebe4ef88f55f74d937d`; GitHub reports PR #3303 mergeable. |
| 7 | Single feature theme | PASS | The branch is one `cmd/gc` lifecycle theme: remove unsafe gc-managed bead hook installation/removal behavior, keep native-store autoclose in-process, and keep test/CLI city discovery from resolving `/tmp/.gc` runtime state as a project city. The emergency relay, doctor check, and pack-release symlink work are absent from this PR. |

## Commands

```text
make test-fast-parallel
go vet ./...
go build ./...
git merge-tree --write-tree origin/main HEAD
git diff --check origin/main...HEAD
```

`git diff --check origin/main...HEAD` initially reported trailing whitespace in
the older `ga-5mgv67` gate file already on the PR branch. This gate commit
removes that whitespace so the final branch passes the check.

## Diff Summary

```text
.beads/config.yaml
cmd/gc/api_state.go
cmd/gc/api_state_test.go
cmd/gc/beads_provider_lifecycle_test.go
cmd/gc/city_discovery.go
cmd/gc/hooks.go
cmd/gc/hooks_test.go
cmd/gc/lifecycle_coordination_test.go
cmd/gc/molecule_autoclose_test.go
release-gates/ga-5mgv67-native-store-hook-autoclose-gate.md
release-gates/ga-ms9iyc-native-store-hook-autoclose-gate.md
```
