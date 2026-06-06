# Rig Binding Phases

Doc state: transition truth

GitHub issues:
- [gastownhall/gascity#588](https://github.com/gastownhall/gascity/issues/588)
- [gastownhall/gascity#587](https://github.com/gastownhall/gascity/issues/587)

This note records the current working POR for rig binding and multi-city rigging.
It intentionally splits the work into two phases:

- Phase A: pre-15.0 path extraction
- Phase B: post-15.0 multi-city rig sharing

Execution posture:

- Phase A belongs to the current 15.0 release branch.
- Phase B does not belong to the current branch or current big-test-pass gate.
- Treat Phase B as a separate post-15.0 follow-on window, not an immediate next
  slice of this branch.

## Why the split exists

These two concerns are related, but they are not the same change:

1. Removing `rig.path` from `city.toml`
2. Allowing multiple cities to bind the same directory

Phase A is a narrow state-model cleanup and should stay as close to zero-risk as
possible for 15.0.

Phase B touches bead storage and redirect behavior. That work stays out of the
15.0 launch path and out of the current integration branch.

## Shared identity model

These terms are used consistently across both phases.

### City name

- Assigned at registration time
- Does not depend on `workspace.name`
- Stable for the operational lifetime of the registered city
- Unique in the machine-local registration space

### City prefix

- Not required to be globally unique
- Operational convenience field, not the canonical machine-global identity

### Rig name

- City-local identity
- Unique only within a city
- Used to correlate a rig declaration in `city.toml` with machine-local binding state

### Rig prefix

- Stable bead namespace for the rig
- User-controlled
- Unique only within a city
- Remains in `city.toml`

## Phase A: remove `rig.path` from `city.toml`

Phase A is the 15.0-safe cleanup.

### Goal

Move only the machine-local rig path binding out of `city.toml`.

### What changes

- `rig.path` leaves `city.toml`
- `rig.name` stays in `city.toml`
- `rig.prefix` stays in `city.toml`
- `rig.suspended` stays in `city.toml` for now
- Bead storage behavior stays effectively as-is
- No multi-city shared-rig semantics are introduced

### Binding key

The correlation key is:

- `(cityPath, rigName)`

That is the join between:

- the rig declaration in `city.toml`
- the machine-local rig binding state

### Rename semantics

If a user edits a rig name by hand in `city.toml`, that is an identity change.

The existing machine-local binding no longer matches and the rig is treated as
unbound until repaired.

The system should not guess intent or silently migrate the binding.

### Doctor contract

- `gc doctor`
  - detect and report binding/name mismatch
  - do not heal by default
- `gc doctor --fix`
  - may heal when the recovery path is unambiguous

### Migration posture

This is a hard break.

- We do not preserve legacy `rig.path` compatibility in `city.toml`
- Migrating users may need to re-register or rebind rigs

### Recovery tooling

If feasible in Phase A, add explicit import/export for machine-local rig bindings:

- `gc rig bindings export <path>`
- `gc rig bindings import <path>`

This is preferred over implicit path inference on a new machine.

The operation is city-scoped:

- it may be invoked from the city root
- or from any rigged directory that resolves back to that city

The exported file represents one city and its rig path bindings, not a
machine-global dump.

#### Phase A import/export file

The exact format may evolve, but the intended shape is:

```toml
version = 1

[city]
path = "/Users/dbox/repos/gc/cities/backstage"
name = "backstage"

[[rigs]]
name = "api-server"
path = "/Users/dbox/src/api-server"

[[rigs]]
name = "frontend"
path = "/Users/dbox/src/frontend"
```

For Phase A, the file may carry either city path, city name, or both. We expect
real usage to clarify which field becomes primary over time.

#### Import validation semantics

Import validates the full file before writing anything.

- If any referenced rig path is missing or invalid:
  - error
  - bind nothing
- If the file references a rig name that does not exist in the target
  `city.toml`:
  - error
  - bind nothing
- If `city.toml` contains rig names that are not present in the import file:
  - allowed
  - those rigs remain unbound

Import should register the city if needed, then apply the rig bindings as one
transaction as far as practical.

### Phase A non-goals

- shared rig directories across multiple cities
- moving bead storage under city `.gc/`
- redirect-driven rig-root `.beads`
- richer multi-city rig records in `~/.gc/cities.toml`

## Phase B: multi-city rig sharing

Phase B is post-15.0 work.

It is intentionally not part of the current 15.0 branch, not part of the
current big-test-pass gate, and should be treated as a separate follow-on
implementation window after release stabilization.

### Goal

Allow two or more cities to bind the same directory safely.

### Rig registry model

For a shared rig directory, the machine-global record should track:

- `path`
- `bindings = [...]`
- optional `default_binding`

Each binding is a tuple of:

- `city`
- `rig`

`default_binding`, if present, must be one of the listed bindings.

Example shape:

```toml
[[rigs]]
path = "/Users/dbox/src/shared-rig"

bindings = [
  { city = "/Users/dbox/repos/gc/cities/backstage", rig = "api-server" },
  { city = "/Users/dbox/repos/gc/cities/switchboard", rig = "api-server" },
]

default_binding = { city = "/Users/dbox/repos/gc/cities/backstage", rig = "api-server" }
```

### Bead storage model

The real bead store moves under the city's `.gc/`.

The rig-root `.beads` artifact becomes a redirect shim whose only job is to
point at the selected city's managed bead store.

### Default switching

`gc rig set-default <path>` updates both, as atomically as possible:

- the `default_binding` record in `~/.gc/cities.toml`
- the rig-root `.beads` redirect target

### Phase B note

This is intentionally deferred until after 15.0 because bead-related code is a
high-risk area and should not be churned on the launch path.

Practical planning rule:

- write down Phase B truth now
- do not mix it into the current branch
- do not let it block 15.0 stabilization

## Field summary

### Keep in `city.toml`

- `rig.name`
- `rig.prefix`
- `rig.suspended` (for now)
- `rig.imports`
- `rig.max_active_sessions`
- `rig.patches`
- `rig.default_sling_target`
- `rig.session_sleep`
- `rig.dolt_host`
- `rig.dolt_port`

### Move out of `city.toml` in Phase A

- `rig.path`

### Legacy fields

These are not part of the new binding model:

- `rig.includes`
- `rig.overrides`
- `rig.formulas_dir`

They belong to the migration / hard-fail story, not the Phase A design.
