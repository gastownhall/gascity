# GitHub merge queue adoption for `main`

Bead: `ga-lqt0o9`

Evaluated: 2026-07-29

Status: evaluation complete; implementation deferred pending explicit operator
approval

## Recommendation

Adopt GitHub's merge queue only after the existing immediate CI-verdict fixes
land and the operator explicitly approves the ruleset migration.

The merge queue is the strongest structural protection considered in
`ga-mvmjxh`: GitHub tests a temporary merge group containing the latest
`main`, the pull request, and any earlier queued changes before advancing
`main`. GitHub requires every required Actions workflow to listen for the
separate `merge_group` event; otherwise the queue waits for checks that never
arrive. See [Managing a merge queue][github-manage] and
[the `merge_group` event][github-event].

Do not enable the queue with the current ruleset. Ruleset `14017226` still
requires the seven-job `Check` roll-up rather than `CI / required`. A queue in
that state would validate the combined tree against the same incomplete gate
that motivated `ga-uqxysq.3`, so it would not deliver the intended verdict
integrity.

No implementation beads are created or routed by this evaluation. The
contingent packages below become schedulable only after the mayor records an
explicit operator GO.

## Current-state evidence

The live repository and ruleset were read on 2026-07-29.

| Surface | Current state | Migration consequence |
| --- | --- | --- |
| Ruleset `14017226` | Active on the default branch; no bypass actors; allows merge and squash; requires `Check` plus four `Analyze (...)` CodeQL contexts; does not contain a merge-queue rule | The operator must decide both the `Check` → `CI / required` migration already tracked by `ga-uqxysq.3` and whether to require the queue |
| `.github/workflows/ci.yml` | Triggers on `workflow_call`, `push` to `main`, and `pull_request`; no `merge_group` trigger | A queued pull request would never receive `Check` or `CI / required` |
| `.github/workflows/codeql.yml` | Triggers on `push`, `pull_request`, schedule, and manual dispatch; no `merge_group` trigger | All four currently required `Analyze (...)` checks would be absent on the merge-group SHA |
| Path filtering | `dorny/paths-filter` is pinned to `fbd0ab8...` (v4.0.1), the change that added native merge-group base/head handling | The existing path-filter graph can support `merge_group`; this must remain covered by policy tests |
| Static-analysis scope | `scripts/ci-static-scope` uses changed-file mode only for `pull_request`; every other event fails safe to full-repository scope | `merge_group` receives full static analysis without dereferencing a pull-request base SHA |
| Runner policy | Non-`pull_request` events use GitHub-hosted runners | Merge-group runs would use GitHub-hosted runners unless an architect approves a different policy |
| Main-only jobs | Unit coverage and full REST integration use `github.event_name == 'push'` | They remain post-merge checks. The merge-group gate stays equivalent to the PR gate and does not inherit the 36–52 minute push-only critical path |
| Verdict watchdog | `ga-r0fbe5` is scoped to cancelled `push` runs | It should continue to ignore merge-group cancellations; queue recalculation is a separate lifecycle |

The pinned path-filter action documents native merge-queue support in its
[v4 README][paths-filter].

### Load and reliability baseline

The repository merged 171 pull requests from July 22 through July 29. The
largest observed burst was 11 merges in one hour, including 10 within one
15-minute bucket.

For the 20 most recent completed, non-cancelled pull-request runs:

| Workflow | Median creation-to-completion | p95 | Maximum |
| --- | ---: | ---: | ---: |
| CI | 9m 47s | 12m 07s | 21m 03s |
| CodeQL | 6m 36s | 8m 47s | 8m 57s |

The last 100 pull-request CI runs contained 60 successes, 34 failures, four
cancellations, one action-required result, and one active run. This is not a
merge-candidate pass-rate calculation, but it is enough to reject a blind
ruleset flip: the canary must distinguish real blocking defects from queue
configuration failures before normal merges depend on the new gate.

The three lower-cost fixes remain independent prerequisites:

