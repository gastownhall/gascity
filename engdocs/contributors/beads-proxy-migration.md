# Beads storage migration runbook

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
