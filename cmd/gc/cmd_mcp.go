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
		Long: fmt.Sprintf(`Inspect the projected MCP catalog for a concrete target.

Projected MCP is target-specific. Use "%s mcp list --agent <name>" when
the agent has a single deterministic projection target from config, or
"%s mcp list --session <id>" for a live session target.`, prog(), prog()),
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			fmt.Fprintf(stderr, "%s: unknown subcommand %q\n", cmdName("mcp"), args[0]) //nolint:errcheck // best-effort stderr
			return errExit
		},
	}
	cmd.AddCommand(newMcpListCmd(stdout, stderr))
	return cmd
}

func newMcpListCmd(stdout, stderr io.Writer) *cobra.Command {
	var agentName string
	var sessionID string
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
				fmt.Fprintf(stderr, "%s: --agent and --session are mutually exclusive\n", cmdName("mcp list")) //nolint:errcheck // best-effort stderr
				return errExit
			case agentName == "" && sessionID == "":
				fmt.Fprintf(stderr, "%s: projected MCP is target-specific; pass --agent or --session\n", cmdName("mcp list")) //nolint:errcheck // best-effort stderr
				return errExit
			}

			cityPath, err := resolveCity()
			if err != nil {
				cmdErr(stderr, "mcp list", err)
				return errExit
			}
			cfg, err := loadCityConfig(cityPath, stderr)
			if err != nil {
				cmdErr(stderr, "mcp list", err)
				return errExit
			}

			var (
				store beads.Store
				view  resolvedMCPProjection
			)
			if sessionID != "" {
				store, err = openCityStoreAt(cityPath)
				if err != nil {
					cmdErr(stderr, "mcp list", err)
					return errExit
				}
				view, err = resolveSessionMCPProjection(cityPath, cfg, store, sessionID, exec.LookPath)
			} else {
				agent, ok := resolveAgentIdentity(cfg, agentName, currentRigContext(cfg))
				if !ok {
					fmt.Fprintf(stderr, "%s: unknown agent %q\n", cmdName("mcp list"), agentName) //nolint:errcheck // best-effort stderr
					return errExit
				}
				template := resolveAgentTemplate(agent.QualifiedName(), cfg)
				cfgAgent := findAgentByTemplate(cfg, template)
				if cfgAgent == nil {
					fmt.Fprintf(stderr, "%s: unknown agent %q\n", cmdName("mcp list"), agentName) //nolint:errcheck // best-effort stderr
					return errExit
				}
				view, err = resolveDeterministicAgentMCPProjection(cityPath, cfg, cfgAgent, exec.LookPath)
			}
			if err != nil {
				cmdErr(stderr, "mcp list", err)
				return errExit
			}

			writeProjectedMCPView(stdout, cityPath, view)
			return nil
		},
	}
	cmd.Flags().StringVar(&agentName, "agent", "", "show the projected MCP config for this agent")
	cmd.Flags().StringVar(&sessionID, "session", "", "show the projected MCP config for this session")
	return cmd
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
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

func displayMCPSourcePath(cityPath, path string) string {
	path = filepath.Clean(path)
	if rel, err := filepath.Rel(cityPath, path); err == nil && rel != "." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(path)
}
