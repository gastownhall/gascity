# Release Gate: ga-t3a0fv round-2 security revert

- Deploy bead: `ga-qtnd2d`
- Review bead: `ga-t3a0fv`
- Reviewed source: `342e8f901b97dac3714b0acc01ac1f243f0ae9c1`
- Source branch: `builder/ga-t3a0fv` (provenance only)
- Base evaluated: `origin/main@7c817e0640fae801631043005f1d54b17ce3e97c`
- Merge base: `a6341f8b1b13dc677ac3d23a8db1e94092da5896`
- Deploy branch: `deploy/ga-t3a0fv-gate`
- Verdict: **PASS**

This is the security-only round-2 landing accepted by the mayor in
`gm-wisp-op6nw8`. It intentionally does **not** satisfy the original live
named-session mail criterion. That work is formally descoped to P0
`ga-1ycmli`; this gate does not represent it as fixed. The accepted scope is
to remove round 1's unsafe alias-only self-match and retain the distinct-
identity collision boundary.

`docs/PROJECT_MANIFEST.md` is absent, so this checklist uses the seven release
criteria in the active deployer protocol and the CI-path conventions in
`engdocs/contributors/release-gate-criteria-conventions.md`.

## Gate checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | The reviewer independently verified round 2 at the exact reviewed SHA, including build, vet, session, API anti-hijack, and CLI conflict tests. The reviewer escalated only the scope decision; the mayor accepted the verified security revert as a partial resolution in `gm-wisp-op6nw8` and directed that no round 3 be opened. |
| 2 | Acceptance criteria met | **PASS (approved partial)** | The accepted criterion is met: a bare alias-only bead is not promoted to canonical self, mismatched or similar identities remain conflicts, and canonical resolution still uses the established authoritative signals. Original criterion #1—mailing a live named session without workarounds—is explicitly unmet and continues on `ga-1ycmli` (P0, mayor-owned). |
| 3 | Tests pass | **PASS with attributed infrastructure failures** | All five diff-owned tests passed by exact name with 0 skips; the full `internal/session/...` suite, build, vet, acceptance tier A, all 12 `cmd/gc` process shards (including `TestTutorial01`), fast baseline, and synthetic-merge affected static lane passed. The raw 46-job CI-equivalent union recorded 41 PASS / 5 FAIL; every failure and rerun is preserved below. Four failure classes have exact-base or existing tracked attribution. The remaining Dolt serialization failure passed 3/3 in isolation and passed the complete 19-test shard on the current-main synthetic merge, which is the tree the PR will test. `waiver_ref: none`. |
| 4 | No high-severity review findings open | **PASS** | Round 1's high-severity alias-hijack finding is closed by complete removal of the unsafe fallback. The reviewer independently traced the safe canonical matcher and found no residual variant. Unresolved HIGH count for this accepted scope: `0`. |
| 5 | Final branch is clean | **PASS** | The isolated branch started at the exact reviewed SHA with a clean index/worktree. `git diff --check` and `gofmt -l` on both touched Go files produced no output. Hooks resolve through `.githooks`; the gate commit is hook-verified and the final branch status is clean. |
| 6 | Branch diverges cleanly from main | **PASS** | No existing PR targeted the reviewed SHA or isolated branch. `git merge-tree --write-tree origin/main 342e8f901b...` completed without conflict (tree `0524c44059a3dd8437006a2632d5aa96a6d7bb86`). The canonical ancestry-scope guard accepted both candidate commits under `ga-t3a0fv` / `ga-qtnd2d` and found no denylisted path. No self-rebase was permitted or needed because criterion 6 passed. |
| 7 | Single feature theme | **PASS** | The two-file diff is one security-revert theme in named-session lookup: remove unsafe alias-only identity trust and add regression coverage for the restored conflict boundary. No API, configuration, or unrelated behavior is bundled. |

## Diff-owned test evidence

At exact reviewed SHA `342e8f901b`:

- `TestLookupConfiguredNamedSession_AliasOnlyBeadWithoutOtherSignalsConflicts`: PASS
- `TestLookupConfiguredNamedSession_UnrelatedAliasOnlyBeadNotTrustedAsSelf`: PASS
- `TestLookupConfiguredNamedSession_DifferentIdentitySimilarAliasStillConflicts`: PASS
- `TestLookupConfiguredNamedSession_AliasMatchWithMismatchedSessionNameStillConflicts`: PASS
- `TestLookupConfiguredNamedSession_SessionNameConflictReportedOverBareAliasMatch`: PASS

`diff_tests_executed`: `5 PASS / 0 FAIL / 0 SKIP`.

`skip_justification`: not applicable—no diff-owned test skipped.

`waiver_ref`: none required.

## CI-equivalent test evidence

The exact reviewed source ran:

