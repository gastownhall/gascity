# Beads storage migration runbook

## Approved migration contract

Fresh managed-local scopes use Beads `proxied-server` by default. Existing
direct (`server`) scopes are never converted automatically. To migrate one,
stop all writers and run the explicit command (use `--dry-run` first):

```sh
bd dolt stop
bd migrate from-server-to-proxied-server --dry-run
bd migrate from-server-to-proxied-server
bd dolt stop
bd migrate from-proxied-server-to-server --dry-run
bd migrate from-proxied-server-to-server
```

Stop the Dolt process before the dry-run as well; migration validates live
ownership and refuses while a server or proxy is running. The forward command
records `prepared`, `target_configured`, `old_controls_retired`, `verified`,
and `committed` in `.beads/dolt-mode-migration.json`. It sets
`metadata.json:dolt_mode` to `proxied-server`, writes
`.beads/proxied_server_client_info.json`, and for shared roots writes
`.beads/config.yaml:dolt.shared-server: false`. Reverse commands restore
`dolt_mode: server`, re-enable shared YAML when selected, and remove the
sidecar. Retry the same command after a fault; never delete the journal or
start writers until verification succeeds.

For a shared-server root, use the corresponding pair:
`from-shared-server-to-proxied-server` and
`from-proxied-server-to-shared-server`. The direct commands are the escape
hatch for operators who need a managed SQL server. Embedded mode has no
in-place flip; re-provision it explicitly.

Shared-root sequence:

```sh
bd dolt stop
bd migrate from-shared-server-to-proxied-server --dry-run
bd migrate from-shared-server-to-proxied-server
bd dolt stop
bd migrate from-proxied-server-to-shared-server --dry-run
bd migrate from-proxied-server-to-shared-server
```

Managed-local mode owns the proxy and child Dolt lifecycle. External TCP or
Unix endpoints are owner-managed; in-place migration refuses them. A migration
journal records each checkpoint, retries repair incomplete work, and a second
successful invocation is idempotent. Missing or malformed journal, metadata,
sidecar, or topology state fails closed without mutating the workspace.

The sidecar identifies the proxied root; `metadata.json` stores the mode and
workspace YAML stores shared-server topology. Proxy/server controls and logs
remain inside their owning roots. Verify a pre-existing sentinel bead and its
dependency (`bd show <id> --json`, `bd dep list <id> <blocker> --json`) after
migration. External TCP/Unix endpoints and embedded scopes are refusal or
re-provision boundaries, not in-place conversions. Migration does not promise
Git remotes or backups; RC2 readiness is a separate gate.

Storage selection is explicit. A normal city start or `gc init` must not move
an existing direct or server-backed Beads workspace, and must preserve its
metadata, sentinel files, ownership, and migration checkpoint.

## Fresh city

Create a new city with the normal command:

```sh
gc init ~/my-city
```

`gc init` does not convert an existing Beads store. The release/RC version is
the binary selected by the operator (verify with `gc version`); do not run a
newer binary against a workspace until its version floor is approved.

## Opt in to proxied storage

1. Stop writers and record the current workspace and checkpoint.
2. Back up the Beads metadata and sentinel files.
3. Set the workspace's explicit proxied provider configuration. There is no
   implicit or automatic in-place conversion command; use the approved
   migration tooling for the release you are running.
4. Run that explicitly approved operation and wait for its checkpoint to reach
   the committed phase before starting agents.
5. Verify reads and writes through the proxy, then retain the prior direct
   configuration as the rollback target.

## Roll back

Stop writers, select the saved direct configuration explicitly, and run the
approved rollback operation (or start the prior direct release against the
saved configuration). Verify the sentinel, ownership, and checkpoint
before reopening the city. If verification fails, leave the workspace closed;
do not fall back to a newly initialized store.

## Safety checks

- Startup with unchanged configuration is a no-op.
- Configuration changes require an explicit migration intent and durable
  checkpoint; they are never inferred from provider availability.
- Missing, malformed, or incomplete metadata fails closed rather than creating
  a new store.
- Managed-local topology owns the child server lifecycle. External TCP and
  Unix topologies do not: they reconnect to the configured endpoint and never
  adopt or restart the external server. Both use the same checkpoint and
  rollback rules.

The migration intent and guard invariants are exercised by
`TestDeriveStartupIntentIsANoOpWhenEveryIdentityMatches`,
`TestDeriveStartupIntentMigratesOnConfigurationChangeAlone`, and
`TestAcquireMigrationGuardRejectsNoncanonicalOrSymlinkedCityDirectory`.