- `ga-tniyjy` — make main-push concurrency per commit.
- `ga-hkdsgc` — cover the `CI / required` dependency graph and add the two
  unit-coverage jobs to its push verdict.
- `ga-r0fbe5` — make a cancelled main-push run visible as an explicit
  non-verdict.

These fixes remain useful after queue adoption because post-merge push checks
still run and must still produce a verdict for every commit.

## Operator decision gate

The mayor should obtain one explicit operator decision covering all four
questions below before scheduling any package:

1. Should `main` require GitHub's merge queue?
2. Should ruleset `14017226` replace `Check` with `CI / required` first, as
   already tracked by held bead `ga-uqxysq.3`?
3. Which queue policy should the operator use for merge method, build
   concurrency, grouping strategy, group size, wait time, and status-check
   timeout?
4. Who owns the rollout window and has authority to restore the captured
   ruleset immediately?

GitHub exposes build concurrency, grouping strategy, merge method, group
limits, wait time, and check timeout as independent queue controls. The
operator should choose them from the measured workload rather than accepting
unreviewed defaults. The available settings are documented under
[Require merge queue][github-rules].

An operator decision to enable the queue while retaining `Check` is a no-go
for this goal. It would add queue latency without closing the known
`CI / required` coverage gap.

## Contingent work packages

These are decomposition targets, not live beads. After operator GO, PM creates
and routes them with the listed labels and dependencies.

### 1. Guard merge-group readiness for every required workflow

Route: `needs-tests` → `gascity/validator`

Acceptance:

- A failing-first policy test proves that both `ci.yml` and `codeql.yml`
  declare `merge_group` with only `checks_requested`.
- The test proves that every GitHub Actions context required by ruleset
  `14017226` is produced by a workflow that supports both `pull_request` and
  `merge_group`.
- Fixtures cover missing triggers, a misspelled activity type, and an
  unaccounted required context.
- Existing `push`, `pull_request`, `workflow_call`, schedule, and manual
  triggers remain asserted where applicable.

### 2. Make CI and CodeQL merge-group aware

Route: `ready-to-build` → `gascity/builder`

Depends on: package 1 and the landed versions of `ga-tniyjy`,
`ga-hkdsgc`, and `ga-r0fbe5`

Acceptance:

- `ci.yml` and `codeql.yml` run on
  `merge_group.types: [checks_requested]`.
- A merge-group SHA receives `CI / required` and all four existing
  `Analyze (...)` CodeQL contexts without introducing a new required context
  name.
- Pull-request supersession still cancels only the obsolete run for that pull
  request. Push and merge-group runs do not collide with PR concurrency
  groups or with one another.
- Merge-group path detection compares the event's base and head through the
  pinned action's native support.
- Static checks use full-repository scope for `merge_group`.
- The current merge-group runner choice is explicit and tested.
- Push-only coverage and full REST jobs remain push-only; their skip is
  accepted by `CI / required` on pull-request and merge-group events.
- Existing workflow-policy and focused tests pass before the normal CI change
  gates run.

### 3. Enable and canary the protected merge queue

Owner: operator action coordinated by mayor; do not sling to an agent

Depends on: package 2, explicit operator GO, and the `ga-uqxysq.3` decision

Acceptance:

- The operator captures the complete pre-change ruleset JSON and records the
  recovery owner before mutation.
- `Check` is not the queue's enforced CI roll-up. If the operator has not
  approved `CI / required`, rollout stops.
- The operator changes required-status enforcement and merge-queue
  enforcement as separately reversible steps.
- One low-risk pull request is queued before normal throughput resumes.
- Within two minutes of enqueue, both CI and CodeQL have `merge_group`
  workflow runs for the same merge-group SHA.
- The merge-group SHA reports `CI / required` and all four required CodeQL
  contexts. `main` advances only after all required contexts succeed.
- The first 10 queue entries or first 24 hours, whichever is longer, record
  queue wait, workflow duration, dequeue reason, runner queueing, and
  post-merge push verdict.

### 4. Publish the maintainer runbook and rollout result

