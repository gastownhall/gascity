---
title: Overview
description: Authoritative specifications for Gas City file formats and contracts.
---

Specifications are the normative reference for Gas City's file formats and
contracts: what the system accepts, what it does with it, and which behavior
you can rely on. Each spec follows the same register — a status header table,
normative keywords, numbered sections, and an "accepted but inert" section
for surface the format parses but no runtime consumes. When a spec and any
other page disagree, the spec wins; when a spec and the code disagree, the
code wins and the spec has a bug.

| Specification | Covers |
|---|---|
| [Gas City Pack Specification](/reference/specs/pack-spec) | Pack format and loading semantics: directory layout, `pack.toml`, imports, patches, layers |
| [Formula Specification — v1](/reference/specs/formula-spec-v1) | The formulas v1 contract: file format, molecule compilation, container semantics — the default when a formula declares nothing |
| [Formula Specification — v2](/reference/specs/formula-spec-v2) | The formulas v2 contract: file format, graph compilation, and the controller-executed runtime constructs |

New specifications land in this section. For the reasoning register — how to
think about packs and formulas rather than what is normative — see the
[guides](/guides/index).
