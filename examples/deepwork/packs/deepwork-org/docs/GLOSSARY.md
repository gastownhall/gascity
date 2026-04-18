# Gas Town Glossary

| Term | Definition |
|------|-----------|
| **Bead** | Unit of work tracked in Dolt. ID format: `prefix-hash` (e.g., `of-lj5`). |
| **Rig** | Self-contained project workspace with its own repo, beads DB, and agents. |
| **Town** | Top-level Gas Town deployment. One machine, one town. |
| **Mayor** | Human-facing coordinator. Dispatches work, reviews, merges. |
| **Deacon** | Automated patrol agent. Spawns witnesses, runs plugins. |
| **Witness** | Per-rig lifecycle agent. Monitors health, recovers orphans. |
| **Refinery** | Per-rig merge processor. Rebases, tests, merges. |
| **Polecat** | Disposable worker. Spawned per-bead via `gt sling`. |
| **Crew** | Persistent worker with domain expertise. |
| **Dog** | Short-lived helper for deacon. |
| **Sling** | Dispatching a bead to an agent. `gt sling <bead> <target>`. |
| **Convoy** | Group of related beads tracked together. |
| **Molecule** | Instance of a formula — a multi-step workflow being executed. |
| **Formula** | Workflow template (TOML) defining steps for agents. |
| **Wisp** | Lightweight, ephemeral bead (patrol reports, status checks). |
| **Hook** | Bead attached to an agent — the agent's current work assignment. |
| **Plugin** | Deacon patrol task on a cooldown gate. |
| **Mesh** | Cross-town communication via DoltHub sync. |
| **Wasteland** | Shared federation board on DoltHub for collaborative work. |
| **bd** | Beads CLI for issue tracking. |
| **gt** | Gas Town CLI for agent orchestration. |
| **Tap** | Guard/hook system intercepting certain gt commands. |
| **Boot** | Ephemeral deacon watchdog agent. |
| **Ralph Loop** | Fresh-context-per-step execution for multi-step work. |
