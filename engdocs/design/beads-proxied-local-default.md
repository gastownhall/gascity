---
title: Beads proxied-local default
description: How a Gas City scope becomes a bd-owned proxied-server store, who owns the Dolt process, and what gc start/stop guarantee.
---

# Beads proxied-local default

> **Status:** Accepted — implemented against beads `v1.3.0-rc.2`.
> **Companion to:** `engdocs/design/beads-dolt-contract-redesign.md` (the bd+Dolt
> contract this specialises), `docs/reference/exec-beads-provider.md` (the exec
> lifecycle protocol the adapter speaks).

A fresh `gc init` produces a **bd-owned proxied-server** store: bd runs a
detached `bd db-proxy-child`, which runs `dolt sql-server` rooted at
`<scope>/.beads/dolt`. Gas City never spawns Dolt for such a scope. Existing
direct/server, embedded, DoltLite and external scopes are untouched.

The escape hatch is explicit: `gc init --beads-transport direct --beads-target
local` (or `GC_BEADS_TRANSPORT`/`GC_BEADS_TARGET`) yields a **bd-owned
server-mode** store — `bd init --server`, with bd's own
`.beads/dolt-server.pid`/`.port`, started and stopped by `bd dolt start`/`bd
dolt stop`. It is not the legacy gc-managed server. The gc-managed server is
reached only by a scope that already has one: an existing city whose metadata
says `dolt_mode: server`, or the legacy `--dolt-host` alias used without a
selector. `gc rig add` has no selectors — a rig inherits the city's topology,
and only from a city that is itself provider-owned (see below).

### bd version floor

Every **fresh** provider-owned init needs bd ≥ 1.3.0 — the default one as much
as a selector-driven one, because both journal a pending ownership record and
`checkHardDependencies` raises the floor whenever one exists
(`bdFreshProviderMinVersion`). On an older bd, `gc init` refuses typed before
touching the store: `missing required dependencies: bd (found vX, need
v1.3.0+)`. There is no silent fallback to the legacy path. The only fresh init
that stays on the 1.0.4 floor is the legacy `--dolt-host` alias used without
`--beads-transport`/`--beads-target`, which is a client-only external
binding. `beads.bd_compatibility` in `city.toml` is a different knob: it
selects which bd CLI *semantics* gc relies on at runtime and does not lower or
raise this init floor.

### External targets

`--beads-target external` requires `--dolt-host`, `--dolt-port` and
`--dolt-database` (or `GC_DOLT_HOST`/`GC_DOLT_PORT`/`GC_DOLT_DATABASE`). The
database is required because it names *which* database on a server somebody
else operates; letting bd derive one from the issue prefix would attach to, or
create, the wrong database there.

`--dolt-project-id` is **not** required with a selector, and is not honoured on
that path: gc journals host/port/database only and hands bd just `--database`,
and bd resolves `project_id` itself (adopting the hosted database's
`_project_id` or minting one). It stays required for the legacy `--dolt-host`
alias, which is the one path that writes the identity
(`contract.WriteProjectIdentity`).

## Topology authority is bd

`dolt_mode` in `<scope>/.beads/metadata.json` is the persisted truth. bd writes
it there and nowhere else (`config.yaml` carries no `dolt.mode`), and it
git-commits the file, so the mode propagates to clones. `city.toml` has no
`[dolt].mode`; adding one back would create a second topology store that can
disagree with bd.

## The ownership journal is intent, not topology

`.gc/scope-ownership.json` is a crash-safe record of an initialization Gas City
started, not a description of a running system. Its states:

- `provider_initializing` — carries the requested `intent` (transport/target)
  and, for an external target, the `endpoint` (host/port/database) the scope
  must initialize against. That endpoint is journaled because it used to live
  only in the initializing process: a `gc init` that died after writing the
  record left every retry unable to reach its upstream.
- `ready` — intent and endpoint are cleared. bd's own binding is authoritative
  from that point; a retained copy would outrank it on a later repair.

The journal keys are `city`, `rig:<name>`, and `path:<abs>` — the last being a
rig detached from `city.toml` by removal, rename, or an interrupted add.

## Provider-owned classification

A scope's beads lifecycle belongs to the provider when **either**:

1. the ownership journal has a record for it, **or**
2. its committed `.beads/ownership-handoff.json` projects a completed handoff,
   **or**
3. bd's own metadata says `backend: dolt` and `dolt_mode: proxied-server`.

Arm 3 is what makes an *un-journaled* proxied scope work. A workspace migrated
in place with `bd migrate from-server-to-proxied-server`, or cloned from a
proxied city, has no journal record, and classifying it as legacy meant
`gc stop` and `gc doctor` skipped it entirely while bd happily ran its
processes. A scope classified this way is `ready` by construction, and its
transport/target come from the binding (metadata plus the
`proxied_server_client_info.json` sidecar), never from the journal.

Malformed metadata is an error, not a legacy classification. Guessing who owns
a live Dolt process is how a scope ends up with two.

