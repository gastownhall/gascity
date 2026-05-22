# Release Gate: gc doctor stale local pack dir warning

Bead: ga-s5j6n
Source bead: ga-371q.8
Branch: builder/ga-371q-8
Commit under review: current PR HEAD after maintainer fixups. This file was
refreshed in the maintainer fixup commit, so use `git show --name-status HEAD`
for the exact reviewed tree.

This gate began as author-provided release evidence. The PR-review workflow
supersedes it: final approval requires a fresh synthesis and quality scorecard.

## Gate Checklist

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review evidence current | PENDING | Attempt 1 requested changes; maintainer fixups are local and require a fresh review pass. |
| 2 | Acceptance criteria met | PASS | `gc doctor` registers the stale local pack dir check; warning-only behavior, operator action text, configured clean state, unconfigured local-dir state, city-level remote imports, rig-level remote imports, root default-rig remote imports, duplicate binding dedupe, and stat-error continuation are covered by tests. |
| 3 | Validation evidence current | PASS | Current-tree targeted tests, `git diff --check`, `go vet ./...`, and `make test` passed after the maintainer fixups. |
| 4 | No high-severity review findings open | PENDING | Attempt 1's major import-surface finding is fixed locally; the workflow must rerun synthesis before this can become PASS. |
| 5 | Final branch is clean | PENDING | The stale claim that this gate was the final branch change is removed. The apply-fixes workflow verifies `git show --name-status HEAD` and `git status --short` after the maintainer fixup commit. |
| 6 | Branch diverges cleanly from main | NOT RERUN | Merge-tree was not rerun during this maintainer fixup. |

## Acceptance Evidence

- Changed files: `cmd/gc/cmd_doctor.go`, `cmd/gc/cmd_doctor_test.go`, `internal/config/compose.go`, `internal/config/compose_test.go`, `internal/doctor/stale_local_pack_dir_check.go`, `internal/doctor/stale_local_pack_dir_check_test.go`, `release-gates/gc-doctor-stale-local-pack-dir-gate.md`.
- The check fires only when a configured remote pack binding has a same-named local `packs/<binding>/` directory.
- City-level, rig-level, and root default-rig remote imports are inspected.
- If legacy `[packs.<binding>]` and PackV2 `[imports.<binding>]` point at the same local `packs/<binding>/` directory, the warning counts one physical stale directory and preserves both config references in details.
- The result is a warning, not a fixable error, and the operator action tells users to delete the stale local directory and route edits through the remote pack repository.

## Test Evidence

- `git diff --check`: PASS.
- `go test ./internal/doctor -run 'TestStaleLocalPackDirCheck'`: PASS.
- `go test ./internal/config -run 'TestLoadWithIncludes_RootPackDefaultRigImportsPreserveOrder'`: PASS.
- `go test ./cmd/gc -run 'TestDoDoctorRegistersStaleLocalPackDirCheck'`: PASS.
- `go test ./internal/doctor ./internal/config ./cmd/gc -run 'StaleLocalPackDir|TestDoDoctorRegistersStaleLocalPackDirCheck|RootPackDefaultRigImportsPreserveOrder'`: PASS.
- `go vet ./...`: PASS.
- `make test`: PASS.
