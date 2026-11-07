{{ define "city-understanding" }}
## City: doltlite-gascity

### Purpose

This city exists to develop, test, and maintain the **doltlite backend** for Gas Town and beads. It runs Gas Town's full machinery (mayor, polecats, witnesses, refinery, dogs) on a doltlite storage backend — an embedded prolly-tree SQLite engine with zero server process.

The city is the **primary development and testing ground** for running Gas Town cities without a Dolt MySQL-compatible server.

### The Ship-It Principle

A fix is only real when another user on another machine gets the same result by running the same commands — no manual steps, no ad-hoc edits, no "we already fixed that on this machine." Every change must be in code or config that ships:

- **gascity source** — patches to `cmd/gc/`, `internal/`, or `examples/*/pack/` that compile into the `gc` binary.
- **beads-doltlite source** — patches to `internal/storage/doltlite/` or `cmd/bd/` that compile into the `bd` binary.
- **doltlite C source** — patches to the prolly-tree engine that produce `libdoltlite.so`.
- **city.toml** — declarative config (`backend = "doltlite"`, pack includes, order overrides) that any `gc init` can reproduce.

Manual edits to runtime files (`.gc/system/packs/`, wrapper scripts, installed binaries) are scaffolding. They prove the fix works but are not the fix. Before declaring done, port every manual edit into its upstream source and rebuild.

### Codebases

| Repo | Location | Purpose |
|------|----------|---------|
| `gastownhall/gascity` | `./gascity/` | Gas Town controller, CLI, packs, molecules |
| `dolthub/doltlite` | `./doltlite/` | C library: prolly-tree SQLite fork (libdoltlite.so) |
| `duncan4123/beads-doltlite` | `./beads-doltlite/` | `bd` CLI: beads issue tracker with doltlite storage backend |

### Build Pipeline

```
doltlite C source              beads-doltlite Go source          gascity Go source
  ../configure && make            gc beads-doltlite build bd        gc beads-doltlite build gc
                                  GOFLAGS=-tags=libsqlite3
  doltlite-lib                    CGO_LDFLAGS=-ldoltlite           CGO_ENABLED=1
                                                                    GOFLAGS=-tags=gascity_doltlite_lib,libsqlite3
  → libdoltlite.so ──────────→  bin/bd                             → bin/gc
          └──────────────────────────────────────────────────────→  libdoltlite-linked binaries
```

1. Build `libdoltlite.so` from `dolthub/doltlite` with `make doltlite-lib`
2. Build `bd` from `duncan4123/beads-doltlite` with `gc beads-doltlite build bd`
3. Build `gc` from `gastownhall/gascity` with `gc beads-doltlite build gc` when direct libdoltlite-linked Gas City behavior is needed
4. `bd` binary provides beads CLI; Gas Town's `gc bd` commands shell out to it
5. Gas Town's `gc` binary embeds pack definitions (including the bd pack with `gc-beads-bd.sh` wrapper)

The `gc beads-doltlite build` command is pack-managed. It still requires an existing `gc` binary to run the city and dispatch pack commands. If the user does not already have DoltLite or beads-doltlite sources, the build command bootstraps them into `.cache/beads-doltlite/sources/` from `https://github.com/dolthub/doltlite.git` and `https://github.com/duncan4123/beads-doltlite.git`, then runs `make doltlite-lib` before building `bd` or `gc`. Use `--no-bootstrap` for offline/reproducible builds that must fail instead of cloning, or override with `--bootstrap-dir`, `--doltlite-source`, `--doltlite-repo`, `--bd-source`, and `--bd-repo`.

By default it builds libdoltlite-linked replacements to `<beads-doltlite>/bin/bd` and `<gascity>/bin/gc`. Use `gc beads-doltlite build all` for both binaries. Add `--install` to copy verified binaries to the existing supervisor unit's `gc` path when present, then to the active binary path when it is under `$HOME`, otherwise `$HOME/.local/bin`. Use `--install-dir`, `--bd-install`, and `--gc-install` to choose exact install paths.

The pack is builtin for now because bootstrap has to work before remote pack registries, import pins, or provider-specific source checkouts are available. The goal is not to make DoltLite a permanent hardcoded role in Gas City; the pack keeps all DoltLite behavior in an example pack boundary while the backend and install story stabilize. Once DoltLite-backed beads can be installed as an ordinary remote pack with released binaries and stable source provenance, this can move out of the builtin/example distribution path.

Installing a rebuilt `bd` affects new `gc bd` calls as soon as that `bd` path is first on `PATH`. Installing a rebuilt `gc` affects new `gc` invocations immediately, but a running controller still uses the old in-memory binary until it is reloaded or restarted.

### Backend Architecture

- **Storage**: `libdoltlite.so` — embedded prolly-tree engine. Single `.db` file per database, no server process.
- **Pack layering**: `beads-doltlite` is not a replacement for the `bd` pack. It imports and exports `bd`, so normal beads provider operations still use the materialized `bd` pack's `gc-beads-bd.sh` wrapper.
- **Beads CLI**: `gc beads-doltlite build bd` rebuilds `bd` with `CGO_ENABLED=1`, `GOFLAGS=-tags=libsqlite3`, and `CGO_LDFLAGS=-ldoltlite`.
- **Gas City binary**: `gc beads-doltlite build gc` rebuilds `gc` with `CGO_ENABLED=1`, `GOFLAGS=-tags=gascity_doltlite_lib,libsqlite3`, and `CGO_LDFLAGS=-ldoltlite`. Do not use `gascity_native_beads` for this; upstream uses that tag for its pure-Go native beads path.
- **Gas Town integration**: `gc bd` commands delegate to `bd` via `gc-beads-bd.sh` wrapper script. The wrapper detects `BEADS_BACKEND=doltlite` and routes init/operations through doltlite-specific code paths. Optional native `gc` reads can bypass the CLI for selected hot paths, but writes and general `gc bd` behavior still go through `bd`.
- **No Dolt server**: No MySQL protocol, no port, no `dolt sql-server` process. The dolt pack is conditionally skipped when backend is doltlite (see `embed_builtin_packs.go`).

### Key Differences from Dolt-Backed Cities

| Aspect | Dolt (default) | Doltlite |
|--------|---------------|----------|
| Storage | Dolt SQL server (port 37282) | Embedded prolly-tree `.db` file |
| Beads init | `DOLT_COMMIT()` via MySQL | `dolt_commit()` via SQLite built-in |
| Pack auto-install | `dolt` pack included | `dolt` pack skipped |
| Formulas | `mol-dolt-health`, `mol-dolt-remotes-patrol` | `mol-doltlite-maintenance` |
| `--dolt-auto-commit` | Controls VCS commit timing | Same flag passed but doltlite ignores VCS semantics |

### Database Layout

```
.beads/
  metadata.json        → {"backend":"doltlite","database":"doltlite","dolt_database":"hq"}
  doltlite/
    hq.db              → Single-file prolly-tree database (city-level beads)
    .lock              → flock() sentinel for exclusive write access
  routes.jsonl         → Rig prefix routing

gascity/.beads/        → Rig-level beads store (same layout, database: gc.db)
beads-doltlite/.beads/ → Rig-level beads store
```

### Rig Status

| Rig | Prefix | Repo | Role |
|-----|--------|------|------|
| gascity | `gc-` | `gastownhall/gascity` | Gas Town source, packs, CLI |
| beads-doltlite | `bd-` | `duncan4123/beads-doltlite` | Beads CLI with doltlite backend |
{{ end }}
