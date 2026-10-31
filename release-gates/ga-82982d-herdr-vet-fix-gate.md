# Release gate: restore repository-wide vet after Herdr test-helper refactor

**Verdict:** **PASS**

WAIVED: criterion 3, waiver_ref mayor-2026-09-17-ga-82982d-c1

- Deploy bead: `ga-82982d`
- Build bead: `ga-0bz01c`
- Review bead: `ga-hpm35r`
- Reviewed source: `05952f5c2418c0d651dccd13e605a84e4257d641`
- Merge base: `ca8c28edc46f55c851f8f7a50c78ca129f09b700`
- Base evaluated: `origin/main@ec4683a6b89cbc6059eb8d6065ab2168cecbe890`
- Deploy mode: remote
- Date: 2026-09-16

The already-merged preflight found no base-repository pull request carrying the
reviewed source. Criterion 6 passed first, so no bounded self-rebase was needed.
`docs/PROJECT_MANIFEST.md` is absent at the reviewed source; this record follows
the deployer release criteria, `TESTING.md`, the Makefile, and
`engdocs/contributors/release-gate-criteria-conventions.md`.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-hpm35r` records a round-2 PASS with no findings and pins the authoritative deploy source to `05952f5c2418c0d651dccd13e605a84e4257d641`. |
| 2 | Acceptance criteria met | **PASS** | The reviewed range is one commit containing only the standalone Herdr test-helper repair: the helper again declares its actual two-value return contract, the caller destructures two values, and the socket listener receives the `*Provider`. Repository-wide vet, the Herdr tests, and the source-scope guard pass. |
| 3 | Tests pass | **PASS (waived)** | The full 40-job local CI union completed with 37 PASS jobs and 3 attributed non-diff-owned failures; no job was omitted. A counted exact-head `make test` run recorded 44,869 PASS, 0 FAIL, and 198 SKIP. All executable diff-owned tests passed. The one diff-owned live-tier SKIP, `TestProviderLiveClaudeKindPath`, is covered only by `mayor-2026-09-17-ga-82982d-c1`; evidence below proves its gated code path is disjoint from this diff. `test_cmd_scope: full-suite`. |
| 3a | Pre-existing failures attributed | **PASS** | Three raw full-suite failures are attributed to pre-existing tracker `ga-vkhfnj` under clause 3(a), with mechanism and path proofs below. The tracker predates this run, was opened before citation, received this run's sighting, and the comment was read back. |
| 3b | Policy/lint lane | **PASS** | On the exact reviewed source: `go build ./...`, `make fmt-check`, `make lint-new LINT_BASE=origin/main`, `make vet`, `make test-ci-policy`, `make test-worker-core`, and `make test-worker-core-phase2-all` all passed. |
| 3c | CI-config lane | **PASS** | `ci_lane_run: n/a (the candidate changes no CI job, matrix, timeout, required-check list, test target, or test resource policy)`. |
| 4 | No high-severity review findings open | **PASS** | The second-pass reviewer found no style, security, specification, correctness, or coverage blocker. Unresolved HIGH count: 0. |
| 5 | Final branch is clean | **PASS** | At the exact reviewed source, `git status --short` was empty, `git diff --check origin/main...05952f5c2418c0d651dccd13e605a84e4257d641` passed, and `make check-hooks` confirmed `.githooks` owns `core.hooksPath`. The isolated deploy branch is committed cleanly with this record as its only additional file. |
| 6 | Branch diverges cleanly from main | **PASS** | After a final fetch, `git merge-tree --write-tree origin/main 05952f5c2418c0d651dccd13e605a84e4257d641` exited 0 against `origin/main@ec4683a6b89cbc6059eb8d6065ab2168cecbe890` and produced tree `2a42b546be60a48a00acf88f3af96cc6db8fad93`. A disposable worktree containing the actual merge result also passed `go build ./...` and `go vet ./...`. No self-rebase was needed. |
| 7 | Single feature theme | **PASS** | The two changed test files complete one interrupted Herdr test-helper refactor. Removing either change restores a compile/vet mismatch; there is no independently shippable second behavior or production-code change. |

## Criterion 2: acceptance evidence

1. `newFakeHerdrProviderForSession` now returns `(*Provider, string)`, matching
   its only return statement and the established sibling helper contract.
2. `TestKindPathNamesWorkThroughFakeProviderLifecycle` destructures those two
   values and passes the returned provider to `listenHerdrSocket`, whose
   parameter is `*Provider`.
3. The reviewed range is exactly one commit, two files, 3 insertions and 3
   deletions. Both files are tests under `internal/runtime/herdr`; production
   behavior and dependencies are unchanged.
4. The ancestry-scope guard passed for `ga-82982d` and its confirmed build bead
   `ga-0bz01c`; the source introduces no `.claude/**` path or unrelated commit.

## Criterion 3: test evidence

The required isolation wrapper ran the documented broad local suite with the
rootless Podman socket configured and Ryuk disabled:

```text
DOCKER_HOST=unix:///run/user/1000/podman/podman.sock
TESTCONTAINERS_RYUK_DISABLED=true
make test-local-full-parallel
```

- full-suite result: 37 PASS jobs, 3 attributed raw FAIL jobs, 0 omitted jobs
- full runner log: `/var/tmp/ga-82982d-full-suite.log`
- per-job logs: `/var/tmp/gc-local-tests.pbVEDG`
- exact-head counted run: `make test` — 44,869 PASS, 0 FAIL, 198 SKIP
- counted event log: `/var/tmp/ga-82982d-make-test.jsonl`
- Herdr package in the counted run: 211 PASS, 0 FAIL, 6 SKIP
- `diff_tests_executed`: `TestKindPathNamesAreUnique` PASS;
  `TestKindPathNamesWorkThroughFakeProviderLifecycle` PASS;
  `TestStartKindPathRegistersAndPersistsBinding` PASS;
  `TestProviderLiveClaudeKindPath` SKIP under its pre-existing live-provider
  gate and covered by the waiver above
- exact reviewed source: `go build ./...` PASS and `go vet ./...` PASS
- merge result with current `origin/main`: `go build ./...` PASS and
  `go vet ./...` PASS
- `waiver_ref: mayor-2026-09-17-ga-82982d-c1`
- `ci_lane_run: n/a (no CI-config change)`

`skip_justification`: the fast tier intentionally skips opt-in live-provider,
integration, and host-dependent cases; the 40-job union exercises the broader
integration tiers. The only diff-owned skip is
`TestProviderLiveClaudeKindPath`. Its body constructs a real provider through
`New`, contains no call to either changed helper, and is gated by the unchanged
`requireLiveHerdr` opt-in. The candidate's compile-time helper contract is
instead exercised by the passing fake-provider lifecycle test and by the whole
package compiling under both `make test` and `go vet ./...`. The mayor verified
that this skip hides nothing changed by the diff and granted the narrowly
scoped waiver recorded above.

### Criterion 3a: attributed raw failures

All three failures occurred outside the candidate's two Herdr test files and
match the existing host/Dolt contention tracker `ga-vkhfnj`:

- `TestCompactScriptRealDoltRemotePush` — the external `dolt sql INSERT`
  process was killed after 43.19 seconds.
- `TestSweep_ReapsRealDoltDataDirAfterSIGKILL` — the external `dolt init`
  process was killed after 34.62 seconds.
- `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix` — fixture
  initialization refused a shared Dolt server with nine pending schema
  migrations (`hq` v57 to v66).

The candidate changes only package-local `_test.go` helper wiring under
`internal/runtime/herdr`; it cannot alter the external Dolt binaries, shared
schema state, or process-kill behavior exercised by these failures. It changes
no harness, concurrency, timeout, container image, or resource setting and adds
no test load. There is no failing-path overlap. The same signatures were
recorded on `ga-vkhfnj` before this gate, satisfying clause 3(a)'s mechanism
proof.

`failure_attribution: TestCompactScriptRealDoltRemotePush -> ga-vkhfnj;
TestSweep_ReapsRealDoltDataDirAfterSIGKILL -> ga-vkhfnj;
TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix -> ga-vkhfnj
(all clause 3(a), mechanism proof)`.

## Decision

**Gate PASS.** Cut `deploy/ga-82982d-gate` from the exact reviewed source,
commit this checklist, push it to the configured fork, open a pull request
against `gastownhall/gascity:main`, publish deploy clearance on the exact gated
PR head, and route the merge request to the merge authority. The deployer does
not merge.