- `go test -count=1 ./internal/session/...`: PASS.
- `go build ./...`: PASS.
- `go vet ./...`: PASS.
- `DOCKER_HOST=... TESTCONTAINERS_RYUK_DISABLED=true LOCAL_TEST_JOBS=2 CMD_GC_PROCESS_TOTAL=12 make test-local-full-parallel`: 46 jobs selected, 41 PASS / 5 FAIL. Raw logs: `/var/tmp/gc-local-tests.Py5Sbj`.
- Fast unit baseline: PASS.
- All 12 `cmd/gc` process shards: PASS, including `TestTutorial01`.
- Product-metrics test hook: PASS.
- The unaffected integration shards other than the failures below: PASS.

The clean synthetic merge of the reviewed source onto current main
(`cec869c862`, tree equivalent for PR evaluation) ran:

- `make test-acceptance`: PASS; tier A completed in 93.817s.
- The complete `integration-rest-full-2-of-8` shard: PASS, all 19 tests in
  151.763s; log
  `/var/tmp/gc-local-tests.Ewy3RI/integration-rest-full-2-of-8.log`.
- CI policy tests, module replacement guard, native dependency guard, event
  export isolation, hosted-service core boundary, and native DoltLite tests:
  PASS.
- `make lint-affected` with a clean isolated golangci cache: PASS, `0 issues`
  across the full reverse-dependent package set.
- `make fmt-check-changed check-docs build`, `./bin/gc version`, and
  `./bin/gc --help`: PASS.

The first two lint invocations used the shared golangci cache and surfaced
diagnostics for already-deleted `/var/tmp/ga-j250d0-gate...` paths. Repeating
the same synthetic-merge command with a task-isolated on-disk golangci cache
produced `0 issues`; no source change was made between those runs.

The K8s conformance step was not run locally because this host does not have
the `GC_K8S_AVAILABLE` CI secret/cluster. The workflow condition skips that
step in the same environment. The change does not touch the K8s provider.

## Failure attribution

The initial union's red results remain red in the raw record:

1. `integration-packages-core-1-of-4` failed
   `TestBdFlagManifestCurrent` because the host's locally patched `bd 1.1.0`
   advertises flags beyond the repository manifest. Exact current
   `origin/main@7c817e0640` reproduced the failure. The complete core shard
   passed with the checksum-pinned official CI `bd 1.1.0`. Tracker:
   `ga-f0uceo`.
2. Runtime-tmux shards 2 and 3 failed
   `TestGetKeyBinding_CapturesDefaultBinding` and its `WithArgs` case because
   host tmux 3.7b returned an empty filtered default key table. Both tests
   reproduced on exact current main. Tracker: `ga-afqddr`.
3. The first rest-full-1 run polluted `TestCleanInstallTutorialPath` stdout
   with circuit-breaker cleanup output. That path is already fixed on current
   main; the focused candidate test passed with the official CI `bd`. A later
   full-shard retry cleared it and instead hit the tracked
   `TestE2E_SuspendResume_City` missing-`citysus.report` flake, which passed in
   the initial union and has prior exact-base A/B evidence. Tracker:
   `ga-yc0e3a`.
4. Rest-full-2 twice observed
   `TestHumaBinary_SessionMessageAsync` leaking a Dolt Error 1213
   serialization conflict as HTTP 500 at suspend. The exact reviewed source
   passed the test 3/3 in isolation; exact current main passed the complete
   shard twice; and the current-main synthetic merge passed the complete
   shard fresh. The candidate-path raw failure is not rewritten as green and
   is tracked separately as `ga-67pslx`.
5. `make test-docker` failed before reaching candidate behavior because this
   host's Podman-backed buildx container driver leaves images only in its
   build cache when the script omits `--load`. Exact current main reproduced
   the same missing-image failure. Tracker: `ga-5icabz`.
6. The first actual push's fast gate timed out after 10 seconds waiting for a
   SQLite child protocol line in
   `TestSQLiteGraphSnapshotSIGKILLAtBoundaries/graph-snapshot-copy-after`.
   The identical fast gate passed immediately beforehand during push dry-run,
   all other packages passed, and the candidate does not touch the SQLite
   store boundary. Tracker: `ga-ap7lpd`; log:
   `/var/tmp/gc-local-tests.VuDbRq/unit-core.log`.
7. The next unchanged push retry passed the SQLite test but missed the
   SIGTERM marker in
   `TestRunBoundedPython3FallbackSendsSigtermBeforeKill`. The immediately
   preceding identical fast run passed that package, all other packages in
   the retry passed, and the candidate does not touch the example Dolt
   fallback. Tracker: `ga-o67333`; log:
   `/var/tmp/gc-local-tests.IZcfJG/unit-core.log`.

None of these failures overlaps the two changed files or the alias-only trust
logic. The final integrated tree passes the candidate-owned tests, required
process coverage, acceptance suite, and the only non-attributable full-shard
failure from the old reviewed tree.
