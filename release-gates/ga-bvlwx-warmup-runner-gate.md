# Release Gate: RunWarmupChecks runner and WarmupReport types

- Deploy bead: `ga-bvlwx`
- Source bead: `ga-wgsv3t`
- Branch: `builder/ga-wgsv3t-2`
- Remote branch: `fork/builder/ga-wgsv3t-2`
- Reviewed commit: `a9a69da5f feat: add gc start warmup runner`
- Base checked: `origin/main` at `e957692a4426`
- Note: `docs/PROJECT_MANIFEST.md` is not present in this worktree, so this gate uses the deployer role criteria and the source bead acceptance checklist.

## Gate Criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | `bd show ga-bvlwx` notes include `Reviewer verdict: PASS`; reviewer mail `gm-wisp-ddfpa` says `ga-bvlwx passes review`. |
| 2 | Acceptance criteria met | PASS | Checked source bead acceptance against code and tests; details below. |
| 3 | Tests pass | PASS | Focused `go test ./cmd/gc -run 'TestRunWarmupChecks|TestBuildDoctorChecks_NameSetUnchanged|TestDefaultMailProviderUsesStartedCityPath|TestDoctorJSONSuccessIsParseableJSONOnly|TestMailSendSuccess' -count=1` passed; `GC_FAST_UNIT=1 go test ./cmd/gc -count=1 -timeout=5m` passed in 264.798s; `make test-fast-parallel` completed with `All fast jobs passed`; `go vet ./...` exited 0. |
| 4 | No high-severity review findings open | PASS | Review notes list no blocking findings and two informational notes only: the currently unused spec-defined `buildDoctorChecksOpts.Stderr` field, and an idempotent double `resolveRigPaths` call. |
| 5 | Final branch is clean | PASS | Branch was clean before gate creation; deployer will recheck clean status after committing this gate before push. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-base --is-ancestor origin/main fork/builder/ga-wgsv3t-2` exited 0; `git merge-tree --write-tree origin/main HEAD` exited 0 and produced tree `a061172158e91835179e9c162c630e8190174746`. |

## Acceptance Evidence

| Source criterion | Result | Evidence |
|---|---|---|
| `cmd/gc/cmd_start_warmup.go` exists with `WarmupOpts`, `WarmupReport`, `ScopeWarmupResult`, `WarmupCheckResult`, `RunWarmupChecks`, and exported default deadline constants. | PASS | `cmd/gc/cmd_start_warmup.go` defines the requested structs, `RunWarmupChecks`, `DefaultWarmupPerCheckDeadline = 5 * time.Second`, and `DefaultWarmupTotalDeadline = 30 * time.Second`. |
| `buildDoctorChecks` and `buildDoctorChecksOpts` exist in `cmd/gc/cmd_doctor.go`; `doDoctor` calls into them. | PASS | `cmd/gc/cmd_doctor.go` defines both symbols and `doDoctor` iterates over `buildDoctorChecks(...)` before registering checks. |
| `cmd_start.go` calls `RunWarmupChecks` after the `healthBeadsProvider` block; return values discarded. | PASS | `cmd/gc/cmd_start.go` constructs `WarmupOpts{Mailer: defaultMailProvider(cityPath), Stderr: stderr}` immediately after the bead health probe and discards `RunWarmupChecks(...)` return values. |
| All 18 named `TestRunWarmupChecks_*` tests exist and pass. | PASS | `rg '^func TestRunWarmupChecks_' cmd/gc/cmd_start_warmup_test.go` found all 18 named tests; focused `go test` passed. |
| `cmd/gc/testdata/doctor_check_names.golden` is checked in. | PASS | Golden file is present and `TestBuildDoctorChecks_NameSetUnchanged` passed. |
| Broad tests pass and `go vet ./...` is clean. | PASS | `make test-fast-parallel` passed; `go vet ./...` exited 0. |
| `gc doctor` output unchanged for a representative city. | PASS | `TestBuildDoctorChecks_NameSetUnchanged` guards registry order/name drift; manual `GC_DOLT=skip go run ./cmd/gc doctor --json` produced parseable JSON with `ok=true`, `passed=88`, `warned=15`, `failed=2`. The nonzero exit came from pre-existing live-city findings `order-firing-current` and `worktree-disk-size`, not warm-up runner output. |
| No new third-party Go modules. | PASS | `git diff origin/main..HEAD -- go.mod go.sum` is empty. Existing `golang.org/x/sync/errgroup` dependency is reused. |
| No status-file writes from inside `RunWarmupChecks`. | PASS | `cmd/gc/cmd_start_warmup.go` does not use file write/create/rename APIs; filesystem writes in this slice are test setup only. |
| No role-conditioned SDK decision logic in the new runner. | PASS | The runner exposes `WarmupOpts.MailTo`; the default recipient remains the pinned `mayor` routing handle from the source contract, with no role-specific branching or hardcoded role behavior. |

## Smoke Notes

- `GC_DOLT=skip go run ./cmd/gc doctor --json` wrote valid JSON to `/tmp/ga-bvlwx-doctor.json`.
- Scratch doctor stderr only contained the Go wrapper exit status line.
- Live-city doctor failures are environmental and already present in this city: stale order firing and the `MCDClient` worktree size threshold.

## Reviewer Notes

- New behavior is fail-open: `gc start` continues regardless of warm-up check, mail, timeout, panic, or runner failures.
- New runtime surface is limited to warm-up-eligible doctor checks and mail alerting through the existing mail provider.
- Suppression state, all-clear mail, opt-out config, and PG-auth warm-up production are explicitly left for later slices.
