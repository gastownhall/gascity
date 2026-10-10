# Gas City Roadmap

> Last updated: 2026-10-05 · Owner: [@julianknutsen](https://github.com/julianknutsen)

This roadmap names the areas the maintainers are investing in now. It
reflects current thinking, not commitments: priorities move as we learn, and
dates are deliberately absent. It is rewritten at least once per minor
release.

## How this roadmap is used

- **Review priority.** Issues and pull requests inside a priority area get
  reviewed first. Work outside them is still welcome, but expect it to wait.
- **Tracking.** Each priority area has one tracking issue on GitHub. Its
  sub-issues are the work; their state is the progress report.
- **Proposing work.** For a change worth discussing, file an issue with the
  feature form (motivation, impact, risk, verification) before or alongside
  your pull request; small, self-explanatory changes can go straight to a
  pull request. To propose a new priority area, open an issue
  and tag the roadmap owner. See [CONTRIBUTING.md](CONTRIBUTING.md).

## Priority areas for 1.6

### Session and reconciler reliability

**Goal.** Sessions and the work they hold converge on their own. After a
crash, restart, or upgrade, a city returns to the correct state with no
manual intervention, and the keyed v2 session reconciler is the only
reconciler.

**This period.**
- Reconciler v2 fully replaces the legacy tick reconciler, and the legacy
  path is deleted
  ([reconciler v2](engdocs/architecture/reconciler-v2.md)).
- Session and reconciler P0/P1 bugs burn down.

**Tracking issue:** [#7093](https://github.com/gastownhall/gascity/issues/7093)

### Performance

**Goal.** Gas City stays fast and quiet as cities grow. Infrastructure
bookkeeping stops loading the versioned work store, and `gc`, `bd`, and the
reconciler get faster on their hot paths. Numeric targets follow once a
benchmark exists.

**This period.**
- Infra-class beads (order tracking, wisps, controller and session
  bookkeeping) move to a local SQLite store; Dolt keeps the versioned work
  beads
  ([#3926](https://github.com/gastownhall/gascity/issues/3926)).
- `gc` and `bd` performance improvements on hot paths.
- Reconciler performance work.

**Tracking issue:** [#7094](https://github.com/gastownhall/gascity/issues/7094)

### Contributor onboarding

**Goal.** A new contributor, human or agent, finds the rules for the code
they are changing next to that code, understands its intent and invariants,
and lands a change with a well-explained pull request, backed by an issue
when the change warrants one.

**This period.**
- Slim root `AGENTS.md`, colocated area rules, and documented-issue intake
  ([#7090](https://github.com/gastownhall/gascity/pull/7090)).
- Per-package `AGENTS.md` files for the busiest packages: `cmd/gc`,
  `internal/beads`, and `internal/dispatch`, each naming its guard tests.
- Package docs for boundary-critical packages, and a codebase map generated
  from them.
- A curated set of `status/help-wanted` starter issues.

**Tracking issue:** [#7095](https://github.com/gastownhall/gascity/issues/7095)
