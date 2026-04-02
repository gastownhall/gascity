# Formula v2 Transient Retries v0

| Field | Value |
|---|---|
| Status | Draft |
| Date | 2026-03-23 |
| Author(s) | Codex |
| Issue | — |
| Supersedes | — |

Design for first-class transient retry semantics on executable formula v2
steps, with explicit hard-vs-soft terminal behavior.

This design is intentionally narrow. It does not try to solve all
provider-routing, backoff scheduling, or generalized error classification
in one pass. The target is the concrete workflow we care about today:
review legs that sometimes fail transiently on one pool worker and
succeed when rerun on a fresh pool worker.

## Problem

Today, formula v2 has durable graph execution and one first-class retry
primitive:

- Ralph retries an entire logical step by appending new `run/check`
  attempt beads, bounded by `max_attempts`.

That is useful when the retry decision comes from an explicit validation
step. It does not cover a common workflow we already have in production:

- an executable work bead closes with a provider-level or worker-level
  transient failure
- rerunning the same logical step on a different pool worker often fixes it

Current formulas compensate for this in prompt text. For example, Gemini
review steps are told to "soft-fail" themselves in-band by writing
metadata and closing successfully. That is not runtime-enforced policy.

We need a first-class formula/runtime feature for:

1. retrying an executable step after a transient failure
2. bounding retries
3. distinguishing hard failure from soft failure
4. allowing formulas to continue after exhausted optional legs
5. making pooled transient retries run on a fresh session/process

## Current State

### What we already support

- Ralph whole-attempt retry loops via `steps.ralph.max_attempts`
- runtime append/resume semantics for Ralph in
  [`internal/workflow/ralph.go`](/data/projects/gascity/internal/workflow/ralph.go)
- scope abort and skip semantics via `gc.on_fail=abort_scope` in
  [`internal/workflow/runtime.go`](/data/projects/gascity/internal/workflow/runtime.go)
- timeout-only retries for convergence condition scripts in
  [`internal/convergence/condition.go`](/data/projects/gascity/internal/convergence/condition.go)

### What we do not support

- no first-class retry policy on ordinary executable formula v2 steps
- no runtime-level transient vs hard vs soft classification for task beads
- no retry budget or exhausted-policy for review fan-out legs
- no runtime-enforced recycle of the failing pooled session on transient failure
- no aggregated degraded-success signal when optional legs soft-fail

### Important boundary

This design does **not** replace existing session/provider crash recovery.

If a worker dies before it terminally closes a bead, the existing session
and claim lifecycle should continue to requeue that work. The new retry
semantics only apply after an attempt bead reaches a terminal closed state
with an explicit result classification.

In short:

- transport/session failure before close: existing recovery path
- terminal closed attempt with classified failure: new formula retry path

This split is important:

- classified transient failure consumes retry budget and appends a new
  attempt
- crash or hang before a classified result stays on the same attempt and
  should recover through the existing session/provider lifecycle

## Goals

1. Add first-class transient retry semantics for executable formula v2 steps.
2. Keep durable graph semantics: retries append new attempt beads instead
   of reopening old work.
3. Preserve current scope behavior: only final hard failure should abort
   enclosing scopes.
4. Support soft-fail optional legs without prompt-only hacks.
5. Reuse the existing pool reconciler so pooled transient retries naturally
   run on a fresh session/process.
6. Keep v0 small enough to implement and test without redesigning the
   whole workflow runtime.

## Non-Goals

- generalized provider selection or model routing
- probabilistic/LLM-based failure classification in runtime v0
- arbitrary retry rule DSLs in v0
- time-based exponential backoff scheduler in v0
- changing workflow root `gc.outcome` away from the current `pass/fail`
  model

## Terms

### Attempt

One executable bead instance for a logical step.

### Logical step

The stable bead identity that downstream `needs` depend on.

### Transient failure

A failed attempt where rerunning the same logical work may succeed
without changing the logical input. Examples:

- provider rate limit
- short-lived provider unavailability
- worker-specific environment glitch
- ephemeral transport issue after the agent already classified the step

### Hard failure

A failed attempt where rerunning the same logical work is not expected to
help. Examples:

- bad prompt contract
- missing required input
- invalid repository state
- deterministic validation failure

### Soft failure

A terminal degraded result that should not fail the enclosing workflow.
Example:

- optional Gemini leg exhausted retries; Claude + Codex synthesis should
  continue with degraded coverage

