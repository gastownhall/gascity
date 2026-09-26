# Plan: Tighten deployer template scope gates (`ga-1fuv7v`)

> Owner: `gascity/pm` - Created: 2026-05-28
> Source: designer handoff `ga-1fuv7v`; triggered by `ga-befwxk`

## Goal

Update the Actual deployer prompt so deploy PRs do not bundle unrelated
feature themes or internal planning documents. The immediate user impact
is cleaner PR review, easier rollback, and fewer deploy conflicts from
source-branch planning artifacts.

The implementation target is:

`packs/actual/deployer/prompts/deployer.md`

## Work breakdown

| Bead | Title | Routes to | Gate |
|------|-------|-----------|------|
| `ga-1fuv7v.1` | Add deployer single-feature-theme gates | builder | ready-to-build |
| `ga-1fuv7v.2` | Add deployer internal-doc contamination scan | builder | ready-to-build |
| `ga-1fuv7v.3` | Integrate deployer gate criterion and consistency pass | builder | ready-to-build |

All children route to builder because design is complete and the target is
a prompt template edit. No further UX design hop is needed.

## Dependency graph

```text
ga-1fuv7v.1 -> ga-1fuv7v.2 -> ga-1fuv7v.3
```

The sequence keeps edits to the same prompt file ordered and preserves the
designer's expectation that the final result can ship as one PR.

## Acceptance summary

1. Rollup deploys fail back to PM when child beads can ship independently.
2. Single-bead deploys fail back to PM when one bead bundles unrelated
   feature themes.
3. Rollup deploys scan candidate commit paths for internal planning docs
   before cherry-pick.
4. Planning/internal doc matches must be listed in `EXCLUDES`; otherwise
   the deployer gates fail back to PM for bead description remediation.
5. The Release Gate Criteria table includes a single-feature-theme
   criterion and remains internally consistent.
6. Failure ownership is clear: PM handles scope/theme/doc contamination;
   builder handles technical implementation failures.
7. Builder records read-through evidence that the prompt additions do not
   contradict existing rollup, `EXCLUDES`, single-bead, or failure handoff
   instructions.

## Out of scope

- Changing deployer executable code.
- Changing the existing `EXCLUDES` strip mechanism.
- Adding automated tests unless executable code changes.
- Reopening or modifying PR #2689 directly.
