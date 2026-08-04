# Release gate: ga-l4jcht WakeBackoffUntil suppression

Date: 2026-07-06

Bead: ga-l4jcht

Source implementation bead: ga-7fldxz.2

Review bead: ga-qoqjle

Release branch: release/ga-l4jcht-wake-backoff-v2

Code head before gate commit: 91dd74e5ae02244dfe0ffa6949a2510845c0282b

Target base: origin/main at 3569d055d1c9392fb2d51bc962205c570c52e1c7

Outcome: PASS

This branch carries the reviewed WakeBackoffUntil suppression change as one
isolated commit on top of current origin/main. The earlier deploy gate failed
because the original reviewed commit depended on a helper from an unrelated
ga-omtqm5 stack. The v2 branch resolves that by preserving the reviewed semantic
delta while using the equivalent inline readiness switch already present on
main, without carrying ga-omtqm5 content.

## Gate criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead ga-qoqjle is closed with `VERDICT: PASS`. The reviewer independently verified build, vet, lint, focused tests, scope, and security posture. |
| 2 | Acceptance criteria met on final branch | PASS | ga-7fldxz.2 is closed. The branch adds WakeBackoffUntil/WakeBackoffCount metadata parsing and suppression in `cmd/gc`, threads `now` through the awake-set checks, and includes coverage for future suppression, elapsed suppression, mixed-bead wake behavior, metadata parsing, external-update invalidation, and grace-window self-invalidation. |
| 3 | Tests pass | PASS | From `/var/tmp/gascity-deploy-ga-l4jcht-v2.CR94Tr`: `gofmt -l cmd/gc/compute_awake_bridge.go cmd/gc/compute_awake_bridge_test.go cmd/gc/compute_awake_set.go cmd/gc/compute_awake_set_test.go` produced no output; `TMPDIR=/var/tmp make test-fast-parallel` passed all 8 fast shards; `TMPDIR=/var/tmp go vet ./...` passed. |
| 4 | No high-severity review findings open | PASS | ga-qoqjle records no blocking or high-severity findings. Its two findings are explicitly non-blocking: prompt/template follow-up for agents to write the metadata, and the deploy-routing hazard resolved by this isolated v2 branch. |
| 5 | Final branch is clean | PASS | Before adding this gate file, `git status --short --branch` in the release worktree printed only `## release/ga-l4jcht-wake-backoff-v2`. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-base --is-ancestor origin/main HEAD` passed at code head 91dd74e5a, whose parent is origin/main 3569d055d. |
| 7 | Single feature theme | PASS | `git diff --name-only origin/main...HEAD` touches only `cmd/gc/compute_awake_bridge.go`, `cmd/gc/compute_awake_bridge_test.go`, `cmd/gc/compute_awake_set.go`, and `cmd/gc/compute_awake_set_test.go`; all changes are one reconciler awake-set/backoff feature. |

## Branch scope

```text
91dd74e5a feat(reconciler): add WakeBackoffUntil suppression for externally-blocked work beads
```

Diff summary:

```text
cmd/gc/compute_awake_bridge.go      |  32 ++++++-
cmd/gc/compute_awake_bridge_test.go | 165 ++++++++++++++++++++++++++++++++++++
cmd/gc/compute_awake_set.go         |  48 ++++++++---
cmd/gc/compute_awake_set_test.go    |  67 ++++++++++++++-
4 files changed, 297 insertions(+), 15 deletions(-)
```

## Manifest note

`docs/PROJECT_MANIFEST.md` is not present in this checkout (`rg --files -g
'*PROJECT_MANIFEST*'` returned no matches). This gate uses the release criteria
provided in the deployer instructions for this session.

