# Session Module

Read `REQUIREMENTS.md` before changing session behavior, tests, or callers that
depend on session lifecycle semantics. Read `PLAN.md` before changing session
ownership boundaries, deciders, command APIs, events, or extraction sequencing.
(`DESIGN.md` is the long-form architecture document; it has not landed yet and
is iterating under design-review bead `ga-unpr2y`.)

## Purpose

`internal/session` owns the session primitive: start, stop, prompt, observe,
identify, and project provider-backed sessions. Session state is bead-backed;
runtime state is observed through providers; work must survive session churn.

## Reconcile Rule

When code, tests, or docs disagree with `REQUIREMENTS.md`, do not silently pick
one. Reconcile them in the same change:

- If the requirement is right, update code and add or adjust tests.
- If current behavior is right, update `REQUIREMENTS.md` and cite proof.
- If the mismatch is ambiguous, stop and ask for the product rule.

New session behavior needs at least one scenario row in `REQUIREMENTS.md` with
evidence from tests, source, an issue, or a commit.

## Boundaries

- Use `ProjectLifecycle` and lifecycle helper APIs for read-side decisions.
- Inside `internal/session`, lifecycle transition/patch helpers may build
  mutations. Outside `internal/session`, call session-owned command APIs instead
  of applying patch maps or writing lifecycle metadata directly.
- Treat duplicated session decisions in API, CLI, worker, or runtime adapters as
  extraction candidates. Prefer moving the rule into `internal/session` and
  leaving the adapter to gather facts, execute actions, or render responses.
- Extract decisions one cluster at a time. Do not introduce a broad session
  facade before a narrow contract and tests prove it.
- Do not add production session lifecycle or identity metadata writes outside
  `internal/session`. Existing external `SetMetadata*`/bead mutation sites are
  extraction candidates unless they are tests, fixtures, or migration/doctor
  repair code.
- Keep provider-specific runtime behavior behind `internal/runtime`.
- Keep production CLI session creation and lifecycle operations on the
  `internal/worker/handle.go` boundary unless root `AGENTS.md` names an active
  exception.
- Do not make ordinary config names or `template:<name>` values act like live
  session targets.
