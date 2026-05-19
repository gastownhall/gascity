---
title: T3 Session Bridge Merge Checklist
description: Practical checklist for merging latest main into feature/t3-session-bridge without losing local work.
---

## Baseline

- [ ] Fetch latest refs:
      `git fetch origin main`
      `git fetch upstream main`
- [ ] Use `upstream/main` as default merge target for this branch.
- [ ] Confirm current head and target SHAs before merging.
- [ ] Confirm current branch is `feature/t3-session-bridge`.

Current known refs from 2026-04-23:

- branch head: `b1df7141`
- upstream main: `bc6058d3`
- origin main: `afb1ecdd`

## Protect Local Work First

- [ ] Record a safety branch from current `HEAD`.
- [ ] Save unstaged and staged changes to patch files.
- [ ] Stash tracked and untracked local changes.
- [ ] Do not run real merge in dirty primary worktree.

Current dirty files to preserve:

- `cmd/gc/api_state.go`
- `cmd/gc/cmd_reload.go`
- `cmd/gc/providers.go`
- `cmd/gc/template_resolve.go`

## Compare Before Merge

- [ ] Run ahead/behind check against `upstream/main`.
- [ ] Review branch diff against `upstream/main`.
- [ ] Review incoming changes from `upstream/main` on current dirty files.
- [ ] Expect manual conflict resolution in hot spots.

Known hot spots from history and probe:

- `cmd/gc/template_resolve.go`
- `cmd/gc/session_beads.go`
- `cmd/gc/beads_provider_lifecycle.go`
- `internal/runtime/t3bridge/provider.go`
- config patch/edit paths

## Use Scratch Worktree

- [ ] Create detached scratch worktree from current `HEAD`.
- [ ] Create temporary `upmerge/...` branch inside scratch worktree.
- [ ] Merge `upstream/main` there with `git merge --no-ff upstream/main`.
- [ ] If merge fails badly, abort in scratch and retry. Do not damage primary tree.

## Resolve Conflicts

- [ ] Resolve all merge conflicts in scratch worktree.
- [ ] Re-check T3 bridge wiring after conflict resolution.
- [ ] Re-check session bead and lifecycle behavior after conflict resolution.
- [ ] Re-check config persistence semantics, especially explicit `suspended = false`.

Known conflict set from isolated probe against `upstream/main`:

- `cmd/gc/agent_build_params.go`
- `cmd/gc/cmd_citystatus.go`
- `cmd/gc/cmd_doctor.go`
- `cmd/gc/cmd_status.go`
- `cmd/gc/session_beads.go`
- `cmd/gc/template_resolve.go`
- `examples/dolt/assets/scripts/runtime.sh`
- `go.mod`
- `internal/api/handler_config_test.go`
- `internal/api/huma_handlers_config.go`
- `internal/doctor/checks.go`

## Restore Local Overlay Carefully

- [ ] Decide whether to apply local dirty overlay in scratch before landing.
- [ ] Prefer `git stash apply --index`.
- [ ] If stash apply fails, fall back to saved patch files with `git apply --3way`.
- [ ] Re-check restored local edits in:
      `cmd/gc/api_state.go`,
      `cmd/gc/cmd_reload.go`,
      `cmd/gc/providers.go`,
      `cmd/gc/template_resolve.go`

## Validate

- [ ] Run `go test ./...`
- [ ] Run `go vet ./...`
- [ ] Smoke-check `gc prime`
- [ ] Smoke-check T3 bridge startup path
- [ ] Smoke-check session audit command
- [ ] Smoke-check city status or dashboard paths if touched by conflict resolution

## Land Merge

- [ ] Fast-forward primary feature branch to validated scratch merge result.
- [ ] Re-apply local dirty overlay in primary tree only if not already landed.
- [ ] Keep safety branch until merge is pushed and verified.
- [ ] Remove scratch worktree only after verification.

## Recovery

- [ ] If local overlay is lost, try stash recovery first.
- [ ] If stash recovery is messy, apply saved patch files.
- [ ] If merge result is bad, switch back to safety branch.
- [ ] Never use `git reset --hard` on primary worktree for this flow.

## References

- [Safe Upmerge Formula](safe-upmerge-formula.md)
- [T3 Session Bridge History Summary](t3-session-bridge-history-summary.md)
