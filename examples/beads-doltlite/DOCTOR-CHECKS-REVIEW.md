# Beads DoltLite Doctor Check Review

This note anchors the doctor-check work for bead `gc-eyyi`. Any follow-up
bead that references this work should link back to this file plus the source
references below, so the implementer starts from the actual integration code
instead of a generic pack checklist.

## Required References

- DoltLite README: [../../../doltlite/README.md](../../../doltlite/README.md)
- Builtin Beads DoltLite pack manifest: [pack.toml](pack.toml)
- Builtin Beads DoltLite pack commands: [commands/](commands/)
- Existing pack-local doctor checks: [doctor/](doctor/)
- Beads DoltLite source README: [../../../beads-doltlite/README.md](../../../beads-doltlite/README.md)
- Beads DoltLite source doctor package: [../../../beads-doltlite/cmd/bd/doctor/](../../../beads-doltlite/cmd/bd/doctor/)
- Beads DoltLite source health entrypoint: [../../../beads-doltlite/cmd/bd/doctor_health.go](../../../beads-doltlite/cmd/bd/doctor_health.go)
- Beads DoltLite backend open path: [../../../beads-doltlite/beads_cgo.go](../../../beads-doltlite/beads_cgo.go)
- Non-CGO DoltLite failure path: [../../../beads-doltlite/beads_nocgo.go](../../../beads-doltlite/beads_nocgo.go)
- Gas City `gc bd` store bridge: [../../cmd/gc/cmd_bd_store_bridge.go](../../cmd/gc/cmd_bd_store_bridge.go)
- Gas City built-in pack coverage for the build command: [../../cmd/gc/embed_builtin_packs_test.go](../../cmd/gc/embed_builtin_packs_test.go)
- Gas City pack doctor script contract: [../../internal/doctor/pack_checks.go](../../internal/doctor/pack_checks.go)

## What Was Reviewed

- Gas City pack doctor checks are executable scripts loaded from pack doctor
  declarations. The script protocol is exit `0` for OK, exit `1` for warning,
  exit `2` or other non-zero for error. The first stdout line is the message;
  later lines are details. Scripts receive `GC_CITY_PATH` and `GC_PACK_DIR`.
- This builtin pack source currently has one pack-local doctor check under
  `doctor/check-sqlite3`. A runtime/imported `gascity-packs` copy has more
  DoltLite checks, but that tree is not the source of truth for this bead. Work
  in this rig should update the builtin pack source here.
- This builtin pack exposes operational commands under `commands/`: `build`,
  `client`, `flatten`, `gc`, and `health`.
- The pack `build` command is responsible for building DoltLite-linked `gc`,
  `bd`, and the debug client with `CGO_ENABLED=1`, libsqlite3/libdoltlite
  link flags, install targets, and build-detail JSON.
- Gas City's `gc bd` store bridge switches to DoltLite by reading
  `.beads/metadata.json` or `GC_BEADS_BACKEND` / `BEADS_BACKEND`, then sets
  `GC_BEADS_BACKEND=doltlite` and `BEADS_BACKEND=doltlite` for bd operations.
- The beads source opens DoltLite only in CGO builds. Non-CGO builds return a
  clear `doltlite requires a CGO build` error.
- The DoltLite README documents that libdoltlite installs a CLI, headers, a
  static library, and shared libraries, and that Go integration uses
  `mattn/go-sqlite3` with the `libsqlite3` build tag and CGO link flags.

## Builtin-Pack Audit Question

Before adding more checks, audit whether `beads-doltlite` still needs to be a
builtin pack.

Initial answer from the current code review: keep it builtin for now, but
record exactly which responsibilities force that. The pack is part of the
bootstrap story for DoltLite-backed Gas City installs because it carries the
`build` command that produces DoltLite-linked `gc` and `bd` binaries, the
maintenance commands used by DoltLite health/maintenance orders, and the
pack-local doctor entrypoint that `gc doctor` can discover during builtin pack
materialization. That is stronger than a normal optional workflow pack.

The weak point in the current case for builtin status is that the source pack
does not yet own much doctor coverage: it has only `check-sqlite3`, while the
operationally interesting checks are either in the `bd` source doctor package,
in Gas City's `gc bd` bridge, or in a runtime/imported pack copy. The audit
should decide whether to move those checks into this builtin source pack,
whether to rely on `bd doctor`, or whether the pack can later become a normal
import once install/bootstrap no longer depends on embedded availability.

Reasons it may need to stay builtin:

- It participates in bootstrapping the active beads backend for a city.
- It installs/builds `gc`, `bd`, and DoltLite-linked binaries that may be
  needed before external pack import is reliable.
- It provides doctor checks for runtime materialization and `gc bd` behavior.

Reasons it may not need to stay builtin:

- If the only remaining responsibilities are optional maintenance commands and
  documentation, a normal imported pack may be sufficient.
- If Gas City can reliably import and materialize this pack before any
  DoltLite-backed bead operation, builtin status may be unnecessary.
- If `gc bd` no longer depends on this pack's assets to operate, the pack can
  potentially be externalized.

The audit should identify which startup/install/doctor paths truly require
builtin availability and which can move to normal pack import later.