The same classification is the fence on gc's own managed-Dolt verbs: `gc
dolt-state start-managed`/`stop-managed`/`probe-managed` refuse a scope it
answers yes for, so an un-journaled proxied city cannot get a second,
gc-managed `sql-server` raised over bd's proxy root.

**Inheritance is downward only.** A fresh rig becomes provider-owned only when
the *city* already is. A rig added to a grandfathered GC-managed direct city
stays on the legacy inherited-city path — a database on the city's one managed
server — because journaling it would run a bare `bd init --server` in the rig
and give the city a second Dolt lifecycle owner that the dolt pack's orders and
backups do not cover. Converting an existing city is `bd migrate`'s job, not a
side effect of `gc rig add` or a re-run of `gc init`.

**Refusal.** bd commits `metadata.json` and gitignores the store itself, so a
clone of a proxied workspace carries `dolt_mode: proxied-server` without any
data. Serving it would silently create an empty store that reads as an empty
tracker. `gc start`, `gc rig add`, health and recover refuse such a scope with a
typed error before invoking bd at all; `gc stop` still reaches it, because a
refusal to clean up is not a safety property.

## Stop semantics

`bd dolt stop` for provider-owned scopes runs **last** on every stop path:

```
controller (agents drain with it) -> sessions -> orphan sessions ->
runtime server teardown -> bd dolt stop per provider-owned scope
```

The order is load-bearing. `BEADS_DOLT_AUTO_START=0` is **inert** on bd's
proxied path — every ordinary command short-circuits into the proxied UOW
provider (beads `cmd/bd/main.go:1758`) before auto-start policy is read — so
**any bd read restarts the proxy and its Dolt child**, measured at ~0.6s. A
dashboard sample, a `gc doctor`, or one straggler agent surviving the stop
undoes it. `applyProxiedDoltEnv` therefore drops the variable for proxied
scopes rather than projecting a promise bd does not keep.

Stop is re-runnable. `bd dolt stop` is idempotent on the proxied path (exit 0,
`stopped`/`verified` true, with or without a live proxy); on the direct path bd
exits 1 with "dolt server is not running", which the adapter maps to success so
a second `gc stop` is a clean no-op.

The fan-out covers the city, every configured rig (each workspace gets its own
proxy root, so each needs its own stop), and — for retiring operations only —
every record in the journal, whatever its key. Start deliberately does *not*
reach detached records: reviving a rig the operator removed is worse than
leaving it alone. A `city.toml` that no longer parses does not block the stop
fan-out either; stranding bd's processes behind a config error is the same leak
by another route, and that is precisely the case where the journal's
`rig:<name>` records are the only remaining list of what this city owns.

A retiring op attempts **every** scope before it reports anything. One rig whose
`bd dolt stop` refuses — bd declines an unverifiable proxy record without
`--force` — must not leave the rest resident, so failures are collected and
returned together. gc does not escalate to `--force`: rc.2 exposes the
force-eligible condition only as message text, and signalling a PID bd could not
identify is irreversible. Health reports the same way, so one bad scope cannot
hide the state of the others.

## Idle policy

GC-owned proxied scopes are initialized with `--proxied-server-idle-timeout 0`
(bd's `IdleTimeoutNever`, recorded as `"idle_timeout": -1` in the client-info
sidecar). Both proxied targets get it: local and external alike own a *local*
proxy plus its Dolt child, only the data upstream differs. Without it bd's
30s default retires the pair after every quiet period and each later command
pays a proxy-plus-Dolt cold start.

Readiness is a single `bd ping`, with no outer retry loop: bd's provider open
already waits up to 15s for the proxy endpoint and then up to 30s for the Dolt
child. Gas City widens the operation budget to 60s for owned proxied
start/ensure-ready/health/recover instead of stacking a second wait on top of
bd's.

Never call `bd dolt start` or `bd dolt status` on a proxied scope: neither is
proxied-aware in rc.2, and `start` would launch a second unmanaged `sql-server`
over the same data directory.

## Deliberately not done

- **Native SQL over the proxy.** Proxied scopes read and write through the bd
  CLI (`BdStore`). Dialing `127.0.0.1:<proxy.pid port>` on the MySQL wire is
  feasible — rc.2 has no supported library open for a proxied workspace — but
  connection-lifetime ownership, idle semantics and proxy identity are a
  separate design. The CLI front door costs a fork per operation, which is a
  real regression for controller-heavy cities.
- **The journaled legacy→bd ownership handoff.** The responder side is present
  and inert; the driver (`bd migrate ownership-handoff`) needs beads ≥ rc.3.
  Until then the migration path for an existing direct city is explicit and
  operator-driven: `gc stop` → `gc beads city migrate-proxied` → `gc start`.
  That command orchestrates bd's own
  `bd migrate from-server-to-proxied-server` per scope and fences the one thing
  bd cannot see — a running Gas City server — because bd's precondition
  consults only its own pid file and would otherwise commit the mode flip onto
  a data dir Dolt still holds locked. Procedure, refusals and recovery:
  `engdocs/runbooks/beads-migrate-proxied.md`. It is an interim path; the
  journaled handoff supersedes it.
- **Remote hosted proxies and Windows/macOS proxied lifecycle.** rc.2 defines
  but does not implement the latter.

## rc.2 refusal matrix

Proxied mode is `[EXPERIMENTAL]` in rc.2 and refuses a list of verbs —
`doctor`, `backup*`, `restore`, `diff`, `flatten`, `migrate*`, `branch`,
`vc*`, `sync`, `--readonly`, `--max-rows`, `--watch` and more. The authority is
beads `cmd/bd/proxy_capability.go`; packs and orders that call those verbs
against a proxied scope fail typed. The dashboard and doctor use `bd ping`
rather than `bd doctor --readonly` for exactly this reason.
