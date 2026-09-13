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
		Long:  "Show the precedence-resolved MCP servers that Gas City would project into the provider-native config for one agent or session target.",
		Args:  cobra.NoArgs,
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

			if jsonOutput {
				if err := writeCLIJSONLine(stdout, projectedMCPJSONFromView(cityPath, view, agentName, sessionID)); err != nil {
					fmt.Fprintf(stderr, "gc mcp list: writing JSON: %v\n", err) //nolint:errcheck // best-effort stderr
					return errExit
				}
				return nil
			}
			writeProjectedMCPView(stdout, cityPath, view)
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

func projectedMCPJSONFromView(cityPath string, view resolvedMCPProjection, agentName, sessionID string) projectedMCPJSON {
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
	return projectedMCPJSON{
		SchemaVersion: "1",
		OK:            true,
		CityPath:      cityPath,
		Query: projectedMCPQueryJSON{
			Agent:   agentName,
			Session: sessionID,
		},
		Projection: projectedMCPTargetJSON{
			Provider:     view.Projection.Provider,
			Target:       view.Projection.Target,
			WorkDir:      view.WorkDir,
			Delivery:     view.Delivery,
			ScopeRoot:    view.ScopeRoot,
			Identity:     view.Identity,
			ProviderKind: view.ProviderKind,
		},
		Servers: servers,
		Shadows: shadows,
	}
}

func writeProjectedMCPView(w io.Writer, cityPath string, view resolvedMCPProjection) {
	fmt.Fprintf(w, "Provider: %s\n", view.Projection.Provider) //nolint:errcheck // best-effort
	fmt.Fprintf(w, "Target: %s\n", view.Projection.Target)     //nolint:errcheck // best-effort
	if view.WorkDir != "" {
		fmt.Fprintf(w, "Workdir: %s\n", view.WorkDir) //nolint:errcheck // best-effort
	}
	if view.Delivery != "" {
		fmt.Fprintf(w, "Delivery: %s\n", view.Delivery) //nolint:errcheck // best-effort
	}
	if len(view.Catalog.Servers) == 0 {
		fmt.Fprintln(w, "No projected MCP servers.") //nolint:errcheck // best-effort
		return
	}
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
