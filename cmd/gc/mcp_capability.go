package main

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/materialize"
)

// mcpTransports lists every neutral transport, in display order.
var mcpTransports = []materialize.MCPTransport{
	materialize.MCPTransportStdio,
	materialize.MCPTransportHTTP,
	materialize.MCPTransportSSE,
}

// supportedMCPTransportsText names the transports a harness's MCP config can
// carry, for messages: "stdio" or "stdio and http".
func supportedMCPTransportsText(harness string) string {
	var names []string
	for _, transport := range mcpTransports {
		if materialize.MCPProjectionSupportsTransport(harness, transport) {
			names = append(names, string(transport))
		}
	}
	return strings.Join(names, " and ")
}

func displayMCPHarness(harness string) string {
	if harness = strings.TrimSpace(harness); harness == "" {
		return "(none)"
	}
	return harness
}

// unsupportedMCPHarnessError reports MCP servers declared in an agent's own
// mcp/ directory while its provider runs on a harness gc cannot write an MCP
// config for. It stops that agent's session start; the rest of the city
// starts. Servers that reach the agent through a shared layer are skipped
// instead (partitionAgentMCPCatalog).
type unsupportedMCPHarnessError struct {
	Harness string
	Servers []string
	// Dir is the agent's mcp/ directory, which may sit in an imported pack.
	Dir string
}

func (e *unsupportedMCPHarnessError) Error() string {
	return fmt.Sprintf(
		"MCP servers [%s] are declared in this agent's own mcp/ directory%s, but its provider runs on the %q harness, "+
			"which cannot receive MCP (supported harnesses: %s); switch the agent to a provider on a "+
			"supported harness or remove those servers from the agent's mcp/ directory",
		strings.Join(e.Servers, ", "), displayMCPDir(e.Dir), displayMCPHarness(e.Harness), strings.Join(mcpProviderKinds, ", "))
}

func displayMCPDir(dir string) string {
	if strings.TrimSpace(dir) == "" {
		return ""
	}
	return " (" + dir + ")"
}

// unsupportedMCPTransportError reports MCP servers declared in an agent's own
// mcp/ directory whose transport its provider's MCP config cannot carry, such
// as an http server for the stdio-only cursor projection. Like
// unsupportedMCPHarnessError, it stops only that agent's session start.
type unsupportedMCPTransportError struct {
	Harness string
	// Servers are "name (transport)" entries.
	Servers []string
	// Dir is the agent's mcp/ directory, which may sit in an imported pack.
	Dir string
}

func (e *unsupportedMCPTransportError) Error() string {
	return fmt.Sprintf(
		"MCP servers [%s] are declared in this agent's own mcp/ directory%s, but gc projects only %s MCP servers "+
			"for the %q harness; switch the agent to a provider that accepts them or remove those servers "+
			"from the agent's mcp/ directory",
		strings.Join(e.Servers, ", "), displayMCPDir(e.Dir), supportedMCPTransportsText(e.Harness), displayMCPHarness(e.Harness))
}

// isAgentMCPCapabilityError reports whether err says an agent's own mcp/
// directory declares servers its provider cannot receive. Such an error stops
// that agent's session start, never the rest of the city.
func isAgentMCPCapabilityError(err error) bool {
	var harnessErr *unsupportedMCPHarnessError
	var transportErr *unsupportedMCPTransportError
	return errors.As(err, &harnessErr) || errors.As(err, &transportErr)
}

// mcpServerSkip is an MCP server an agent starts without: its provider
// cannot receive it, and it reached the agent through a shared layer.
type mcpServerSkip struct {
	Server materialize.MCPServer
	Reason string
}

// mcpReceiveReason says why an agent whose provider runs on providerKind
// cannot receive the servers gc leaves out of its projection.
func mcpReceiveReason(providerKind string) string {
	if !supportsMCPProviderKind(providerKind) {
		return fmt.Sprintf("its provider runs on the %q harness, which cannot receive MCP", displayMCPHarness(providerKind))
	}
	return fmt.Sprintf("gc projects only %s MCP servers for the %q harness", supportedMCPTransportsText(providerKind), providerKind)
}

