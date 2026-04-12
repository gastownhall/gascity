# Tutorial Goldens TODO

This directory intentionally tracks temporary workarounds and prose/product gaps
that should be burned down before the tutorial goldens and the canonical
tutorial prose are merged together.

## Open Workarounds

- Tutorial 01: harness satisfies `brew install gascity` as bootstrap instead of
  executing package installation in-suite.
- Tutorial 02: page driver seeds `hello.py` because Tutorial 01 no longer
  creates it, but Tutorial 02 still asks readers to review it.
- Tutorial 03: page driver seeds a live `reviewer` session because Tutorial 02
  does not guarantee one still exists when Tutorial 03 begins.
- Tutorial 03: page driver resolves the spawned reviewer session's concrete
  `session_name` before `peek` because the published `gc session peek reviewer`
  text targets a template label instead of a stable session handle.
- Tutorial 03: page driver waits for the hidden `reviewer` seed to become
  peekable before the visible `gc session peek reviewer` step because
  `gc session new --no-attach` does not guarantee an already-live runtime.
- Tutorial 03: page driver seeds hidden `helper` and `hal` sessions because the
  page renders them later without first establishing the helper agent or those
  sessions.
- Tutorial 03: page driver waits for `mayor` to become peekable and for
  `gc session logs mayor --tail 1` to become readable before the visible
  peek/log steps because native Claude transcript files can lag session start.
- Tutorial 04: page driver nudges the mayor after `gc mail send` so the visible
  `gc session peek mayor --lines 6` step can exercise the communication path in
  a bounded timeframe.
- Tutorial 04: page driver seeds a rig-scoped `my-project/reviewer` because the
  prose shows that route, but earlier tutorials only define a city-scoped
  `reviewer`.
- Tutorials 02, 03, 04, and 06 currently keep rig-qualified reviewer targets
  in the acceptance fixtures (`my-project/reviewer`) until bare rig-local
  shorthand is reliable in acceptance-style paths. Tracking:
  `gastownhall/gascity#632`.
- Tutorial 04: page driver explicitly wakes `mayor` before the nudge/peek flow
  because the page assumes a live mayor session reacts to mail immediately.
- Tutorial 05: page driver seeds `my-project`, `my-api`, and a hidden `helper`
  agent because the page assumes those earlier tutorial steps have already
  happened.
- Tutorial 06: page driver seeds hidden helper/worker/reviewer agent
  definitions because the page assumes those agents were created by prior
  tutorials.
- Tutorial 06: page driver marks the refactor bead `in_progress` before the
  filtered in-progress list because the page assumes live runtime work already
  exists.
- Tutorial 06: page driver removes the hidden blocking dependency before the
  ready query because the page asks for ready pool work before unblocking it.
- Tutorial 07: docs-style top-level `orders/` is mirrored into current
  `formulas/orders/` discovery paths until prose and product converge.
- Tutorial 07: page driver stops the standalone controller before the visible
  `gc start` step because `gc init` currently leaves the city already running.

## Product Follow-Ups

- `gc session new` should adopt the existing async auto-title flow used by the
  API session-create path so manual sessions get Haiku-generated summaries too.
  Tracking: `gastownhall/gascity#500`.
- `gc session list` now shows a `TARGET` column (`alias` if present, otherwise
  `session_name`) alongside `TITLE`. Tutorial prose and examples that treat the
  title column as the command target need reconciliation during the final prose
  merge.
- `gc sling` from a rig working directory should preserve that task/worktree
  context for city-scoped agents. Tutorial 02 currently closes the task bead,
  but the reviewer session runs in `~/my-city` and writes `review.md` there
  instead of `~/my-project`, which breaks the published happy path.
