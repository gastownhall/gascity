# Release Gate: native-store hook removal and in-process autoclose

- Deploy bead: `ga-rjqmi0`
- Review bead: `ga-xjqji2`
- Source bead: `ga-xjqji2`
- PR: https://github.com/gastownhall/gascity/pull/3303
- Branch: `builder/ga-frmdxd.2`
- Reviewed head: `39a22bf36b421edcdd59a78d3a7a327d3dc61593`
- Base checked locally: `origin/main` at `2315679e25a9b196710edfcaf502496cee576cfc`
- Gate date: 2026-06-13

`docs/PROJECT_MANIFEST.md` is not present in this checkout. This gate uses the
active deployer release criteria plus `TESTING.md` and the acceptance evidence
recorded on `ga-xjqji2`.

## Release Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `ga-xjqji2` is closed with `Review Verdict: PASS` for `39a22bf36b421edcdd59a78d3a7a327d3dc61593` on PR #3303. |
| 2 | Acceptance criteria met | PASS | Reviewer notes state all four acceptance criteria are met: gc-installed hooks are removed idempotently, user-authored same-name hooks are preserved, autoclose fires in-process on `BeadClosed`, and `/tmp` runtime state is not resolved as a project city. The diff implements those behaviors in `cmd/gc/hooks.go`, `cmd/gc/api_state.go`, and `cmd/gc/city_discovery.go` with matching tests. |
| 3 | Tests pass | PASS | Final local gate run passed from clean detached worktree `/home/jaword/projects/gc-management/.gc/worktrees/gascity/deploy-ga-rjqmi0-test`: `LOCAL_TEST_JOBS=4 CMD_GC_PROCESS_TOTAL=3 make test-fast-parallel`, `TMPDIR=/home/jaword/tmp/gascity-test-tmp go vet ./...`, and `TMPDIR=/home/jaword/tmp/gascity-test-tmp go build ./...`. `git diff --check origin/main...HEAD` passed. GitHub checks for PR #3303 are green, including CI, CodeQL, integration shards, and `cmd/gc process` shards. |
| 4 | No high-severity review findings open | PASS | `ga-xjqji2` reports no blockers. The only non-info finding is LOW on the implicit `stores[0]` ownership assumption; reviewer marked it non-blocking. |
| 5 | Final branch is clean | PASS | `git status -sb` on `deploy/ga-rjqmi0-gate` reported only `## deploy/ga-rjqmi0-gate...origin/builder/ga-frmdxd.2` before writing this gate file. After this file is committed, the branch contains the reviewed PR head plus this gate artifact. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` succeeded with tree `99fe8c242a274d619bc531e9e01048b0c19d02e1`; GitHub reports PR #3303 `mergeStateStatus=CLEAN`. |
| 7 | Single feature theme | PASS | The branch is one native-store lifecycle theme: remove gc-managed `bd` forwarder hook installation/removal, preserve user hooks, and run convoy/wisp/molecule autoclose from the controller event path. The existing release-gate files on the branch are prior gate artifacts for this same PR; the product-code diff stays within `.beads/config.yaml` and `cmd/gc` hook/autoclose/city-discovery surfaces. |

## Acceptance Evidence

| Acceptance criterion | Result | Evidence |
|----------------------|--------|----------|
| gc-installed hooks are removed on `installBeadHooks` and the operation is idempotent. | PASS | `cmd/gc/hooks.go` removes stamped gc-managed hook files instead of writing hook scripts; `cmd/gc/hooks_test.go` covers hook removal and missing-hook success. |
| User-authored hooks with the same names are preserved. | PASS | `isGCManagedHook` checks the gc stamp before removal; tests cover user-owned `on_create`, `on_update`, and `on_close` preservation. |
| Autoclose fires in-process on `BeadClosed`. | PASS | `cmd/gc/api_state.go` dispatches convoy, wisp, and molecule autoclose from `applyBeadEventToStores`; `TestApplyBeadEventToStoresTriggersConvoyAutoclose` covers the event path with synchronous dispatch. |
| `/tmp` supervisor runtime state is not treated as a project city. | PASS | `cmd/gc/city_discovery.go` ignores implicit system temp runtime roots; reviewer notes confirm the `/tmp` city discovery fix. |

## Commands

```text
LOCAL_TEST_JOBS=4 CMD_GC_PROCESS_TOTAL=3 make test-fast-parallel
TMPDIR=/home/jaword/tmp/gascity-test-tmp go vet ./...
TMPDIR=/home/jaword/tmp/gascity-test-tmp go build ./...
git diff --check origin/main...HEAD
git merge-tree --write-tree origin/main HEAD
gh pr checks 3303 --repo gastownhall/gascity
```

## Test Notes

An initial default-parallel local run in `/tmp/gascity-deploy-ga-rjqmi0` failed
because `/tmp` was 98% full and Go compiler/linker temp writes hit `no space
left on device`. After clearing generated `go-build*` temp directories, a
reduced retry from the same `/tmp` source checkout failed path-sensitive
`cmd/gc` city-discovery/supervisor tests. The final gate result comes from the
same commit in a detached `/home` worktree, where the fast sharded baseline
passed.

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
