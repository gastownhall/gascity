---
title: Docs Workspace
description: Mintlify source files and contributor docs for Gas City.
---

This directory is the source of truth for the Gas City documentation site.

- Mintlify configuration lives in `docs.json`.
- The published docs home page is [`index.mdx`](/index).
- Preview locally with `./mint.sh dev` (Mintlify currently requires Node 22/24 LTS, not Node 25+).
- Run link checks with `make check-docs`.
