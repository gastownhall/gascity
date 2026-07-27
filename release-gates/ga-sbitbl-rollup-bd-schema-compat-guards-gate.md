# Release gate: bd schema-compat guards rollup

Gate date: 2026-07-03

Final branch: `release/ga-sbitbl-rollup-bd-schema-compat-guards`

Base: `origin/main` at `38c51fd5f26e23a6440ec6699c1575811dc83e19`

Head before gate file: `7d88e6dbd3bb96ffde8dab647d5e90e3a2b6e263`

Primary deploy beads: `ga-sbitbl`, `ga-36hv1p`

Note: `docs/PROJECT_MANIFEST.md`, `PROJECT_MANIFEST.md`, and
`SOFTWARE_FACTORY_MANIFEST.md` are not present in this tree. This gate uses the
deployer prompt's release criteria table as the operative release criteria.

## Candidate commits

| Unit | Release branch commit | Source/review evidence | Verdict |
| --- | --- | --- | --- |
| `ga-qyw3wn` work-query schema-skew failure surfacing | `722dcc6f5` | Review beads `ga-0win3a` and `ga-th3zom` both closed PASS | PASS |
| `ga-ac6t6q` poolDemandCountShell legacy-ephemeral fail-loud fix | `84389505c` | Review bead `ga-5tg9az` closed PASS | PASS |
| `ga-ooka7o` on_death/on_boot bd_or_fatal guard | `856c0f636` | Review bead `ga-ggx90t` closed PASS | PASS |
| `ga-ua1h7d` Claude/Codex SessionStart bd schema-compat guard | `ebc7c6869` | Review bead `ga-hutkxc` reached RE-REVIEW PASS after fixing its high finding | PASS |
| `ga-hutkxc` Codex hook drift fixture fix | `94106d082` | Same review bead `ga-hutkxc`, re-review PASS | PASS |
| `ga-3w44ma` Antigravity gascity-prime bd schema-compat guard | `7d88e6dbd` | Review bead `ga-8rplpo` closed PASS | PASS |

## Acceptance criteria

| Unit | Acceptance check | Evidence |
| --- | --- | --- |
| `ga-qyw3wn` | Default work-query surfaces bd schema-skew as a hard failure while genuine empty results remain "no work"; `gc doctor` warns on skewed bd. | Branch includes `cmd/gc/doctor_bd_schema_skew.go`, `cmd/gc/doctor_bd_schema_skew_test.go`, guarded query generation in `internal/config/config.go`, and config/doctor regression tests. Review PASS verified these exact criteria. |
| `ga-ac6t6q` | A hard bd failure in the legacy-ephemeral pool-demand query exits non-zero; genuine empty ephemeral state still counts as 0. | Branch includes the `ephemeral_json=$(...) || exit $?` change and matching call-site propagation plus three tests in `internal/config/config_test.go`. Review PASS verified red-before/green-after behavior. |
| `ga-ooka7o` | `effectiveOnDeath` and `effectiveOnBoot` wrap bd reads/writes with the existing `bd_or_fatal` guard; empty/happy paths unchanged; legacy-ephemeral reachability accounted for. | Branch includes guarded recovery-hook shell generation and tests covering read/write skew failure paths in `internal/config/config_test.go`. Review PASS verified nested-subshell exit relay behavior. |
| `ga-ua1h7d` / `ga-hutkxc` | Claude and native Codex SessionStart hooks get a bd schema-compat guard; old managed hook forms upgrade; provider behavior documented; stale Codex fixture fixed. | Branch updates `internal/hooks/hooks.go`, `internal/hooks/config/claude.json`, Codex overlay hooks, and `cmd/gc/doctor_codex_hooks_test.go`; review PASS verifies the fixed stale fixture and relevant hook tests. |
| `ga-3w44ma` | Antigravity `gascity-prime` gets Variant B guard, writes diagnostics to stderr, does not append `exit 1`, leaves other Antigravity hooks untouched, and converges stale entries through existing merge behavior. | Branch updates only Antigravity's `gascity-prime` command and `internal/hooks/hooks_test.go`; review PASS verified the exact command, no duplicated `cmd/gc` fixture, no new doctor check, and source-backed merge behavior. |

## Release criteria

| # | Criterion | Result | Evidence |
| --- | --- | --- | --- |
| 1 | Review PASS present | PASS | Every unit above has a closed PASS review record. The only high finding (`ga-hutkxc` Finding 1) was fixed in the fixture commit and re-reviewed PASS. |
| 2 | Acceptance criteria met | PASS | Criteria are checked per unit in the acceptance table above and are reflected in the final branch diff. |
| 3 | Tests pass | PASS | `go vet ./...` exited 0. `make test-fast-parallel` exited 0: `fsys-darwin-compile`, `unit-core`, and all six `unit-cmd-gc-*` shards passed; summary: `All fast jobs passed`. |
| 4 | No high-severity review findings open | PASS | Current PASS reviews have no unresolved high findings. The high `ga-hutkxc` fixture finding is resolved by `94106d082`; remaining noted follow-ups are low/non-blocking (`ga-9ex12k`, `ga-cs4a2a`). |
| 5 | Final branch is clean | PASS | Clean before gate file write: `git status --short --branch` showed only `## HEAD (no branch)`. Gate file is committed as the final deployer commit. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree origin/main HEAD` exited 0 and emitted tree `e886c7698c53cdcbd92c9ae7687342975e36803f`; no conflicts reported. |
| 7 | Single feature theme | PASS | All commits are one bd schema-compat/fail-loud reliability theme across work queries, pool-demand, recovery hooks, and managed provider hooks. The rollup is necessary because the reviewed fixes were stacked and later commits rely on earlier guard/test scaffolding. |

## Commands run

```sh
git fetch origin main release/ga-sbitbl-rollup-bd-schema-compat-guards
git worktree add --detach /home/jaword/projects/gc-management/.gc/worktrees/gascity/deploy-ga-sbitbl-rollup-gate origin/release/ga-sbitbl-rollup-bd-schema-compat-guards
git log --oneline origin/main..HEAD
git diff --stat origin/main..HEAD
git merge-tree origin/main HEAD
go vet ./...
make test-fast-parallel
```
