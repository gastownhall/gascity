# Plan: review-formulas CI slowdown current plan

> PM owner: `gascity/pm` - Updated: 2026-06-20
> Sources: architecture bead `ga-pmhi6g.1`, operator decision
> `ga-rncghg.2.1`, mayor mail `gm-wisp-867faph`

## Goal

Route the current builder packages for the 16x
`TestPersonalWorkFormulaCompileAndRun` CI slowdown. The exit criterion is
three consecutive `basic-2-of-2` review-formulas CI runs under 20 minutes.

`gc hook --wait`, `ga-rncghg.2`, and PR #3599 are operator-abandoned and are
not part of this exit path.

## Operator Decisions

- `ga-rncghg.2` is superseded, not shipped. Do not reopen or route it without
  a new operator decision.
- Keep the timeout bumps from #3590 and #3593. Do not do a blanket revert.
- Any timeout-bump revert must be Track-C-justified and must have explicit
  operator confirmation.
- The active path is diagnostics plus the targeted Dolt-query optimization,
  with `ga-pmhi6g.1.3` as the primary remaining tractable speed lever.

## Work Breakdown

| Bead | Title | Routes to | Gate |
|------|-------|-----------|------|
| `ga-pmhi6g.1.1` | Collect `GC_WORKFLOW_TRACE` in review-formula CI failure artifacts | `gascity/builder` | `ready-to-build` |
| `ga-pmhi6g.1.2` | Log ralph check script duration in control dispatcher trace | `gascity/builder` | `ready-to-build` |
| `ga-pmhi6g.1.3` | Optimize review-formula check scripts with targeted Dolt queries | `gascity/builder` | `ready-to-build` |

## Dependency Graph

```text
ga-pmhi6g.1.1 (dispatcher trace artifact)
ga-pmhi6g.1.2 (ralph check duration trace)
ga-pmhi6g.1.3 (targeted Dolt queries)

all three -> timing evidence for 3 consecutive basic-2-of-2 runs under 20m
```

The three packages can proceed in parallel. The diagnostic packages improve
confidence in the root-cause read; the targeted-query package is the current
performance fix path.

## Acceptance Summary

`ga-pmhi6g.1.1` is complete when CI failure artifacts include the control
dispatcher `GC_WORKFLOW_TRACE` alongside `graph-workflow-trace.log`, with
enough detail to see whether `idle_sweeps` accumulates toward 30 seconds.

`ga-pmhi6g.1.2` is complete when the control dispatcher trace brackets ralph
check execution before and after `RunCondition`, including script identity,
outcome, and duration.

`ga-pmhi6g.1.3` is complete when the review-formula check scripts stop scanning
the full bead store with `--all --limit=0` and instead use a query targeted to
the current root bead or equivalent scoped identifier, preserving the existing
check semantics.

## Handoff Notes

- Builder should not treat `gc hook --wait` as approved or required.
- Builder should not revert #3590 or #3593 as part of these packages.
- Timing closeout requires three consecutive `basic-2-of-2` runs under 20m.
- If evidence shows the bottleneck is not trace backoff or check-query cost,
  route a new architecture bead rather than expanding these packages.
