package main

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/materialize"
	"github.com/spf13/cobra"
)

func newInternalProjectMCPCmd(stdout, stderr io.Writer) *cobra.Command {
	var agentName, identity, workdir string
	cmd := &cobra.Command{
		Use:    "project-mcp",
		Short:  "Project MCP for one agent into one workdir",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if strings.TrimSpace(agentName) == "" {
				fmt.Fprintln(stderr, "gc internal project-mcp: --agent is required") //nolint:errcheck // best-effort stderr
				return errExit
			}
			if strings.TrimSpace(workdir) == "" {
				fmt.Fprintln(stderr, "gc internal project-mcp: --workdir is required") //nolint:errcheck // best-effort stderr
				return errExit
			}

			cityPath, err := resolveCity()
			if err != nil {
				fmt.Fprintf(stderr, "gc internal project-mcp: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			cfg, err := loadCityConfig(cityPath, stderr)
			if err != nil {
				fmt.Fprintf(stderr, "gc internal project-mcp: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}

			agent, ok := resolveAgentIdentity(cfg, agentName, currentRigContext(cfg))
			if !ok {
				fmt.Fprintf(stderr, "gc internal project-mcp: unknown agent %q\n", agentName) //nolint:errcheck // best-effort stderr
				return errExit
			}
			if strings.TrimSpace(identity) == "" {
				identity = agent.QualifiedName()
			}
			absWorkdir, err := filepath.Abs(workdir)
			if err != nil {
				fmt.Fprintf(stderr, "gc internal project-mcp: resolving workdir %q: %v\n", workdir, err) //nolint:errcheck // best-effort stderr
				return errExit
			}

			resolved, err := config.ResolveProvider(&agent, &cfg.Workspace, cfg.Providers, exec.LookPath)
			if err != nil {
				// A provider-resolution failure only blocks projection when this
				// agent actually has effective MCP to project. Empty catalogs are a
				// no-op so unsupported/absent providers can be ignored there.
				catalog, lerr := loadEffectiveMCPForAgent(cityPath, cfg, &agent, identity, absWorkdir)
				if lerr != nil {
					fmt.Fprintf(stderr, "gc internal project-mcp: %v\n", lerr) //nolint:errcheck // best-effort stderr
					return errExit
				}
				if len(catalog.Servers) == 0 {
					return nil
				}
				fmt.Fprintf(stderr, "gc internal project-mcp: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}

			// The stage-2 pre-start writes the provider-native file a tmux
			// session reads; ACP sessions take their servers in-band.
			mcp, err := resolveAgentMCPProjection(cityPath, cfg, &agent, identity, absWorkdir, resolvedProviderLaunchFamily(resolved), false)
			if err != nil {
				fmt.Fprintf(stderr, "gc internal project-mcp: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			if len(mcp.Skipped) > 0 {
				fmt.Fprintf(stderr, "gc internal project-mcp: warning: %s\n", describeMCPServerSkips([]string{strconv.Quote(identity)}, mcp.Skipped)) //nolint:errcheck // best-effort stderr
			}
			catalog, projection := mcp.Catalog, mcp.Projection
			if projection.Provider == "" {
				return nil
			}
			if err := validateStage2TargetClaimants(cityPath, cfg, &agent, projection, exec.LookPath); err != nil {
				fmt.Fprintf(stderr, "gc internal project-mcp: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			if err := projection.ApplyWithStderr(fsys.OSFS{}, stderr); err != nil {
				var refused *materialize.MCPAdoptionRefusedError
				if errors.As(err, &refused) {
					// The work dir's own MCP config (often committed to the
					// repository) stays as it is; the session starts without
					// gc's servers. A pre-start's output is discarded when it
					// succeeds, so resolveTemplate reports this too.
					fmt.Fprintf(stderr, "gc internal project-mcp: warning: %s\n", describeMCPAdoptionRefused([]string{identity}, projection, refused)) //nolint:errcheck // best-effort stderr
					return nil
				}
				fmt.Fprintf(stderr, "gc internal project-mcp: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			if len(catalog.Servers) > 0 {
				ensureMCPGitignoreBestEffort(absWorkdir, []string{mcpGitignoreEntry(projection)}, stderr)
				fmt.Fprintf(stdout, "projected %d MCP server(s) into %s\n", len(catalog.Servers), projection.Target) //nolint:errcheck // best-effort stdout
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&agentName, "agent", "", "qualified template identity (dir/name or name)")
	cmd.Flags().StringVar(&identity, "identity", "", "concrete agent/session identity for template expansion")
	cmd.Flags().StringVar(&workdir, "workdir", "", "workdir whose native provider config should be reconciled")
	return cmd
}
