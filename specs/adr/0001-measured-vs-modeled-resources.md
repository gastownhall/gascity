---
status: accepted
---

# Measured vs. modeled resources, with bounds in a checked ledger

The CI-throughput program (`specs/proposals/0001-increasing-ci-throughput.md`)
needs capacity claims about resources it can never saturation-test: GitHub's
publication path and human work. Pretending to measure them created a
contradiction (Phase 0 demanded saturation curves that Phase 7 rightly forbade
generating against github.com). We split constrained resources into
**measured** (owned infrastructure — runners, caches, queues, the beads
ledger, the event bus — with real saturation curves) and **modeled** (forge,
humans — a registered capacity model with a named validating canary and an
extrapolation cap of 20× that canary). Every registered bound lives in the
checked ledger `specs/proposals/0001-bounds.toml`, guarded by a docsync test;
a bound counts as registered for a run only when its commit predates that run,
so git history is the audit trail. Because the 10,000/day figure is
directional, bounds are ratchets anchored to measured baselines, re-derived at
each sustained volume doubling — not fixed horizon values.

Considered and rejected: a fake-forge simulator presented as measurement
(relaunders the contradiction); absolute horizon bounds (start red at 500× the
current volume and teach people to ignore them); human-minutes budgets (no
collection mechanism exists — interventions are counted from forge audit
records instead, converted to minutes only by sampled time study).
