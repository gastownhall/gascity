---
title: Schemas
description: Machine-readable schema artifacts published with the Gas City docs.
---

This section publishes generated schema artifacts for tooling. The canonical
JSON files stay in `docs/schema/`, and the download links below use Mint-served
text mirrors so local preview and production both offer a working file
download.

## OpenAPI 3.1

The supervisor HTTP and SSE control plane is published as a raw OpenAPI
document:

- <a href="/schema/openapi.txt" download="openapi.json">Download <code>openapi.json</code></a>

Use this file with Swagger UI, Stoplight, Postman, or client generators. To
regenerate it from the live supervisor schema:

```bash
go run ./cmd/genspec
```

For the narrative API overview, endpoint families, and wire-level notes, see
the [Supervisor REST API](/reference/api) page.

## City Config JSON Schema

The `city.toml` configuration schema is also published as a raw JSON Schema
document:

- <a href="/schema/city-schema.txt" download="city-schema.json">Download <code>city-schema.json</code></a>

Use this file for validation, editor integration, and external tooling. To
regenerate it:

```bash
go run ./cmd/genschema
```
