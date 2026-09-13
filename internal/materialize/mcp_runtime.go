package materialize

import (
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/runtime"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
)

// EffectiveMCPForSession loads, expands, and resolves the effective MCP
// catalog for one concrete session context.
//
// topo carries the city's query topology so the {{.WorkQuery}} family this
// catalog expands names the reader the city actually serves work from. Rendering
// a single-store `bd ready` into an MCP catalog on a split city tells the agent
// to run the blind query by hand — the same fail-open the generated work_query
// closes. Only topo.FederatedReady is read from the caller: the bd semantics
// still come from cfg, exactly as they did before, so a caller that already
// passes cfg supplies one fact and not two.
func EffectiveMCPForSession(
	cfg *config.City,
	cityPath string,
	agent *config.Agent,
	identity string,
	workDir string,
	topo config.QueryTopology,
) (MCPCatalog, error) {
	cfgForMCP := cfg
	if cfg != nil && cfg.PackMCPDir == "" {
		cityMCPDir := filepath.Join(cityPath, "mcp")
		if info, err := os.Stat(cityMCPDir); err == nil && info.IsDir() {
			clone := *cfg
			clone.PackMCPDir = cityMCPDir
			cfgForMCP = &clone
		}
	}
	return EffectiveMCPForAgent(cfgForMCP, agent, MCPTemplateData(cfgForMCP, cityPath, agent, identity, workDir, topo))
}

// MCPTemplateData builds the template expansion surface used by MCP catalogs.
// topo.Beads is overwritten from cfg when cfg is non-nil; see
// EffectiveMCPForSession.
func MCPTemplateData(
	cfg *config.City,
	cityPath string,
	agent *config.Agent,
	identity string,
	workDir string,
	topo config.QueryTopology,
) map[string]string {
	if agent == nil {
		branch := defaultMCPBranch(workDir)
		return map[string]string{
			"CityRoot":      cityPath,
			"AgentName":     identity,
			"TemplateName":  identity,
			"WorkDir":       workDir,
			"Branch":        branch,
			"DefaultBranch": branch,
		}
	}
	var rigs []config.Rig
	if cfg != nil {
		rigs = cfg.Rigs
		topo.Beads = cfg.Beads
	}
	rigName := workdirutil.ConfiguredRigName(cityPath, *agent, rigs)
	rigRoot := workdirutil.RigRootForName(rigName, rigs)
	templateName := agentutil.RoutedToIdentity(agent)
	if templateName == "" {
		templateName = identity
	}
	data := make(map[string]string, len(agent.Env)+14)
	for key, value := range agent.Env {
		data[key] = value
	}
	branch := defaultMCPBranch(workDir)
	data["CityRoot"] = cityPath
	data["AgentName"] = identity
	data["TemplateName"] = templateName
	data["RigName"] = rigName
	data["RigRoot"] = rigRoot
	data["WorkDir"] = workDir
	data["IssuePrefix"] = mcpRigPrefix(rigName, rigs)
	data["Branch"] = branch
	data["DefaultBranch"] = branch
	data["WorkQuery"] = agent.EffectiveWorkQueryFor(topo)
	data["AssignedInProgressQuery"] = agent.EffectiveAssignedInProgressQueryFor(topo)
	data["AssignedReadyQuery"] = agent.EffectiveAssignedReadyQueryFor(topo)
	data["RoutedPoolQuery"] = agent.EffectiveRoutedPoolQueryFor(topo)
	data["SlingQuery"] = agent.EffectiveSlingQuery()
	return data
}

// RuntimeMCPServers converts neutral MCP servers into runtime-owned ACP
// session/new server definitions.
func RuntimeMCPServers(servers []MCPServer) []runtime.MCPServerConfig {
	if len(servers) == 0 {
		return nil
	}
	out := make([]runtime.MCPServerConfig, 0, len(servers))
	for _, server := range servers {
		entry := runtime.MCPServerConfig{
			Name:    server.Name,
			Command: server.Command,
			Args:    append([]string(nil), server.Args...),
			Env:     cloneStringMap(server.Env),
			URL:     server.URL,
			Headers: cloneStringMap(server.Headers),
		}
		switch server.Transport {
		case MCPTransportHTTP:
			entry.Transport = runtime.MCPTransportHTTP
		case MCPTransportSSE:
			entry.Transport = runtime.MCPTransportSSE
		default:
			entry.Transport = runtime.MCPTransportStdio
		}
		out = append(out, entry)
	}
	return runtime.NormalizeMCPServerConfigs(out)
}

func mcpRigPrefix(rigName string, rigs []config.Rig) string {
	for i := range rigs {
		if rigs[i].Name == rigName {
			return rigs[i].EffectivePrefix()
		}
	}
	return ""
}

func defaultMCPBranch(dir string) string {
	if dir == "" {
		return "main"
	}
	g := git.New(filepath.Clean(dir))
	branch, _ := g.DefaultBranch()
	if branch == "" {
		return "main"
	}
	return branch
}
