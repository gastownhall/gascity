# Suspended-store API audit work package

Root: **ga-lt7a60**. Source ruling: **ga-h9rvl1**. Prepared 2026-10-10.
Code inventory checked at `e55d79665711fd560ded4fd4f8e7c66a9f6629e3`.

The sessions-list lookup has been measured forking `bd` into suspended rigs
about 420 times a minute, with lists taking about 45 seconds. These are the
investigator's pre-fix observations recorded in ga-h9rvl1, not new measurements
by PM. The architect specified the first fix in **ga-ehhkgm**, which is still
open and routed to the builder at packaging time. Its effect in the running city
has not been verified.

This package schedules the post-deploy evidence and the remaining architectural
audit. It authorizes no implementation, live-city mutation, deployment or
restart. Architectural decisions remain with the architect. No tracker skill
is installed, so external tracker import is a no-op.

## Work and dependencies

| Bead | Deliverable | Intake and owner | Prerequisite |
| --- | --- | --- | --- |
| ga-pkbg59 | Post-deploy fork census, sessions latency and live-rig active-bead evidence | `needs-mayor`; mayor assigns a read-only observer | ga-ehhkgm, plus verified actual landing and running deployment |
| ga-e544q2 | Complete site classification; live=true and suspended-city rulings | `needs-architecture`; architect | ga-pkbg59's retrievable evidence |
| ga-wsv5i3 | Measured resume-window ruling; authoritative effective-suspension contract | `needs-architecture`; architect | ga-pkbg59's retrievable evidence |

Every child carries `source:actual-pm` and a `discovered-from` edge to ga-lt7a60.
The blocking edges are ga-pkbg59 → ga-ehhkgm and each architecture child →
ga-pkbg59. The two architectural scopes can proceed independently once the
census is complete. All three are P3; the mayor can reprioritize based on
observed residual cost and user impact.

The census uses the documented singleton route to mayor, with `needs-mayor`;
mayor assigns the observer. The architecture children use `gc sling` with
`--nudge`. Neither scope is ready for a builder or validator yet: the eligible
implementation scope depends on observations and architectural decisions.

## Evidence required before the audit

The scheduling dependency is insufficient to prove deployment. Before sampling,
the observer records the fix's landed SHA, running build identity, city path,
effective suspended rigs and UTC window. A closed build bead alone does not
satisfy this prerequisite.

Use the read-only `/proc` census method in **ga-rk4tai CHECKPOINT C1**
(`spawncensus2.py`) for **300 seconds**. Preserve the exact command and raw
artifacts. Count both the assignee/in-progress `bd list` lookup and the
ephemeral assignee/in-progress `bd query` lookup, grouped by cwd rig, command
shape and observed request traffic. Include zero counts and sampling limits.
Record sessions-list latency with running rows and live rigs' `active_bead`
values. Append the evidence to ga-pkbg59 and ga-h9rvl1 before closing the census.

A clean result closes the live=true extension question as specified by the
source ruling. A residual must be tied to observed list/detail traffic where
possible; unobserved attribution remains unknown. Sustained polling and its
measured user impact go to mayor for priority review. PM introduces no new
rate or latency threshold.

## Endpoint inventory and decisions

The architect accounts for all **16 candidate call sites**, resolving lines
to symbols at the deployed/current commit:

| File under internal/api | Source-ruling lines |
| --- | --- |
| handler_beads.go | 156, 172 |
| handler_convoys.go | 14 |
| residency_by_id.go | 130 |
| handler_sling.go | 388, 403 |
| huma_handlers_convoys.go | 83, 162, 263, 300, 408, 454, 488 |
| handler_convoy_dispatch.go | 612 |
| huma_handlers_beads.go | 72, 527 |

`handler_status.go:721` (`statusWorkCounts`) is the reference control and already
skips cache-cold rigs. Each candidate's row names the symbol/route, on-demand
and polling callers, measured frequency, cache-decline/backing-read reachability,
suspension handling, evidence and disposition. Distinguish possible callers
from observed polling. Record removed or moved sites explicitly.

The live=true ruling follows ga-h9rvl1 section 5.5's trigger: detail traffic
linked to lookup forks into suspended rigs, or retired pairs restarting from
detail reads. The suspended-city ruling traces GET /agents and the candidates
under `beadsQuiescent` with declining caches. Existing condition **ga-v3bt9i**
and fix **ga-3xnq72** must be checked for overlap before related work is filed.

## Resume and shared-contract decisions

The root and source ruling describe the resume window differently. The root
says a cold resumed rig remains skipped until the store rebuild; ga-h9rvl1
section 5.7 says effective state flips immediately and backing reads resume.
The architect must reconcile this from deployed code and evidence, then measure
resume-to-refreshed-store elapsed time with timestamps, rig, SHA and maintenance
context. An existing authorized observation or isolated fixture is appropriate;
this package does not authorize a live resume. An unavailable measurement stays
an unmet prerequisite, not an inferred measurement. Follow the `gc trace`
artifact workflow in `engdocs/contributors/reconciler-debugging.md`.

Compare `buildEffectiveSuspendedRigNames`, /status's set and the API helper.
The architect decides whether to consolidate, pin an equivalent contract, or
defer. `internal/suspensionstate` is a candidate from the source ruling, not a
PM placement decision. Account for effective config/runtime suspension, runtime
resume, nil config, empty city path and unreadable state. Preserve the distinct
mutable /status inferred-suspension set and its readiness behavior.

## Completion and subsequent implementation

Each architecture child publishes its classifications/rulings, identifies
measured versus inferred claims, links outcomes from ga-lt7a60 and ga-h9rvl1,
and mails mayor. Close a child only on its own acceptance evidence.

File implementation only for **observed polled, unguarded sites**, with a
reproduction and architecture contract. An explicit-query disposition or clean
census does not produce a speculative build bead. One test-first scope is one
`needs-tests` bead with `Build:` notes, later handed to builder on that same
bead. Architecture can explicitly defer consolidation-only work.

The slow tick in ga-rk4tai(a), ga-bhm5uq's deferred-bead sweep and changes to
cmd/gc's #7115 cache policy are outside this package. No new wire field, State
interface or role decision in Go is proposed. PM completion means this plan is
committed and verified, the dependencies exist, and downstream routes/context
are verified; it does not mean the post-deploy audit has already run.
