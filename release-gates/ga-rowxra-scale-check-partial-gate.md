# Release Gate: ga-rowxra - partial scale_check create gate

Date: 2026-06-23

Deploy bead: ga-rowxra
Source review bead: ga-0xljyj.1
Branch: builder/ga-0xljyj-scale-check-partial-gate
Reviewed head: 108c4a3e28782aa4e5ac3ae30b4c80f697299cca
Base: origin/main at 32ca47acd639b80eee37f4623d0277018b674c06
Existing PR: https://github.com/gastownhall/gascity/pull/3686

Note: docs/PROJECT_MANIFEST.md is not present in this repo checkout. This gate uses the deployer prompt criteria plus the bead acceptance criteria.

## Gate Summary

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | ga-0xljyj.1 records RE-REVIEW PASS by reviewer-gm-wisp-fgacy95 at 108c4a3e28782aa4e5ac3ae30b4c80f697299cca. Deploy bead ga-rowxra was created from that review PASS. |
| 2 | Acceptance criteria met | PASS | Verified exact sentinel text `pool session create skipped: demand read partial`, exact log suffix `(partial demand read, fresh create blocked)`, create guard before `tryClaimPoolSessionCreate`, and retained-capacity behavior for active/awake plus valid in-flight creates. |
| 3 | Tests pass | PASS | `go test ./cmd/gc -run 'TestBuildDesiredState_ScaleCheckPartialPoolBlocksNewCreates|TestRetainScaleCheckPartialPoolDesired_InFlightCreatingBeadRetained'` passed; `make test-fast-parallel` passed; `go vet ./...` passed; `go build -o /tmp/gc-ga-rowxra ./cmd/gc` plus `/tmp/gc-ga-rowxra --help` smoke passed. |
| 4 | No high-severity review findings open | PASS | Review bead ga-0xljyj.1 had one MEDIUM spec-compliance blocker, resolved in commit 108c4a3e2. No HIGH findings are recorded as open. GitHub PR #3686 has no comments or reviews. |
| 5 | Final branch is clean | PASS | Pre-gate worktree was clean at reviewed head. Gate file is the only deployer change to commit on top of 108c4a3e2. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` exited 0 and produced tree 3ee84c5e6d2a93f972314e1ed538cc447f23cf12. GitHub reports PR #3686 `mergeable: MERGEABLE`. |
| 7 | Single feature theme | PASS | Diff is confined to `cmd/gc` desired-state pool create gating, regression tests, and prior release-gate artifact `release-gates/ga-f963pu-scale-check-partial-gate.md`. No API, schema, config, or unrelated subsystem changes. |

## Acceptance Evidence

| Acceptance check | Result | Evidence |
|------------------|--------|----------|
| Start only after review PASS | PASS | ga-0xljyj.1 contains `RE-REVIEW PASS` at reviewed head 108c4a3e2. |
| Prepare branch without unrelated changes | PASS | `origin/builder/ga-0xljyj-scale-check-partial-gate` resolves to 108c4a3e2. Diff versus origin/main is limited to `cmd/gc/agent_build_params.go`, `cmd/gc/build_desired_state.go`, two `cmd/gc` test files, and an existing release-gate artifact. |
| Verify ga-0xljyj scope and ga-4qbgqf.2/3 contracts | PASS | `errPoolSessionCreatePartial` is defined with `pool session create skipped: demand read partial`; partial-read log suffix is `(partial demand read, fresh create blocked)`; old sentinel name is absent; create blocking occurs after reuse selection and before fresh create claim. |
| Run required gate commands | PASS | Focused `go test`, `make test-fast-parallel`, `go vet ./...`, and build/smoke all exited 0 on the deploy worktree. |
| Open or update PR and record release evidence | PASS | Existing PR #3686 is the branch PR and is open/mergeable. This gate file will be committed and pushed before updating PR metadata and bead notes. |
| Route merge/MPR only after pass | PASS | Merge authority remains mayor/mpr. Deployer will send a merge-request after push/PR update; no merge command will be run. |

## Commands Run

```text
go test ./cmd/gc -run 'TestBuildDesiredState_ScaleCheckPartialPoolBlocksNewCreates|TestRetainScaleCheckPartialPoolDesired_InFlightCreatingBeadRetained'
make test-fast-parallel
go vet ./...
git merge-tree --write-tree origin/main HEAD
go build -o /tmp/gc-ga-rowxra ./cmd/gc
/tmp/gc-ga-rowxra --help
```
