---
title: Contributors
description: The shortest path for new contributors to get productive in Gas City.
---

## Read These First

- [Codebase Map](codebase-map.md)
- [Docs Organization](docs-organization.md) when adding or moving pages in
  the published `docs/` tree
- [Architecture Overview](../architecture/index.md)
- [Primitive Test](primitive-test.md)
- [PR Review Handoff Notes](pr-review-handoff.md)
- [Reconciler Debugging](reconciler-debugging.md)
- [Infra-Class Store Routing Audit](infra-class-store-routing-audit.md) when
  opening a store on a bead id — which class a bead belongs to decides which
  store answers, and the audit inventories the sites that got it wrong
- [Huma Usage Notes](huma-usage.md) when touching `internal/api/`,
  OpenAPI generation, or SSE registration
- [Excalidraw Setup](excalidraw-setup.md) when authoring diagrams for the docs
- [Hold and Blocked Label Conventions](hold-label-conventions.md) when a bead
  needs to pause on a specific actor or condition — only `hold:mayor` and
  `hold:external` are canonical
- [Release Gate Criteria Conventions](release-gate-criteria-conventions.md)
  when signing off the "Tests pass" criterion on a `release-gates/*.md`
  deploy gate — it must cite the CI jobs `ci-required` actually gates on
- [Contributor Response and Attribution Conventions](contributor-response-conventions.md)
  when replying to, superseding, adopting, or closing someone else's issue or
  PR — what the contributor is owed and how credit is recorded
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

## Active Proposals

- [Testing Pyramid Audit and Hardening Plan](testing-pyramid-hardening-plan.md)
  for the proposed test-size, ownership, doubles, synchronization, and E2E
  direction

## When to Update Docs

- Update architecture docs when code behavior changes.
- Update design-doc status when a proposal is accepted, implemented, or
  superseded.
- Move exploratory notes, audits, and roadmaps into the archive instead of
  presenting them as current onboarding material.
