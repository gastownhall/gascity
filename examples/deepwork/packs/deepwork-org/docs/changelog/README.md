# Town Changelog

Rolling record of what happened in Gas Town — decisions, deployments, fixes, incidents, and milestones.

## How It Works

Each month gets a file: `YYYY-MM.md` (e.g., `2026-04.md`). Entries are newest-first within each file.

### Entry Format

```markdown
## YYYY-MM-DD — Short title

**Type:** decision | deploy | fix | incident | milestone | infra
**Rigs:** comma-separated rig names, or "town" for cross-cutting

What happened, why, and what changed. Keep it to 2-5 lines.
Link to beads if relevant: `of-lj5`, `ds-dws-a59`.
```

### Who Writes Entries

- **Mayor**: decisions, milestones, cross-rig changes
- **Any agent**: can append via `gt changelog add` (if implemented) or direct file edit
- **Self-evolving**: the knowledge system may auto-generate entries from bead closures

### Types

| Type | When |
|------|------|
| `decision` | A choice was made that affects how things work |
| `deploy` | A service was deployed or updated |
| `fix` | A bug or incident was resolved |
| `incident` | Something broke — include root cause |
| `milestone` | A product or feature shipped |
| `infra` | Infrastructure change (ports, crons, agents, mesh) |

### Rules

- One entry per event. Don't combine unrelated things.
- Include the "why" — future agents need context, not just facts.
- Reference bead IDs when they exist.
- Don't log routine patrols or witness activity — only notable events.
