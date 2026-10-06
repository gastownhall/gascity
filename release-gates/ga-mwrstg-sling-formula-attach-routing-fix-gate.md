# Release gate: sling formula-attach routing fix

- **Deploy bead:** `ga-mwrstg`
- **Build bead:** `ga-f43t9b`
- **Review bead:** `ga-gr10vs`
- **Reviewed source:** `13362a5c4b42a30ee8cdcd3f5e8632e1911f0126`
- **Base checked:** `origin/main` at `ad4d0ab4a9e14f57faed3eaa20a658ef743e1c09`
- **Isolated gate branch:** `deploy/ga-mwrstg-gate`
**Verdict:** **PASS**

`docs/PROJECT_MANIFEST.md` is absent from both the reviewed source and current
`origin/main`, so there are no additional repository-local release criteria to
apply beyond the seven deployer criteria below.

## Gate criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead `ga-gr10vs` records `REVIEWER VERDICT: PASS` for the feature at `23f6f1af7`, with all six acceptance items checked directly. The post-review gate repair at `13362a5c4` only removes the newly introduced legacy `formulatest` coupling; the deploy bead records that SHA as the repaired authoritative source. |
| 2 | Acceptance criteria met | PASS | Direct inspection confirms formula attachment writes `beadmeta.ExecutionRoutedToMetadataKey` after resolving and normalizing pool routes, preserves the default-formula idempotent warning, limits workflow-root dry-run disclosure to formulas v2, and leaves `gc.routed_to` unset on the source bead. Focused tests: `go test -json -count=1 ./internal/sling` = 232 PASS, 0 FAIL, 0 SKIP; `GC_FAST_UNIT=0 go test -json -count=1 ./cmd/gc -run 'TestDryRun\|Sling'` = 226 PASS, 0 FAIL, 0 SKIP. |
| 3 | Tests pass | PASS | Documented CI-equivalent coverage used `make test-local-full-parallel` with the pinned `bd` v1.1.0 release (`8e4e59d39`). Gate accounting after environment-corrected reruns: 38 jobs PASS, 0 feature FAIL, 2 justified SKIP. The two skipped tmux jobs use exact-key `list-keys` syntax that returns empty under host tmux 3.7b; the exact three-test comparison produces the same two failures on this head and current `origin/main`, while the hidden-client test passes. This is safe for a change limited to `cmd/gc/cmd_sling*` and `internal/sling/*`. Five corrected REST shards then passed with the real platform home and an isolated home only for the pinned `bd` process (90 top-level tests, 0 FAIL). The regression guard `GC_FAST_UNIT=0 go test -json -count=1 ./internal/testenv -run '^TestLegacyFormulaV2MechanismFrozen$'` = 1 PASS, 0 FAIL, 0 SKIP. |
| 4 | No high-severity review findings open | PASS | The review records zero unresolved HIGH findings, and no new high-severity finding was identified during gate inspection. |
| 5 | Final branch is clean | PASS | The isolated branch was clean at the reviewed source before this checklist was added. The checklist is the only gate-owned file and will be committed before push; the post-commit cleanliness check is required below. |
| 6 | Branch diverges cleanly from main | PASS | Evaluated first and refreshed after `origin/main` advanced. `git merge-base origin/main 13362a5c4` is `a585e07a9`; `git merge-tree --write-tree origin/main 13362a5c4` returned 0 against `ad4d0ab4a` and produced tree `3c52e0a15dc6dadb2d4df3cd47b1702ca8def514`. No bounded self-rebase was needed. The already-merged pre-flight found no base-repository commit or PR for the reviewed source. |
| 7 | Single feature theme | PASS | All four commits and all four changed files form one `sling` feature: formula-attachment route restamping, its idempotent messaging and formulas-v2 dry-run disclosure, plus direct regression coverage. |

## Additional static evidence

- `go vet ./...` — PASS.
- `LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=a585e07a93782c24a629359cf635f9e95beded5d make lint-affected` — PASS, 0 issues.
- `LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=a585e07a93782c24a629359cf635f9e95beded5d make fmt-check-changed` — PASS.
- `git diff --check a585e07a93782c24a629359cf635f9e95beded5d..HEAD` — PASS.
- `git config core.hooksPath` — `.githooks`.

## Reviewed history

```text
8038caf7a fix(sling): restamp gc.routed_to on formula-attach and disclose it in --dry-run
d1e958485 test(sling): red — regression coverage for #4763 fix plan (refs ga-f43t9b)
23f6f1af7 fix(sling): fix default-formula skip message and scope dry-run wisp-root disclosure to graph.v2 (refs ga-f43t9b)
13362a5c4 test(sling): drop legacy formulatest coupling from graph.v2 dry-run test (refs ga-mwrstg)
```
