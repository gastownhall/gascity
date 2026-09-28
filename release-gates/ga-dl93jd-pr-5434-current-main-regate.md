# Release Gate Revalidation: ga-dl93jd / PR #5434

Recovery bead: `ga-dl93jd`
Original deploy bead: `ga-0ckn7x`
Review bead: `ga-09qq0u`
Reviewed content commit: `341069eee3aa90b32afe2ff015600d7f0090acce`
Previous PR head: `7417f90b89b9a3838110df2b8a1cb02722daf0eb`
Evaluated source head: `d13fa00927d1d34a02e7783825f63daa965a337a`
Deploy branch: `deploy/ga-0ckn7x-gate-r2-20260820`
Base: `origin/main@c340e656f5b769be49ae335240ee54bd9866300f`
Merge tree: `64008a1aba9845153066319fad4a9797ccff9a3d`
Gate evaluated: 2026-09-16
Verdict: **PASS**

This record supersedes the 2026-08-20 gate result for the current PR head. The
old record remains as evidence of the earlier candidate and its narrow mayor
waiver; no waiver is needed for this revalidation.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead `ga-09qq0u` records PASS at `341069eee3aa90b32afe2ff015600d7f0090acce`, with no style, security, or specification findings. `git diff --exit-code 341069eee3aa90b32afe2ff015600d7f0090acce d13fa00927d1d34a02e7783825f63daa965a337a -- cmd/gc/cmd_reload_test.go` returned 0, so the reviewed implementation is byte-identical after rebase. |
| 2 | Acceptance criteria met | PASS | Both duplicated five-second polling loops use the existing `awaitCond(..., hangBudget, ...)` path. The synchronized census is 482 calls / 173 files for all fixed sleeps and 319 calls / 121 files for both untagged fixed-sleep rows. `TestRepositoryLedgerMatchesCensusAndDocumentation` passed on the final tree. |
| 3 | Tests pass | PASS | The final 40-job union completed 39 green jobs with 51,224 PASS / 1 attributed FAIL / 227 SKIP results, counting subtests. Both changed reload tests passed in their process shards. The only raw failure, `TestCleanInstallTutorialPath`, is the independently reproduced concurrent-initializer schema race tracked by `ga-lejnse` and root-fix bead `ga-e2z1zb`; details are below. No test was rerun. |
| 4 | No high-severity review findings open | PASS | The reviewer reported no findings at any severity. The sole raw gate failure is outside the candidate's package and mechanism. |
| 5 | Final branch is clean | PASS | The rebase produced the expected five-file feature diff before this record. The only conflict in the first rebase was the fixed-sleep ledger, resolved by preserving current owners and totals while applying the reviewed two-call reduction. The second rebase onto `c340e656f5` replayed without conflict. |
| 6 | Branch diverges cleanly from main | PASS | `origin/main@c340e656f5b769be49ae335240ee54bd9866300f` is the exact merge base. `git merge-tree --write-tree d13fa00927d1d34a02e7783825f63daa965a337a origin/main` produced `64008a1aba9845153066319fad4a9797ccff9a3d`. Range-diff preserves both prior gate commits exactly; the feature commit differs only in current-main ledger context and retains the reviewed two-call reduction. |
| 7 | Single feature theme | PASS | The source delta replaces two reload-test polling loops and synchronizes the three fixed-sleep ledgers. The two release-gate records document that same change and its revalidation. |

## Test evidence

The authoritative final-tree command was:

```text
DOCKER_HOST=unix:///run/user/1000/podman/podman.sock
TESTCONTAINERS_RYUK_DISABLED=true
GOFLAGS=-v
GO_TEST_TIMEOUT=30m
LOCAL_TEST_JOBS=4
CMD_GC_PROCESS_TOTAL=6
make test-local-full-parallel
```

Logs are retained at `/var/tmp/ga-dl93jd-final-full-logs`, with the coordinator
log at `/var/tmp/ga-dl93jd-final-full.log`.

- 40 jobs ran: 39 green, 1 raw failure.
- 51,224 tests/subtests passed, 1 failed, and 227 skipped.
- `TestSendReloadControlRequestInvalidConfig` passed in
  `cmd-gc-process-2-of-6` in 0.50 seconds.
- `TestSendReloadControlRequestNoChange` passed in
  `cmd-gc-process-6-of-6` in 0.51 seconds.
- Both tests were also selected in their integration package shards and skipped
  there by the expected process-test guard.

The only raw failure was `TestCleanInstallTutorialPath` in
`integration-rest-full-2-of-8`. Its temporary `hq` database was observed at
schema cursor v59 while another initializer was still migrating it, then the
shared-server safety gate refused seven pending migrations to v66. This is the
exact random-cursor condition already reproduced for the same test on unrelated
candidate `ga-8ys0v4`, including failure with the pinned beads binary. The root
cause and fix are tracked by `ga-e2z1zb`; a further independent exact-test
sighting is recorded there from `ga-kkktn4`.

Attribution satisfies the non-diff-owned failure protocol through cross-PR proof
3(b). The candidate changes only reload test polling and census documentation,
does not touch `test/integration` or the beads initialization path, adds no test
target or concurrent load, and removes two fixed sleeps. The sighting was added
to both `ga-lejnse` and `ga-e2z1zb`. It was not rerun.

An earlier full union on the first rebased candidate
`9eaf9eab42c4feab3405aee5e7969cd2f455a443` completed 38/40 jobs with two
attributed failures. Main then advanced with the Dolt test-store cleanup from PR
#6271, so the branch was rebased again and the full union was run once on the new
candidate. The earlier shared-Dolt process failure and retry-harness race both
passed in the final run; their original raw results remain preserved in
`/var/tmp/ga-dl93jd-full-logs`.

## Static and policy evidence

All of the following passed on the final source tree:

- `make test-ci-policy`
- `LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=c340e656f5b769be49ae335240ee54bd9866300f make lint-affected fmt-check-changed`
- `make build`
- `go vet ./...`
- `make check-core-boundary check-native-dependency-surface check-residency-boundary check-gomod-replace check-eventexport-isolation`
- `make check-routed-test-rows check-split-topology-rows`
- `make check-release-dist-ignore`
- `make check-hooks`
- `go test ./internal/testpolicy/resourcecensus`
- `govulncheck -show verbose ./...` with `govulncheck@v1.7.0`: zero reachable or imported-package vulnerabilities; three module-only informational findings.

`git diff --check` initially reported Markdown hard-break spaces in the old gate
record added by this branch. This revalidation commit removes those spaces; the
check is rerun after the edit.

## Disposition

Force-update the existing PR branch with lease, verify PR #5434 at the new exact
head, and post `release-gate/deploy-clearance=success` on that head. Merge
authority remains with the operator, mayor, or mpr; the deployer does not merge.
