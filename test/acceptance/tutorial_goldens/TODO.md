# Tutorial Goldens TODO

This directory intentionally tracks temporary workarounds and prose/product gaps
that should be burned down before the tutorial goldens and the canonical
tutorial prose are merged together.

## Open Workarounds

- Tutorial 01: harness satisfies `brew install gascity` as bootstrap instead of
  executing package installation in-suite.
- Tutorial 03: page driver seeds the `helper` and `hal` sessions explicitly
  because the canonical Tutorial 02 -> 03 flow does not create them.
  Tracking: `gastownhall/gascity#499`.
- Tutorial 03: seeded `hal` session currently uses an explicit alias in the
  workaround so `gc session suspend|wake|close hal` is actually addressable.
  Tutorial prose still needs to either:
  - create `hal` with an alias before first use, or
  - use the actual `session_name`/`TARGET` shown by `gc session list`.
- Tutorial 03: page driver seeds additional `my-api` / `helper` / `worker`
  prerequisite state that the canonical prose assumes but does not establish.
- Tutorial 03: `idle_timeout = "1h"` is compressed to `1s` in the driver so the
  sleep/wake path is observable inside acceptance time bounds.
- Tutorial 06: docs-style top-level `orders/` is mirrored into current
  `formulas/orders/` discovery paths until prose and product converge.

## Product Follow-Ups

- `gc session new` should adopt the existing async auto-title flow used by the
  API session-create path so manual sessions get Haiku-generated summaries too.
  Tracking: `gastownhall/gascity#500`.
- `gc session list` now shows a `TARGET` column (`alias` if present, otherwise
  `session_name`) alongside `TITLE`. Tutorial prose and examples that treat the
  title column as the command target need reconciliation during the final prose
  merge.
