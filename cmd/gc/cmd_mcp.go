package main

import (
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/materialize"
	"github.com/gastownhall/gascity/internal/shellquote"
	"github.com/spf13/cobra"
)

func newMcpCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Inspect projected MCP config",
		Long: `Inspect the projected MCP catalog for a concrete target.

Projected MCP is target-specific. Use "gc mcp list --agent <name>" when
the agent has a single deterministic projection target from config, or
"gc mcp list --session <id>" for a live session target.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			fmt.Fprintf(stderr, "gc mcp: unknown subcommand %q\n", args[0]) //nolint:errcheck // best-effort stderr
			return errExit
		},
	}
	cmd.AddCommand(newMcpListCmd(stdout, stderr))
	return cmd
}

func newMcpListCmd(stdout, stderr io.Writer) *cobra.Command {
	var agentName string
	var sessionID string
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "Show projected MCP servers",
		Long: `Show the precedence-resolved MCP servers that Gas City would project into the
provider-native config for one agent or session target.

Servers from a shared layer (the city or rig pack, or an import) that the
target's provider cannot receive are listed separately under "Not projected",
with the reason: the agent starts without them.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			agentName = strings.TrimSpace(agentName)
			sessionID = strings.TrimSpace(sessionID)
			switch {
			case agentName != "" && sessionID != "":
				fmt.Fprintln(stderr, "gc mcp list: --agent and --session are mutually exclusive") //nolint:errcheck // best-effort stderr
				return errExit
			case agentName == "" && sessionID == "":
				fmt.Fprintln(stderr, "gc mcp list: projected MCP is target-specific; pass --agent or --session") //nolint:errcheck // best-effort stderr
				return errExit
			}

			cityPath, err := resolveCity()
			if err != nil {
				fmt.Fprintf(stderr, "gc mcp list: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			cfg, err := loadCityConfig(cityPath, stderr)
			if err != nil {
				fmt.Fprintf(stderr, "gc mcp list: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}

			var (
				store beads.Store
				view  resolvedMCPProjection
			)
			if sessionID != "" {
				store, err = openCityStoreAt(cityPath)
				if err != nil {
					fmt.Fprintf(stderr, "gc mcp list: %v\n", err) //nolint:errcheck // best-effort stderr
					return errExit
				}
				view, err = resolveSessionMCPProjection(cityPath, cfg, cliSessionFrontDoor(store, cfg, cityPath), sessionID, exec.LookPath)
			} else {
				agent, ok := resolveAgentIdentity(cfg, agentName, currentRigContext(cfg))
				if !ok {
					fmt.Fprintf(stderr, "gc mcp list: unknown agent %q\n", agentName) //nolint:errcheck // best-effort stderr
					return errExit
				}
				template := resolveAgentTemplate(agent.QualifiedName(), cfg)
				cfgAgent := findAgentByTemplate(cfg, template)
				if cfgAgent == nil {
					fmt.Fprintf(stderr, "gc mcp list: unknown agent %q\n", agentName) //nolint:errcheck // best-effort stderr
					return errExit
				}
				view, err = resolveDeterministicAgentMCPProjection(cityPath, cfg, cfgAgent, exec.LookPath)
			}
			if err != nil {
				fmt.Fprintf(stderr, "gc mcp list: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}

			refused := mcpViewAdoptionRefusal(view)
			if jsonOutput {
				if err := writeCLIJSONLine(stdout, projectedMCPJSONFromView(cityPath, view, agentName, sessionID, refused)); err != nil {
					fmt.Fprintf(stderr, "gc mcp list: writing JSON: %v\n", err) //nolint:errcheck // best-effort stderr
					return errExit
				}
				return nil
			}
			writeProjectedMCPView(stdout, cityPath, view, refused)
			return nil
		},
	}
	cmd.Flags().StringVar(&agentName, "agent", "", "show the projected MCP config for this agent")
	cmd.Flags().StringVar(&sessionID, "session", "", "show the projected MCP config for this session")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output one JSONL result record")
	return cmd
}

type projectedMCPJSON struct {
	SchemaVersion string                   `json:"schema_version"`
	OK            bool                     `json:"ok"`
	CityPath      string                   `json:"city_path"`
	Query         projectedMCPQueryJSON    `json:"query"`
	Projection    projectedMCPTargetJSON   `json:"projection"`
	Servers       []projectedMCPServerJSON `json:"servers"`
	Shadows       []projectedMCPShadowJSON `json:"shadows,omitempty"`
	// Skipped lists shared-layer servers this target's provider cannot
	// receive; the agent starts without them.
	Skipped []projectedMCPSkippedJSON `json:"skipped,omitempty"`
}

type projectedMCPSkippedJSON struct {
	Name       string   `json:"name"`
	Transport  string   `json:"transport"`
	Command    string   `json:"command,omitempty"`
	Args       []string `json:"args,omitempty"`
	URL        string   `json:"url,omitempty"`
	Source     string   `json:"source"`
	Layer      string   `json:"layer,omitempty"`
	Origin     string   `json:"origin,omitempty"`
	EnvKeys    []string `json:"env_keys,omitempty"`
	HeaderKeys []string `json:"header_keys,omitempty"`
	Reason     string   `json:"reason"`
}

type projectedMCPQueryJSON struct {
	Agent   string `json:"agent,omitempty"`
	Session string `json:"session,omitempty"`
}

type projectedMCPTargetJSON struct {
	Provider     string `json:"provider"`
	Target       string `json:"target"`
	WorkDir      string `json:"work_dir,omitempty"`
	Delivery     string `json:"delivery,omitempty"`
	ScopeRoot    string `json:"scope_root,omitempty"`
	Identity     string `json:"identity,omitempty"`
	ProviderKind string `json:"provider_kind,omitempty"`
	// NotAdopted is set when gc leaves the target as it is, so the agent
	// starts without the servers listed.
	NotAdopted *projectedMCPNotAdoptedJSON `json:"not_adopted,omitempty"`
}

type projectedMCPNotAdoptedJSON struct {
	Reason           string   `json:"reason"`
	Remedy           string   `json:"remedy"`
	UnmanagedServers []string `json:"unmanaged_servers,omitempty"`
	RedefinedServers []string `json:"redefined_servers,omitempty"`
	UnmanagedKeys    []string `json:"unmanaged_keys,omitempty"`
	Symlink          string   `json:"symlink,omitempty"`
	ReadError        string   `json:"read_error,omitempty"`
	ParseError       string   `json:"parse_error,omitempty"`
}

type projectedMCPServerJSON struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Transport   string   `json:"transport"`
	Command     string   `json:"command,omitempty"`
	Args        []string `json:"args,omitempty"`
	URL         string   `json:"url,omitempty"`
	Source      string   `json:"source"`
	Layer       string   `json:"layer,omitempty"`
	Origin      string   `json:"origin,omitempty"`
	Template    bool     `json:"template,omitempty"`
	EnvKeys     []string `json:"env_keys,omitempty"`
	HeaderKeys  []string `json:"header_keys,omitempty"`
}

type projectedMCPShadowJSON struct {
	Name       string `json:"name"`
	Winner     string `json:"winner"`
	Loser      string `json:"loser"`
	WinnerFile string `json:"winner_file"`
	LoserFile  string `json:"loser_file"`
}

// mcpViewAdoptionRefusal reports whether gc leaves the view's provider file
// as it is, so the agent starts without the servers listed. It reads the
// file and writes nothing; a file it cannot read is not reported here.
func mcpViewAdoptionRefusal(view resolvedMCPProjection) *materialize.MCPAdoptionRefusedError {
	if view.Delivery != "stage1" && view.Delivery != "stage2" {
		return nil
	}
	refused, err := view.Projection.AdoptionRefusal(fsys.OSFS{})
	if err != nil {
		return nil
	}
	return refused
}

// acpMCPTarget is gc mcp list's target for an ACP session, which receives its
// servers in the session/new request; gc writes no provider-native file.
const acpMCPTarget = "(none: an ACP session receives these servers in-band, in session/new)"

func projectedMCPJSONFromView(cityPath string, view resolvedMCPProjection, agentName, sessionID string, refused *materialize.MCPAdoptionRefusedError) projectedMCPJSON {
	servers := make([]projectedMCPServerJSON, 0, len(view.Catalog.Servers))
	for _, server := range view.Catalog.Servers {
		servers = append(servers, projectedMCPServerJSON{
			Name:        server.Name,
			Description: server.Description,
			Transport:   string(server.Transport),
			Command:     server.Command,
			Args:        append([]string(nil), server.Args...),
			URL:         server.URL,
			Source:      displayMCPSourcePath(cityPath, server.SourceFile),
			Layer:       server.Layer,
			Origin:      server.Origin,
			Template:    server.Template,
			EnvKeys:     sortedMCPKeys(server.Env),
			HeaderKeys:  sortedMCPKeys(server.Headers),
		})
	}
	shadows := make([]projectedMCPShadowJSON, 0, len(view.Catalog.Shadows))
	for _, shadow := range view.Catalog.Shadows {
		shadows = append(shadows, projectedMCPShadowJSON{
			Name:       shadow.Name,
			Winner:     shadow.Winner,
			Loser:      shadow.Loser,
			WinnerFile: displayMCPSourcePath(cityPath, shadow.WinnerFile),
			LoserFile:  displayMCPSourcePath(cityPath, shadow.LoserFile),
		})
	}
	var skipped []projectedMCPSkippedJSON
	for _, skip := range view.Skipped {
		skipped = append(skipped, projectedMCPSkippedJSON{
			Name:       skip.Server.Name,
			Transport:  string(skip.Server.Transport),
			Command:    skip.Server.Command,
			Args:       append([]string(nil), skip.Server.Args...),
			URL:        skip.Server.URL,
			Source:     displayMCPSourcePath(cityPath, skip.Server.SourceFile),
			Layer:      skip.Server.Layer,
			Origin:     skip.Server.Origin,
			EnvKeys:    sortedMCPKeys(skip.Server.Env),
			HeaderKeys: sortedMCPKeys(skip.Server.Headers),
			Reason:     skip.Reason,
		})
	}
	var notAdopted *projectedMCPNotAdoptedJSON
	if refused != nil {
		notAdopted = &projectedMCPNotAdoptedJSON{
			Reason:           refused.Reason(),
			Remedy:           refused.Remedy(),
			UnmanagedServers: refused.Servers,
			RedefinedServers: refused.Redefined,
			UnmanagedKeys:    refused.Keys,
			Symlink:          refused.Symlink,
		}
		if refused.ReadErr != nil {
			notAdopted.ReadError = refused.ReadErr.Error()
		}
		if refused.ParseErr != nil {
			notAdopted.ParseError = refused.ParseErr.Error()
		}
	}
	target := view.Projection.Target
	if view.Delivery == "acp" {
		// No file is written for an ACP session; delivery says how it gets
		// its servers.
		target = ""
	}
	return projectedMCPJSON{
		SchemaVersion: "1",
		OK:            true,
		CityPath:      cityPath,
		Skipped:       skipped,
		Query: projectedMCPQueryJSON{
			Agent:   agentName,
			Session: sessionID,
		},
		Projection: projectedMCPTargetJSON{
			Provider:     view.Projection.Provider,
			Target:       target,
			WorkDir:      view.WorkDir,
			Delivery:     view.Delivery,
			ScopeRoot:    view.ScopeRoot,
			Identity:     view.Identity,
			ProviderKind: view.ProviderKind,
			NotAdopted:   notAdopted,
		},
		Servers: servers,
		Shadows: shadows,
	}
}

func writeProjectedMCPView(w io.Writer, cityPath string, view resolvedMCPProjection, refused *materialize.MCPAdoptionRefusedError) {
	provider, target := view.Projection.Provider, view.Projection.Target
	if provider == "" {
		provider = view.ProviderKind
	}
	switch {
	case view.Delivery == "acp":
		target = acpMCPTarget
	case view.Projection.Provider == "" && strings.TrimSpace(view.ProviderKind) != "":
		// A harness gc cannot write an MCP config for has no target.
		target = "(none: this harness cannot receive MCP)"
	}
	fmt.Fprintf(w, "Provider: %s\n", provider) //nolint:errcheck // best-effort
	fmt.Fprintf(w, "Target: %s\n", target)     //nolint:errcheck // best-effort
	if view.WorkDir != "" {
		fmt.Fprintf(w, "Workdir: %s\n", view.WorkDir) //nolint:errcheck // best-effort
	}
	if view.Delivery != "" {
		fmt.Fprintf(w, "Delivery: %s\n", view.Delivery) //nolint:errcheck // best-effort
	}
	if len(view.Catalog.Servers) == 0 {
		fmt.Fprintln(w, "No projected MCP servers.") //nolint:errcheck // best-effort
	} else {
		fmt.Fprintln(w) //nolint:errcheck // best-effort
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tTRANSPORT\tCOMMAND/URL\tSOURCE\tENV\tHEADERS") //nolint:errcheck // best-effort
		for _, server := range view.Catalog.Servers {
			_, _ = fmt.Fprintf(
				tw,
				"%s\t%s\t%s\t%s\t%s\t%s\n",
				server.Name,
				server.Transport,
				mcpCommandOrURL(server),
				displayMCPSourcePath(cityPath, server.SourceFile),
				formatMCPKeyNames(server.Env),
				formatMCPKeyNames(server.Headers),
			)
		}
		_ = tw.Flush()
		if refused != nil {
			fmt.Fprintf(w, "\nNot delivered: gc leaves %s as it is, so the agent starts without these servers: %s; %s.\n", //nolint:errcheck // best-effort
				view.Projection.Target, refused.Reason(), refused.Remedy())
		}
	}
	writeSkippedMCPServers(w, cityPath, view.Skipped)
}

// writeSkippedMCPServers lists the shared-layer servers the target's provider
// cannot receive, so the listing never presents them as projected.
func writeSkippedMCPServers(w io.Writer, cityPath string, skipped []mcpServerSkip) {
	if len(skipped) == 0 {
		return
	}
	fmt.Fprintf(w, "\nNot projected (%s; the agent starts without these):\n", skipped[0].Reason) //nolint:errcheck // best-effort
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tTRANSPORT\tCOMMAND/URL\tSOURCE") //nolint:errcheck // best-effort
	for _, skip := range skipped {
		_, _ = fmt.Fprintf(
			tw,
			"%s\t%s\t%s\t%s\n",
			skip.Server.Name,
			skip.Server.Transport,
			mcpCommandOrURL(skip.Server),
			displayMCPSourcePath(cityPath, skip.Server.SourceFile),
		)
	}
	_ = tw.Flush()
}

func mcpCommandOrURL(server materialize.MCPServer) string {
	if server.Transport == materialize.MCPTransportHTTP {
		return server.URL
	}
	parts := make([]string, 0, 1+len(server.Args))
	if strings.TrimSpace(server.Command) != "" {
		parts = append(parts, server.Command)
	}
	parts = append(parts, server.Args...)
	if len(parts) == 0 {
		return ""
	}
	return shellquote.Join(parts)
}

func formatMCPKeyNames(values map[string]string) string {
	if len(values) == 0 {
		return "-"
	}
	return strings.Join(sortedMCPKeys(values), ",")
}

func sortedMCPKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func displayMCPSourcePath(cityPath, path string) string {
	path = filepath.Clean(path)
	if rel, err := filepath.Rel(cityPath, path); err == nil && rel != "." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(path)
}
