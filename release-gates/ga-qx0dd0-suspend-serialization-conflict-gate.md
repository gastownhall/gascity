# Release gate: suspend serialization conflicts return 503

- Deploy bead: `ga-qx0dd0`
- Build bead: `ga-4q87pe`
- Review bead: `ga-47aqz6`
- Reviewed commit: `cf9e8468e522dd584a6188e901ae99b9679e5e51`
- Base: `origin/main@4f4a37b28c9cfaa1ebe1c587576b69663a47f078`
- Deploy mode: remote
- Gate verdict: **PASS**

| # | Criterion | Result | Evidence |
| --- | --- | --- | --- |
| 1 | Review PASS present | **PASS** | `ga-47aqz6` records `REVIEW (gascity/reviewer): PASS` for the exact reviewed commit. |
| 2 | Acceptance criteria met | **PASS** | The existing delete-conflict classifier is generalized once as `isRetryableStoreConflict`; `humaStoreError` maps retry-exhausted `1213`/`40001`/serialization conflicts to the declared `apierr.StoreUnavailable` 503 response; the new unit test proves the suspension write is reached and rejects both 500 and any status other than 503; the Huma binary path tolerates only bounded 503 retries; the checked fixed-sleep ledger is updated by exactly one call. |
| 3 | Tests pass | **PASS** | Diff-owned tests passed by name with 0 skips. The documented full union completed 35/40 jobs PASS, 5/40 jobs FAIL, 0 tests SKIP; every failure is independently attributed below, and the two beads#4566 failures are preserved as **FAIL — WAIVED** under the pre-existing mayor authorization. No candidate-owned failure remains. |
| 4 | No unresolved HIGH findings | **PASS** | Reviewer recorded no security findings and no open HIGH finding. |
| 5 | Final branch clean | **PASS** | `git status --short` was empty at the reviewed commit after every generated/dashboard check. |
| 6 | Branch diverges cleanly from main | **PASS** | `git merge-tree --write-tree origin/main cf9e8468e522dd584a6188e901ae99b9679e5e51` returned rc 0 and tree `a307dd78e245c342e508edf9682768da76481717`; merge base `7c817e0640fae801631043005f1d54b17ce3e97c`. |
| 7 | Single feature theme | **PASS** | All seven changed files implement or test one behavior: retry-exhausted store serialization conflicts on session suspend are exposed as a retryable, declared HTTP 503 instead of an undeclared 500. |

## Test evidence

- `go test -v -count=1 -run '^TestHandleSessionSuspend_SerializationConflictIsNotUndeclared500$' ./internal/api/...` — PASS; diff-owned test executed, 0 FAIL, 0 SKIP.
- `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true go test -tags integration -timeout 3m -count=1 -run '^TestHumaBinary_SessionMessageAsync$' -v ./test/integration/` — PASS; diff-owned test executed, 0 FAIL, 0 SKIP.
- `go test -v -count=1 ./internal/testpolicy/...` — PASS.
- `make test-ci-policy` — PASS.
- `go vet ./...` — PASS.
- `make dashboard-ci` — PASS, including generated-client/OpenAPI consistency; the built frontend also served successfully through the frontend workspace's Vite preview.
- `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true LOCAL_TEST_JOBS=4 GO_TEST_TIMEOUT=30m make test-local-full-parallel` — 35/40 jobs PASS, 5/40 jobs FAIL, 0 tests SKIP. Candidate logs: `/var/tmp/gc-local-tests.5RgrkX`.
- Exact-base comparison used the same 40-job command at `origin/main@4f4a37b28c9cfaa1ebe1c587576b69663a47f078`; logs: `/var/tmp/gc-local-tests.9SMMVL`.

`diff_tests_executed`:

- `TestHandleSessionSuspend_SerializationConflictIsNotUndeclared500` — PASS.
- `TestHumaBinary_SessionMessageAsync` — PASS.

`policy_lane`: `make test-ci-policy` — PASS.

`waiver_ref`: `ga-6bnc42`, mayor standing authorization dated 2026-08-18, limited to the exact `ga-lpfjhc` / `gastownhall/beads#4566` dirty-table schema-migration signature when the diff cannot reach schema migration or store bootstrap.

## Failure attribution

- `TestBdFlagManifestCurrent` -> `ga-f0uceo`. The exact installed-`bd` manifest skew reproduced on the pinned base in `integration-packages-core-4-of-4.log`. The candidate does not touch `internal/bdflags`.
- `TestGetKeyBinding_CapturesDefaultBinding` and `TestGetKeyBinding_CapturesDefaultBindingWithArgs` -> `ga-afqddr`. Both empty-default-binding failures reproduced on the pinned base in the corresponding tmux shards. The candidate does not touch `internal/runtime/tmux`.
- `TestLegacyCombinedSourceRecoversHotRollbackJournalInPrivateSnapshot` -> `ga-22dskp`. The tracker records the identical load-sensitive failure on an unrelated diff and predates this candidate commit by more than four hours. Focused and four-way concurrent base package probes passed, which does not disprove nondeterminism. Structural evidence is decisive: `go list -deps -test ./internal/storebinding/sqlite` contains none of the changed Go packages, and the diff has no SQLite path overlap.
- `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix` and `TestCleanInstallTutorialPath` -> `ga-lpfjhc`. Both candidate failures carry the exact `gastownhall/beads#4566` pending-schema-migrations dirty-table signature during fixture bootstrap. The same signature reproduced on the pinned base as `TestGraphWorkflowSuccessPath`. The diff cannot alter Dolt schema migration or store bootstrap. Raw results remain **FAIL — WAIVED** under `ga-6bnc42`; the occurrence was logged on `ga-lpfjhc` with the deploy/build ids and test names as required.

No failure is diff-owned, every failure has a tracker, the pre-existing evidence is recorded above, and there is no failing-test path overlap with the candidate.
