**Verdict:** **PASS**

# Release gate: explicit session restarts

- Deploy bead: `ga-rjz1g8`
- Feature bead: `ga-kadams`
- Review bead: `ga-bchkwl`
- Reviewed commit: `ce010f37948618675e66ee91850f408b40e2f2b4`
- Base: `origin/main@7a34e556ebb8bac29f1ff74d1f3848c1ac7314f7`
- Deploy mode: `remote`; push remote: `fork`
- Evaluated: 2026-09-16 PDT / 2026-09-17 UTC
- Criteria source: authoritative deployer release-gate criteria fragment. The older prompt path `docs/PROJECT_MANIFEST.md` is absent from both base and candidate.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Round-2 review `ga-bchkwl` records `ROUND 2 REVIEW VERDICT: PASS` for the exact reviewed commit. The prior `ga-kh3zsq` census-ledger finding was fixed and independently re-reviewed. |
| 2 | Acceptance criteria met | **PASS** | The CLI now persists the restart request, signals the city orchestrator for an immediate reconciliation tick, and returns without waiting for the periodic patrol. Stop/start judgment stays in reconciliation; the CLI introduces no direct stop, kill, start, or relaunch action. `TestExplicitRestartCommandsPokeControllerWithoutPeriodicTick`, `TestExplicitRestartControllerUnavailableIsTruthfulAndDurable`, `TestExplicitRestartSignalFailureIsTruthfulAndDurable`, `TestExplicitRestartRepeatedRequestIsIdempotentAndRepokes`, and `TestExplicitRestartAlreadyStoppedStillPersistsAndPokes` all executed in the successful `cmd/gc` shards. No production role-name branch or new production polling loop was added. Existing handoff, drain, periodic reconciliation, remote-handoff, and named-session coverage passed. The resource census and generated CLI reference are synchronized. |
| 3 | Tests pass | **PASS** (one attributed non-diff-owned failure) | `test_cmd: make test-local-full-parallel`; `test_cmd_scope: full-suite`; invoked through `isolated-test-run.sh` with the rootless Podman socket active and Ryuk disabled. `test_counts: 39 job PASS, 1 job FAIL, 0 job SKIP` across 40 jobs. The sole raw failure was `internal/runtime/herdr.TestProviderLiveClaudeKindPath`, attributed below under criterion 3a. No `TRIPWIRE` occurred. Logs: `/var/tmp/gc-deploy-ga-rjz1g8.Q7wsra`. `waiver_ref: none`. `ci_lane_run: n/a (no CI configuration change)`. |
| 3a | Pre-existing failures may be attributed | **PASS** | `failure_attribution: TestProviderLiveClaudeKindPath -> ga-iepsvr | clause 3(a) MECHANISM`. The candidate has no `internal/runtime/herdr` path overlap. `go list -deps -test ./internal/runtime/herdr` contains neither changed Go package (`cmd/gc` nor `internal/testpolicy/resourcecensus`), so the failing package cannot reach the candidate's production changes. The exact `agent_pane_busy` condition predates this run; this occurrence was appended and verified as tracker comment 20. |
| 3b | Policy/lint lane | **PASS** (attributed baseline lint findings) | `make test-ci-policy`: PASS (5 + 15 Python policy tests and all Go policy packages). `make vet`: PASS. `make build`: PASS. `make check-hooks`: PASS. Full `make lint` reported only non-diff-owned baseline/environment findings: deleted sibling-worktree cache diagnostics are attributed to `ga-039od0` by mechanism/no-overlap proof; unchanged generated API client and ignored dashboard `node_modules` findings are attributed to `ga-tcdrnz` under the same-run tracker escape after proving the tracked inputs and lint configuration byte-identical to base. No candidate path overlaps either condition. |
| 4 | No high-severity review findings open | **PASS** | Zero unresolved HIGH findings. The only prior blocking finding was the diff-owned census ledger mismatch in `ga-kh3zsq`; the candidate banks the reduction and `ga-bchkwl` verifies it resolved. |
| 5 | Final branch is clean | **PASS** | `git status --short` was empty after the full gate, docs regeneration, build, and vet. This checklist was then added as the only deploy-only change. |
| 6 | Branch diverges cleanly from main | **PASS** | `origin/main@7a34e556ebb8bac29f1ff74d1f3848c1ac7314f7` is the exact merge base and a direct ancestor of the reviewed commit. `git merge-tree --write-tree origin/main HEAD` exited 0. No self-rebase was needed. Preflight found no PR carrying the reviewed commit and no existing PR for `builder/ga-kadams`. |
| 7 | Single feature theme | **PASS** | The three-commit range is one explicit-session-restart feature: RED tests, implementation/test updates, generated CLI reference, and the resource-census baseline reduction caused by removal of obsolete polling sleeps. No independently shippable second feature is present. |

## Test evidence details

- Full-suite job results: 39 PASS, 1 attributed FAIL, 0 SKIP.
- All six `cmd-gc` process shards and all six integration-package `cmd/gc` shards passed.
- Diff-owned test files executed successfully: `cmd_handoff_test.go`, `cmd_runtime_drain_test.go`, `drain_ack_release_test.go`, `explicit_restart_signal_test.go`, and `repro_ga_rble9w_test.go`.
- `diff_tests_executed`: the five newly added `TestExplicitRestart*` tests listed in criterion 2 PASS; every retained top-level test in the four modified existing test files PASS through the successful sharded package run. Five obsolete wait/poll tests were deleted because production no longer blocks on restart completion.
- `make check-docs`: PASS.
- `go run ./cmd/genschema`: PASS with no generated-reference diff.
- `go vet ./...`: PASS.
- `make build`: PASS.
- `policy_lane`: `make test-ci-policy` PASS; full lint raw findings attributed to `ga-039od0` and `ga-tcdrnz` as recorded above.

## Acceptance evidence

- Prompt progress: both explicit restart entry points call `pokeControllerForRestart` after durable request persistence and return without waiting for a patrol tick.
- Centralized safety decisions: restart eligibility and actual stop/start remain in the established session/reconciliation path; the new tests structurally reject direct lifecycle side effects.
- Failure truthfulness: a signal failure returns an error while leaving the durable restart request available to the next reconciliation tick.
- Attribution: the existing `session.draining` event retains the target session as actor and subject; controller command errors identify the city-scoped signaling step.
- Resource accounting: the removed sleeps lower the checked fixed-sleep baselines to 319 calls / 120 files (untagged) and 482 / 172 (all tracked), with TOML, Go policy, and `TESTING.md` in sync.
