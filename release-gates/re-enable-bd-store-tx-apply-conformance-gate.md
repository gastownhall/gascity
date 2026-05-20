# Release Gate: re-enable bd store tx apply conformance

Bead: ga-rvso5lf
Source bead: ga-so5lf
Workflow root: ga-wisp-e4koeu
Branch: builder/ga-so5lf-1
Original PR head checked by GitHub CI: 55d0d947424d794548563dae89a46fd01a3600de
Local adopted head before maintainer fixup: 02a92dc388

## Gate Checklist

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review findings addressed | LOCAL FIX APPLIED | The review synthesis blocked on bd/Dolt 2.0.3 clobbering fields across separate Tx writes, then iteration 2 found that the combined close path bypassed `bd close --reason`. The latest apply-fixes review also required transient retry coverage for staged Tx applies and label add/remove coverage. The maintainer fixup now stages same-bead writes, routes preserving non-close applies through the same retrying `BdStore.Update` path as direct writes, and finishes every staged `tx.Close` through `bd close --reason`. |
| 2 | Acceptance criteria met | PASS LOCAL | `test/integration/bdstore_test.go` uses the normal conformance path, so `TxRunsCallbackAndAppliesWriteSurface` runs for BdStore. The conformance test now checks that untouched `Title`, staged metadata, `close_reason`, and closed status survive the Tx. |
| 3 | Focused tests pass | PASS LOCAL | `go test ./internal/beads -run 'TestBdStoreTx(RetriesTransientUpdateApply|PreservesAddsAndRemovesLabels|CombinesWritesForSameBead|CloseOnlyUsesCloseCommand)|TestBdStoreSetMetadataBatchRetriesDoltSerializationFailure|TestBdStoreUpdatePassesPriority' -count=1` PASS. `go test -tags integration -run 'TestBdStoreConformance/TxRunsCallbackAndAppliesWriteSurface' -count=1 -timeout 120s ./test/integration` PASS. `go test ./internal/beads -count=1` PASS. |
| 4 | Required GitHub CI | PENDING RERUN | `gh pr view 2371 --repo gastownhall/gascity --json headRefOid,statusCheckRollup` still reports `Integration / bdstore`, `CI / integration`, and `CI / required` as FAILURE on original live PR head `55d0d947424d794548563dae89a46fd01a3600de`. This apply-fixes step is local-only and must not push; finalization must push the maintainer fixup and require a fresh green rollup before merge-ready. |
| 5 | Final branch is clean | PASS LOCAL | After the maintainer fixup commit, `git status --short --branch` reported only the detached `HEAD` line and no modified, staged, or untracked files. |
| 6 | Branch diverges cleanly from main | PASS LOCAL | `git merge-tree $(git merge-base HEAD origin/main) HEAD origin/main` produced no output after the maintainer fixup commit, indicating a clean merge tree with no conflict records. |

## Acceptance Evidence

- PR scope after maintainer fixup: `test/integration/bdstore_test.go`, `internal/beads/bdstore.go`, `internal/beads/bdstore_test.go`, `internal/beads/beadstest/conformance.go`, and this gate artifact.
- `BdStore.Tx` no longer forwards the conformance sequence as three separate bd writes (`update`, `set-metadata`, `close`) for the same bead. It reads the bead once, stages the final state, applies one preserving non-close update for combined writes, and then closes through `bd close --force --json --reason`.
- Staged non-close Tx applies now call `BdStore.Update`, so Dolt serialization failures and other transient bd write errors get the same retry behavior as direct updates.
- Tx label add/remove semantics are covered by `TestBdStoreTxPreservesAddsAndRemovesLabels`, including preserving an untouched existing label while removing the requested one.
- Close-only and combined close transactions both use the existing close-reason path.
- The conformance test now catches clobbering of an untouched field by asserting `Title` remains `"before"` after the Tx, and it stamps a validator-compatible `close_reason` before closing.

## Test Evidence

- `go test ./internal/beads -run 'TestBdStoreTx(RetriesTransientUpdateApply|PreservesAddsAndRemovesLabels|CombinesWritesForSameBead|CloseOnlyUsesCloseCommand)|TestBdStoreSetMetadataBatchRetriesDoltSerializationFailure|TestBdStoreUpdatePassesPriority' -count=1`: PASS.
- `go test -tags integration -run 'TestBdStoreConformance/TxRunsCallbackAndAppliesWriteSurface' -count=1 -timeout 120s ./test/integration`: PASS.
- `go test ./internal/beads -count=1`: PASS.
- `go vet ./internal/beads ./test/integration`: PASS.
- `make test`: PASS.
- `git diff --check`: PASS.
