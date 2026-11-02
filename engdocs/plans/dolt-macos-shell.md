# Restore bd/dolt compatibility with the macOS system shell

The bd/dolt example pack must run with the macOS system shell. A shared
runtime parser failure currently reported in Mac CI prevents commands from
starting and hides other Mac regressions.

Planning bead: `ga-z4icexn` (P2, source: mayor). Planning base:
`7cc627b9f5fe8989a8a101787145c00323d3fc41`. This document records the work
breakdown; implementation and platform acceptance remain downstream work.

## Evidence and uncertainty

The intake reports `runtime.sh:270: syntax error near unexpected token ';;'`
in [Mac / make test, job 125503179792](https://github.com/gastownhall/gascity/actions/runs/41510967820/job/125503179792),
including `TestCompactScript*` and `TestCleanup*` failures. The mayor also
reports the same lane red on several unrelated PRs. Those CI findings are
intake evidence, not independently re-run PM results.

At the planning base, `examples/bd/dolt/assets/scripts/runtime.sh` contains
the cited case statement inside the command substitution in
`managed_runtime_listener_pid`. File history identifies `d1037acf8f3`
(PR #8330) as its most recent change. The proposed Bash 3.2 parser cause
requires an actual Bash 3.2 reproduction before being treated as confirmed.

## Work packages

| Bead | User outcome and deliverable | Intake and target | Prerequisite | Effort |
|---|---|---|---|---|
| `ga-b0ubgg1` | Mac users run pack commands without the parser failure; reproduce, pin the regression, and implement the smallest compatible fix | `needs-tests` → validator → builder, same bead | None | Small; interpreter availability is the main risk |
| `ga-2gko963` | Linux users retain listener selection and command behavior; collect existing-check acceptance evidence | `ready-to-build` → builder | `ga-b0ubgg1` landed | Small; reuse matching evidence |
| `ga-qf5sdqj` | Maintainers regain trustworthy Mac results; collect Mac CI or the root's Bash 3.2 parser fallback proof | `ready-to-build` → builder | `ga-b0ubgg1` landed | Small execution effort; Mac queue/access may delay completion |

Each bead carries a `discovered-from` edge to the planning bead. Both
evidence tasks carry same-store `blocks` dependencies on `ga-b0ubgg1`.
After its confirmed landing, those tasks can proceed independently.
A closed fix bead alone does not satisfy the landing guard in their notes.

The first bead owns both regression authoring and implementation. The
other two produce evidence in bead notes using existing checks; they request
no new tests, CI mechanism, or duplicate production change. No UI or
architecture decision is needed. The builder chooses the verified syntax
correction within the existing runtime contract.

## Acceptance

1. Capture the original failure under actual Bash 3.2 at a pinned base,
   including interpreter version, command, SHA, and diagnostic. On the fix
   SHA, Bash 3.2 accepts the complete script and the system-shell path can
   source it without this error. Users need no shell upgrade.
2. Preserve malformed/non-running-holder filtering, preference for a live
   recorded candidate when another listener has a lower PID, and the
   existing no-candidate fallback. Reuse existing listener tests.
3. `bazel test //examples/bd/dolt:dolt_test` passes on Linux. Filtered
   evidence must name real PASS tests with the repository's verbose flags;
   skips and empty filters prove nothing. Reuse identical-SHA evidence.
4. Prefer a successful existing Mac test job on a tree containing the fix.
   The root also accepts actual Bash 3.2 syntax-check success on that script.
   Report that fallback as parser compatibility only; it does not establish
   that the full Mac lane is green. Record unrelated failures separately.
5. Follow repository implementation gates and `TESTING.md`. Complexity
   acceptance is zero growth against the gate's pinned base under the
   shared complexity rule. This feature never updates
   `engdocs/complexity-baseline.json`.

## Risks and handoff

Bash 5 success cannot establish Bash 3.2 compatibility. If a qualifying
interpreter or Mac environment is unavailable, downstream work records the
missing prerequisite and escalates with evidence. No passing result may be
inferred from missing tooling or a skipped test.

Do not change CI interpreters to hide the defect or broaden this into a shell
rewrite. New unrelated defects need concrete follow-ups. The evidence
packages record tested SHAs, commands, results, log or job references, and
confirmed landing provenance.

The tracker import was a skip-safe no-op because its import script was
absent. This plan uses the existing bead-first intake and changes no tracker
manifest. PM commits and pushes this document on
`docs/ga-z4icexn-dolt-macos-shell`, verifies its exact path is clean before
closing the planning bead, then slings each child with `--nudge` and verifies
routes and context mail. Closing the planning bead completes decomposition;
it does not claim the runtime defect is fixed.
