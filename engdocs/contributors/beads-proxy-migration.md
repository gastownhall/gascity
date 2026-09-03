# Beads storage migration runbook

Storage selection is explicit. A normal city start or `gc init` must not move
an existing direct or server-backed Beads workspace, and must preserve its
metadata, sentinel files, ownership, and migration checkpoint.

## Opt in to proxied storage

1. Stop writers and record the current workspace and checkpoint.
2. Back up the Beads metadata and sentinel files.
3. Set the workspace's explicit proxied provider configuration.
4. Run the migration command explicitly and wait for its checkpoint to reach
   the committed phase before starting agents.
5. Verify reads and writes through the proxy, then retain the prior direct
   configuration as the rollback target.

## Roll back

Stop writers, select the saved direct configuration explicitly, and run the
documented rollback operation. Verify the sentinel, ownership, and checkpoint
before reopening the city. If verification fails, leave the workspace closed;
do not fall back to a newly initialized store.

## Safety checks

- Startup with unchanged configuration is a no-op.
- Configuration changes require an explicit migration intent and durable
  checkpoint; they are never inferred from provider availability.
- Missing, malformed, or incomplete metadata fails closed rather than creating
  a new store.
- Managed-local and external server topologies use the same checkpoint and
  rollback rules; only lifecycle ownership differs.
