package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/spf13/cobra"
)

func newMcpCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "List MCP catalog visibility",
		Long: `List MCP catalog visibility for the current city pack.

The first MCP slice is list-only. Provider projection and reconciliation
are later work.`,
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
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List visible MCP definitions",
		Long:  "List the current city pack's visible MCP definitions, optionally scoped to an agent or session.",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if strings.TrimSpace(agentName) != "" && strings.TrimSpace(sessionID) != "" {
				fmt.Fprintln(stderr, "gc mcp list: --agent and --session are mutually exclusive") //nolint:errcheck // best-effort stderr
				return errExit
			}
			cityPath, err := resolveCity()
			if err != nil {
				fmt.Fprintf(stderr, "gc mcp list: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			cfg, err := loadCityConfig(cityPath)
			if err != nil {
				fmt.Fprintf(stderr, "gc mcp list: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}

			var store beads.Store
			if strings.TrimSpace(sessionID) != "" {
				store, err = openCityStoreAt(cityPath)
				if err != nil {
					fmt.Fprintf(stderr, "gc mcp list: %v\n", err) //nolint:errcheck // best-effort stderr
					return errExit
				}
			}

			entries, err := listVisibleMcpEntries(cityPath, cfg, store, agentName, sessionID)
			if err != nil {
				fmt.Fprintf(stderr, "gc mcp list: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			writeVisibilityEntries(stdout, entries)
			return nil
		},
	}
	cmd.Flags().StringVar(&agentName, "agent", "", "show the effective MCP view for this agent")
	cmd.Flags().StringVar(&sessionID, "session", "", "show the effective MCP view for this session")
	return cmd
}

func listVisibleMcpEntries(cityPath string, cfg *config.City, store beads.Store, agentName, sessionID string) ([]visibilityEntry, error) {
	cityEntries := discoverMcpEntries(cityPath, "city")
	if strings.TrimSpace(agentName) == "" && strings.TrimSpace(sessionID) == "" {
		return cityEntries, nil
	}
	agent, err := resolveVisibilityAgent(cityPath, cfg, store, agentName, sessionID)
	if err != nil {
		return nil, err
	}
	// When the agent has an explicit attachment config (mcp or shared_mcp),
	// filter the city catalog to the attached set. See listVisibleSkillEntries.
	attached := attachmentSet(agent.MCP, agent.SharedMCP)
	if len(attached) > 0 {
		cityEntries = filterEntriesByName(cityEntries, attached)
	}
	cityEntries = append(cityEntries, discoverAgentMcpEntries(agentAssetRoot(cityPath, agent), agent.Name, "agent")...)
	sortVisibilityEntries(cityEntries)
	return cityEntries, nil
}
