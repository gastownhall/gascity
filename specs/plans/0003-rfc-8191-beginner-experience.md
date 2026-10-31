# Plan 0003 — A complete beginner journey

Status: in progress on feat/rfc-8191-beginner-experience. This is an acceptance
contract, not a statement that these capabilities already pass.

Source: https://github.com/gastownhall/gascity/issues/8191
Public sequence: https://github.com/gastownhall/gascity/issues/8191#issuecomment-6544908411

## Product contract

A newcomer chooses a guided experience in the existing init flow. It uses the
existing default Gas City pack; there is no additional starter pack to find,
install, configure, or maintain. The quick start delivers a useful, independently
reviewed, tested local change before introducing more concepts. Tutorials 1–7
continue from that same workspace and data.

One authenticated provider, local Git and the documented dependencies suffice.
No GitHub account, remote origin, gh CLI, hand-edited pack, or planning conversation
is required. Idle means no model turns, including scheduled maintenance turns.

## Implementation order

1. Reconcile existing dependency, validation, doctor, startup, readiness, status,
   wake, failure-policy, native-claim and report PRs against current boundaries.
   Imported PRs are candidate inputs; preserve authorship and test their combined
   behavior. Schema-only workspace records do not satisfy source delivery.
2. Make dependency and provider preflight truthful. Distinguish incomplete checks
   from confirmed failures; bound startup and status; retain actionable diagnostics.
3. Add the beginner choice to init using existing configuration. On-demand named
   sessions and dormant worker pools must consume no model tokens without work.
4. Complete one local task end to end using existing native worktree machinery:
   isolated implementation, independent review, at most two repair rounds,
   deterministic verification, local integration, then source-bead closure and
   safe worker/worktree shutdown.
5. Preserve source continuity across dependent tasks, retry and stop/resume. Offer
   existing-repository/beads adoption with a preview and preserved history.
6. Write the expanded quick start and continuous tutorials; introduce parallelism,
   optional GitHub publication and customization only after first local success.
7. Qualify the exact distributable, embedded pack pins and published instructions.
   Present the working disposable candidate for explicit promotion approval.

## Source delivery invariants

Record the fixed input SHA, accepted predecessor SHA, owned worktree identity,
output SHA, reviewed/tested SHA and integrated SHA. Retry uses the same input;
a dependent task starts from its accepted predecessor. Reject stale review,
foreign/dirty worktrees and conflicting integration. A source bead closes only
after the reviewed and tested commit is integrated. Failed verification, exhausted
repair and interrupted execution leave actionable, resumable work. Cleanup must
never discard unintegrated output. GitHub is an optional publishing adapter.

## Release evidence

Run repository-mandated Bazel unit, policy, generated and affected acceptance/
integration gates. Run pack structural and runtime tests against the actual
candidate binary. Keep logs bound to source commits, tool versions and pack pins.
Use disposable clean installations and real authenticated model execution.

Demonstrate: fresh install to first merged local change; no-origin repository;
independent review rejection and bounded repair exhaustion; validator timeout and
missing dependency; expired auth; startup/readiness failure; dependent task source
continuity; stop/resume; migration preserving existing beads/history; safe cleanup;
zero idle model turns after init and after completion. Repeat supported-platform
installation checks. Never substitute closed beads or schema validation for
verified source integration. Any untested requirement remains explicitly open.

Owner: Chris Sells / RFC8191 candidate work. Stable installations remain unchanged
until the staged candidate is reviewed and approved. Preserve source and evidence
outside disposable staging before teardown.