## Proposed Formula Surface

Add a new step sub-table:

```toml
[[steps]]
id = "review-gemini"
title = "Code review: Gemini"
assignee = "{reviewer_gemini}"
description = "..."

[steps.retry]
max_attempts = 3
on_exhausted = "soft_fail"
```

### `steps.retry` fields

| Field | Required | Meaning |
|---|---|---|
| `max_attempts` | yes | Total attempt budget including the first attempt |
| `on_exhausted` | no | What to do when a transient failure exhausts the budget: `hard_fail` (default) or `soft_fail` |

### Defaults

```toml
[steps.retry]
max_attempts = 1
on_exhausted = "hard_fail"
```

`max_attempts = 1` means "no retries after the first attempt", but the
step still uses the richer classification and exhausted semantics.

### Validation rules

- `steps.retry` and `steps.ralph` are mutually exclusive on the same step

## Worker Result Contract

V0 uses worker-supplied classification metadata, but only for
`transient` vs `hard`.

Soft-fail is **not** worker-authored in v0. It is a formula/runtime
policy that happens only when a transient failure exhausts its retry
budget and `on_exhausted=soft_fail`.

An attempt closes with one of these states:

### Success

```text
gc.outcome=pass
```

For retry-managed attempt beads, `gc.outcome` is required. A bare close is
treated as an invalid worker result contract, not as success.

### Transient failure

```text
gc.outcome=fail
gc.failure_class=transient
gc.failure_reason=<short-stable-reason>
```

### Hard failure

```text
gc.outcome=fail
gc.failure_class=hard
gc.failure_reason=<short-stable-reason>
```

### Classification rules

- `gc.failure_class=transient` means "retry if budget remains"
- `gc.failure_class=hard` means "fail logical step immediately"
- missing or unknown `gc.failure_class` on a failed attempt is treated as
  `hard`

### Metadata firewall

Retry-managed attempt parsing is fail-closed:

1. `gc.outcome` is authoritative for pass/fail
2. `gc.failure_class` is consulted only when `gc.outcome=fail`
3. Any invalid or contradictory tuple is treated as:

```text
gc.outcome=fail
gc.failure_class=hard
gc.failure_reason=invalid_worker_result_contract
```

Examples of invalid tuples:

- missing `gc.outcome`
- `gc.outcome=pass` with any `gc.failure_class`
- `gc.outcome=pass` with any `gc.failure_reason`
- unknown `gc.failure_class`

### Reason taxonomy

V0 should standardize on short machine-readable reasons, for example:

- `rate_limited`
- `provider_unavailable`
- `worker_glitch`
- `prompt_too_large`
- `missing_input`
- `invalid_repo_state`
- `invalid_worker_result_contract`

The runtime does not need to interpret these in v0; they are for
observability and future policy. `gc.failure_reason` should be a short
enum-like token, not an unbounded free-form blob.

## Graph Shape

For a retry-enabled step `review-gemini`, the compiler emits:

- stable logical bead `review-gemini`
- attempt bead `review-gemini.run.1`
- control bead `review-gemini.eval.1`

Downstream `needs = ["review-gemini"]` depend on the logical bead, not on
an individual attempt.

This is the critical property that lets intermediate attempt failures be
retried without aborting the outer scope.

### Why not just reuse Ralph directly?

The runtime should reuse Ralph append/resume ideas internally, but the
formula surface should stay separate.

Ralph models "run work, then validate with an explicit check step".
This design models "the attempt itself reported whether the failure was
transient or hard, and the formula decides whether exhausted transients
become hard-fail or soft-fail".

They are related, but not the same abstraction.

To avoid ambiguous nesting in v0, `steps.retry` and `steps.ralph` are a
compile-time error when used together on the same step.

## Ownership and Idempotency

V0 explicitly inherits the current graph.v2 ownership invariant:

- one `workflow-control` lane owns graph mutation for a given workflow root
- `workflow-control` remains a singleton pool (`max = 1`)

Within that single-owner model, retry append uses the same crash-resume
pattern as Ralph:

1. `eval.n` starts open with no retry state
2. controller writes `gc.retry_state=spawning`
3. controller appends `run.(n+1)` and `eval.(n+1)` if they do not already
   exist
4. controller writes `gc.retry_state=spawned`
5. controller terminally updates the logical step or closes `eval.n`

If the controller dies after step 2 or 3, the still-open `eval.n` is the
recovery unit. On re-entry:

