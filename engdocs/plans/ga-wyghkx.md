# Inline-task identity in cross-store dry-run refusals

PM root: **ga-wyghkx**. Implementation scope: **ga-7dgyzp** (P2).
Base: `2e0fa4edef514a58b1eb02927f1ab4e191138775`, containing merged PR #6076.

## Problem and provenance

MPR's review of PR #6076 reported that an inline-text dry run can name the
supplied task prose as the bead ID in a cross-store refusal. The refusal itself
is correct, but its diagnostic makes a task that has not been created look like
an existing identified bead.

The source is `fix-executor-output.md`, lines 26–30, in the MPR run directory
`.gc/maintainer-pr-review/gastownhall-gascity/pr-6076/runs/20261007T121243Z`
under the city root. PM read this report and the current refusal/preview paths;
PM did not run a live-city reproduction. Validator owns the meaningful RED.

The report uses the term `--inline-text`. The current CLI infers inline task
prose and carries `Options.InlineText`; no new command-line switch is requested.
Existing accepted inline previews already use `<new-bead-id>` for the identity
of the task that a real run would create.

## Required outcomes

- A refused inline preview identifies a not-yet-created task consistently with
  the existing preview identity. It never presents the supplied prose as an
  existing persisted bead ID, including quoted or multiline prose.
- Refusal remains nonzero and retains accurate source/target store references,
  target identity and actionable re-file guidance. Preserve the existing
  cross-store policy, including its behavior under `--force`.
- A refused dry run performs no runner calls, bead writes, routing, formula or
  convoy creation, or wake/nudge/mail effects. Verify with isolated fixtures.
- Refused existing-bead inputs still identify their actual ID. Same-store
  inline previews retain their creation hint and `<new-bead-id>` rendering.
  Preserve applicable existing batch-ID and error-envelope controls without
  inventing a batch inline feature.
- Record RED and GREEN for actual affected regressions and focused
  `internal/sling` and CLI coverage under `TESTING.md`.

## Work and dependency graph

**ga-7dgyzp → validator → builder** is one `needs-tests` RED + Build + GREEN
scope. It carries `discovered-from:ga-wyghkx`, `plan:ga-wyghkx`, measurable
acceptance criteria and the complete `Build:` scope. Validator finishes its
own molecule, releases the claim, and relabels and routes that same open bead
to builder. There is no second implementation bead.

This bounded diagnostic bug has one scope; it meets the planning molecule's
explicit 1–10-child exit criterion. It needs no separate architecture, UX or
documentation feature. The already-landed #6076 guard is a baseline requirement,
not an open prerequisite. Coordinate shared formatting with **ga-6i4t86**, whose
real-run non-force exit/message parity work is separate; no artificial blocker
or duplicate parity scope is added.

## Completion and limits

PM completes after this artifact is committed and verified clean, the content
root and planning steps close, and the child route/context mail are verified.
That closure does not establish a runtime fix or GREEN result. The implementation
uses its own current-main branch and documented issue/PR through the normal
review/deploy/MPR pipeline. Do not modify the contributor's #6076 branch or
change refusal policy, parsing, lifecycle behavior or unrelated store machinery.
No external tracker skill is installed; import and manifest updates are a no-op.
