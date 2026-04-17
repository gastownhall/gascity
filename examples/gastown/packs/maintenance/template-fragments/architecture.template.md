{{ define "architecture" }}
## Gas City Maintenance Context

```
City ({{ .CityRoot }})
├── city.toml         ← deployment/runtime config
├── pack.toml         ← authored pack/city definition
├── agents/           ← convention-discovered agent prompts/config
├── commands/         ← command entrypoints
├── doctor/           ← doctor checks
├── formulas/         ← formula definitions
├── orders/           ← order definitions
├── template-fragments/ ← shared prompt fragments
└── .gc/              ← runtime state and embedded system packs
```

**Key concepts:**
- **City**: the working root for this Gas City instance
- **Maintenance pack**: shared infrastructure for dogs, doctor checks, formulas, and orders
- **Dog**: utility agent pool for operational cleanup and shutdown dance work
- **Beads**: work ledger used to route and track infrastructure tasks
- **Molecule**: multi-step formula instance guiding an agent's work
{{ end }}
