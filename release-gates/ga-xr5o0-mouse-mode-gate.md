# Release gate - mouse_mode config field (ga-weme6 / ga-xr5o0)

**Verdict:** PASS

- Deploy bead: `ga-weme6` (review bead)
- Source bead: `ga-xr5o0` (closed)
- Branch: `builder/ga-xr5o0-1`
- PR: https://github.com/gastownhall/gascity/pull/2563
- HEAD: `f16074f8d` (`feat(config): add agent mouse mode`)

## Criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | Reviewer PASS verdict in bead notes | PASS | `ga-weme6` notes contain `VERDICT: pass` and the reviewer summary reports PASS at `f16074f8d`. |
| 2 | Acceptance criteria met | PASS | `mouse_mode` is threaded through `config.Agent`, patch/override apply paths, pool deep copy, migration structs, template resolution, runtime config, schemas, and generated clients. Runtime `MouseOn` participates in the v3 fingerprint. Tmux startup disables mouse/activity by default and skips that step when `mouse_mode = "on"`. |
| 3 | Tests pass on final branch | PASS | `make test-fast-parallel` PASS from detached `/tmp/gascity-release-ga-xr5o0.umExZv` worktree at `f16074f8d`; focused tests, vet, dashboard check, dashboard smoke, and whitespace check also passed. |
| 4 | No high-severity review findings open | PASS | Reviewer notes list no blocking or HIGH findings; only non-blocking observations were recorded. |
| 5 | Working tree clean | PASS | `git status --short --branch` clean before gate-file commit; `git diff --check` clean. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree HEAD origin/main` returned a tree hash with exit 0; `origin/main...HEAD` is 0 behind / 1 ahead before this gate commit. |

## Acceptance evidence

- Default/empty `mouse_mode` preserves headless behavior by mapping to `MouseOn=false`.
- Explicit `mouse_mode = "off"` is accepted and preserves default mouse-off startup behavior.
- Explicit `mouse_mode = "on"` maps to runtime `MouseOn=true` and skips tmux mouse/activity disable.
- `ValidateAgents` rejects invalid `mouse_mode` values outside `""`, `"on"`, and `"off"`.
- `AgentPatch`, `AgentOverride`, `applyAgentPatch`, `applyAgentOverride`, migration config, schema generation, and `deepCopyAgent` all include the new field.
- Runtime fingerprint includes `MouseOn`, with `FingerprintVersion` bumped from `v2` to `v3`.

## Validation

- `make test-fast-parallel` from `/tmp/gascity-release-ga-xr5o0.umExZv` - PASS
- `go test ./internal/config ./internal/runtime ./internal/runtime/tmux ./internal/migrate -count=1` - PASS
- `go test ./cmd/gc -run 'TestDeepCopyAgentCoversAllFields|Test.*Template|Test.*Pool' -count=1` - PASS
- `go test ./internal/api ./internal/api/genclient -run 'TestOpenAPISpecInSync|TestGeneratedClientInSync' -count=1` - PASS
- `go test ./test/docsync -run TestSchemaFreshness -count=1` - PASS
- `go vet ./...` - PASS
- `make dashboard-check` - PASS
- `make dashboard-smoke` - PASS
- `git diff --check` - PASS

## Local environment note

The first `make test-fast-parallel` run from the nested
`/home/jaword/projects/gc-management/.gc/worktrees/gascity/deployer`
checkout failed in `cmd/gc` shard 5 because the enclosing management city
loads `/home/jaword/projects/gc-management/packs/maintainer-pr-review/pack.toml`,
which still uses deprecated `[formulas].dir`. The same shard passed from the
detached `/tmp` worktree at identical HEAD, so the failure is not attributed to
this branch.

## Push target

Dry-run push to `origin` succeeded. Use `origin` for the final branch push.
