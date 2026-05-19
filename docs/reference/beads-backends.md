---
title: "Beads Backends"
---

Gas City stores work in beads. The `[beads]` section has two layers:

```toml
[beads]
provider = "bd"
backend = "dolt"
```

`provider` selects the Gas City store adapter. `backend` selects the storage
engine used by the `bd` provider.

## Providers

| Provider        | Purpose                                                                       |
| --------------- | ----------------------------------------------------------------------------- |
| `bd`            | Default production provider. Gas City shells out to the Beads CLI (`bd`).     |
| `file`          | JSON file store for lightweight local/tutorial use.                           |
| `exec:<script>` | External store provider. The script implements Gas City's JSON CRUD protocol. |

`GC_BEADS` overrides `[beads].provider`.

## `bd` Backends

The `bd` provider keeps Gas City on the normal Beads path. Backend selection
happens inside `bd`, based on `.beads/metadata.json`.

| Backend     | Config                        | Lifecycle                                                                      |
| ----------- | ----------------------------- | ------------------------------------------------------------------------------ |
| Dolt server | `backend = "dolt"` or omitted | Gas City starts/manages the Dolt SQL server and auto-includes the `dolt` pack. |
| doltlite    | `backend = "doltlite"`        | Gas City initializes `bd` with doltlite and does not start Dolt.               |

`GC_BEADS_BACKEND` overrides `[beads].backend`.

## doltlite Mode

Use doltlite when you want the normal `bd` command surface without a managed
Dolt SQL server:

```toml
[beads]
provider = "bd"
backend = "doltlite"
```

In this mode:

- Gas City still uses `BdStore` and normal `bd` subprocess calls for CRUD.
- The `bd` pack remains active.
- The `dolt` pack is not auto-included.
- `gc-beads-bd.sh start`, `health`, and `probe` are no-ops.
- `gc-beads-bd.sh init <dir> <prefix> [database]` runs `bd init --backend=doltlite`.
- Managed-Dolt env vars such as `GC_DOLT_HOST`, `GC_DOLT_PORT`, and
  `BEADS_DOLT_SERVER_PORT` are cleared before `bd` runs.

The resulting `.beads/metadata.json` identifies the backend:

```json
{
  "database": "doltlite",
  "backend": "doltlite",
  "dolt_mode": "embedded",
  "dolt_database": "tr"
}
```

## Difference From `exec`

Use `exec:<script>` only when the whole bead store lives outside Beads, such
as a Rust implementation. The script must implement Gas City's CRUD protocol.

doltlite is different: it is a Beads storage backend. Gas City should continue
to use `provider = "bd"` and let `bd` dispatch to doltlite.
