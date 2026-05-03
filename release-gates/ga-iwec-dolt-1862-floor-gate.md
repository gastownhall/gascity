# Release gate - dolt 1.86.2 version floor (ga-iwec + ga-kmb4)

**Verdict:** PASS

Branch: `release/ga-iwec-dolt-1862-floor`
Base: `refs/adopt-pr/ga-uc3d3j/upstream-base` at `936dea150ca8`

Commits present at review input:
- `c4cbec40d` - feat(dolt): require dolt >= 1.86.2 in pack guards (ga-iwec)
- `6defe1dd3` - test(dolt/doctor): cover dolt 1.86.2 version-floor + missing prereqs (ga-kmb4)
- `5e9b00932` - chore: release gate PASS for ga-iwec-dolt-1862-floor
- `da662ea00` - fix: address Dolt floor review findings

Maintainer review-loop fixup in this commit:
- Reject `1.86.2-rc*` and `1.86.2-dev*` Dolt builds in both the shell pack doctor and Go `DoltVersionCheck`.
- Keep final releases with build metadata such as `1.86.2+build.5` accepted.
- Correct `mol-dog-backup` preflight text so it no longer claims framework-enforced `abort_scope` behavior.
- Add shell and Go regression coverage for prerelease/dev versions plus parser edge cases.

Diff vs base after the maintainer fixup: 9 files changed, 580 insertions, 32 deletions.

Changed files:
- `cmd/gc/embed_builtin_packs_test.go`
- `examples/dolt/doctor/check-dolt/run.sh`
- `examples/dolt/doctor_test.go`
- `examples/dolt/formulas/mol-dog-backup.toml`
- `examples/dolt/pack.toml`
- `internal/doctor/checks.go`
- `internal/doctor/checks_test.go`
- `internal/doltversion/doltversion.go`
- `release-gates/ga-iwec-dolt-1862-floor-gate.md`

## Review Beads Bundled In This PR

| Review bead | Reviews | Verdict | Reviewer |
|-------------|---------|---------|----------|
| ga-zguq | ga-iwec (`run.sh` + `mol-dog-backup.toml` + `pack.toml`) | PASS | gascity/reviewer-1 |
| ga-245m | ga-iwec second review pass | PASS | gascity/reviewer-1 |
| ga-57v7 | ga-kmb4 (`examples/dolt/doctor_test.go`) | PASS | gascity/reviewer-1 |

## Criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | Review PASS present | PASS | All three source review beads carry reviewer-1 PASS verdicts. |
| 2 | Acceptance criteria met | PASS | The pack doctor rejects Dolt below `1.86.2`, rejects prerelease/dev builds at the floor, accepts `1.86.2` final and later versions, and reports the upstream `ccf7bde206` context. The backup formula still runs a preflight before `sync`, and the text now matches the actual dependency semantics. |
| 3 | Tests pass | PASS | `git diff --check`; `bash -n examples/dolt/doctor/check-dolt/run.sh`; `go test -run 'TestDoctorCheckVersionFloor|TestDoctorCheckVersionFloorDoesNotRequireVersionSort|TestBuiltinDoltDoctorAllowsAtMinimumVersionWhenProbeSucceeds|TestBuiltinDoltDoctorBoundsVersionProbe|TestDoltVersionCheck|TestParseDoltVersion' -count=1 ./examples/dolt ./cmd/gc ./internal/doctor`; `go test -count=1 ./internal/formula/...`. |
| 4 | No high-severity review findings open | PASS after maintainer fixup | The review-loop fixup addresses the major prerelease acceptance finding and the scorecard-required formula wording, gate refresh, and parser edge coverage. A fresh review pass must confirm this before `review.verdict=done`. |
| 5 | Branch evidence matches current reviewed state | PASS | Base, commit stack, diff summary, changed file list, and validation evidence above reflect the reviewed worktree after the maintainer fixup. |

## Notes

The broader repository suite was not rerun in this review-loop step. The prior scorecard noted unrelated broader failures in environment/config harness areas, so this gate records the scoped checks that cover the changed Dolt floor and formula surfaces.
