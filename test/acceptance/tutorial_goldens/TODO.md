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
- Tutorial 04: page driver nudges the mayor after `gc mail send` so the visible
  `gc session peek mayor --lines 6` step can exercise the communication path in
  a bounded timeframe.
- Tutorial 04: page driver seeds a rig-scoped `my-project/reviewer` because the
  prose shows that route, but earlier tutorials only define a city-scoped
  `reviewer`.
- Tutorial 07: docs-style top-level `orders/` is mirrored into current
  `formulas/orders/` discovery paths until prose and product converge.

## Product Follow-Ups

- `gc session new` should adopt the existing async auto-title flow used by the
  API session-create path so manual sessions get Haiku-generated summaries too.
  Tracking: `gastownhall/gascity#500`.
- `gc session list` now shows a `TARGET` column (`alias` if present, otherwise
  `session_name`) alongside `TITLE`. Tutorial prose and examples that treat the
  title column as the command target need reconciliation during the final prose
  merge.