// partitionAgentMCPCatalog splits an agent's effective catalog by what its
// provider can receive: nothing when its harness has no MCP projection, and
// otherwise every server whose transport the provider's MCP config carries.
//
// A server the provider cannot receive is skipped when it reached the agent
// through a shared layer: the city or rig pack, or an import. Those layers
// attach their servers to every agent in scope, so a server one agent cannot
// use must not stop that agent, or its city, from starting. A server declared
// in the agent's own mcp/ directory targets that agent explicitly, so one it
// cannot receive stays an error for that agent.
//
// The returned catalog holds only the servers the provider receives.
func partitionAgentMCPCatalog(providerKind string, catalog materialize.MCPCatalog) (materialize.MCPCatalog, []mcpServerSkip, error) {
	providerKind = strings.TrimSpace(providerKind)
	harnessSupported := supportsMCPProviderKind(providerKind)
	var (
		skipped  []mcpServerSkip
		declared []string
		dir      string
	)
	rejected := make(map[string]bool)
	for _, server := range catalog.Servers {
		if harnessSupported && materialize.MCPProjectionSupportsTransport(providerKind, server.Transport) {
			continue
		}
		rejected[server.Name] = true
		if server.Layer == materialize.MCPLayerAgent {
			dir = server.SourceDir
			if harnessSupported {
				declared = append(declared, fmt.Sprintf("%s (%s)", server.Name, server.Transport))
			} else {
				declared = append(declared, server.Name)
			}
			continue
		}
		skipped = append(skipped, mcpServerSkip{Server: server, Reason: mcpReceiveReason(providerKind)})
	}
	if len(declared) > 0 {
		if !harnessSupported {
			return materialize.MCPCatalog{}, nil, &unsupportedMCPHarnessError{Harness: providerKind, Servers: declared, Dir: dir}
		}
		return materialize.MCPCatalog{}, nil, &unsupportedMCPTransportError{Harness: providerKind, Servers: declared, Dir: dir}
	}
	if len(rejected) == 0 {
		return catalog, nil, nil
	}
	return catalog.Filter(func(server materialize.MCPServer) bool { return !rejected[server.Name] }), skipped, nil
}

// describeMCPOrigin names the layer an MCP server came from in the terms the
// user configured it with: an import binding, the city or rig pack, or the
// agent's own mcp/ directory.
func describeMCPOrigin(server materialize.MCPServer) string {
	origin := strings.TrimSpace(server.Origin)
	if origin == "" {
		origin = strings.TrimSpace(server.Layer)
	}
	kind, binding, _ := strings.Cut(origin, ":")
	named := func(article, what string) string {
		if binding != "" {
			return fmt.Sprintf("%s %q", what, binding)
		}
		return article + " " + what
	}
	switch kind {
	case "import":
		return named("an", "import")
	case "rig-import":
		return named("a", "rig import")
	case "implicit":
		return named("an", "implicit import")
	case "bootstrap":
		return named("a", "bootstrap import")
	case "city":
		return "the city pack"
	case "rig":
		return "the rig pack"
	case materialize.MCPLayerAgent:
		return "the agent's mcp/ directory"
	case "":
		return "an unknown layer"
	default:
		return origin
	}
}

// describeMCPServerSkips renders one line for the agents that start without
// the same skipped servers: who, which servers (transport and origin), and
// why their provider cannot receive them. agents are labels
// (mcpAgentLabel), already quoted.
func describeMCPServerSkips(agents []string, skipped []mcpServerSkip) string {
	quoted := agents
	who, verb := "agent", "starts"
	if len(agents) > 1 {
		who, verb = "agents", "start"
	}
	servers := make([]string, len(skipped))
	for i, skip := range skipped {
		servers[i] = fmt.Sprintf("%q (%s, from %s)", skip.Server.Name, skip.Server.Transport, describeMCPOrigin(skip.Server))
	}
	noun := "server"
	if len(skipped) > 1 {
		noun = "servers"
	}
	reason := ""
	if len(skipped) > 0 {
		reason = skipped[0].Reason
	}
	return fmt.Sprintf("%s %s %s without MCP %s %s: %s",
		who, strings.Join(quoted, ", "), verb, noun, strings.Join(servers, ", "), reason)
}

// mcpSkipGroups collects agents by the servers they start without, so one
// line covers every agent on one harness that skips the same shared servers
// (a provider's implicit agents in the city and in each rig, for one).
type mcpSkipGroups struct {
	byKey map[string]*mcpSkipGroup
}

type mcpSkipGroup struct {
	agents  []string
	skipped []mcpServerSkip
}

func (g *mcpSkipGroups) add(agent string, skipped []mcpServerSkip) {
	if len(skipped) == 0 {
		return
	}
	key := describeMCPServerSkips(nil, skipped)
	if g.byKey == nil {
		g.byKey = make(map[string]*mcpSkipGroup)
	}
	group := g.byKey[key]
	if group == nil {
		group = &mcpSkipGroup{skipped: skipped}
		g.byKey[key] = group
	}
	group.agents = append(group.agents, agent)
}

// lines returns one description per group, in a stable order.
func (g *mcpSkipGroups) lines() []string {
	keys := make([]string, 0, len(g.byKey))
	for key := range g.byKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		group := g.byKey[key]
		agents := append([]string(nil), group.agents...)
		sort.Strings(agents)
		out = append(out, describeMCPServerSkips(agents, group.skipped))
	}
	return out
}

