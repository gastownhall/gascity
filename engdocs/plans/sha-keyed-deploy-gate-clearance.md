# Plan — SHA-keyed deploy-gate clearance

**Bead:** `ga-6z76y6`
**Rig:** `gascity`
**Branch:** `gc-pm-b8e04a1ccdb4`
**Supersedes:** none

## Goal

Prevent an internally authored Gas City pull request from merging before its
deploy gate completes. The motivating incident was PR #4943: maintainer review
merged the pull request after the reviewer created a deploy bead but before the
deployer claimed it, leaving the release gate bypassed and the deploy bead
stranded.

The accepted design is recorded on `ga-xykek3`. It uses a GitHub commit status
named `release-gate/deploy-clearance` as the boundary between two autonomous
loops:

- The deployer writes one `success` status for the exact head SHA after a remote
  gate passes.
- Maintainer review reads the status for the pull request's current head and
  holds merge-intent actions until that exact SHA is clear.

The signal is success-only and SHA-keyed. A new commit automatically requires a
new gate pass. Missing, non-success, wrong-context, stale-SHA, and API-error
results remain held. Enforcement is opt-in and defaults off for repositories
without a deployer.

## Ownership and routing

Every implementation file named by the design lives under
`/home/jaword/projects/gc-management/packs/**`. That tree is owned by the
city-scoped `pack-author.pack-author`, not the rig-scoped Gas City builder.
The implementation beads therefore carry `needs-pack-work` and
`gc.routed_to=pack-author.pack-author`. They are not sent through `gc sling`;
the pack-author intake query discovers both the label and route across rig
stores.

No tracker integration skill was materialized for this PM session, so external
tracker import was a no-op.

## Work packages

| Bead | Outcome | Acceptance focus |
| --- | --- | --- |
| `ga-6z76y6.1` | Publish deploy clearance after remote gate PASS | Both remote single-bead and rollup paths post one success status for the exact head after PR creation and before merge-request mail; the local-only path never posts; failures remain visible. |
| `ga-6z76y6.2` | Enforce clearance in maintainer review | Current-head lookup, per-repo kill switch, external-PR exemption, fail-closed hold behavior, label transitions, and DG1-DG12 including the no-hold-and-merge contract. |
| `ga-6z76y6.3` | Reconcile already-merged deploy beads | The merged-state preflight runs before criterion 6; current-main pass and failure outcomes both terminate without duplicate branch, PR, or merge mutation. |

Each bead contains its full measurable acceptance criteria and links back to
the accepted design sections it implements.

## Dependency graph

```text
ga-6z76y6.1  deployer writes clearance
      |
      v
ga-6z76y6.2  maintainer review enforces clearance

ga-6z76y6.3  already-merged reconciliation (parallel)
```

The enforcement bead depends on the writer bead. This ordering prevents the
Gas City policy opt-in from landing before any deploy path can produce the
required status. Reconciliation is independent and can proceed in parallel.

## Guardrails

- Reuse the existing internal-author predicate; maintainer review must not gain
  direct beads awareness.
- Use a live GitHub lookup rather than a local marker file or separate clear
  operation.
- Keep structural PR checks and explicit human review ahead of the deploy hold,
  and keep the hold ahead of every merge call.
- Do not gate `close-superseded`, `close_pr_as_docs_noop`, or
  `request-changes`. Those actions do not land code and gating them would strand
  legitimate cleanup.
- Keep the logic in pack scripts, policy, prompts, formulas, and tests. The
  accepted design requires no Go changes or new platform primitive.
- Verify quoted line numbers against current `gc-management/main`; surrounding
  source text is the durable anchor.

The deferred Cairn work item `crn-r05r` concerns GitHub CI and SHA-matched
review-PASS criteria inside the deploy gate. It is adjacent but not a blocker
or substitute for this deploy-clearance boundary.

## Follow-up outside this package

`ga-f64kyv` asks the Gas City architect to define how an active deploy bead is
linked or reconciled when its target PR closes as superseded or docs-noop. That
gap is real but does not bypass the release gate, so it is tracked separately
and does not block these three work packages.

## Completion evidence

This package is complete when all three implementation beads meet their own
acceptance criteria, the pack test suites and prompt/formula rendering checks
pass, and the end-to-end contract proves that a held current head cannot reach
`gh pr merge` while a correctly cleared current head preserves existing merge
behavior.

## Planning-system note

The repository moved internal PM artifacts to `engdocs/plans` in `512b066e8`.
The active PM prompt still names `docs/plans`; open bead `ga-f74ph9.1` owns the
pack-level rollout. The public `docs/` tree rejects unpublished engineering
plans through `TestEveryDocsPageIsPublished`, so this artifact follows the
current repository decision and is committed from `engdocs/plans`.
