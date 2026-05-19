# Release Gate: gc doctor stale local pack dir warning

Bead: ga-s5j6n
Source bead: ga-371q.8
Branch: builder/ga-371q-8
Commit under review: 1e9c161a6

## Gate Checklist

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-s5j6n` notes contain `VERDICT: pass`; findings: none. |
| 2 | Acceptance criteria met | PASS | `gc doctor` registers the stale local pack dir check; warning-only behavior, operator action text, configured clean state, and unconfigured local-dir state are covered by tests. |
| 3 | Tests pass | PASS | `go test ./internal/doctor ./cmd/gc -run 'Doctor|PackCache|ConfigRefs|StaleLocalPackDir'` PASS; `go vet ./...` PASS; `make test-fast-parallel` PASS. |
| 4 | No high-severity review findings open | PASS | Review notes list `Findings: none`; unresolved HIGH findings count is 0. |
| 5 | Final branch is clean | PASS | `git status --short` was clean before writing this gate artifact; the gate artifact is committed as the final branch change. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree $(git merge-base HEAD origin/main) HEAD origin/main` completed with no conflicts. |

## Acceptance Evidence

- Changed files: `cmd/gc/cmd_doctor.go`, `cmd/gc/cmd_doctor_test.go`, `internal/doctor/stale_local_pack_dir_check.go`, `internal/doctor/stale_local_pack_dir_check_test.go`.
- The check fires only when a configured remote pack binding has a same-named local `packs/<binding>/` directory.
- The result is a warning, not a fixable error, and the operator action tells users to delete the stale local directory and route edits through the remote pack repository.

## Test Evidence

- `go test ./internal/doctor ./cmd/gc -run 'Doctor|PackCache|ConfigRefs|StaleLocalPackDir'`: PASS.
- `go vet ./...`: PASS.
- `make test-fast-parallel`: PASS.
- `git diff --check origin/main...HEAD`: PASS.
