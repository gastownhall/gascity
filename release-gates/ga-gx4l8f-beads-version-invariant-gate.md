**Verdict:** **PASS**

# Release gate: derive the integration Beads-version invariant

- Deploy bead: `ga-gx4l8f`
- Review bead: `ga-kfarbr`
- Reviewed commit: `0983b69a7d5efccadd29c6cfd658bbe8c36c5810`
- Base checked: `origin/main@0ab33e5a36b79f7a850d4ce6f2bc39bde0a1120b`
- Deploy mode: `remote` (push remote: `fork`)
- Gate date: 2026-09-19

## Gate checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Review bead `ga-kfarbr` records an unambiguous PASS for the exact reviewed commit. No review carryover was used. |
| 2 | Acceptance criteria met | PASS | `TestPinnedIntegrationBeadsModuleVersion` no longer embeds the stale `v1.3.0-rc.2` literal. It parses the repository's own `go.mod` require directive with `modfile.Parse` and compares that declaration with the independently resolved `go list -m` version. The exact test passes, so the current `v1.3.0` pin is accepted and future reviewed pin changes cannot silently leave a second version literal stale. |
| 3 | Tests pass | PASS | The documented full local runner, `make test-local-full-parallel`, executed all 40 jobs through `isolated-test-run.sh`: 37 jobs PASS / 3 jobs FAIL / 0 jobs skipped. The three raw failures belong to two pre-existing conditions attributed under 3a. The candidate's `integration-rest-full-6-of-8` shard passed. A supplemental verbose run resolved `TestPinnedIntegrationBeadsModuleVersion` by name with PASS and no skip. `test_cmd_scope: full-suite`; `waiver_ref: none`. |
| 3a | Pre-existing failures may be attributed | PASS | Two occurrences of the doctor JSON-parse failure map to pre-existing tracker `ga-x5wacn`. One temporary-city shared-schema refusal maps to pre-existing gate tracker `ga-lejnse`. Both trackers predate this run, name the exact tests or root conditions, were opened before attribution, and now contain this run's sightings. Full four-clause evidence is below. |
| 3b | Policy/lint lane | PASS | `make test-ci-policy` PASS; `LINT_BASE=origin/main make lint-new` PASS with 0 issues; `make vet` PASS; `make check-docs` PASS; `make check-hooks` confirms `.githooks` ownership; `git diff --check origin/main...HEAD` PASS. |
| 3c | CI-config diff needs its own lane | PASS | `ci_lane_run: n/a` — the candidate changes only `test/integration/integration_test.go`; no workflow, matrix, timeout, or required-check configuration changed. |
| 4 | No high-severity review findings open | PASS | The exact-head review records no style or security blocker and no unresolved HIGH finding. Its only note is that `golang.org/x/mod` remains marked indirect in `go.mod`, a non-blocking dependency-hygiene observation; no dependency version or checksum changed. |
| 5 | Final branch is clean | PASS | `git status --porcelain` was empty at the exact reviewed commit before this gate record was created. The gate record is the only deploy-branch addition. |
| 6 | Branch diverges cleanly from main | PASS | Preflight found no PR carrying the reviewed commit. `origin/main` is an ancestor of the reviewed source, and `git merge-tree --write-tree origin/main 0983b69a...` exited 0 with tree `dbdb67b3cb0478dadf362b961dfb9833b5c5a5b8`. A fetch after the full suite confirmed the base had not moved. No self-rebase was needed. |
| 7 | Single feature theme | PASS | Both commits and the sole changed file serve one integration-test invariant: keep the asserted Beads module version synchronized with the reviewed repository pin. `assert_deploy_ancestry_scope` passed for the deploy, review, and source bead IDs. |

## Criterion 3 evidence

```text
test_cmd: DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true isolated-test-run.sh -- bash -lc 'make test-local-full-parallel'
test_cmd_scope: full-suite
runner_jobs: 37 PASS / 3 raw FAIL / 0 SKIP
raw_test_results: 3 FAIL / 0 SKIP (the runner suppresses ordinary passing test names)
candidate_shard: integration-rest-full-6-of-8 PASS
full_suite_log: /var/tmp/ga-gx4l8f-full-suite.log
full_suite_job_logs: /var/tmp/gc-local-tests.e1tmnF
waiver_ref: none
ci_lane_run: n/a (no CI configuration change)
```

