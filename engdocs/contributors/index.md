---
title: Contributors
description: The shortest path for new contributors to get productive in Gas City.
---

## Read These First

- [Codebase Map](codebase-map.md)
- [Architecture Overview](../architecture/index.md)
- [Primitive Test](primitive-test.md)
- [Reconciler Debugging](reconciler-debugging.md)
- [`CONTRIBUTING.md`](https://github.com/gastownhall/gascity/blob/main/CONTRIBUTING.md)
- [`TESTING.md`](https://github.com/gastownhall/gascity/blob/main/TESTING.md)

## Expectations

- Keep current-state behavior in the architecture docs and future changes in
  the design docs.
- Treat the [Primitive Test](primitive-test.md) as the gate before adding new
  SDK surface area.
- Run `make check` before you open a PR.
- Run `make check-docs` when changing navigation, cross-links, or docs
  structure.

## When to Update Docs

- Update architecture docs when code behavior changes.
- Update design-doc status when a proposal is accepted, implemented, or
  superseded.
- Move exploratory notes, audits, and roadmaps into the archive instead of
  presenting them as current onboarding material.
