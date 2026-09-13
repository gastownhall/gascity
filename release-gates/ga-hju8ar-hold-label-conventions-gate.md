# Release Gate: retired hold-label convention doctor check

- Bead: ga-hju8ar
- Source bead: ga-tug8ry.5.1
- Review bead: ga-xikkj2
- Feature branch: gc-builder-2-c722b7dbf407
- Candidate commit: 4b492edfa9c77cbba0843047dfd5a6c0f2fc5a86
- Base: origin/main d1b7c04262e44a4eaef160feafb6c74675991022
- Evaluated: 2026-07-16T12:19:34Z

Note: ga-hju8ar metadata.commit still lists dbd070c6d49ca1d2c4e6226f3ebb874419f2bbb1, but the remote branch tip is 4b492edfa9c77cbba0843047dfd5a6c0f2fc5a86. This gate evaluates the branch tip that will be pushed and opened as the PR head.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 6 | Branch diverges cleanly from main | PASS | `git rev-list --left-right --count origin/main...HEAD` returned `0 1`; merge-base is `origin/main` d1b7c04262e44a4eaef160feafb6c74675991022; `git merge-tree --write-tree origin/main HEAD` completed successfully. |
| 1 | Review PASS present | PASS | Review bead ga-xikkj2 is closed and its notes begin with `REVIEW: PASS`. |
| 2 | Acceptance criteria met | PASS | The diff implements the retired-label doctor check, exact-match retired-label queries, closed-bead exclusion, store-error warning behavior, and city plus per-rig registration. Tests cover all seven acceptance criteria from ga-tug8ry.5.1. |
| 3 | Tests pass | PASS | `HOME=/home/jaword LOCAL_TEST_JOBS=16 make test-fast-parallel` completed with all 8 fast jobs passed. `HOME=/home/jaword go vet ./...` completed with no output. |
| 4 | No high-severity review findings open | PASS | Review notes include OWASP/security review with no findings; search found no HIGH or request-changes finding in ga-xikkj2 notes. |
| 5 | Final branch is clean | PASS | Before writing this gate file, `git status --short` was empty and `git diff --check origin/main...HEAD` was clean. This gate file is committed as the only deployer change. |
| 7 | Single feature theme | PASS | Commit set is one commit touching only the `gc doctor` check registration, hold-label-convention check implementation, its tests, and the doctor check-name golden fixture. |

## Diff Scope

```text
cmd/gc/cmd_doctor.go
cmd/gc/doctor_hold_label_conventions.go
cmd/gc/doctor_hold_label_conventions_test.go
cmd/gc/testdata/doctor_check_names.golden
```

## Commit Set

```text
4b492edfa feat(doctor): flag retired hold/blocked labels (ga-tug8ry.5.1)
```
