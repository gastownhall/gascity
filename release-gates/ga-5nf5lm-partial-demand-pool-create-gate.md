# Release Gate: fail-closed pool session creates

Result: PASS

Date: 2026-06-23

## Candidate

- Deploy bead: `ga-dfybgu`
- Review bead: `ga-hs4fqf`
- Title: `needs-deploy: fail-closed pool creates on partial demand reads`
- Pull request: https://github.com/gastownhall/gascity/pull/3687
- Source branch: `builder/ga-4qbgqf.2-partial-demand-create-gate`
- Candidate commit: `d4c0fa80b8572609e4124504e0c8aafcae8a6f57`
- Base: `origin/main` at `32ca47acd639b80eee37f4623d0277018b674c06`
- Merge base: `32ca47acd639b80eee37f4623d0277018b674c06`

## Release Criteria Source

`docs/PROJECT_MANIFEST.md` is not present in this repository. This gate applies
the deployer release criteria and the repository testing guidance in
`TESTING.md`.

## Changed Paths

- `cmd/gc/agent_build_params.go`
- `cmd/gc/build_desired_state.go`
- `cmd/gc/build_desired_state_legacy_bound_recovery_test.go`
- `cmd/gc/build_desired_state_test.go`
- `release-gates/ga-5nf5lm-partial-demand-pool-create-gate.md`

## Gate Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `ga-hs4fqf` is closed with PASS by `reviewer-gm-wisp-3krxjnn` for commit `d4c0fa80b8572609e4124504e0c8aafcae8a6f57`; deploy bead `ga-dfybgu` records reviewed + passed status. |
| 2 | Acceptance criteria met | PASS | Shipped scope covers `ga-4qbgqf.1`, `.2`, `.3`, and `.5`; `ga-4qbgqf.4` is explicitly deferred until post-merge fleet validation and is not part of this release. Acceptance evidence: partial scale-check reads mark the affected template partial and block only fresh pool creates; existing active/awake and resumable pool sessions remain eligible; reserved slots are released before the partial-demand sentinel returns; stale creating beads can roll back once no active `pending_create_claim` remains; provider-health red entries block only the fresh-create path and fail open for absent/stale registry data. |
| 3 | Tests pass | PASS | Focused acceptance test PASS; `make test-fast-parallel` PASS (`All fast jobs passed`); `go vet ./...` PASS. |
| 4 | No high-severity review findings open | PASS | Review notes list two non-blocking findings only: LOW stale gate evidence and INFO comment wording. This gate refreshes the stale evidence. No HIGH findings remain open. |
| 5 | Final branch is clean | PASS | Clean detached deploy worktree was used for gate evaluation. `git status --short --branch` was clean before editing this gate file. After this gate file is committed, the PR branch tip contains only committed changes. `git config core.hooksPath` reports `.githooks`. |
| 6 | Branch diverges cleanly from main | PASS | `git fetch origin main builder/ga-4qbgqf.2-partial-demand-create-gate` completed; `git merge-tree --write-tree HEAD origin/main` returned tree `fe065046538cd35e7bc7caf2fb9d3d085e3140a7` with exit 0. |
| 7 | Single feature theme | PASS | The commit set touches one supervisor desired-state subsystem: fail-closed handling for fresh pool session creates when the planner cannot trust create eligibility. Partial-demand and provider-health gates share the same `selectOrPlanPoolSessionBead` fresh-create boundary and regression surface. |

## Acceptance Notes

- `agentBuildParams.poolScaleCheckPartialTemplates` remains package-private and is assigned after `evaluatePendingPoolsMap`.
- `selectOrPlanPoolSessionBead` evaluates preferred/resume and reuse paths before either fresh-create gate.
- The partial-demand sentinel message is `pool session create skipped: demand read partial`.
- The partial-demand debug suffix is `(partial demand read, fresh create blocked)`.
- `scaleCheckPartialSessionRetainable` counts confirmed alive sessions plus fresh `pending_create_claim=true` creates, not stale creating/start-pending states.
- `discoverSessionBeadsWithRoots` uses the narrower `poolPartialAlive` check so stale creating beads can clear.
- `agentBuildParams.providerHealthSnapshot` is loaded once per desired-state build and shared through pool realization.
- The provider-health create gate uses the configured agent/provider fallback order and blocks only `(present=true, healthy=false)` registry entries.
- The provider-health sentinel message is `pool session create skipped: provider red`.
- The provider-health debug suffix is `(provider red, fresh create blocked)`.
- There is no new config, API, schema, or wire shape.

## Test Evidence

- `git diff --check origin/main...HEAD`: PASS.
- `go test ./cmd/gc -run 'TestBuildDesiredState_(ScaleCheckPartialPoolBlocksNewCreates|ProviderRedBlocksNewPoolSessionCreate)|TestRetainScaleCheckPartialPoolDesired_InFlightCreatingBeadRetained'`: PASS (`cmd/gc` in 0.242s).
- `make test-fast-parallel`: PASS (`All fast jobs passed`).
- `go vet ./...`: PASS.

## Deploy Notes

- `gh auth status` passed for account `quad341`.
- PR #3687 is open, in-repository, and authored by `quad341`.
- The normal deployer worktree is in an unrelated interrupted rebase over an external contributor commit. This gate was evaluated in `/tmp/gascity-deploy-ga-dfybgu.hZMjbm` to avoid touching that state.