- `spawning` means "resume append/finalize"
- `spawned` means "finalize without cloning again"

This is deliberately the same shape as the existing Ralph resume logic,
not a second concurrency model.

### Monotonic terminal close

Logical steps gain:

```text
gc.closed_by_attempt=<n>
```

Only the highest attempt number may terminally close the logical step.
If `eval.n` observes `gc.closed_by_attempt >= n`, it becomes a no-op.

This prevents stale or replayed eval work from double-closing the logical
step with contradictory dispositions.

## Runtime Semantics

### Pass

If the attempt closes pass:

1. close `eval.n`
2. close the logical step with:
   - `gc.outcome=pass`
   - `gc.final_disposition=pass`
   - `gc.closed_by_attempt=<n>`

### Hard failure

If the attempt closes fail with `gc.failure_class=hard` (or missing):

1. close `eval.n`
2. close the logical step with:
   - `gc.outcome=fail`
   - `gc.final_disposition=hard_fail`
   - `gc.closed_by_attempt=<n>`

The enclosing scope then behaves exactly as it does today.

### Transient failure with remaining budget

If the attempt closes fail with `gc.failure_class=transient` and
`attempt < max_attempts`:

1. record retry state on `eval.n`
2. append `run.(n+1)` and `eval.(n+1)`
3. keep the logical step open
4. record on the logical step:
   - `gc.retry_count=<n>`
   - `gc.last_failure_class=transient`
   - `gc.last_failure_reason=<reason>`

### Transient failure with exhausted budget

If the attempt closes fail with `gc.failure_class=transient` and
`attempt == max_attempts`:

- `on_exhausted=hard_fail`:
  - close logical step with `gc.outcome=fail`
  - `gc.final_disposition=hard_fail`
  - `gc.exhausted_attempts=<n>`
  - `gc.closed_by_attempt=<n>`

- `on_exhausted=soft_fail`:
  - close logical step with `gc.outcome=pass`
  - `gc.final_disposition=soft_fail`
  - `gc.exhausted_attempts=<n>`
  - `gc.closed_by_attempt=<n>`

In both cases the final disposition is chosen by formula policy, not by
the worker.

## Session Semantics

The motivating case is pooled review work where a fresh polecat often
fixes the problem. V0 should get that behavior by recycling the pooled
session, not by encoding session exclusion on the retry bead.

### Pooled routes

If a pooled attempt closes with:

```text
gc.outcome=fail
gc.failure_class=transient
```

then:

1. workflow-control appends the next attempt if retry budget remains
2. the current pooled session is drained/exited after persisting the
   terminal result
3. the running pool count drops
4. the pool's normal desired-count logic sees ready retry work and starts
   a fresh session/process to claim it

This is intentionally the same pool lifecycle we already have today.
Nothing special is needed on the retry bead beyond "there is ready work
for this pool."

Fresh means a fresh session/process incarnation, not necessarily a
different slot name. If `polecat-2` dies and the pool later creates a new
`polecat-2`, that is still a fresh execution environment.

### Fixed routes

If the assignee is a fixed single-agent lane rather than a pool, there is
no freshness concept. A transient failure still appends the next attempt,
but the same lane may execute it.

### Crash or hang before a classified result

If a worker crashes, hangs, or is killed before writing a terminal
classified result, that is not a formula retry event yet.

That case stays in the existing recovery lane:

1. the same attempt bead is recovered/requeued by session/provider logic
2. retry budget is not consumed
3. no `run.(n+1)` is appended yet

This is the critical split between:

- classified transient failure: formula retry
- unclassified crash/hang: same-attempt recovery

### Provider consequences

V0 should be explicit about what "recycle the session" means:

- `subprocess` provider:
  the current process exits/drains after reporting transient failure, so
  the pool starts a fresh process for the retry attempt
- interactive providers like `tmux`:
  the current session should be marked draining/quarantined and replaced
  with a fresh session incarnation rather than nudging the same live
  session again

## Metadata Surface

### Logical bead

```text
gc.kind=retry
gc.max_attempts=<n>
gc.retry_count=<attempts-already-spawned>
gc.final_disposition=pass|soft_fail|hard_fail
gc.last_failure_class=<transient|hard>
gc.last_failure_reason=<reason>
gc.exhausted_attempts=<n>
gc.closed_by_attempt=<n>
```

### Attempt bead

```text
gc.kind=run
gc.attempt=<n>
gc.retry_parent=<logical-id>
gc.completed_by_session=<session-name>    # when available
```

