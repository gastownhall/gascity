# Pool-session sweep lease decision

**Origin:** ga-y1ld0q, found during the architecture review of ga-y9qakg.  
**Priority:** P3 for the sweep correction; P4 for later rule convergence.  
**Recommendation:** adopt the explicit-now pending-create lease rule after ga-y9qakg lands, subject to mayor acknowledgement in ga-5hig0d.

## User impact and decision

An undesired ephemeral pool session can retain its alias and slot after provider start wedges. When it has a pending-create claim and an old `last_woke_at`, today's sweep treats the claim as leased indefinitely. The status and wake work in ga-y9qakg will identify this session as abandoned, but the sweep would still leave it behind.

Adopting the shared lease rule lets the sweep close that old session **only when no runtime is live**. The existing post-create window, configuration and ephemeral checks, and runtime probe still apply. A live runtime or unreadable liveness remains protected. This extends a destructive path, so no child work starts until the mayor explicitly approves ga-5hig0d. If the mayor rejects it, PM cancels the work packages and records the keep-policy rationale on ga-y1ld0q.

## Work packages

| Bead | Owner | Acceptance | Blocked by |
| --- | --- | --- | --- |
| ga-5hig0d | Mayor | Record an explicit approval or rejection of the bounded destructive reach. On rejection, leave the gate open while PM cancels the packages. | — |
| ga-801j53 | Validator | Reproduce the old-claim/no-runtime case and pin the live, unknown-liveness, recent-attempt, in-flight, post-create, and non-ephemeral safeguards at their boundaries. | ga-5hig0d |
| ga-uqvs7n | Builder | Apply the architect's shared lease semantics to the sweep in a separate change; the validator cases pass and no protected session is closed. | ga-5hig0d, ga-801j53, ga-y9qakg |
| ga-8zq7bq | Builder | After the sweep fix, converge reconciler and `internal/session` lease checks with the shared rule and prove parity at edge times without changing fail-closed behavior. | ga-5hig0d, ga-uqvs7n |

Each work package traces to ga-y1ld0q through `discovered-from`. Scheduling blockers are `blocks` dependencies. The architecture ruling in ga-y9qakg already defines the shared rule; this plan does not change its scope. PRs #6929 and #6936, the v2 reconciler groundwork cited by the architect, are already on `origin/main` at planning time.

## Handoff and exit

PM routes the blocked packages to their agents and sends the mayor the concrete approval request. ga-y1ld0q remains open and blocked on ga-5hig0d. After an approval, PM checks the recorded answer, closes the decision bead, and lets the dependency graph release the packages. After a rejection, PM cancels the packages before closing the approval gate and records the rationale on the decision bead.
