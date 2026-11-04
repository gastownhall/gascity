# Triage adjacent OpenCode and MiMo SessionStart observations

PM disposition for ga-7g4yi7xa, 2026-11-04, against main
`c10884f82bdcf3e8b1f9670519a6055746b77465`. The architecture pass on
ga-bfvxmcvb identified source-level observations, not reproduced incidents.
Two justify focused follow-up work; the accepted duplication and optional
hardening stay outside the build queue.

All adjacent work is P3 agent-minted machinery under normal admission. None
blocks source build ga-9x5bnoa6, its review, or its deployment. That build is
currently budget-deferred until `2026-11-05T00:00:00Z`; this plan changes
neither its priority nor its deferral.

## Dispositions

| Source item | Disposition | Scope |
|---|---|---|
| 1. Cached step reminder | Diagnose after the source build lands | ga-bb280xl7, together with item 2 |
| 2. Unsequenced lazy/forced prime reads | Diagnose after the source build lands | Same ga-bb280xl7; no speculative repair bead |
| 3. Duplicate beacon and reminder | Retain the ruling's accepted behavior | Existing ga-dv5212wu check (e) supplies real-binary evidence; no new build |
| 4. Synthetic context appears as user text | Obtain a presentation design | ga-5hgak0fw; design may proceed independently |
| 5. Ambient hook-marker scrubbing | Defer pending concrete evidence | No executable child; reopen on observed marker contamination or a maintainer requirement |

Existing ga-meilzh continues to own pi's `session_compact` marker and the
stale `hookIdentityEnv` comment. No scope here duplicates it.

## Ordering and shared landing condition

ga-1tklrva2 is the shared actual-main landing record for ga-9x5bnoa6. It is
open, unassigned and unrouted, with `gate-tracker` and `parent-record` labels.
PM owns its standing watch. Both ga-bb280xl7 and existing smoke follow-up
ga-dv5212wu have native blocks dependencies on it; the smoke follow-up's
original dependency on ga-9x5bnoa6 is retained.

Close the condition only on independently verified main landing, with the
merged SHA/source mapping, freshly fetched main and intended owner changes.
Build/review closure, a pushed branch or budget expiry is insufficient. The
condition does not depend on the smoke result and therefore cannot gate its
own proof. On retirement, reconcile the follow-ups explicitly.

The source build's delivery records must carry the condition and notify PM at
landing or retirement. Future code work arising from either adjacent scope
must retain the same landing prerequisite. The transcript-design artifact can
be prepared from the specified data shape before that landing.

## Freshness diagnosis: ga-bb280xl7

Read the landed plugins and current architecture, including its correction
and amendment. Preserve actual OpenCode/MiMo differences. First establish
whether either observation survived the source build.

For item 1, show what reminder text successive generations receive when work
advances between prime-refresh events. Separate stable persona bytes from
per-turn context and distinguish actual misleading context from an intentional
caching tradeoff. This task chooses no refresh policy.

For item 2, reproduce or refute the controlled ordering in which a lazy read
starts, a forced refresh starts and settles, then the older lazy read settles.
Record what the next generation receives for both plugins. Use the existing
hermetic harness where possible, with explicit ordering rather than timing
sleeps. Such evidence proves plugin cache behavior, not real host delivery.

Return each finding as confirmed, refuted or inconclusive, with pinned source,
commands, logs and practical impact. Recommend the lowest-rung next step while
preserving stable persona text and at-most-once, fail-open delivery. Return
this same scope to PM for any justified repair or precise architecture question;
do not create a parallel implementation. A refuted observation needs no build.

## Transcript design: ga-5hgak0fw

The user outcome is clear attribution: readers can distinguish SessionStart
context from their own prompt and retain access to the original conversation.
PM does not choose hiding versus labeling or a new domain/wire field.

Read `internal/sessionlog/AGENTS.md`, the provider readers and their consumers;
read the API/dashboard rules for any affected surface. Preserve branch selection,
tool attachments, provider turn/entry identities and failure semantics. Never
recover identity or attribution by parsing delivered prompt text.

Provide examples for both providers covering human-only messages, mixed human
and synthetic parts, absent/unknown metadata, and other host synthetic parts.
State default visibility, source attribution and access to original content
for plain transcripts, exports and relevant dashboard views. A broad synthetic
filter must not silently remove human text or unrelated conversation parts.
Any required domain, wire or storage decision goes to architect before code.

Return a reviewable design with measurable implementation criteria. It is an
artifact, not a second plugin repair. Any later implementation has its own
normal review and tests and must show zero complexity growth against its pinned
base without refreshing the complexity baseline.

## Existing smoke scope and limits

ga-dv5212wu remains the sole FR-15 real-binary follow-up: both hosts, checks
(a)–(c) first, at least 20 cold starts, and explicit PASS/FAIL/NOT RUN evidence.
It also owns the title fallback, persona-delivery check, compaction, part-id
source check and first-hook latency measurement defined by the ruling. Host
doubles do not substitute for that evidence.

Item 3's token duplication is already accepted; no suppression work is justified
by this triage alone. An unexpected persona-delivery result belongs to the
existing smoke scope's gc-side follow-up rules. Item 5 has no reproduced
contamination and no new supported-behavior requirement, so it receives no
active work at this priority. These dispositions grant no new transport design,
runtime operation, platform change, deployment blocker or budget override.

Commit and re-verify this plan before closing the PM root as triage complete.
Route and verify the two work handoffs, preserve the shared condition, and leave
the existing smoke/build carriers in place. No external tracker import or
production/test implementation is part of this PM task.