### Eval bead

```text
gc.kind=retry-eval
gc.attempt=<n>
gc.retry_state=<spawning|spawned>
gc.next_attempt=<n+1>
```

## Workflow-Level Outcome

V0 keeps the existing root outcome model:

- any hard-failed logical step can still fail the workflow
- soft-failed logical steps do **not** fail the workflow

To preserve degraded-success visibility, the finalizer should also
surface:

```text
gc.final_disposition=pass|soft_fail|hard_fail
gc.soft_fail_count=<n>
```

Reduction rule:

- if any logical step is `hard_fail`: workflow `gc.outcome=fail`,
  `gc.final_disposition=hard_fail`
- else if any logical step is `soft_fail`: workflow `gc.outcome=pass`,
  `gc.final_disposition=soft_fail`
- else: workflow `gc.outcome=pass`, `gc.final_disposition=pass`

Meaning:

- `gc.outcome=pass`, `gc.final_disposition=pass`: clean success
- `gc.outcome=pass`, `gc.final_disposition=soft_fail`: success with
  degraded optional coverage
- `gc.outcome=fail`, `gc.final_disposition=hard_fail`: terminal failure

## Recommended Formula Usage

### Optional review leg

Gemini-like optional leg:

```toml
[[steps]]
id = "review-gemini"
assignee = "{reviewer_gemini}"
condition = "!{{skip_gemini}}"
description = "..."

[steps.retry]
max_attempts = 3
on_exhausted = "soft_fail"
```

### Required review leg

Claude/Codex-like required leg:

```toml
[[steps]]
id = "review-claude"
assignee = "{reviewer_claude}"
description = "..."

[steps.retry]
max_attempts = 3
on_exhausted = "hard_fail"
```

## Operator / Prompt Ergonomics

Writing raw metadata by hand is error-prone. V0 should therefore also
ship a tiny helper command for workers, for example:

```bash
gc workflow finish --status pass
gc workflow finish --status fail --class transient --reason rate_limited
gc workflow finish --status fail --class hard --reason missing_input
```

This is not the core runtime mechanism, but it is the right UX for LLM
workers and shell workers alike.

## Observability

Emit or persist enough state to answer:

1. how many retries happened?
2. why did we retry?
3. which session failed?
4. did the workflow succeed cleanly or with degraded coverage?

Minimum fields:

- on attempts:
  - `gc.failure_class`
  - `gc.failure_reason`
  - `gc.attempt`
  - `gc.completed_by_session`
- on logical steps:
  - `gc.retry_count`
  - `gc.final_disposition`
  - `gc.exhausted_attempts`
- on workflow root:
  - `gc.final_disposition`
  - `gc.soft_fail_count`

## Testing Plan

### Deterministic runtime tests

Add store-driven tests similar to the existing Ralph and scope tests:

1. transient fail on attempt 1 -> attempt 2 appended
2. transient fail to exhausted budget -> hard fail
3. transient fail to exhausted budget with `on_exhausted=soft_fail`
4. explicit hard failure -> logical fail
5. malformed worker result contract -> hard fail with
   `invalid_worker_result_contract`
6. stale `eval.n` cannot close a logical step already closed by attempt
   `n+1`
7. pooled transient failure recycles the current session and leaves the
   retry attempt to normal pool reconciliation

### Live integration tests

Use the existing subprocess graph harness:

1. fake polecat transient failure on first attempt, success on second
2. exhausted optional Gemini leg still lets synthesis continue
3. exhausted required Claude leg fails the workflow
4. pooled transient failure causes the old subprocess worker to exit and a
   fresh pool process to claim the retry attempt

## Deferred Work

Explicitly deferred from v0:

- time-based backoff and jitter
- rule-based or exec-based classifier plugins
- retrying whole fan-out groups as one unit
- automatic provider failover instead of same-step retry
- richer workflow root aggregation beyond `soft_fail_count`

## Why this is the right v0

This gives us the specific missing capability we need right now:

- durable retry for transient review-leg failures
- explicit hard vs soft terminal behavior
- fresh pooled retries by reusing the existing pool reconciler

without mixing it up with:

- session crash recovery
- prompt-only conventions
- a generalized scheduler redesign

It also composes cleanly with the runtime we already have:

- stable logical beads
- append-only retry attempts
- existing scope/finalize control semantics
- existing deterministic workflow-control lane
