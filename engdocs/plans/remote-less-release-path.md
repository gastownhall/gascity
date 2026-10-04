# Plan: Remote-less release path for reviewed deploys (`ga-ozrmfy`)

> Owner: `gascity/pm` - Created: 2026-05-30
> Source: reviewer handoff `ga-ozrmfy`; source implementation `ga-qqxxzj`

## Why this work exists

The MPR large-diff fallback has already passed review on commit `ab47806`
from `builder/ga-4gecl9-mpr-large-diff`. The deploy gate failed because
the target repository, `/home/jaword/projects/gc-management`, has no git
remote. Mayor analysis on `ga-ozrmfy` says that is intentional for this
repo: city code lands by local merge to `main`, not by push plus PR.

This plan separates the immediate release from the systemic deployer-flow
fix so the reviewed MPR change can land while the durable local-only
contract is made explicit.

## Goal

Remote-less target repositories should not fail deploy solely because push
and PR creation are unavailable. For repositories with a valid remote, the
existing push/PR release path remains unchanged.

## Work breakdown

| Bead | Title | Routes to | Gate |
|------|-------|-----------|------|
| `ga-ozrmfy.4` | Land reviewed MPR large-diff fallback through local-only release | deployer | needs-deploy |
| `ga-ozrmfy.1` | Define local-only release contract for remote-less repos | architect | needs-architecture |
| `ga-ozrmfy.2` | Specify regression coverage for remote-less deploy gates | validator | needs-tests |
| `ga-ozrmfy.3` | Implement local-only deploy path for remote-less targets | builder | ready-to-build |

## Dependency graph

```text
ga-ozrmfy.4

ga-ozrmfy.1 -> ga-ozrmfy.2 -> ga-ozrmfy.3
ga-ozrmfy.1 ----------------> ga-ozrmfy.3
```

The immediate landing bead is intentionally unblocked because the MPR fix
already passed review and the target repo's local-only release model is
recorded on the source bead. The systemic builder work waits for the
architecture contract and regression coverage.

## Acceptance summary

1. The reviewed MPR fallback commit `ab47806` is landed on `main` through
   the established local-only release process for `gc-management`.
2. The deployer records gate evidence, final local `main` commit, and any
   failure routing on the landing bead and source bead.
3. Architecture documents when a remote-less target is valid local-only
   release policy versus a true missing-remote configuration error.
4. Validator coverage proves remote-less and remote-backed deploy behavior,
   plus failure-before-merge evidence requirements.
5. Builder updates the deployer flow so remote-less targets use the
   documented local-only contract while remote-backed targets continue to
   push and open PRs.
6. The systemic fix preserves Gas City invariants: no hardcoded role names
   in Go source, no status files, and evidence stored on beads/gate
   artifacts.

## Risks and unknowns

- The deployer prompt currently describes push plus PR as the normal output
  contract. Architecture must decide whether the remote-less path belongs in
  prompt/template policy, executable code, or both.
- The immediate landing must not silently skip gate evidence just because it
  is local-only.
- A missing remote should still fail for repositories that are expected to
  have a push target.

## Out of scope

- Re-reviewing the MPR large-diff implementation.
- Changing MPR behavior beyond the already-reviewed fallback commit.
- Creating a git remote for `gc-management`.