// mcpAgentLabel names an agent in an MCP warning: its quoted qualified name,
// and for an implicit agent, which gc adds for every declared provider and
// the user never defined, which provider it stands for.
func mcpAgentLabel(agent *config.Agent) string {
	label := strconv.Quote(agent.QualifiedName())
	if agent.Implicit {
		label += " (the implicit agent for provider " + strconv.Quote(agent.Provider) + ")"
	}
	return label
}

// resolveAnyProviderBinary is a LookPathFunc that finds every binary, so a
// provider's family resolves from config alone. Static checks use it: what a
// city's agents can receive must not depend on what this host has installed.
func resolveAnyProviderBinary(name string) (string, error) { return name, nil }

// mcpCapabilityReport is what a city's MCP config means for each agent whose
// provider cannot receive some of its servers.
type mcpCapabilityReport struct {
	// skips maps an agent's label (mcpAgentLabel) to the shared-layer
	// servers it starts without.
	skips map[string][]mcpServerSkip
	// blocked maps an agent's label to the error that stops it from
	// starting: its own mcp/ directory declares servers its provider cannot
	// receive.
	blocked map[string]string
}

// collectMCPCapabilityReport checks every configured agent's effective MCP
// against what its provider can receive. It reads config only: provider
// families resolve without the provider binaries, and catalogs load against
// each agent's scope root. ACP sessions receive their servers in-band and
// are never reported. Agents whose provider or catalog does not resolve are
// left out; gc doctor and their own session start report why.
func collectMCPCapabilityReport(cityPath string, cfg *config.City) mcpCapabilityReport {
	report := mcpCapabilityReport{skips: map[string][]mcpServerSkip{}, blocked: map[string]string{}}
	if cfg == nil {
		return report
	}
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		resolved, err := config.ResolveProvider(agent, &cfg.Workspace, cfg.Providers, resolveAnyProviderBinary)
		if err != nil || mcpInBand(agent, resolved) {
			continue
		}
		kind := resolvedProviderLaunchFamily(resolved)
		if !supportsMCPProviderKind(kind) && shouldSkipDeterministicControlDispatcherMCP(agent, kind) {
			continue
		}
		catalog, err := loadEffectiveMCPForAgent(cityPath, cfg, agent, agent.QualifiedName(), agentScopeRoot(agent, cityPath, cfg.Rigs))
		if err != nil {
			continue
		}
		_, skipped, err := partitionAgentMCPCatalog(kind, catalog)
		switch {
		case err != nil:
			report.blocked[mcpAgentLabel(agent)] = err.Error()
		case len(skipped) > 0:
			report.skips[mcpAgentLabel(agent)] = skipped
		}
	}
	return report
}

// since returns the part of r that is not in before: servers an agent newly
// starts without, and agents newly blocked or blocked for a new reason.
func (r mcpCapabilityReport) since(before mcpCapabilityReport) mcpCapabilityReport {
	out := mcpCapabilityReport{skips: map[string][]mcpServerSkip{}, blocked: map[string]string{}}
	for agent, skipped := range r.skips {
		had := make(map[string]bool, len(before.skips[agent]))
		for _, skip := range before.skips[agent] {
			had[skip.Server.Name] = true
		}
		for _, skip := range skipped {
			if !had[skip.Server.Name] {
				out.skips[agent] = append(out.skips[agent], skip)
			}
		}
	}
	for agent, msg := range r.blocked {
		if before.blocked[agent] != msg {
			out.blocked[agent] = msg
		}
	}
	return out
}

// lines renders the report: one line per group of agents that start without
// the same servers, then one per group of agents blocked for the same reason.
func (r mcpCapabilityReport) lines() []string {
	var groups mcpSkipGroups
	for agent, skipped := range r.skips {
		groups.add(agent, skipped)
	}
	return append(groups.lines(), groupBlockedAgents(r.blocked)...)
}

// groupBlockedAgents renders agents that cannot start, one line per reason:
// `agents "a", "b" cannot start: <reason>`. blocked is keyed by agent label
// (mcpAgentLabel).
func groupBlockedAgents(blocked map[string]string) []string {
	byReason := make(map[string][]string)
	for agent, reason := range blocked {
		byReason[reason] = append(byReason[reason], agent)
	}
	reasons := make([]string, 0, len(byReason))
	for reason := range byReason {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	out := make([]string, 0, len(reasons))
	for _, reason := range reasons {
		quoted := byReason[reason]
		sort.Strings(quoted)
		who := "agent " + quoted[0] + " cannot start"
		if len(quoted) > 1 {
			who = "agents " + strings.Join(quoted, ", ") + " cannot start"
		}
		out = append(out, who+": "+reason)
	}
	return out
}
