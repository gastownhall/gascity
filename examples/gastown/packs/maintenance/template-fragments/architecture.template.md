{{ define "architecture" }}
## Gas City Maintenance Context

City root: `{{ .CityRoot }}`.

- `city.toml` configures deployment/runtime state; `pack.toml` defines authored
  pack content.
- `agents/`, `commands/`, `doctor/`, `formulas/`, `orders/`, and
  `template-fragments/` hold maintenance pack assets.
- `.gc/` holds runtime state and embedded system packs.
- **Dogs** run cleanup and shutdown-dance work. **Beads** route and track tasks;
  **molecules** are the multi-step formula instances.
{{ end }}
