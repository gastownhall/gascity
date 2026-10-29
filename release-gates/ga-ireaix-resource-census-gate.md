# Subprocess census reconciliation release gate

**Verdict:** **PASS**

Evaluated 2026-09-27 for ga-ireaix; build ga-1lt6oc, reviewed by ga-fs7lud.
Remote deployment to gastownhall/gascity through an isolated fork branch.

- Reviewed source: `a85a70655b7af1062d525a4af6f04f52590b9a86` (resolved commit, never a moving branch tip).
- Tested base: `4db5e13bf80143edeffa15f64d844bb6579876dd` (`origin/main`, freshly fetched before publication).
- Canonical prospective merge: `9fc90f527cd7ec67a79ee14e3db6b40b109f4063`.
- Tested merge tree: `c6a9a74130e9b1283b9cd43f15a1dd09a7dd4585`; recomputed clean merge-tree matches.
- Deployment branch: `deploy/ga-ireaix-gate`; publication adds only this gate record to the reviewed source.
- Artifacts: `/var/tmp/ga-ireaix-gate.CtEDjW` (commands, raw shard logs, terminal events, counters, and tracker readbacks).

| # | Result | Evidence |
| --- | --- | --- |
| 1 | PASS | Reviewer ga-fs7lud records PASS on the resolved source above. No review carryover or rebase substitution. |
| 2 | PASS | Source-derived census is 705 subprocess calls in 206 files. Go policy, TOML ledger and generated TESTING.md agree; reported historical 495/135 unchanged. Existing TestRepositoryLedgerMatchesCensusAndDocumentation PASS in both unit-core and integration-packages-core-1-of-4 of the full run, plus independent affected-package run. Generated-documentation drift check PASS. No exemption or resource-generating test added. |
| 3 | PASS | Fresh documented full-scope 40-job run completed. Six unchanged fixture failures are attributed under 3a below; all other tests and required supplemental lanes passed. Counts and skips below. |
| 4 | PASS | Review notes contain zero unresolved HIGH findings. |
| 5 | PASS | Tested merge checkout and deployer worktree were clean before writing this record. This record is the sole publication addition. |
| 6 | PASS | Source cleanly merges into the pinned current main; canonical materialization, full build and full vet PASS on the combined tree. Preflight commit/pulls returned no PR; source is not already shipped. No self-rebase needed. |
| 7 | PASS | Exactly one reviewed implementation commit, three files, +5/-5, one resource-ledger consistency theme. Absolute deploy ancestry scope helper PASS with ga-ireaix/ga-1lt6oc/ga-fs7lud; no stack or unrelated ancestry. |

## Test evidence

`test_cmd`: `make test-local-full-parallel LOCAL_TEST_JOBS=4`, with GOFLAGS=-v and GO_TEST_TIMEOUT=30m, through load-gate-run then isolated-test-run on the canonical merge.

`test_cmd_scope`: **full-suite**. All 40 jobs started and completed: 36 PASS / 4 FAIL jobs; command exit 2.
`test_counts`: **95,886 PASS / 6 FAIL / 330 SKIP** terminal root/subtest executions across repeated unit and integration variants.
Top-level executions: **53,563 PASS / 6 FAIL / 235 SKIP**.
`diff_tests_executed`: **none (no test files in diff)**.
`waiver_ref`: **none**. Attributions are the standing non-diff-owned protocol, not waivers.
`ci_lane_run`: **n/a — no CI configuration change in this diff**.

Additional checks on the same merged tree:

| Command / scope | Result |
| --- | --- |
| `go build ./...`; `go vet ./...` | Both exit 0 |
| `make test-acceptance ACCEPTANCE_TIMEOUT=90m ACCEPTANCE_GO_TEST_FLAGS='-count=1 -v'` (full unfiltered Tier A) | Exit 0; 437 PASS / 0 FAIL / 11 SKIP |
| `make test-acceptance ACCEPTANCE_TIMEOUT=30m ACCEPTANCE_TOPOLOGY_MATRIX=1`, with the correctly quoted/Make-escaped CI selector `TestBeadsInitTopologyMatrix$/^(M1-proxied-local\|M5-legacy-gc-managed)$` (focused required supplement) | Exit 0; 11 PASS / 0 FAIL / 1 SKIP; M1 real/native lifecycle PASS |
| `make test-bd-cli-contract` with checksum-pinned bd 1.0.4 (minimum-supported contract) | Exit 0; 37 PASS / 0 FAIL / 0 SKIP |
| `make test-bd-cli-contract test-bd-conditional-release-contract` with pinned bd 1.3.0 at BD_CURRENT_REF | Exit 0; CLI 37 PASS / 0 FAIL / 0 SKIP; CAS existence/no-skip enforcing target PASS (9.209s) |
| `go test ./internal/testpolicy/resourcecensus/... -count=1 -v` (independent affected-package acceptance) | Exit 0; 228 PASS / 0 FAIL / 0 SKIP; named ledger test PASS6.26s |
| `./scripts/check-generated-docs-drift.sh`; checksum-verified GoReleaser2.18.2 `check` | Both exit 0 |

