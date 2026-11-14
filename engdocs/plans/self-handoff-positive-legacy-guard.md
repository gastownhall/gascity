# Deliver self-handoff briefs when staging cannot be released

Work bead: `ga-4wvx2jl0` (P1), from architect ruling `ga-di993682`.
This is one test-first scope: validator writes the RED tests, then hands the
same bead to the builder for implementation and delta review.

The reviewed #8666 change stages self-handoff briefs for release by the legacy
reconciler. A new split-storage city now selects v2 by default, whose release
arm is not implemented. Staging there hides the successor's brief and blocks
its next handoff. The architect ruled that an interim guard joins the reviewed
change before the combined change lands.

## Base and delivery order

The implementation base is `origin/builder/ga-i1p65ze`, pinned at
`838d601e5040de016811301731a5132f6910c964`. The staging code is absent from main.
Preserve that base; merge current `origin/main` into an owned working branch
before writing the regression tests. Record the reviewed source SHA, merged
main SHA, merge commit, RED commit and guard commit separately. The source
branch and existing PR remain provenance; do not overwrite either.

Review the combined result as a delta from the already reviewed source. The
guard and staging implementation go through one fresh deploy gate together.
The architect's ruling replaces unguarded deploy bead `ga-d83s330p` and old
guard `ga-ptqkv2w`; verify their live supersession before handing on
this package. Do not add a dependency requiring either obsolete path to land.
The two existing `discovered-from` edges carry lineage and are not blockers.

At the validator-to-builder handoff, explicitly supply the stacked base:

```sh
gc sling gascity/builder ga-4wvx2jl0 --on mol-tdd-build \
  --var base_branch=origin/builder/ga-i1p65ze --nudge
```

`base_ref` alone does not fill the formula variable. The builder's standard
review handoff also copies the build base into `target`; preserve the stacked
ref for delta review and rework, while ensuring the final deployment is the
combined head to main. Resolve that handoff explicitly rather than publishing
onto the old source branch by inference.

## Acceptance

1. Only a restartable self-handoff with a nonempty instance token and a
   positively established legacy reconciler stages its brief. For a qualified
   self-handoff, use this architect-defined truth table:

   | Controller identify result | Config resolution | Delivery | Stderr reason |
   | --- | --- | --- | --- |
   | Known legacy | Not consulted | Staged | None |
   | Known v2 | Not consulted | Immediate | v2 |
   | No answer | Valid legacy | Staged | None |
   | No answer | Valid v2 | Immediate | v2 |
   | No answer | Nil config or resolution error | Immediate | undetermined |

   A known controller answer overrides a conflicting edited config. Unknown
   answers and latch errors must not be mistaken for the zero-valued legacy
   mode. An unset seam must fail toward immediate delivery.
2. Immediate delivery succeeds through the existing ordinary brief path,
   creates a visible brief without staged labels, mail metadata or row keys,
   and prints one explanatory stderr line. Preserve existing restart fencing
   and the outstanding-staged-handoff implementation. Sessions that do not
   qualify retain their existing behavior, make no mode probe and emit no
   new explanatory line.
3. T1 exercises every truth-table row through `doHandoffWithOutcome` and a
   real store, with controller results supplied through the existing seam.
   Assert persisted delivery state and stderr. Tests swapping the mutable
   controller seam must not use `t.Parallel()`.
4. T2 uses the born-split city fixture and no controller: incarnation A and
   its successor B, with distinct instance tokens, each deliver an immediate
   brief. B is not refused as still awaiting staged release. T3 retains
   explicit-legacy staging and existing staging coverage. T4 covers nil
   config without a controller, including no panic. T5 counts zero mode
   probes for non-restartable and tokenless callers.
5. Keep compiling, named RED evidence for the defect and named GREEN evidence
   after the fix. The exact expected-RED cases versus unchanged GREEN controls
   are awaiting the architect's clarification of the source's blanket
   T1–T5 RED sentence; do not manufacture failures in preserved behavior.
6. Use Bazel only: focused verbose runs with actual named PASS lines,
   `bazel test //cmd/gc:gc_test`, staged-handoff/reconciler coverage and
   `make check`. Run `make bazel-sync` for file/import changes. Default-config
   nogo remains required; the integration configuration does not replace it.
7. Report the controller probe's timeout and measured handoff latency both
   with a running controller and without one. Report which truth-table row
   applies at a worker site, respecting its existing no-local-poke boundary.
   A full boot of a v2 city has not been verified; distinguish any new live
   evidence from store-level regression coverage.
8. Complexity has zero growth against the pinned base. Use the full
   complexity-zero-growth protocol when that base is red, inspect diff change
   lines rather than its exit status, and never edit
   `engdocs/complexity-baseline.json`.

## Build and boundaries

The architect specifies a small sibling of `cityRunsV2` that reports positive
legacy knowledge and a reason otherwise. Ask
`controllerLatchedReconcilerMode` first, checking its `ok`; if unanswered and
config is nonnil, call `latchReconcilerMode`, checking its error. Do not use
`!cityRunsV2`. The builder chooses the config/result seam into the handoff
path; an omitted value must not permit staging. Probe only after the two
existing eligibility conditions hold.

Keep `cityRunsV2`, drain behavior, boot fallback, latch registries,
`outstandingStagedHandoff` and the pending session-field registry unchanged.
The future v2 release arm stays in `ga-8xqm9i7`; it will replace this interim
guard after the combined change is on main. No new public documentation or
claim of v2 staged-handoff support belongs in this scope.

Accepted residuals are a mode flip after staging, an older controller paired
with a newer CLI, and the extra identify round trip for eligible self-handoffs.
The guard does not repair pre-existing staged briefs. Keep these limits and
the worker-site finding in the reviewer handoff.
