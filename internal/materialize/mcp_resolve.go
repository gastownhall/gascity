package materialize

import (
	"path/filepath"

	"github.com/gastownhall/gascity/internal/config"
)

// MCPPackSourcesForAgent returns the effective MCP directory stack for one
// agent, ordered from lowest to highest precedence. Later sources win.
//
// Unlike the v0.15.1 skills materializer, MCP intentionally includes imported
// shared pack layers because shared imported-pack mcp/ is part of the issue
// #670 contract. Repeated pack directories are collapsed to their
// highest-precedence occurrence so a shared dependency only participates once in
// the final stack.
func MCPPackSourcesForAgent(cfg *config.City, agent *config.Agent) []MCPDirSource {
	if cfg == nil || agent == nil {
		return nil
	}
	sources := make([]MCPDirSource, 0, 16)
	for _, dir := range cfg.BootstrapImportPackDirs {
		sources = append(sources, MCPDirSource{
			Dir:    filepath.Join(dir, "mcp"),
			Label:  "bootstrap",
			Origin: mcpOrigin("bootstrap", cfg.BootstrapImportMCPBindings[dir]),
		})
	}
	for _, dir := range cfg.ImplicitImportPackDirs {
		sources = append(sources, MCPDirSource{
			Dir:    filepath.Join(dir, "mcp"),
			Label:  "implicit",
			Origin: mcpOrigin("implicit", cfg.ImplicitImportMCPBindings[dir]),
		})
	}
	for _, dir := range cfg.ExplicitImportPackDirs {
		sources = append(sources, MCPDirSource{
			Dir:    filepath.Join(dir, "mcp"),
			Label:  "import",
			Origin: mcpOrigin("import", cfg.ExplicitImportMCPBindings[dir]),
		})
	}
	for _, dir := range cfg.PackGraphOnlyDirs {
		sources = append(sources, MCPDirSource{
			Dir:    filepath.Join(dir, "mcp"),
			Label:  "city",
			Origin: "city",
		})
	}
	if cfg.PackMCPDir != "" {
		sources = append(sources, MCPDirSource{Dir: cfg.PackMCPDir, Label: "city", Origin: "city"})
	}
	if agent.Dir != "" {
		for _, dir := range cfg.RigImportPackDirs[agent.Dir] {
			origin := "rig-import"
			if cfg.RigImportMCPBindings != nil {
				origin = mcpOrigin("rig-import", cfg.RigImportMCPBindings[agent.Dir][dir])
			}
			sources = append(sources, MCPDirSource{
				Dir:    filepath.Join(dir, "mcp"),
				Label:  "rig-import",
				Origin: origin,
			})
		}
		for _, dir := range cfg.RigPackGraphOnlyDirs[agent.Dir] {
			sources = append(sources, MCPDirSource{
				Dir:    filepath.Join(dir, "mcp"),
				Label:  "rig",
				Origin: "rig",
			})
		}
	}
	if agent.MCPDir != "" {
		sources = append(sources, MCPDirSource{Dir: agent.MCPDir, Label: "agent", Origin: "agent"})
	}
	return dedupeMCPSources(sources)
}

// EffectiveMCPForAgent loads, expands, and resolves the effective MCP catalog
// for one agent from the composed config.
func EffectiveMCPForAgent(cfg *config.City, agent *config.Agent, templateData map[string]string) (MCPCatalog, error) {
	return MergeMCPDirs(MCPPackSourcesForAgent(cfg, agent), templateData)
}

func dedupeMCPSources(sources []MCPDirSource) []MCPDirSource {
	if len(sources) < 2 {
		return sources
	}
	seen := make(map[string]bool, len(sources))
	deduped := make([]MCPDirSource, 0, len(sources))
	for i := len(sources) - 1; i >= 0; i-- {
		key := filepath.Clean(sources[i].Dir)
		if seen[key] {
			continue
		}
		seen[key] = true
		deduped = append(deduped, sources[i])
	}
	for i, j := 0, len(deduped)-1; i < j; i, j = i+1, j-1 {
		deduped[i], deduped[j] = deduped[j], deduped[i]
	}
	return deduped
}

func mcpOrigin(layer, binding string) string {
	if binding == "" {
		return layer
	}
	return layer + ":" + binding
}
