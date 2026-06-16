---
title: "Exec Beads Provider"
---

The Bead is the WHAT primitive — a unit of work — and the bead store is the
machinery that persists beads (tasks, messages, convoy members, and the
ephemeral beads a formula run materializes). See
[How Gas City Works](/getting-started/how-gas-city-works) for where the Bead
sits among the six. Gas City's exec beads provider delegates each `beads.Store`
operation to a user-supplied script, the same pattern the
[exec session provider](/reference/exec-session-provider) uses for sessions. It
makes the bead store a pluggable boundary: change one config line and Gas City
persists beads through your script instead of the default `bd` (Dolt-backed)
store.

## Why

The default `bd` provider couples the store to a specific stack — the Go `bd`
CLI wrapping a Dolt SQL database. A script backend lets you point Gas City at:

- **A different beads engine** — for example [`beads_rust`](https://github.com/Dicklesworthstone/beads_rust)
  (`br`), a SQLite + JSONL hybrid with no JVM/Dolt dependency.
- **Custom persistence** — bead writes that also trigger S3 snapshots, git
  commits, or other durability strategies.
- **Alternative databases** — Postgres, SQLite, flat files, or any backend
  reachable from a CLI.

## Usage

Select the provider in `city.toml`:

```toml
[beads]
provider = "exec:/path/to/gc-beads-br"   # absolute path
# provider = "exec:gc-beads-br"          # or a bare name resolved on PATH
```

`GC_BEADS` overrides the config for a single command:

```bash
export GC_BEADS=exec:gc-beads-br
```

Resolution order is `GC_BEADS` → `[beads] provider` → `bd` (the default).

## Calling Convention

The script receives the operation name as its first argument:

```
<script> <operation> [args...]
```

The script is exec'd directly — no shell. Mutations pass their payload as JSON
on **stdin**; reads return JSON on **stdout**. Each invocation has a 30-second
timeout. The script is spawned fresh per operation; there is no long-lived
process to manage.

## Exit Codes

| Code | Meaning |
|------|---------|
| 0 | Success (stdout holds the result, when any) |
| 1 | Failure (stderr holds the error message) |
| 2 | Unknown operation (treated as success — forward compatible) |

Exit code 2 is the forward-compatibility mechanism: when Gas City adds an
operation, an older script returns exit 2 and the provider treats it as a
no-op success, so scripts only implement the operations they care about.

The one exception is `ready` invoked with contract arguments (such as
`--include-ephemeral`): there, exit 2 is surfaced as an error rather than a
silent success, so runnable ephemeral beads are never quietly hidden from the
orchestrator. A script that cannot serve those ready queries should exit 1 with
a stderr message instead.

## Operations

These are the subcommands a backend implements. Bead reads return a JSON array
(`[]` when empty, never `null`).

| Operation | Invocation | Stdin | Stdout |
|-----------|-----------|-------|--------|
| `create` | `script create` | create request JSON | bead JSON |
| `get` | `script get <id>` | — | bead JSON |
| `update` | `script update <id>` | update request JSON | — |
| `close` | `script close <id>` | — | — |
| `reopen` | `script reopen <id>` | — | — |
| `delete` | `script delete --force <id>` | — | — |
| `list` | `script list [--status=S] [--assignee=A] [--type=T] [--limit=N]` | — | bead JSON array |
| `children` | `script children <parent-id>` | — | bead JSON array |
| `list-by-label` | `script list-by-label <label> 0` | — | bead JSON array |
| `ready` | `script ready [--include-ephemeral]` | — | bead JSON array |
| `set-metadata` | `script set-metadata <id> <key>` | value (raw bytes) | — |
| `dep-add` | `script dep-add <id> <depends-on-id> <type>` | — | — |
| `dep-remove` | `script dep-remove <id> <depends-on-id>` | — | — |
| `dep-list` | `script dep-list <id> <down\|up>` | — | dependency JSON array |

The remaining `Store` methods are **composed** by the provider from the
subcommands above, so a script that implements this table supports them with no
extra work:

- `ListOpen`, `ListByAssignee`, `ListByMetadata` route to `list`; the provider
  applies the status/assignee/metadata filter after the script returns.
- `CloseAll` is a sequence of `close` calls; `SetMetadataBatch` a sequence of
  `set-metadata` calls (neither is atomic — design scripts to be idempotent).
- `Tx` runs its callback sequentially against the same store; writes are not
  grouped into a single transaction.
- `Ping` runs `list` to confirm the script is reachable.

### `ready` and ephemeral tiers

`ready` returns open, unblocked beads. The orchestrator's demand scans pass
`--include-ephemeral`, and the script must then include ephemeral run beads in
its output; Gas City applies the caller's tier filter after parsing. Future-
deferred beads (`defer_until` in the future) are filtered at the store boundary,
so a script may return them and let the provider drop them.

### Wire format

Beads are exchanged as JSON matching the `bd --json` shape:

```json
{
  "id": "WP-42",
  "title": "digest run",
  "status": "open",
  "type": "task",
  "created_at": "2026-02-27T10:00:00Z",
  "updated_at": "2026-02-27T10:00:00Z",
  "assignee": "",
  "parent_id": "",
  "ref": "",
  "needs": [],
  "description": "",
  "labels": ["order-run:digest", "pool:dog"],
  "metadata": {},
  "ephemeral": true,
  "defer_until": "2026-02-27T11:00:00Z"
}
```

The script assigns `id`, `status`, `created_at`, and `updated_at` on `create`.
On `create` the provider defaults a missing `type` to `task` before calling the
script; on every read it normalizes a missing or empty `status` to `open`.
`metadata` values may be any JSON type but are coerced to strings.
Preserve `ephemeral` and `defer_until` on every read path so the provider can
keep run beads out of normal work queries.

The **create request** is a subset (`title`, `type`, `priority`, `labels`,
`parent_id`, `ref`, `needs`, `description`, `assignee`, `from`, `metadata`,
`ephemeral`, `defer_until`). The **update request** carries only the fields
being changed (`title`, `status`, `type`, `priority`, `description`,
`parent_id`, `assignee`, plus `labels`/`remove_labels` and a `metadata`
overlay); omitted fields are left unchanged, and `labels` appends rather than
replaces. `dep-list` returns objects of `{ "issue_id", "depends_on_id", "type" }`.

### Conventions

- **JSON on stdin for mutations** avoids shell-quoting issues with titles,
  descriptions, and labels; `set-metadata` passes its value as raw stdin bytes
  so it can hold newlines, quotes, or JSON.
- **Not found → exit 1** with `not found` (or `no issue found`) on stderr;
  `get`, `close`, and `reopen` map that to the platform's not-found error.
- **Idempotent writes** — `close` on an already-closed bead should still exit 0;
  batch and transaction operations are not atomic.
- **Return supersets** — `list`, `children`, and `list-by-label` should return
  every matching bead the script can see, including ephemeral ones; the provider
  applies the caller's tier, query filters, and result limit after parsing (it
  always passes `list-by-label` a limit of `0`).
