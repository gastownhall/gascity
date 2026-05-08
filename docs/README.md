---
title: Docs Workspace
description: Mintlify source files and contributor docs for Gas City.
---

This directory is the source of truth for the Gas City documentation site.

- Mintlify configuration lives in `docs.json`.
- The published docs home page is [`index.mdx`](/docs/index.mdx).
- Downloadable specs live under `schema/`: supervisor OpenAPI,
  `gc events` JSONL, and `city.toml` JSON Schema.
- Preview locally from the repo root with `./mint.sh dev` (Mintlify currently requires Node 22/24 LTS, not Node 25+).
- Run link checks with `make check-docs`.
