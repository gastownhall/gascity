# Release Gate: initialize bd rigs under sqlite cities

Deploy bead: `ga-4i7lkz`
Source bead: `ga-c26emx`
Review bead: `ga-q2ox7o`
Source branch: `builder/ga-c26emx-sqlite-bd-rig`
Release branch: `release/ga-4i7lkz-sqlite-bd-rig`
Reviewed commit: `df2d4024af3dd2069d6d8e70c92f09a43993175a`

`docs/PROJECT_MANIFEST.md` is not present in this checkout, so this gate
uses the deployer release criteria plus the source bead acceptance criteria.

## Gate Summary

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | Review PASS present | PASS | `ga-q2ox7o` is closed with close reason `pass`; reviewer notes include `REVIEW VERDICT: PASS`. |
| 2 | Acceptance criteria met | PASS | The change initializes fresh rig scopes under sqlite-backed cities with a bd-backed server-mode store and pins `BEADS_DIR` for the rig. Regression coverage exercises both the low-level initialization path and `gc rig add` followed by routing metadata persistence. |
| 3 | Tests pass | PASS | `go test ./cmd/gc -run 'TestInitBeadsForDirSqliteCityInitializesRigBdStore|TestDoRigAddSqliteCityCreatesBdBackedRig' -count=1` passed; `make test` passed with `observable go test: PASS`; `go vet ./...` produced no findings; `git diff --check origin/main...HEAD` was clean. |
| 4 | No high-severity review findings open | PASS | Review notes list one INFO finding only; no HIGH findings are open. |
| 5 | Final branch is clean | PASS | Gate evidence is committed as the branch tip; `git status --short --branch` is clean after the gate commit. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree HEAD origin/main` succeeded with tree `0bd0c3cd57a16c0d8ff0c8e30c4c6252007e8171`; no merge conflicts were reported. The branch is 2 behind and 1 ahead of `origin/main`, but merge-clean. |
| 7 | Single feature theme | PASS | Commit set is one reviewed commit touching one subsystem: `cmd/gc` rig/beads initialization and adjacent tests. |

## Acceptance Criteria

| Acceptance criterion | Verdict | Evidence |
|---------------------|---------|----------|
| `gc rig add` on a `provider=sqlite` city creates a working bd-tracked rig or documents the required override. | PASS | `cmd/gc/cmd_rig_test.go` adds `TestDoRigAddSqliteCityCreatesBdBackedRig`, covering rig creation under sqlite city config and subsequent bd-backed rig behavior. |
| The rig dolt is server-mode or `gc` ships `native_embedded`; not the current broken hybrid. | PASS | `cmd/gc/beads_provider_lifecycle.go` now routes non-bd inherited rig scopes to bd-backed rig initialization, avoiding the sqlite-provider hybrid. |
| `gc sling` persists routing metadata on such a rig without a bd-CLI workaround. | PASS | `TestDoRigAddSqliteCityCreatesBdBackedRig` covers metadata persistence through the rig's bd-backed store after creation. |

## Changed Surface

- `cmd/gc/beads_provider_lifecycle.go`
- `cmd/gc/beads_provider_lifecycle_test.go`
- `cmd/gc/cmd_rig_test.go`

## Commands Run

```text
go test ./cmd/gc -run 'TestInitBeadsForDirSqliteCityInitializesRigBdStore|TestDoRigAddSqliteCityCreatesBdBackedRig' -count=1
make test
go vet ./...
git diff --check origin/main...HEAD
git merge-tree --write-tree HEAD origin/main
```