## How Gas City Doctor Checks Run

Gas City discovers pack doctor checks from pack metadata and conventional
`doctor/<name>/run.sh` files. Each discovered check becomes a `PackScriptCheck`.
The check script runs with its working directory set to the pack directory and
receives `GC_CITY_PATH` plus `GC_PACK_DIR`. A script can also declare a fix
script through `doctor.toml`; `gc doctor --fix` runs that fix script with the
same environment contract.

Pack doctor output is intentionally simple:

- exit `0`: OK
- exit `1`: warning
- exit `2` or any other non-zero: error
- first stdout line: short result message
- remaining non-empty stdout lines: verbose details

This means new `beads-doltlite` checks should be small shell scripts under
`doctor/<check-name>/run.sh` with optional `doctor.toml` metadata. They should
not require changes to Gas City's core doctor implementation unless the pack
doctor protocol itself is insufficient.

## How The Beads DoltLite Pack Works

The builtin pack is embedded from `examples/beads-doltlite/embed.go`. Its
`PackFS` includes `pack.toml`, `doctor`, `commands`, `formulas`, `orders`, and
`assets`. The pack imports and exports the `bd` pack, so regular beads
operations still flow through the `bd` provider wrapper. The DoltLite pack adds
DoltLite-specific operational surfaces:

- `commands/build`: builds DoltLite-linked `gc`, `bd`, and debug client
  binaries using `CGO_ENABLED=1`, `libsqlite3`, and `libdoltlite`.
- `commands/health`: runs a bd-backed health/status probe with
  `BEADS_BACKEND=doltlite` and `GC_BEADS_BACKEND=doltlite`.
- `commands/flatten`: runs `bd flatten --force --json`.
- `commands/gc`: runs `bd gc --force --json`.
- `commands/client`: runs the DoltLite debug client built by the pack.

Gas City's `gc bd` bridge detects DoltLite mode by checking
`GC_BEADS_BACKEND`, `BEADS_BACKEND`, or `.beads/metadata.json` with
`backend = "doltlite"` or `database = "doltlite"`. In DoltLite mode it sets
`GC_BEADS_BACKEND=doltlite` and `BEADS_BACKEND=doltlite` for bd operations
instead of configuring a managed Dolt SQL server.

The beads source code has a separate backend open path: in CGO builds,
`OpenBestAvailable` opens `internal/storage/doltlite` when config says
DoltLite. In non-CGO builds, DoltLite returns a clear error because DoltLite
requires CGO.

## Doctor Check Inventory

These checks should remain pack-local. If a check needs Gas City state, the
pack script should inspect it through supported runtime environment or CLI
output rather than moving logic into unrelated Gas City core doctor code.

- Pack materialization: verify this builtin pack is active, `GC_PACK_DIR`
  points at a readable pack checkout, and the active pack contains expected
  command and doctor directories.
- Backend metadata: verify the active `.beads/metadata.json` selects
  `backend = "doltlite"` or `database = "doltlite"` when the city expects the
  DoltLite backend.
- `gc bd` bridge: verify `gc bd` operations run with
  `GC_BEADS_BACKEND=doltlite` / `BEADS_BACKEND=doltlite` and do not fall back
  to an unrelated global bd database.
- Binary availability: verify `gc`, `bd`, `sqlite3`, and `doltlite` or
  libdoltlite artifacts are discoverable using documented pack discovery rules.
- Linked build details: verify the build command wrote usable build-detail JSON
  for `gc` and `bd`, including tags, CGO state, libdoltlite path, binary path,
  and checksum.
- Linked binary sanity: verify linked `gc` and `bd` report `CGO_ENABLED=1` via
  `go version -m` where available, and warn when the check cannot be performed
  on the platform.
- DoltLite library layout: verify libdoltlite shared/static library and headers
  are present in the configured or discovered build/install location.
- Safe read smoke: run a read-only bd status/list style check against the active
  DoltLite store and report actionable errors.
- Optional write smoke: provide an opt-in disposable-scope write/read/update
  smoke check so routine doctor runs do not mutate production work.
- Maintenance safety: warn before `flatten` or `VACUUM` style maintenance runs
  while the city is not quiesced or active writers are present.
- SQLite/DoltLite pragmas: report WAL/busy-timeout or equivalent state where it
  is inspectable, and warn when the active build path cannot prove lock
  behavior.
- Direct-write contract: report whether direct table writes are expected to be
  versioned and durable; point to the chosen single-writer path when raw writes
  are unsafe.
- Cross-platform behavior: guard Linux/macOS-specific probes and emit
  unsupported warnings instead of failing mysteriously on other systems.
- Remediation text: every warning/error should include a concrete command or
  source reference suitable for a fresh Gas City install.

## Bead Authoring Rule

Any new bead created from this review should include links to:

- this file,
- [../../../doltlite/README.md](../../../doltlite/README.md),
- [pack.toml](pack.toml),
- [commands/](commands/),
- [doctor/](doctor/), and
- [../../../beads-doltlite/README.md](../../../beads-doltlite/README.md).

Do not sling doctor-check work until the implementer has reviewed these
references, the current pack-local doctor scripts, and the actual DoltLite
integration code paths listed above.
