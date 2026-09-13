# Release Gate: WarmupEligible interface and pack manifest warmup field

- Deploy bead: `ga-87u29d`
- Source bead: `ga-r1iqmy`
- Review bead: `ga-7y3a9a`
- Branch: `builder/ga-r1iqmy-1`
- Remote branch: `fork/builder/ga-r1iqmy-1`
- Reviewed commit: `6c45ac86a feat(doctor): add warmup eligibility contract`
- Note: `docs/PROJECT_MANIFEST.md` is not present in this worktree, so this gate uses the deployer role criteria and the source bead acceptance checklist.

## Gate Criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | `bd show ga-7y3a9a` shows closed review bead with `Reviewer verdict: PASS`. |
| 2 | Acceptance criteria met | PASS | Checked source bead acceptance against code and tests; details below. |
| 3 | Tests pass | PASS | `make test-fast-parallel` completed with `All fast jobs passed`; `go vet ./...` exited 0. |
| 4 | No high-severity review findings open | PASS | Review notes list one LOW/STYLE finding about repetitive doc comments; unresolved HIGH findings count is 0. |
| 5 | Final branch is clean | PASS | Branch was clean before gate creation; deployer rechecked clean status after committing this gate before push. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` exited 0 and produced tree `dec2d8f2c8df5b72c8de9d88557aa76870728759`. |

## Acceptance Evidence

| Source criterion | Result | Evidence |
|---|---|---|
| `doctor.Check` interface has `WarmupEligible() bool` as its last method. | PASS | `internal/doctor/types.go` adds `WarmupEligible() bool` after `Fix(...)`. |
| Every concrete in-tree `Check` implementation has the default-false method with the pinned comment. | PASS | `grep -rn 'func.*CanFix() bool' internal/doctor/ cmd/gc/` enumerated the production and test implementations; `internal/doctor/warmup_eligible.go`, `cmd/gc/doctor_warmup_eligible.go`, and updated test mocks provide the method. |
| `PackScriptCheck.WarmupEligible()` returns `c.Warmup`. | PASS | `internal/doctor/pack_checks.go` implements `func (c *PackScriptCheck) WarmupEligible() bool { return c.Warmup }`. |
| `PackScriptCheck` has new `Warmup bool` field, populated in `cmd/gc/cmd_doctor.go`. | PASS | `internal/doctor/pack_checks.go` adds `Warmup bool`; `cmd/gc/cmd_doctor.go` passes `Warmup: entry.Warmup`. |
| `PackDoctorEntry`, `DiscoveredDoctor`, and `doctorManifest` have `Warmup bool` fields with the TOML tag. | PASS | `internal/config/config.go` adds `Warmup bool` with TOML tag `warmup,omitempty`; `internal/config/doctor_discovery.go` adds `Warmup bool` to `DiscoveredDoctor` and `doctorManifest` with TOML tag `warmup`. |
| `legacyPackDoctors` copies the field through. | PASS | `internal/config/pack.go` copies `Warmup: entry.Warmup`. |
| `TestCheckWarmupEligibleDefaultsFalse` exists with one subtest per concrete check type. | PASS | `internal/doctor/doctor_test.go` includes `TestCheckWarmupEligibleDefaultsFalse`. |
| `TestPackDoctorWarmupFlagParses` exists with explicit true, explicit false, and omitted/default subtests. | PASS | `internal/config/pack_test.go` includes `TestPackDoctorWarmupFlagParses`. |
| `TestDoctorManifestWarmupFieldParses` exists with explicit true, explicit false, and omitted/default subtests. | PASS | `internal/config/doctor_discovery_test.go` includes `TestDoctorManifestWarmupFieldParses`. |
| `TestPackScriptCheckWarmupEligibleReflectsField` exists with default and opted-in subtests. | PASS | `internal/doctor/pack_checks_test.go` includes `TestPackScriptCheckWarmupEligibleReflectsField`. |
| Broad tests pass. | PASS | `make test-fast-parallel` passed. This is the repo-documented broad local fast runner for this codebase. |
| `go vet ./...` clean. | PASS | `go vet ./...` exited 0. |
| No behavioral change to `gc doctor` or `gc start`. | PASS | Diff only adds the opt-in contract, manifest/config plumbing, and tests. No warmup runner or `gc start` behavior is wired in this slice. |

## Reviewer Notes

- New config surface: `warmup = true` on pack `[[doctor]]` entries and `doctor.toml` manifests.
- Default remains false for all existing in-tree checks.
- The only variable in-tree implementation is `PackScriptCheck`, which reflects the pack manifest field.
- Follow-up slices are expected to consume this contract; this PR does not add the warmup runner or alerting behavior.
