# Explicit restart and successor-only handoff delivery

Root decision bead: `ga-ihbl3e.2`  
Technical investigation: `ga-ihbl3e`  
Original incident: `gm-iomz4y`

## Outcome

Gas City will treat an explicit session restart as an operator action that gets
prompt service independently of the periodic full-fleet reconciliation tick.
Shortening that tick remains necessary fleet work, but it is not the latency
contract for an explicit reset.

A self-handoff brief will be made durable before restart begins, but it will
not become ordinary, consumable mail until a successor session incarnation is
confirmed running. Persistence happens before the destructive action; delivery
happens after the successor exists.

These are product requirements. Architecture owns the state machine and the
mechanism that satisfies them.

## Evidence behind the decision

The measured controller tick is a synchronous 600-900 second pipeline against
a configured 30 second patrol interval. One captured tick took 609.9 seconds;
two phases consumed 81.3% of it, and the wake/sleep decision itself consumed
only 5.0 seconds at the end. Across 98 restart stalls in 22.7 hours, the median
restart latency was 682 seconds, the minimum was 372 seconds, and the fleet lost
roughly 19 cumulative session-hours per day.

Provider startup is not the bottleneck. Across 300 successful starts, median
start time was 10.2 seconds and the maximum was 16.9 seconds. The delay is in
when the controller reaches the action, not in booting the successor.

Current self-handoff ordering creates ordinary mail before restartability and
restart persistence are known. The command then waits only until the old
runtime is stopped. If the controller misses the timeout, the old incarnation
continues working while an ordinary handoff brief describing an earlier state
is already waiting in its own inbox. Retrying can also move the restart marker
and extend the outage.

The reported exit-zero symptom needs no product change: `gc handoff` already
returns non-zero on timeout. The observed zero came from a shell pipeline whose
last command was `tail`.

## Policy 1: explicit restart is not periodic work

An explicit restart request must be noticed and advanced without waiting for an
unrelated full-fleet tick to complete. This is a scheduling guarantee, not
permission to bypass session safety.

The implementation must preserve one canonical source of truth for identity,
liveness, kill protection, circuit/quarantine state, and worker-boundary
lifecycle operations. A prompt path may signal or prioritize that logic; it
must not copy the decision tree into a divergent fast path.

Completion is measured in two ways:

1. A deterministic test with periodic patrol advancement withheld (or set far
   beyond the test window) proves that an explicit request still progresses.
2. A production-like observation of at least 20 controller-restartable resets
   records request-to-successor-wake p95 at or below 60 seconds and no sample
   above 120 seconds under a fleet load comparable to the 15-session incident.

The operational measure is a release criterion, not a new timeout constant.
Raising the existing five-minute wait above the observed eleven-minute delay
would only conceal the defect and is not an acceptable primary fix.

## Policy 2: persist first, deliver to the successor

The handoff brief must survive the old runtime disappearing. The command
therefore stages a durable handoff record before requesting restart. That staged
record is provisional: normal inbox reads, mail checks, and startup injection
must not expose it to the incarnation that authored it.

The brief becomes delivered exactly once after the controller confirms a newer
incarnation is running. The old command process cannot be responsible for that
transition because stopping its runtime can kill the process; the architecture
must place release ownership on a durable controller/session boundary.

If restart is rejected, times out, or the controller disappears, the record
remains auditable but unavailable as ordinary mail. Retrying must resume or
supersede the same logical handoff rather than create two deliverable briefs.
Terminal cleanup and retention must be explicit so provisional records cannot
silently accumulate.

User-visible output and events must distinguish these facts:

- the brief was staged durably;
- the restart request was accepted;
- the successor started;
- the brief was released/delivered;
- the attempt failed or timed out.

The CLI must not print `sent` for the staging transition. A side effect is only
claimed after its matching system-of-record transition exists.

## Scope

This plan changes self-handoff for controller-restartable sessions and the
shared explicit-reset scheduling behavior used by `gc runtime
request-restart`.

The following behavior stays unchanged unless the architecture contract finds
a necessary shared invariant and calls it out explicitly:

- `gc handoff --auto`, where the provider manages compaction and no restart is
  requested;
- remote `gc handoff --target`;
- on-demand configured named sessions that intentionally skip restart;
- ordinary periodic reconciliation and wake decisions.

The sibling instrumentation bead `ga-ihbl3e.1` still owns diagnosis of the two
dominant tick phases. This plan neither replaces nor waits for that work.

## Work packages

| Bead | Route | Deliverable | Depends on |
|---|---|---|---|
| `ga-lysw40` | architect | One implementation-ready explicit-restart and staged-handoff state contract | decision root |
| `ga-m4nkvq` | validator | RED contract for prompt explicit-reset progress independent of periodic ticks | `ga-lysw40` |
| `ga-s7a8t6` | validator | RED contract for durable provisional mail and successor-only exactly-once delivery | `ga-lysw40` |
| `ga-kadams` | builder | Prompt explicit-restart scheduling through canonical safety decisions | `ga-m4nkvq` |
| `ga-gylcvg` | builder | Staged self-handoff lifecycle and truthful output/events | `ga-s7a8t6`, `ga-kadams` |

The two RED suites can proceed in parallel after architecture closes. The
restart implementation lands before the mail lifecycle because successor-only
release needs a reliable, attributable successor transition.

## End-to-end acceptance

- An explicit self-handoff/reset progresses while the periodic tick is
  deliberately withheld.
- The old runtime stops and a newer incarnation starts within the operational
  service objective above.
- The brief exists durably before the stop, is unavailable to the old
  incarnation, and is delivered once to the successor.
- Timeout, rejection, cancellation, controller loss, repeated reconcile, and
  retry are all deterministic and leave no duplicate deliverable mail.
- CLI output and typed events never claim `sent`, `stopped`, `started`, or
  `delivered` before the corresponding recorded transition.
- Existing auto, remote, on-demand named-session, ordinary reconciliation,
  class-routed store, and provider behaviors remain green.
- No hardcoded roles or new judgment logic enter Go code.

## Risks to manage

- **Fast-path drift:** a copied wake decision tree would eventually disagree
  with periodic reconciliation. The architecture contract must centralize the
  decision and vary only how it is scheduled.
- **Message loss:** delaying persistence until after stop lets the old process
  die with the only copy. The staged record must exist first.
- **Stale exposure:** treating staged as ordinary mail recreates the incident.
  Every read/injection path needs one authoritative eligibility rule.
- **Duplicate delivery:** retries and controller restarts can replay a release.
  The transition must be idempotent and tied to one logical handoff plus a newer
  incarnation.
- **Store relocation:** message state and session state may live in different
  configured stores. Architecture must preserve their class boundaries and
  define recovery when one write succeeds before the other.
