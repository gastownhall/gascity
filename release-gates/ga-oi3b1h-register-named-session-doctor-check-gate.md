# Release gate: ga-oi3b1h

**Bead:** ga-oi3b1h - needs-deploy: register named session doctor check
**Source review bead:** ga-lxpw04
**Branch:** `builder/ga-ihrikr.2-register-named-session-doctor`
**Code HEAD before gate:** `2f4aa7371f019d5003d3086023f62098ed8cb549`
**Base:** `origin/main` at `fa150384f`
**Stack note:** depends on open base PR #2762 (`builder/ga-ihrikr.1-named-session-doctor`)
**Verdict:** **PASS**

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `ga-lxpw04` is closed with reviewer PASS from `gascity/reviewer`; deploy bead records reviewed + PASSED evidence. |
| 2 | Acceptance criteria met | PASS | `buildDoctorChecks` now registers `NamedAlwaysMinConflictCheck`, the check remains absent when city config is unavailable, and the doctor check-name golden file is updated. |
| 3 | Tests pass | PASS | See "Test runs" below. |
| 4 | No high-severity review findings open | PASS | Review notes report no findings; unresolved HIGH count is 0. |
| 5 | Final branch is clean | PASS | `git status --short --branch` is clean before writing this gate file. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` exits 0 and writes tree `de41f0f589e997d4e2dda476d823ca680e52015a`. |
| 7 | Single feature theme | PASS | The PR diff is limited to named-session doctor conflict detection and its registration tests/golden file. |

## Test Runs

```
$ go test ./internal/doctor -run TestNamedAlwaysMinConflictCheck -count=1
ok  	github.com/gastownhall/gascity/internal/doctor	0.005s

$ go test ./cmd/gc -run 'TestBuildDoctorChecks_(NameSetUnchanged|RegistersNamedAlwaysMinConflictCheck|SkipsNamedAlwaysMinConflictCheckWithoutConfig)' -count=1
ok  	github.com/gastownhall/gascity/cmd/gc	1.061s

$ go vet ./...
(clean)

$ make test-fast-parallel
All fast jobs passed
```

## Diff Scope

GitHub PR diff (`origin/main...HEAD`) is six files:

```
cmd/gc/cmd_doctor.go
cmd/gc/cmd_doctor_extract_test.go
cmd/gc/testdata/doctor_check_names.golden
internal/doctor/checks_named_session.go
internal/doctor/checks_named_session_test.go
internal/doctor/warmup_eligible.go
```

The first three files are the registration slice for this deploy bead. The
last three are the open base PR #2762, which must merge first.
