package main

import (
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
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

			catalog, projection, err := resolveAgentMCPProjection(cityPath, cfg, &agent, identity, absWorkdir, resolvedProviderLaunchFamily(resolved))
			if err != nil {
				fmt.Fprintf(stderr, "gc internal project-mcp: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			if projection.Provider == "" {
				return nil
			}
			if err := validateStage2TargetClaimants(cityPath, cfg, &agent, projection, exec.LookPath); err != nil {
				fmt.Fprintf(stderr, "gc internal project-mcp: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			if err := projection.ApplyWithStderr(fsys.OSFS{}, stderr); err != nil {
				fmt.Fprintf(stderr, "gc internal project-mcp: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			if len(catalog.Servers) > 0 {
				ensureMCPGitignoreBestEffort(absWorkdir, stderr)
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
