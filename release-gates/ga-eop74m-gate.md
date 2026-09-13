# Release Gate: ga-eop74m

Generated: 2026-05-15T12:47:50-07:00

## Scope

- Deploy bead: `ga-eop74m` - Review: L1-authoritative project identity reconcile
- Source feature bead: `ga-h0pyln` - L1-authoritative reconcile flow + endpoint validator
- Parent design bead: `ga-a75ro`
- Release branch: `release/ga-eop74m`
- Source branch: `origin/builder/ga-h0pyln-2`
- Source commit: `e9c226da3`
- Base checked: `origin/main`

`docs/PROJECT_MANIFEST.md` is not present in this repository at this branch. This gate applies the deployer release-gate criteria plus the acceptance criteria from `ga-h0pyln` and the refined parent design in `ga-a75ro`.

## Gate Criteria

| # | Criterion | Status | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | `ga-eop74m` notes contain `REVIEW VERDICT: PASS` from `gascity/reviewer`, naming commit `e9c226da3` on `builder/ga-h0pyln-2`. |
| 2 | Acceptance criteria met | PASS | Acceptance trace below. The apparent `gc bd doctor --reseed-identity` mismatch is stale in the build bead; `ga-a75ro` section 7.1 explicitly says that command does not exist yet and replaces it with manual-resolution wording for this child. |
| 3 | Tests pass | PASS | Focused `cmd/gc` identity tests passed; `internal/beads/contract` passed; `go vet ./...` passed; `make test-fast-parallel` passed after clearing stale `/tmp` quota and rerunning with short `TMPDIR=/tmp`. |
| 4 | No high-severity review findings open | PASS | Reviewer notes for `ga-eop74m` report PASS, "No security concerns", and no request-changes/high-severity findings. |
| 5 | Final branch is clean | PASS | `git status --short --branch` returned only `## release/ga-eop74m...origin/builder/ga-h0pyln-2` before writing this gate file. Clean status is rechecked after the gate commit. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` exited 0; `git diff --check origin/main...HEAD` exited 0. |

## Acceptance Trace

| Acceptance item | Status | Evidence |
|---|---|---|
| `ensureManagedDoltProjectID` reads L1 first via `contract.ReadProjectIdentity`. | PASS | `cmd/gc/dolt_project_id.go:121` reads L1 before L2/L3. |
| Reconcile table covers all required L1/L2/L3 states. | PASS | `cmd/gc/dolt_project_id_decide_test.go:5` defines `TestReconcileDecisionTable`; rows R1-R14 cover all presence/value combinations. |
| Generate fresh only when L1, L2, and L3 are all absent. | PASS | R14 in `TestReconcileDecisionTable`; generated-path assertions in `cmd/gc/dolt_project_id_test.go` verify `Source == "generated"`, `Layer == "generated"`, and all layers are written. |
| `readCanonicalProjectID` reads L1 first and falls back to L2. | PASS | `cmd/gc/cmd_rig_endpoint.go:594`; tests at `cmd/gc/cmd_rig_endpoint_test.go:1193`, `:1211`, and `:1226`. |
| Mismatch wording follows the refined design and reports both IDs. | PASS | `cmd/gc/dolt_project_id.go:380` and `cmd/gc/cmd_rig_endpoint.go:578` print both canonical/local and database project IDs. `ga-a75ro` section 7.1 explicitly defers `--reseed-identity` to child D. |
| `seedDatabaseProjectID` L3 clobber refusal preserved. | PASS | L1/L3 mismatch paths refuse through `formatL1L3MismatchError`; `TestEnsureManagedDoltProjectIDRefusesL1L3Mismatch` asserts no writes happen. |
| Diff is scoped to intended surfaces. | PASS | `git diff --name-only origin/main...HEAD` lists only `cmd/gc/cmd_rig_endpoint.go`, `cmd/gc/cmd_rig_endpoint_test.go`, `cmd/gc/dolt_project_id.go`, `cmd/gc/dolt_project_id_decide_test.go`, and `cmd/gc/dolt_project_id_test.go`. |
| ZFC and role-name constraints. | PASS | Diff is identity reconciliation and endpoint validation code; no role names or SDK role behavior added. |

## Test Evidence

| Command | Result |
|---|---|
| `env -u GC_AGENT -u GC_ALIAS -u GC_TEMPLATE GOCACHE=/home/jaword/.gotmp/gc-deploy-go-cache GOTMPDIR=/home/jaword/.gotmp/gc-deploy-go-tmp TMPDIR=/home/jaword/.gotmp/gc-deploy-go-tmp go test ./cmd/gc -run 'Test(EnsureManagedDoltProjectID|ReconcileDecision|RigEndpoint|ReadCanonicalProjectID).*' -count=1` | PASS: `ok github.com/gastownhall/gascity/cmd/gc 0.024s` |
| `env -u GC_AGENT -u GC_ALIAS -u GC_TEMPLATE GOCACHE=/home/jaword/.gotmp/gc-deploy-go-cache GOTMPDIR=/home/jaword/.gotmp/gc-deploy-go-tmp TMPDIR=/home/jaword/.gotmp/gc-deploy-go-tmp go test ./internal/beads/contract -count=1` | PASS: `ok github.com/gastownhall/gascity/internal/beads/contract 2.129s` |
| `env -u GC_AGENT -u GC_ALIAS -u GC_TEMPLATE GOCACHE=/home/jaword/.gotmp/gc-deploy-go-cache GOTMPDIR=/home/jaword/.gotmp/gc-deploy-go-tmp TMPDIR=/home/jaword/.gotmp/gc-deploy-go-tmp go vet ./...` | PASS |
| `env -u GC_AGENT -u GC_ALIAS -u GC_TEMPLATE GOCACHE=/home/jaword/.gotmp/gc-deploy-go-cache GOTMPDIR=/home/jaword/.gotmp/gc-deploy-go-tmp TMPDIR=/tmp make test-fast-parallel` | PASS: all fast jobs passed |
| `git diff --check origin/main...HEAD` | PASS |
| `git merge-tree --write-tree origin/main HEAD` | PASS |

## Notes

- First `make test-fast-parallel` attempt failed before producing valid gate evidence because `/tmp` was at the user quota limit and the temporary root was too long for `TestStartLongSocketPathUsesShortSocketName`. After removing stale, user-owned test temp directories older than one day and rerunning with `TMPDIR=/tmp`, all fast jobs passed.
- `ga-ue02fr` / `builder/ga-qku0jy` is stacked on this change and should deploy after this PR merges.
