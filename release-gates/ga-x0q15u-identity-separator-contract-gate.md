# Release gate: identity-separator cross-repository contract

- Deploy bead: `ga-x0q15u`
- Build bead: `ga-dn4em5`
- Review bead: `ga-ysihu3`
- Reviewed commit: `7255045800bde96caaed12c61b610f647d204bb9`
- Base: `origin/main@8556a801c380ba9e43c04daa58c969f988021324`
- Merge base: `2cd07e018bf3680d24b037b509e6a4bad5e623ba`
- Deploy mode: remote
- Gate verdict: **PASS**

| # | Criterion | Result | Evidence |
| --- | --- | --- | --- |
| 1 | Review PASS present | **PASS** | `ga-ysihu3` is closed with an exact-SHA PASS for `7255045800bde96caaed12c61b610f647d204bb9`. |
| 2 | Acceptance criteria met | **PASS** | The candidate adds the authoritative contract, reference navigation, exact source citations, a source comment backlink, and contract-presence tests. The `/` and `.` axes stay distinct. The beads-side build record `be-fh0g` explicitly carries this document path in its cross-store handoff. |
| 3 | Tests pass | **PASS** | The documented 40-job full-suite command completed with 45,593 PASS / 7 FAIL / 187 SKIP. Both diff-owned tests passed twice. All six unique failure signatures are non-diff-owned, path-disjoint, covered by trackers that predate this run, and attributable by structural mechanism evidence under criterion 3a. |
| 4 | No unresolved HIGH findings | **PASS** | The exact-SHA reviewer reported no unresolved HIGH-severity findings. Independent policy, docs, build, vet, formatting, and diff-check lanes passed. |
| 5 | Final branch clean | **PASS** | `git status --short` was empty at the reviewed commit after evaluation and before this gate record was written. The gate record is committed as the only deploy-branch addition. |
| 6 | Branch diverges cleanly from main | **PASS** | Pre-flight found no PR carrying the reviewed commit. `git merge-tree --write-tree origin/main 7255045800bde96caaed12c61b610f647d204bb9` returned 0 and tree `83ee3580c20a92998cad316ce52aa81c5014bfb0`. `assert_deploy_ancestry_scope origin/main ... ga-x0q15u ga-dn4em5` returned 0. |
| 7 | Single feature theme | **PASS** | The five changed files are one documentation contract, its navigation wiring, a source backlink, and tests that keep those artifacts connected. |

## Acceptance evidence

- `docs/reference/specs/identity-separator-contract-v1.md` defines `/` to
  `--` and `.` to `__`, keeps the axes distinct, and explains that single
  `-` and `_` characters remain legal inside name segments.
- The source-of-truth citations resolve to the encode/decode tables in
  `internal/agent/session_name.go`; the source now links maintainers back to
  the contract.
- `docs/docs.json` and `docs/reference/specs/index.md` expose the contract in
  reference navigation.
- The two added contract-presence tests both executed and passed in both
  full-suite lanes that loaded `internal/agent`.
- The cross-store beads build `be-fh0g` records
  `docs/reference/specs/identity-separator-contract-v1.md` as the contract to
  cite for the bundled `canonicalActor` / `be-vc51` work.

## Test evidence

- `test_cmd`: `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true GOFLAGS=-v GO_TEST_TIMEOUT=30m make test-local-full-parallel`
- `test_cmd_scope`: `full-suite`
- `test_counts`: 45,593 PASS / 7 FAIL / 187 SKIP (7 failure occurrences
  across 6 unique test names)
- Runner result: all 40 documented jobs completed; 33 jobs were green and 7
  jobs were red. The command exited 2 because raw attributed failures remain
  visible. Raw per-shard logs: `/var/tmp/gc-local-tests.vio2Ga`.
- `diff_tests_executed`:
  - `TestIdentitySeparatorContractDocExists` — PASS in
    `unit-core` and `integration-packages-core-1-of-4`
  - `TestSessionNameCommentReferencesIdentitySeparatorContract` — PASS in
    `unit-core` and `integration-packages-core-1-of-4`
- `waiver_ref`: none
- `skip_justification`: no diff-owned test skipped. The remaining skips are
  pre-existing operating-system gates, opt-in live integrations, unavailable
  capabilities, and pinned dependency prerequisites; none exercises the two
  contract-presence tests added by this diff.
- `failure_attribution`:
  - `TestCatalogMatchesProductionWiringAndDocumentation` (two lanes) ->
    `ga-uz5t3a` | clause 3(a), mechanism: the candidate does not touch
    `internal/testutil/providerledger`; the stale reviewed ancestry sees the
    known 2026-08-26 expiry while current `origin/main` already contains the
    independent-waiver/live-owner repairs.
  - `TestBdFlagManifestCurrent` -> `ga-f0uceo` | clause 3(a), mechanism: the
    installed `bd` command surface and `internal/bdflags` are outside the diff;
    the exact host-binary/manifest skew is the tracker signature.
  - `TestGetKeyBinding_CapturesDefaultBinding` and
    `TestGetKeyBinding_CapturesDefaultBindingWithArgs` -> `ga-afqddr` | clause
    3(a), mechanism: the host tmux 3.7b default-keytable probe is outside the
    diff and returned the tracker's exact empty-binding signature.
  - `TestE2E_SuspendResume_City` -> `ga-yc0e3a` | clause 3(a), mechanism: the
    candidate changes no executable suspend/resume behavior and the test hit
    the tracker's exact missing `citysus.report` timeout.
  - `TestGraphWorkflowSuccessPath` -> `ga-lpfjhc` | clause 3(a), mechanism:
    fixture initialization failed on the exact beads#4566 pending-dirty-table
    schema-migration signature before graph workflow behavior executed.
- Clause 3a checks: none of the six unique failures is diff-owned; every cited
  tracker was opened and predates this run; every failing package/path is
  disjoint from the diff; structural mechanism proof lands for each signature.
- `inconclusive-guard`: not invoked because clause 3(a) mechanism proof is
  conclusive for every failure. The diff has no resource-census change and
  adds no new suite target; its two new tests are in an existing package and
  both passed.

## Independent lanes

- `policy_lane`: `make test-ci-policy` — PASS.
- `make check-docs` — PASS.
- `go build ./...` — PASS.
- `go vet ./...` — PASS.
- `gofmt -d internal/agent/identity_separator_doc_test.go internal/agent/session_name.go` — no output.
- `git diff --check origin/main...7255045800bde96caaed12c61b610f647d204bb9` — PASS.
- `git config core.hooksPath` — `/home/jaword/projects/gascity/.githooks`.

## Disposition

Gate PASS. Cut `deploy/ga-x0q15u-gate` at the exact reviewed commit, commit
this checklist as the only deploy-only change, push that isolated branch, and
open a pull request. Publish deploy clearance on the exact PR head before
routing the merge-request. The deployer does not merge.
