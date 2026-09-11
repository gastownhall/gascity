---
title: Overview
description: Diagnosis walkthroughs and operational runbooks for a running city.
---

When something is wrong — or when you're operating a running city day to
day — start here. This section pairs diagnosis walkthroughs that match a
symptom to its fix with operational runbooks for the deployments you keep
alive.

## Diagnose

- [Diagnose a Failed gc start](/troubleshooting/gc-start-walkthrough) — match a `gc start` failure symptom to its cause and resolution.
- [Recover from Dolt Bloat](/troubleshooting/dolt-bloat-recovery) — recover a beads store whose Dolt noms directory has grown out of proportion.
- [Clean Up bd Auto-Backups](/troubleshooting/bd-backup-cleanup) — reclaim space when bd's `.beads/backup/` directory grows large enough to threaten disk pressure.

## Operate

- [Operate Managed-City Dolt Endpoints](/runbooks/managed-city-endpoints) — mental model, forbidden edits, sanctioned escape hatches, and recovery recipe for the city-level Dolt endpoint architecture.