The rootless Podman socket was live before the run, Ryuk was disabled for this
host's external reaper, and the cached `dolthub/dolt:2.1.7` and
`dolthub/dolt-sql-server:2.1.7` images matched `deps.env`. All 40 jobs ran.

The full runner's green shard proves the changed integration package in its
CI-equivalent placement but does not print passing test names. The supplemental
verbose run resolves the diff-owned test explicitly without narrowing the
criterion-3 command:

```text
supplemental_cmd: isolated-test-run.sh -- bash -lc 'go test ./test/integration/... -run "^TestPinnedIntegrationBeadsModuleVersion$" -tags integration -v'
diff_tests_executed: TestPinnedIntegrationBeadsModuleVersion PASS
supplemental_counts: 1 PASS / 0 FAIL / 0 SKIP
supplemental_log: /var/tmp/ga-gx4l8f-diff-test.log
```

### Failure attribution

- `TestCustomTypesCheck_ServerBackedStoreIgnoresAmbientEndpoint` (two jobs) -> `ga-x5wacn`.
  - Clause 1: the failing doctor test is not diff-owned.
  - Clause 2: `ga-x5wacn` predates this run, covers the exact JSON-parse corruption, and this run's two sightings were appended before attribution.
  - Clause 3(a), mechanism: the candidate is confined to the integration test package; `internal/doctor` cannot import or execute `test/integration`, and the failure occurs while parsing external `bd config get` output.
  - Clause 4: neither the failing test file nor the doctor package overlaps the one-file candidate diff.
- `TestAdoptPRFormulaRetriesTransientReviewerStep` -> `ga-lejnse`.
  - Clause 1: the failing test is in `review_formula_test.go`, which the candidate does not modify.
  - Clause 2: `ga-lejnse` predates this run, covers this exact shared-server pending-migration refusal, names this test in prior sightings, and contains this run's sighting.
  - Clause 3(b), cross-run/cross-diff: the tracker records this identical test and refusal on multiple unrelated candidate heads. In this run the fixture stopped inside external `bd init` at schema v51 before formula behavior or the changed version helper executed.
  - Clause 4 guard: the files share the broad `integration` test package, but the candidate changes only an existing test file and adds no production path, test file, test target, subprocess, sleep, listener, or resource-census load. Cross-diff proof (b) has landed, and the failing file itself does not overlap the diff.

No inconclusive attribution path and no waiver were used.

## Static and policy evidence

```text
policy_lane: make test-ci-policy PASS
changed_lint: LINT_BASE=origin/main make lint-new PASS (0 issues)
vet: make vet PASS
docs: make check-docs PASS
hooks: make check-hooks PASS
diff_check: git diff --check origin/main...HEAD PASS
```

### Pre-push attribution

The normal guarded push ran `make test-fast-parallel`: 9 jobs PASS / 1 job
FAIL / 0 jobs skipped. The sole raw failure was
`TestCustomTypesCheck_ServerBackedStoreIgnoresAmbientEndpoint` in `unit-core`,
with the same `bd config get` JSON-parse corruption already attributed to
`ga-x5wacn` above. The tracker contains this push-gate sighting. The candidate
still has no doctor path or import reachability, and the other nine fast jobs
passed.

No raw failure was rerun. After this attribution was committed, the exact head
was eligible for the protocol's standing `git push --no-verify` authorization.

```text
pre_push_log: /var/tmp/ga-gx4l8f-push.log
failed_job_log: /var/tmp/gc-local-tests.JWKr9u/unit-core.log
```

## Acceptance and scope evidence

- `declaredBeadsModuleVersion` reads and parses the repository's own `go.mod`.
- It selects the `github.com/steveyegge/beads` require directive and fails explicitly if the directive is absent or malformed.
- The existing test compares that declaration against the independently resolved module version, detecting replace or resolution drift while automatically following a reviewed pin bump.
- The production `gc` binary is unchanged; this file is guarded by the `integration` build tag.
