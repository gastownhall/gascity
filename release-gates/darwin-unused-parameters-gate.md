**Verdict:** **PASS**

# Release gate: non-Linux Dolt lock stub unused parameters

- Deploy bead: `ga-gbzu9y`
- Feature bead: `ga-yahncw`
- Review bead: `ga-nwny05`
- Reviewed commit: `fc2c5db4f1529199e25768fe0fa513be4556d277`
- Base: `origin/main@cfb3a9cec0237031c49fbacee8849c035657fbf6`
- Deploy mode: `remote`; push remote: `fork`
- Evaluated: 2026-09-17 PDT / 2026-09-17 UTC
- Criteria source: authoritative deployer release-gate criteria fragment and `engdocs/contributors/release-gate-criteria-conventions.md`. The older prompt path `docs/PROJECT_MANIFEST.md` is absent from the current checkout.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-nwny05` is closed with `verdict: pass` for the exact reviewed commit. Its style, security, and specification lanes are clean. |
| 2 | Acceptance criteria met | **PASS** | `cmd/gc/dolt_data_dir_lock_other.go` now declares the non-Linux stub as `managedDoltLockHolderPIDs(_, _ string)`. `GOOS=darwin golangci-lint run --enable-only=revive ./cmd/gc/...` reports `0 issues`, removing both latent `unused-parameter` findings. The function body and fail-closed error are unchanged. The durable cross-platform lint lane is independently tracked by `ga-3m5jrx` and is not bundled into this reviewed one-file fix. |
| 3 | Tests pass | **PASS** | `test_cmd: make test`; `test_cmd_scope: full-suite`; invoked through `isolated-test-run.sh` at the exact reviewed SHA with rootless Podman active, Ryuk disabled, and cached Dolt `2.1.7` matching the repository pin. `test_counts: 547 PASS, 0 FAIL, 219 SKIP`; 45,196 individual run events; no `TRIPWIRE`. The 219 skips comprise 21 packages with no tests and 198 existing fast-tier/environment/optional-backend skips. `make test-cmd-gc-process-parallel` also passed all six process shards plus the product-metrics testhook. `diff_tests_executed: none (no test files in diff)`. `waiver_ref: none`. `ci_lane_run: n/a (no CI configuration change)`. |
| 3a | Pre-existing failures may be attributed | **PASS** | No test failed. The raw `lint-affected` findings under criterion 3b are attributed to existing trackers: deleted sibling-worktree cache diagnostics -> `ga-039od0`, and unchanged generated-client/ignored-dashboard-dependency findings -> `ga-tcdrnz`. Clause 3(a) MECHANISM: those packages and deleted paths cannot import or execute the changed `cmd/gc` stub. Clause 4: no path overlap. Both trackers predate this run; this run's sightings were appended and verified. |
| 3b | Policy/lint lane | **PASS** (attributed baseline lint findings) | Exact Darwin revive: PASS, 0 issues. `make fmt-check`, `make vet`, `make test-ci-policy`, `make check-core-boundary`, `make test-native-doltlite-beads`, and `make check-hooks`: PASS. `make lint-affected` selected the full repository because the reviewed branch predates newer `main` file deletions and reported 11 non-diff-owned findings: two stale-cache diagnostics from a deleted `/var/tmp/ga-82982d-static...` worktree (`ga-039od0`) plus five `gocritic` and one `gofumpt` finding in unchanged generated API code and two `govet` plus one `revive` finding in ignored dashboard `node_modules` (`ga-tcdrnz`). The candidate file itself is clean under the platform-specific oracle. |
| 3c | CI-config diff needs its own lane's run | **PASS** | Not applicable: the diff changes no workflow, matrix, timeout, or required-check configuration. |
| 4 | No high-severity review findings open | **PASS** | Zero unresolved HIGH findings. Security review found no behavior or attack-surface change; style and specification review are clean. |
| 5 | Final branch is clean | **PASS** | `git status --porcelain` was empty after the tests, static checks, and `make build`. This checklist is the sole deploy-only change and is committed below. |
| 6 | Branch diverges cleanly from main | **PASS** | Already-merged preflight found no PR carrying the reviewed SHA. `git merge-tree --write-tree origin/main fc2c5db4f1529199e25768fe0fa513be4556d277` exited 0 and produced merge tree `478a071fefc660b25b79205884fafe107905535d`; no self-rebase was needed. |
| 7 | Single feature theme | **PASS** | The reviewed range contains one commit changing one function signature in one non-Linux stub. It has one theme: remove platform-specific revive failures without changing runtime behavior. |

## Test evidence details

- `make test`: 547 PASS, 0 FAIL, 219 SKIP; 45,196 individual run events; observable log `/var/tmp/gascity-test.jsonl.UYBzjE`.
- Skip justification: 21 package-level no-test results and 198 pre-existing fast-tier, environment, optional-backend, tmux, and platform-gated skips. None is diff-owned. The separate process lane covers the process-backed `cmd/gc` scenarios intentionally gated out of `GC_FAST_UNIT=1`.
- `make test-cmd-gc-process-parallel`: all six `cmd/gc` process shards PASS; product-metrics testhook PASS.
- `diff_tests_executed`: none (no test files in diff).
- `make build`: PASS.
- `go vet ./...`: PASS through `make vet`.
- `policy_lane`: exact Darwin revive PASS; CI policy, formatting, core-boundary, native DoltLite, and hook-ownership checks PASS; unrelated full-lint findings attributed to `ga-039od0` and `ga-tcdrnz`.

## Acceptance evidence

- Before: both names in the grouped non-Linux signature were unused; revive surfaced them one at a time.
- After: both names are blank identifiers, and the exact `GOOS=darwin` revive command returns zero findings.
- Runtime behavior is unchanged: the stub still returns the same explicit unsupported-platform error and performs no lock inspection.
- Scope remains one file and one signature line. The broader cross-platform lint-lane work remains visible on `ga-3m5jrx` rather than being silently folded into this deploy.
