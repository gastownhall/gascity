---
title: Schemas
description: Machine-readable schema artifacts published with the Gas City docs.
---

This section publishes generated schema artifacts for tooling. The canonical
JSON files stay in `docs/schema/`, and the links below open the GitHub-hosted
raw artifacts so they work in both local preview and production.

## OpenAPI 3.1

The supervisor HTTP and SSE control plane is published as a raw OpenAPI
document:

- <a href="https://raw.githubusercontent.com/gastownhall/gascity/main/docs/schema/openapi.json" target="_blank" rel="noopener"><code>openapi.json</code></a>

Use this file with Swagger UI, Stoplight, Postman, or client generators. To
regenerate it from the live supervisor schema:

```bash
go run ./cmd/genspec
```

For the narrative API overview, endpoint families, and wire-level notes, see
the [Supervisor REST API](/reference/api) page.

## gc events JSONL Schema

`gc events` list/watch/follow output is published as a small JSON Schema that
references the OpenAPI DTO components instead of duplicating their fields:

- <a href="https://raw.githubusercontent.com/gastownhall/gascity/main/docs/schema/events.json" target="_blank" rel="noopener"><code>events.json</code></a>

Use this file to validate one JSON object line emitted by `gc events`,
`gc events --watch`, or `gc events --follow`. Cursor mode is intentionally
outside the JSON Schema because `gc events --seq` writes a plain-text cursor,
not JSONL.

For the explicit CLI output contract, including scope selection, empty-output
behavior, heartbeat suppression, and cursor formats, see
[gc events Formats](/reference/events).

## City Config JSON Schema

The `city.toml` configuration schema is also published as a raw JSON Schema
document:

- <a href="https://raw.githubusercontent.com/gastownhall/gascity/main/docs/schema/city-schema.json" target="_blank" rel="noopener"><code>city-schema.json</code></a>

Use this file for validation, editor integration, and external tooling. To
regenerate it:

```bash
go run ./cmd/genschema
```