- **Three-state status** — the store vocabulary is `open`, `in_progress`, and
  `closed`. A backend with a richer status set maps the extras onto these three.

## Environment Variables

The provider sets these before exec'ing the script:

| Variable | Value |
|----------|-------|
| `GC_PROVIDER` | the full `exec:<script>` selector |
| `GC_STORE_ROOT` | the scope root directory (the city or rig directory) |
| `GC_STORE_SCOPE` | `city` or `rig` |
| `GC_BEADS_PREFIX` | the bead-ID prefix for this scope |
| `GC_RIG` / `GC_RIG_ROOT` | the rig name and root path (rig scope only) |

The parent environment is otherwise inherited, except that `BEADS_*` and
`GC_DOLT_*` keys are stripped so a script cannot accidentally inherit the
default store's backend configuration.

## Writing Your Own Script

1. Start from `contrib/beads-scripts/gc-beads-br` as a template.
2. Implement the operations your backend supports; let the provider compose the
   rest.
3. Return exit 2 for operations you do not implement (but exit 1 for
   `ready --include-ephemeral` if you cannot serve it).
4. Test with `GC_BEADS=exec:./your-script gc status`.

### Minimal script (create / get / list)

```bash
#!/bin/sh
op="$1"
case "$op" in
  create) payload=$(cat); my-store create "$payload" ;;   # echoes the new bead JSON
  get)    my-store show "$2" ;;                            # echoes the bead JSON
  list)   my-store list --json ;;                          # echoes a JSON array
  *)      exit 2 ;;
esac
```

## Shipped Scripts

See `contrib/beads-scripts/` for maintained implementations:

- **gc-beads-br** — [`beads_rust`](https://github.com/Dicklesworthstone/beads_rust)
  (`br`) backend: SQLite + JSONL, no Dolt dependency. Dependencies: `br`, `jq`,
  `bash`.
- **gc-beads-k8s** — runs `bd` inside a "beads runner" pod via `kubectl exec`,
  connecting to Dolt running as a StatefulSet in the cluster. Dependencies:
  `kubectl`, `jq`, `bash`.

The bundled `bd` provider itself ships an exec script (`gc-beads-bd.sh`) under
the `bd` pack's assets, which is how the default Dolt-backed store is driven
through the same protocol.
