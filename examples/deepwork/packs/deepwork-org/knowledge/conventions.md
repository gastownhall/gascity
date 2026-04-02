# Conventions — Deepwork-AI Mesh

## Config Files

| File | Purpose |
|------|---------|
| `mesh.yaml` / `gtconfig.yaml` | Node identity, mesh connection, installed packs |
| `pack.yaml` | Pack manifest (in each pack directory) |
| `.mesh-config/` | Local cache of mesh config pulled from DoltHub |
| `.mesh-packs/` | Locally installed packs |
| `.mesh-telemetry.jsonl` | Command telemetry (timing, exit codes, errors) |
| `.mesh-inbox-pending.log` | Unread mail log for session pickup |
| `.mesh-config/knowledge/mesh-learnings.md` | Auto-accumulated knowledge from improve loop |

## Naming

- GT instance IDs: lowercase, hyphenated (gt-local, gt-docker, gt-alex)
- Pack names: lowercase, hyphenated (deepwork-base, frontend-skills)
- Skill names: lowercase, hyphenated (gt-mesh-setup, excalidraw-diagram-generator)
- Role names: lowercase single word (planner, worker, reviewer)
- Improvement IDs: `imp-<timestamp>-<random>`
- Knowledge IDs: `k-<timestamp>-<random>`

## DoltHub Tables

| Table | Purpose |
|-------|---------|
| messages | Mesh mail between GTs |
| peers | Registered mesh nodes + heartbeats |
| mesh_rules | Governance rules |
| mesh_config | Config distribution manifest |
| mesh_config_files | Config file contents |
| mesh_packs | Pack registry (marketplace) |
| mesh_pack_files | Pack file contents |
| mesh_improvements | Self-improving loop — reported findings |
| mesh_knowledge_entries | Graduated knowledge from improvements |
| shared_beads | Cross-GT work items |
| claims | Bead claim tracking |
| shared_skills | Individual skill registry |
| activity_log | Auto-sync activity feed |
| invites | Mesh invite codes |

## Versioning

- Packs use semver: MAJOR.MINOR.PATCH
- Config version: integer, incremented on each publish
- gtconfig.yaml: config_version field for forward compatibility
- Releases tagged on main after dev->main PR merge

## CLI Commands (v0.7.0+)

| Command | Purpose |
|---------|---------|
| `gt mesh init` | Initialize node |
| `gt mesh join` | Join with invite code |
| `gt mesh status` / `dash` | View mesh state |
| `gt mesh send` / `inbox` | Mesh mail |
| `gt mesh sync` | Force DoltHub sync |
| `gt mesh packs list/install/publish` | Pack marketplace |
| `gt mesh improve report/graduate` | Self-improving loop |
| `gt mesh auto-sync broadcast/log/digest` | Broadcast context |
| `gt mesh beads` / `skills` / `rules` | Shared resources |
| `gt mesh config` | Config distribution |
| `gt mesh invite` / `access` / `peers` | Membership management |

## Mail Routing

Incoming mesh mail is dynamically routed by keyword matching:

| Keywords | Routed To |
|----------|-----------|
| planogram, vap, ai-planogram | vap-crew-manager |
| alc, vaa, alc-ai | vaa-crew-manager |
| arcade, gta, gt_arcade | gta-crew-manager |
| mesh, config, pack, sync, invite | hq-mayor |
| task, bead, issue, pr, review | hq-mayor |
| (no match) | first alive agent |

The routing table lives in `mesh-mail-handler.sh` and can be extended.
