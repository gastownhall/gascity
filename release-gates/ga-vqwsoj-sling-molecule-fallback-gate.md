# Release gate: default-formula sling fallback

- **Deploy bead:** `ga-vqwsoj`
- **Build bead:** `ga-darmzs` (implementation source: `ga-ueugmi`)
- **Review bead:** `ga-hc2j1e`
- **Reviewed source:** `7f8984ca081d5813c2d011ec576ae7d470046544`
- **Base checked:** `origin/main` at `a4e4cc2bfac251b65116d536addbb4a7be9d95cd`
- **Isolated gate branch:** `deploy/ga-vqwsoj-gate`
- **Verdict:** **PASS**

`docs/PROJECT_MANIFEST.md` is absent from both the reviewed source and current
`origin/main`, so there are no additional repository-local release criteria to
apply beyond the seven deployer criteria below.

## Gate criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead `ga-hc2j1e` records `verdict: pass` for the authoritative deploy source `7f8984ca081d5813c2d011ec576ae7d470046544`. It also records fresh style, security, specification, build, vet, and focused-test evidence. |
| 2 | Acceptance criteria met | PASS | Direct inspection confirms that a live non-workflow formula attachment now returns the typed `MoleculeAttachedError`; only the implicit `default_sling_formula` path handles that type by warning and calling the normal plain-bead `finalize` path. The explicit `--on` path passes the fallback flag as false, and workflow conflicts or unrelated errors still hard-fail. The fallback therefore still performs the route write that records `gc.routed_to`. `TestDoSlingDefaultFormulaFallsBackToPlainRouteWhenMoleculeAttached` passed and verifies a single plain-route call plus a warning naming the blocking attachment. The separately reviewed `--notify` pack-config repair is commit `36df5542ed61f58ad62b9b53fa4f3047e40db094` in the local-only `gc-management` repository and is intentionally outside this PR. |
| 3 | Tests pass | PASS | With the rootless Podman socket configured and the repository-pinned Dolt `2.1.7` image present, the documented fast CI-equivalent command `GO_TEST_TIMEOUT=30m make test-fast-parallel` passed all 10 runner jobs, 0 FAIL, 0 SKIP. `go test -json -count=1 ./internal/sling` produced 239 PASS, 0 FAIL, 0 SKIP. `diff_tests_executed`: `TestDoSlingDefaultFormulaFallsBackToPlainRouteWhenMoleculeAttached` PASS. `waiver_ref`: none. `go vet ./...` and `go build ./...` also passed. |
| 4 | No high-severity review findings open | PASS | The review records `style_findings: none`, `security_findings: none`, and no unresolved HIGH finding. Independent gate inspection found no new high-severity issue. |
| 5 | Final branch is clean | PASS | The detached worktree at the reviewed source was clean before this checklist was added. This checklist is the only gate-owned file and is committed on the isolated deploy branch; a post-commit cleanliness check is required before push. |
| 6 | Branch diverges cleanly from main | PASS | Evaluated first after the already-merged pre-flight. No pull request carries the reviewed SHA in the base repository. `git merge-base origin/main 7f8984ca081d5813c2d011ec576ae7d470046544` returned `a4e4cc2bfac251b65116d536addbb4a7be9d95cd`; `git merge-tree --write-tree origin/main 7f8984ca081d5813c2d011ec576ae7d470046544` returned 0 and produced tree `5d2798160d4bcfacbad566e071d3963cd58c354b`. No bounded self-rebase was needed. |
| 7 | Single feature theme | PASS | Both reviewed commits and all three changed files belong to `internal/sling` and implement one behavior: preserve bare dispatch when an implicit default formula cannot attach to a bead that already has a live formula attachment, without weakening explicit formula-request conflicts. |

## Additional static evidence

- `gofmt -l` over all three changed Go files — PASS, no output.
- `git diff --check origin/main...7f8984ca081d5813c2d011ec576ae7d470046544` — PASS.
- `go vet ./...` — PASS.
- `go build ./...` — PASS.

## Reviewed history

```text
dc9e652369 test(sling): red — default-formula sling must not hard-fail on attached molecule (refs ga-ueugmi)
7f8984ca08 feat: green — mol-code-review handback: gc sling to builder hard-fails on molecule-attached review bead (refs ga-ueugmi)
```