`policy_lane`: **PASS**, exit0, complete composite:
`make lint-affected fmt-check-changed test-ci-policy check-gomod-replace check-native-dependency-surface check-eventexport-isolation check-core-boundary test-native-doltlite-beads check-docs spec-ci dashboard-ci`.
Lint used golangci-lint2.12.0 with a private on-disk analysis cache and pinned diff base; shared Go cache was untouched. Active `.githooks` ownership verified by `make check-hooks`.

`skip_justification`: Full-run skips are pre-existing helper-only rows; process tests delegated from the unit variant to process/integration variants in this same run; Darwin/root/SSH/provider capability requirements; opt-in live registry/MCP/herdr/tmux/cleanup checks; documented absent fixture/upstream capability rows and known placeholders. None is diff-owned or in resourcecensus; the ledger consistency test actually passed in both full variants. Individual skip names and output contexts are retained in `skip-evidence.json`. Tier A's 11 skips comprise two legacy-gc migration fixtures, the separately exercised opt-in topology matrix, seven selfhost placeholders, and one opt-in live catalog. M5's supplemental skip is the legacy-gc fixture gap explicitly documented in ci.yml; M1 and native lifecycle/safety/shared-server isolation actually ran and passed.

## Attributed failures (criterion 3a)

All six map to **ga-4w6d2r**, opened and created before this run. Its investigation proves the scope-prefix fixture defect independently on untouched main. This run observed:

| Test | Shard | Symptom |
| --- | --- | --- |
| TestGastown_MailArchive | rest-full-1-of-8 | Valid inbox ID `ak-16` rejected |
| TestGastown_PipelineMailChain | rest-full-1-of-8 | Prefix-filtered agent misses ack |
| TestMail_BashAgent | rest-full-2-of-8 | Prefix-filtered agent misses reply; obsolete diagnostic command |
| TestGastown_MailRoundTrip | rest-full-6-of-8 | Prefix-filtered agent misses ack |
| TestGastown_PipelineMailAndWork | rest-full-6-of-8 | Existing work `qq-5` missed |
| TestGastown_PipelineConvoyTracking | rest-full-8-of-8 | Valid convoy `ai-20` rejected |

`failure_attribution`: each test above → ga-4w6d2r; **clause3(a), conclusive mechanism**. Clauses1/2/4 pass: test/integration and test/agents files are unchanged, this opened condition record predates the run and names these tests, and no changed production file is in their package.

Import reach was checked, including the spawned gc executable: integration imports no resourcecensus package; its only text hit is a comment. gc imports it only for doctor LoadLedger and Ledger/Baseline types. LoadLedger parses a scope-local file; owner-liveness reads owner/scope/resource, never the changed bootstrapPolicy audit_baseline call/file values. No production gc caller of Validate or ScanRepository exists, and these fixture cities do not contain the source repository's ledger. Thus changed values cannot affect ID minting, unchanged `^gc-` filters, extractBeadID or replies. The declared census bump is present; this is a conclusive proof, not the inconclusive added-load exception or a same-package attribution.

Typed repeat rule verified live: carrier ga-1lt6oc has gc.fixes_tracker=ga-yihql5. Blocking fix ga-c2atlu has gc.fixes_tracker=ga-4w6d2r, closed with outcome blocked; its resolved commit `66b9671a38491c723479ee56d13fc3cb3bfb8732` is not on the pinned main (ancestor rc1). This unlanded cross-fix condition qualifies for attribution. No test rerun, new tracker, waiver, or scope broadening. Sighting comment `536b461b-b829-5825-a684-1b06f3ddedd8` was verified by tracker readback.

## Isolation, load and setup record

Task-local pinned bd/Dolt2.1.7, rootless Podman socket and cached dolthub/dolt-sql-server:2.2.0 confirmed before tests; Ryuk disabled with the paired unreaped-container opt-out. Heavy lanes used load and isolation wrappers; TMPDIR=/var/tmp, default shared Go cache preserved.

| Lane | load_threshold | load_waited_seconds | load_wait_timed_out | load_start | sampled load_max | load_mean |
| --- | --- | --- | --- | --- | --- | --- |
| Full suite r4 | 15 | 0 | 0 | 13.45 | 42.92 | 26.45 |
| Full acceptance r3 | 15 | 870 | 0 | 27.89 | 32.02 | 24.00 |
| Required supplements r4 | 15 | 1801 | 1 | 20.09 | 43.11 | 26.27 |

Sampler read errors0 in all lanes; full97 / acceptance66 / supplements73 samples. Supplement wait timed out boundedly and proceeded as specified; every supplemental test target completed successfully.

Setup failures preserved separately and not counted as source/test failures: initial shared lint cache replayed diagnostics from deleted foreign checkouts (complete private-cache policy rerun passed); first full-sweep launcher lacked its log directory (zero tests ran; fresh full40-job r4 executed); first topology invocation lost its regex quoting through Make (zero tests ran; corrected CI selector r4 passed). Original outputs and corrected-run results remain in the artifact directory.
