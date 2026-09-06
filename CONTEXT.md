# Gas City

Shared language for the Gas City platform. The six primitives (Agent, Bead,
Formula, Rig, Pack, Event) are defined normatively in
`docs/getting-started/how-gas-city-works.md`; this glossary covers terms that
crystallized outside that model.

## Change admission and verification

**Logical change**:
One independently attributable behavior change, selectively reversible or
repairable subject to its dependency closure.
_Avoid_: commit, PR, diff (a logical change need not map one-to-one to any of
them)

**Parent outcome**:
The user-visible result requested by the originating Bead or formula run;
reported beside logical-change throughput so decomposition cannot manufacture
apparent value.

**Measured resource**:
A constrained resource on owned infrastructure whose sustainable service curve
is established by controlled saturation testing.

**Modeled resource**:
A constrained resource that cannot be saturation-tested (the forge, human
work); its capacity is claimed only through a registered capacity model.

**Capacity model**:
A registered claim about a modeled resource's sustainable capacity, naming its
evidence basis, the largest canary that validated it, and an extrapolation
factor capped relative to that canary.

**Bounds ledger**:
The checked TOML file holding every registered bound, budget, assumption, and
ratchet; a bound is registered for a run only when the commit recording it
predates that run.
_Avoid_: "registered" without a ledger entry

**Ratchet**:
A bound anchored to a measured baseline that must not regress and is
re-derived at each sustained doubling of accepted-change volume, rather than a
fixed horizon value.

**Admission latency**:
Ready-to-canonical latency over the merge-group and batch build population —
the runs that actually admit code. Distinct from the developer-facing PR
feedback SLO in `TESTING.md`.

**Full-suite-equivalent (FSE)**:
The unit of verification compute: one full-union suite priced at the SHA
pinned in the bounds ledger. Re-priced only at explicit re-baselining entries.

**Intervention**:
A counted, forge-attributable manual action on the admission path (rerun,
dequeue, manual rebase, forced merge). The unit of the human-work budget.
_Avoid_: human minutes (no collection mechanism; minutes come only from
sampled time studies)

**Base-advance rerun**:
A rerun of an identical patch (same `git patch-id`) against a new base SHA.
Classified mechanically, never by judgment.

**Flake**:
An identical patch and base that fails and then passes under the registered
rerun criterion.

**External value anchor**:
An output metric produced outside the beads/formula loop (user-facing issues
closed, shipped release-notes entries) that scheduled automation cannot mint.
