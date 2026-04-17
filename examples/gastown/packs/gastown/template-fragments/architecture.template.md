{{ define "architecture" }}
## Gas Town Architecture

```
Town ({{ .CityRoot }})
├── controller        ← Go process: lifecycle management
├── deacon/           ← Town-wide coordination + judgment tasks
├── mayor/            ← Global coordinator
├── <rig>/            ← Per-rig infrastructure
│   ├── .beads/       ← Issue tracking (shared ledger)
│   ├── crew/         ← Named workspaces (persistent)
│   ├── polecats/     ← Worker worktrees (transient)
│   ├── refinery/     ← Merge queue processor
│   └── witness/      ← Work-health monitor
```

**Key concepts:**
- **Town**: Workspace root containing all rigs
- **Rig**: Container for a project (polecats, refinery, witness)
- **Polecat**: Transient worker agent with its own git worktree
- **Crew**: Persistent workspace managed by the overseer (human)
- **Witness**: Per-rig work-health monitor (orphaned beads, stuck polecats)
- **Refinery**: Per-rig merge queue processor
- **Deacon**: Town-wide patrol (gates, convoys, stuck agents)
- **Dog**: Utility agent pool (shutdown dance, warrants)
- **Beads**: Issue tracking system shared by all rig agents
- **Molecule**: Multi-step formula instance guiding an agent's work
{{ end }}
