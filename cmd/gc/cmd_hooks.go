package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/hooks"
)

func newHooksCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hooks",
		Short: "Manage agent hook files",
		Long: `Manage provider agent hook files for the current city.

Providers (claude, codex, gemini, opencode, copilot, cursor, pi, omp)
each ship a hook file embedded into the gc binary. "gc init" and
"gc rig add" install these files; "gc hooks sync" reinstalls them so
changes to hooks/claude.json propagate to .gc/settings.json.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return c.Help()
		},
	}
	cmd.AddCommand(newHooksSyncCmd(stdout, stderr))
	return cmd
}

func newHooksSyncCmd(stdout, stderr io.Writer) *cobra.Command {
	var providersFlag []string
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Reinstall agent hook files for the current city",
		Long: `Reinstall agent hook files for the current city.

Resolves providers from workspace.install_agent_hooks (default
["claude"] when unset), validates them, and calls the installer.
Propagates hooks/claude.json changes into .gc/settings.json. The
operation is idempotent — unchanged files are left alone, so running
sync twice is a no-op on the second call.

Use --providers to override the config list (comma-separated or
repeated). Use --dry-run to report the resolved provider list
without writing.`,
		Example: `  gc hooks sync
  gc hooks sync --providers claude,gemini
  gc hooks sync --dry-run`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdHooksSync(providersFlag, dryRun, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&providersFlag, "providers", nil,
		"override providers to install (comma-separated; default: from workspace.install_agent_hooks)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"report the resolved providers without writing files")
	return cmd
}

// cmdHooksSync resolves the city root and delegates to doHooksSync.
func cmdHooksSync(providersFlag []string, dryRun bool, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc hooks sync: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return doHooksSync(fsys.OSFS{}, cityPath, providersFlag, dryRun, stdout, stderr)
}

// doHooksSync is the pure logic for "gc hooks sync". Loads city.toml to
// discover workspace-level install_agent_hooks, validates the resolved
// provider list, and calls hooks.Install. Accepts an injected FS so
// tests can assert writes without touching the real filesystem.
func doHooksSync(fs fsys.FS, cityPath string, providersFlag []string, dryRun bool, stdout, stderr io.Writer) int {
	providers := providersFlag
	flagOverride := len(providers) > 0
	if !flagOverride {
		cfg, err := loadCityConfig(cityPath)
		if err != nil {
			fmt.Fprintf(stderr, "gc hooks sync: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		providers = cfg.Workspace.InstallAgentHooks
		if len(providers) == 0 {
			providers = []string{"claude"}
		}
	}

	if err := hooks.Validate(providers); err != nil {
		fmt.Fprintf(stderr, "gc hooks sync: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if dryRun {
		fmt.Fprintf(stdout, "would sync providers: %s\n", strings.Join(providers, ", ")) //nolint:errcheck // best-effort stdout
		return 0
	}

	if err := hooks.Install(fs, cityPath, cityPath, providers); err != nil {
		fmt.Fprintf(stderr, "gc hooks sync: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	fmt.Fprintf(stdout, "synced providers: %s\n", strings.Join(providers, ", "))                     //nolint:errcheck // best-effort stdout
	fmt.Fprintln(stdout, "hint: run 'gc supervisor reload' to pick up changes in a live supervisor") //nolint:errcheck // best-effort stdout
	return 0
}
