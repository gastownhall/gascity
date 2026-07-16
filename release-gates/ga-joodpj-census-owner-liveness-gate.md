# Release Gate: ga-joodpj census-owner-liveness

Date: 2026-07-16

Bead: ga-joodpj

Candidate branch: deploy/ga-joodpj-census-owner-liveness-20260716154637

Source branch: origin/gc-builder-2-census-owner-liveness-recut

Candidate head before gate commit: 62cc8687a36e37dd2e567d3fc8a7e89431b9a834

Base: origin/main at 17c7894c5b5b334462de101b9be57cee2651e074

Release criteria source: deployer prompt. No docs/PROJECT_MANIFEST.md or PROJECT_MANIFEST.md file exists in this checkout.

## Summary

PASS. This is a single-bead deploy for the census-owner-liveness doctor check and periodic alert wrapper. The candidate branch is one commit ahead of origin/main, conflict-free, review-passed, test-passed, and limited to one feature theme.

## Criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 6 | Branch diverges cleanly from main | PASS | Evaluated first and re-checked after the full test run. `git rev-list --left-right --count origin/main...HEAD` returned `0 1`; merge-base is `17c7894c5b5b334462de101b9be57cee2651e074`; `git merge-tree --write-tree origin/main HEAD` produced tree `f41aee5cdddcb49d164ccbd809d289c8c6dbbfd3` with no conflicts. |
| 1 | Review PASS present | PASS | Review bead `ga-06zg1q` is closed with close reason `PASS`; notes contain `Review verdict: PASS` and `Verdict: PASS - routing to deployer`. |
| 2 | Acceptance criteria met | PASS | Original build bead `ga-kr3glv.1` required a doctor check, a cron wrapper script, and an operator order snippet documented outside the repo. Candidate diff contains `cmd/gc/doctor_census_owner_liveness.go`, its tests, doctor registration/warmup/golden updates, and `scripts/check-census-owner-liveness.sh`. No in-repo order file was added, matching the non-goal. |
| 3 | Tests pass | PASS | In scratch worktree `/var/tmp/codex-deployer-ga-joodpj-gate-20260716154637`: `bash -n scripts/check-census-owner-liveness.sh` passed; `shellcheck scripts/check-census-owner-liveness.sh` passed; `go vet ./...` passed; `go test ./cmd/gc/... -run 'CensusOwnerLiveness|WarmupEligible|DoctorChecks_NameSetUnchanged' -count=1` passed; `HOME=$(getent passwd "$(whoami)" \| cut -d: -f6) make test-fast-parallel` passed all 8 fast jobs. |
| 4 | No high-severity review findings open | PASS | Review bead `ga-06zg1q` lists one primary finding already resolved by the builder follow-up before the PASS verdict. Remaining notes are informational/non-blocking; no unresolved HIGH finding is present. |
| 5 | Final branch is clean | PASS | `git status --short --branch` was clean before writing this gate file. The only pending change before commit is this release gate file. |
| 7 | Single feature theme | PASS | `git diff --name-status origin/main...HEAD` touches one subsystem/theme: doctor registration/check/test/golden plus the census-owner-liveness wrapper script. Diff scope is 6 files, 537 insertions, 0 deletions. |

## Diff Scope

```text
M cmd/gc/cmd_doctor.go
A cmd/gc/doctor_census_owner_liveness.go
A cmd/gc/doctor_census_owner_liveness_test.go
M cmd/gc/doctor_warmup_eligible.go
M cmd/gc/testdata/doctor_check_names.golden
A scripts/check-census-owner-liveness.sh
```

## Test Output Summary

```text
bash -n scripts/check-census-owner-liveness.sh
shellcheck scripts/check-census-owner-liveness.sh
go vet ./...
go test ./cmd/gc/... -run 'CensusOwnerLiveness|WarmupEligible|DoctorChecks_NameSetUnchanged' -count=1
ok github.com/gastownhall/gascity/cmd/gc 1.291s
HOME=$(getent passwd "$(whoami)" | cut -d: -f6) make test-fast-parallel
All fast jobs passed
```