Route: `ready-to-build` → `gascity/builder`

Depends on: package 3 canary completion

Acceptance:

- Contributor guidance explains enqueue, dequeue, failed-group recovery,
  queue-jump policy, and the difference between PR, merge-group, and
  post-merge push verdicts.
- The runbook names the operator rollback path and links the captured
  ruleset evidence without embedding credentials or temporary local paths.
- The measured canary result and any approved setting changes are recorded.

## Rollout plan

1. **Land prerequisites.** Complete the three immediate fixes. Do not couple
   their delivery to this migration.
2. **Stage workflow support.** Land packages 1 and 2 through the normal PR
   path. Adding an inactive `merge_group` trigger is safe while no queue rule
   is enabled and ensures the workflow definitions exist on the default
   branch before the first queue event.
3. **Capture recovery evidence.** The operator saves the full live ruleset,
   current required contexts, queue settings if any, and the current workflow
   SHAs. The mayor names the person present for rollback.
4. **Promote the real roll-up.** Resolve `ga-uqxysq.3` first. Confirm a normal
   PR reports the exact `CI / required` context from GitHub Actions.
5. **Enable conservatively.** Add the merge-queue rule with operator-approved
   settings and enqueue one low-risk canary. Do not release a burst of queued
   work until both required workflows are visible on its merge-group SHA.
6. **Observe before tuning.** Hold the initial settings through the stated
   10-entry/24-hour observation window. Increase build concurrency or group
   size only from measured queue wait and runner capacity.

Existing maintainer use of GitHub auto-merge remains compatible: when a branch
requires a queue, GitHub places an eligible pull request into that queue rather
than merging it directly. See [Merging a pull request with a merge
queue][github-use].

## Rollback plan

Rollback is operator-owned because ruleset mutation is operator-only.

Trigger rollback when any of these occurs:

- either required workflow fails to start for the canary merge-group SHA
  within two minutes;
- a required context is still absent at the configured check timeout;
- a queue configuration failure, rather than a code/test failure, removes the
  canary;
- queue-induced runner saturation materially prevents ordinary PR checks from
  reaching a verdict; or
- the recovery owner cannot explain why a merge-eligible pull request is
  blocked.

Rollback order:

1. Stop adding entries and notify maintainers that the queue is draining or
   being cleared.
2. Remove the merge-queue rule by restoring the captured ruleset shape.
3. Preserve the `CI / required` promotion if it is healthy and independently
   approved; otherwise restore its separately captured required-status
   section.
4. Verify the normal merge control is restored and a PR can proceed under the
   pre-queue rules.
5. Leave the inert `merge_group` workflow triggers in place unless they
   caused the defect. Revert them later through a normal PR if desired.
6. Keep all three immediate CI-verdict fixes; none is rolled back with the
   queue.

This sequence restores merge throughput first and avoids conflating queue
rollback with the independently valuable verdict-integrity fixes.

## Final acceptance for adoption

Adoption is complete only when:

- every ruleset-required Actions context is emitted for `merge_group`;
- the enforced CI roll-up is `CI / required`, not `Check`;
- one canary and the full observation window complete without a
  configuration-caused dequeue;
- no commit reaches `main` before the required merge-group contexts succeed;
- post-merge push CI still produces a verdict for every resulting main
  commit; and
- the operator has demonstrated that the captured ruleset can be restored.

[github-manage]: https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/configuring-pull-request-merges/managing-a-merge-queue
[github-event]: https://docs.github.com/en/actions/reference/workflows-and-actions/events-that-trigger-workflows#merge_group
[github-rules]: https://docs.github.com/en/enterprise-cloud@latest/repositories/configuring-branches-and-merges-in-your-repository/managing-rulesets/available-rules-for-rulesets#require-merge-queue
[github-use]: https://docs.github.com/en/pull-requests/collaborating-with-pull-requests/incorporating-changes-from-a-pull-request/merging-a-pull-request-with-a-merge-queue
[paths-filter]: https://github.com/dorny/paths-filter#supported-workflows
