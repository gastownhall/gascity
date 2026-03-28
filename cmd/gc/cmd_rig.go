package main

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/hooks"
	"github.com/spf13/cobra"
)

func newRigCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rig",
		Short: "Manage rigs (projects)",
		Long: `Manage rigs (external project directories) registered with the city.

Rigs are project directories that the city orchestrates. Each rig gets
its own beads database, agent hooks, and cross-rig routing. Agents
are scoped to rigs via their "dir" field.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				fmt.Fprintln(stderr, "gc rig: missing subcommand (add, list, restart, resume, status, suspend)") //nolint:errcheck // best-effort stderr
			} else {
				fmt.Fprintf(stderr, "gc rig: unknown subcommand %q\n", args[0]) //nolint:errcheck // best-effort stderr
			}
			return errExit
		},
	}
	cmd.AddCommand(
		newRigAddCmd(stdout, stderr),
		newRigListCmd(stdout, stderr),
		newRigRestartCmd(stdout, stderr),
		newRigResumeCmd(stdout, stderr),
		newRigStatusCmd(stdout, stderr),
		newRigSuspendCmd(stdout, stderr),
	)
	return cmd
}

func newRigAddCmd(stdout, stderr io.Writer) *cobra.Command {
	var include string
	var startSuspended bool
	cmd := &cobra.Command{
		Use:   "add <path>",
		Short: "Register a project as a rig",
		Long: `Register an external project directory as a rig.

Initializes beads database, installs agent hooks if configured,
generates cross-rig routes, and appends the rig to city.toml.
If the target directory doesn't exist, it is created. Use --include
to apply a pack directory that defines the rig's agent configuration.

Use --start-suspended to add the rig in a suspended state (dormant-by-default).
The rig's agents won't spawn until explicitly resumed with "gc rig resume".`,
		Example: `  gc rig add /path/to/project
  gc rig add ./my-project --include packs/gastown
  gc rig add ./my-project --include packs/gastown --start-suspended`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdRigAdd(args, include, startSuspended, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&include, "include", "", "pack directory for rig agents")
	cmd.Flags().BoolVar(&startSuspended, "start-suspended", false, "add rig in suspended state (dormant-by-default)")
	return cmd
}

func newRigListCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List registered rigs",
		Long: `List all registered rigs with their paths, prefixes, and beads status.

Shows the HQ rig (the city itself) and all configured rigs. Each rig
displays its bead ID prefix and whether its beads database is initialized.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdRigList(args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

// cmdRigAdd registers an external project directory as a rig in the city.
func cmdRigAdd(args []string, include string, startSuspended bool, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "gc rig add: missing path") //nolint:errcheck // best-effort stderr
		return 1
	}

	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc rig add: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	rigPath, err := filepath.Abs(args[0])
	if err != nil {
		fmt.Fprintf(stderr, "gc rig add: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return doRigAdd(fsys.OSFS{}, cityPath, rigPath, include, startSuspended, stdout, stderr)
}

// doRigAdd is the pure logic for "gc rig add". Operations are ordered so that
// city.toml is written last — if any earlier step fails, config is unchanged.
// This prevents partial-state bugs where city.toml lists a rig but the rig's
// infrastructure (beads, routes) was never created.
func doRigAdd(fs fsys.FS, cityPath, rigPath, include string, startSuspended bool, stdout, stderr io.Writer) int {
	fi, err := fs.Stat(rigPath)
	if err != nil {
		// Directory doesn't exist — create it.
		if err := fs.MkdirAll(rigPath, 0o755); err != nil {
			fmt.Fprintf(stderr, "gc rig add: creating %s: %v\n", rigPath, err) //nolint:errcheck // best-effort stderr
			return 1
		}
	} else if !fi.IsDir() {
		fmt.Fprintf(stderr, "gc rig add: %s is not a directory\n", rigPath) //nolint:errcheck // best-effort stderr
		return 1
	}

	name := filepath.Base(rigPath)

	// Check for git repo.
	_, gitErr := fs.Stat(filepath.Join(rigPath, ".git"))
	hasGit := gitErr == nil

	// Load existing config to check for duplicates.
	tomlPath := filepath.Join(cityPath, "city.toml")
	cfg, err := loadCityConfigForEditFS(fs, tomlPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc rig add: loading config: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Check for existing rig with same name.
	var reAdd bool
	var existingRig *config.Rig
	for i, r := range cfg.Rigs {
		if r.Name == name {
			existPath := r.Path
			if !filepath.IsAbs(existPath) {
				existPath = filepath.Join(cityPath, existPath)
			}
			if filepath.Clean(existPath) != filepath.Clean(rigPath) {
				fmt.Fprintf(stderr, "gc rig add: rig %q already registered at %s (not %s)\n", //nolint:errcheck // best-effort stderr
					name, r.Path, rigPath)
				return 1
			}
			reAdd = true
			existingRig = &cfg.Rigs[i]
			break
		}
	}

	// Derive prefix. On re-add, use the existing rig's effective prefix
	// to avoid splitting bead state when an explicit prefix is configured.
	var prefix string
	if reAdd {
		prefix = existingRig.EffectivePrefix()
	} else {
		prefix = config.DeriveBeadsPrefix(name)
	}

	// --- Phase 1: Infrastructure (all fallible, before touching city.toml) ---

	w := func(s string) { fmt.Fprintln(stdout, s) } //nolint:errcheck // best-effort stdout
	if reAdd {
		w(fmt.Sprintf("Re-initializing rig '%s'...", name))
		// Warn when explicitly-provided flags differ from existing config.
		// Only warn for non-default values to avoid spurious warnings when
		// re-running without flags (e.g., plain "gc rig add /path").
		if startSuspended && startSuspended != existingRig.Suspended {
			fmt.Fprintf(stderr, "gc rig add: warning: --start-suspended ignored (existing: suspended=%v); edit city.toml to change\n", //nolint:errcheck // best-effort stderr
				existingRig.Suspended)
		}
		if include != "" && (len(existingRig.Includes) == 0 || existingRig.Includes[0] != include) {
			fmt.Fprintf(stderr, "gc rig add: warning: --include=%s ignored (existing: %v); edit city.toml to change\n", //nolint:errcheck // best-effort stderr
				include, existingRig.Includes)
		}
	} else {
		w(fmt.Sprintf("Adding rig '%s'...", name))
	}
	if hasGit {
		w(fmt.Sprintf("  Detected git repo at %s", rigPath))
	}
	w(fmt.Sprintf("  Prefix: %s", prefix))
	if !reAdd {
		switch {
		case include != "":
			w(fmt.Sprintf("  Include: %s", include))
		case len(cfg.Workspace.DefaultRigIncludes) > 0:
			w(fmt.Sprintf("  Include: %s (default)", strings.Join(cfg.Workspace.DefaultRigIncludes, ", ")))
		}
	}

	// Initialize beads for the rig. Probes the backing service first;
	// if the probe fails (e.g. Dolt not yet ready), falls back to
	// direct init — the city is likely already running and the probe
	// script may just be checking the wrong state.
	deferred, err := initDirIfReady(cityPath, rigPath, prefix)
	if err != nil {
		fmt.Fprintf(stderr, "gc rig add: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if deferred {
		// City is probably running — try direct init.
		if err := initAndHookDir(cityPath, rigPath, prefix); err != nil {
			w("  Beads init deferred to controller")
		} else {
			w("  Initialized beads database")
		}
	} else {
		w("  Initialized beads database")
	}

	// Install provider agent hooks (Claude, Gemini, etc.) if configured.
	if ih := cfg.Workspace.InstallAgentHooks; len(ih) > 0 {
		if err := hooks.Install(fs, cityPath, rigPath, ih); err != nil {
			fmt.Fprintf(stderr, "gc rig add: installing agent hooks: %v\n", err) //nolint:errcheck // best-effort stderr
			// Non-fatal.
		}
	}

	// --- Phase 2: Commit config (only after infrastructure succeeds) ---
	// Skipped for re-adds (config already has this rig).

	if !reAdd {
		// Add rig to config and validate before writing.
		rig := config.Rig{
			Name:      name,
			Path:      rigPath,
			Suspended: startSuspended,
		}
		switch {
		case include != "":
			rig.Includes = []string{include}
		case len(cfg.Workspace.DefaultRigIncludes) > 0:
			rig.Includes = append([]string{}, cfg.Workspace.DefaultRigIncludes...)
		}
		cfg.Rigs = append(cfg.Rigs, rig)
		cityName := cfg.Workspace.Name
		if cityName == "" {
			cityName = filepath.Base(cityPath)
		}
		if err := config.ValidateRigs(cfg.Rigs, config.EffectiveHQPrefix(cfg.Workspace)); err != nil {
			fmt.Fprintf(stderr, "gc rig add: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}

		data, err := cfg.Marshal()
		if err != nil {
			fmt.Fprintf(stderr, "gc rig add: marshaling config: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}

		if err := fs.WriteFile(tomlPath, data, 0o644); err != nil {
			fmt.Fprintf(stderr, "gc rig add: writing config: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}

	// --- Phase 3: Routes (uses config, best-effort) ---

	// Ensure rig paths are absolute before route generation.
	resolveRigPaths(cityPath, cfg.Rigs)
	// Keep newly added or re-added rigs on the city-managed Dolt endpoint,
	// including rigs that live outside the city directory.
	syncConfiguredDoltPortFiles(cityPath, cfg.Rigs)

	// Generate routes for all rigs (HQ + all configured rigs).
	allRigs := collectRigRoutes(cityPath, cfg)
	if err := writeAllRoutes(allRigs); err != nil {
		fmt.Fprintf(stderr, "gc rig add: writing routes: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	w("  Generated routes.jsonl for cross-rig routing")

	// Resolve formulas for the new rig. Rigs inherit city formulas
	// so pool agents can use default sling formulas (mol-do-work).
	reloadedCfg, _, _ := config.LoadWithIncludes(fsys.OSFS{}, tomlPath)
	if reloadedCfg != nil {
		layers, ok := reloadedCfg.FormulaLayers.Rigs[name]
		if !ok || len(layers) == 0 {
			layers = reloadedCfg.FormulaLayers.City
		}
		if len(layers) > 0 {
			if rfErr := ResolveFormulas(rigPath, layers); rfErr != nil {
				fmt.Fprintf(stderr, "gc rig add: resolving formulas: %v\n", rfErr) //nolint:errcheck // best-effort
			}
		}
	}

	// Poke controller after config is committed so it picks up
	// deferred beads init and implicit agents for the new rig.
	_ = pokeController(cityPath)

	switch {
	case reAdd:
		w("Rig re-initialized.")
	case startSuspended:
		w("Rig added (suspended — use 'gc rig resume' to activate).")
	default:
		w("Rig added.")
	}
	return 0
}

// findEnclosingRig returns the rig whose path is a prefix of dir. It does
// prefix matching so that subdirectories of a rig are recognized.
func findEnclosingRig(dir string, rigs []config.Rig) (name, rigPath string, found bool) {
	cleanDir := filepath.Clean(dir)
	bestName, bestPath := "", ""
	for _, r := range rigs {
		cleanRig := filepath.Clean(r.Path)
		if cleanDir == cleanRig ||
			strings.HasPrefix(cleanDir, cleanRig+string(filepath.Separator)) {
			if len(cleanRig) > len(bestPath) {
				bestName = r.Name
				bestPath = cleanRig
				found = true
			}
		}
	}
	if found {
		return bestName, bestPath, true
	}
	return "", "", false
}

// cmdRigList lists all registered rigs in the current city.
func cmdRigList(args []string, stdout, stderr io.Writer) int {
	_ = args // no arguments used yet
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc rig list: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return doRigList(fsys.OSFS{}, cityPath, stdout, stderr)
}

// doRigList is the pure logic for "gc rig list". It reads rigs from city.toml
// and prints each with its prefix and beads status. Accepts an injected FS for
// testability.
func doRigList(fs fsys.FS, cityPath string, stdout, stderr io.Writer) int {
	cfg, err := loadCityConfigFS(fs, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		fmt.Fprintf(stderr, "gc rig list: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	cityName := cfg.Workspace.Name
	if cityName == "" {
		cityName = filepath.Base(cityPath)
	}
	hqPrefix := config.DeriveBeadsPrefix(cityName)

	w := func(s string) { fmt.Fprintln(stdout, s) } //nolint:errcheck // best-effort stdout
	w("")
	w(fmt.Sprintf("Rigs in %s:", cityPath))

	// HQ rig (the city itself).
	hqBeads := rigBeadsStatus(fs, cityPath)
	w("")
	w(fmt.Sprintf("  %s (HQ):", cityName))
	w(fmt.Sprintf("    Prefix: %s", hqPrefix))
	w(fmt.Sprintf("    Beads:  %s", hqBeads))

	// Configured rigs.
	for i := range cfg.Rigs {
		prefix := cfg.Rigs[i].EffectivePrefix()
		beads := rigBeadsStatus(fs, cfg.Rigs[i].Path)
		header := cfg.Rigs[i].Name
		if cfg.Rigs[i].Suspended {
			header += " (suspended)"
		}
		w("")
		w(fmt.Sprintf("  %s:", header))
		w(fmt.Sprintf("    Path:   %s", cfg.Rigs[i].Path))
		w(fmt.Sprintf("    Prefix: %s", prefix))
		w(fmt.Sprintf("    Beads:  %s", beads))
	}
	return 0
}

// rigBeadsStatus returns a human-readable beads status for a directory.
func rigBeadsStatus(fs fsys.FS, dir string) string {
	metaPath := filepath.Join(dir, ".beads", "metadata.json")
	if _, err := fs.Stat(metaPath); err == nil {
		return "initialized"
	}
	return "not initialized"
}

func newRigSuspendCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "suspend <name>",
		Short: "Suspend a rig (reconciler will skip its agents)",
		Long: `Suspend a rig by setting suspended=true in city.toml.

All agents scoped to the suspended rig are effectively suspended —
the reconciler skips them and gc hook returns empty. The rig's beads
database remains accessible. Use "gc rig resume" to restore.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdRigSuspend(args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

// cmdRigSuspend is the CLI entry point for suspending a rig.
func cmdRigSuspend(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "gc rig suspend: missing rig name") //nolint:errcheck // best-effort stderr
		return 1
	}
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc rig suspend: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if c := apiClient(cityPath); c != nil {
		err := c.SuspendRig(args[0])
		if err == nil {
			fmt.Fprintf(stdout, "Suspended rig '%s'\n", args[0]) //nolint:errcheck // best-effort stdout
			return 0
		}
		if !api.ShouldFallback(err) {
			fmt.Fprintf(stderr, "gc rig suspend: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		// Connection error — fall through to direct mutation.
	}
	return doRigSuspend(fsys.OSFS{}, cityPath, args[0], stdout, stderr)
}

// doRigSuspend sets suspended=true on the named rig in city.toml.
// Accepts an injected FS for testability.
func doRigSuspend(fs fsys.FS, cityPath, rigName string, stdout, stderr io.Writer) int {
	tomlPath := filepath.Join(cityPath, "city.toml")
	cfg, err := loadCityConfigForEditFS(fs, tomlPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc rig suspend: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	found := false
	for i := range cfg.Rigs {
		if cfg.Rigs[i].Name == rigName {
			cfg.Rigs[i].Suspended = true
			found = true
			break
		}
	}
	if !found {
		fmt.Fprintln(stderr, rigNotFoundMsg("gc rig suspend", rigName, cfg)) //nolint:errcheck // best-effort stderr
		return 1
	}

	content, err := cfg.Marshal()
	if err != nil {
		fmt.Fprintf(stderr, "gc rig suspend: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := fs.WriteFile(tomlPath, content, 0o644); err != nil {
		fmt.Fprintf(stderr, "gc rig suspend: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	fmt.Fprintf(stdout, "Suspended rig '%s'\n", rigName) //nolint:errcheck // best-effort stdout
	return 0
}

func newRigResumeCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "resume <name>",
		Short: "Resume a suspended rig",
		Long: `Resume a suspended rig by clearing suspended in city.toml.

The reconciler will start the rig's agents on its next tick.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdRigResume(args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

// cmdRigResume is the CLI entry point for resuming a suspended rig.
func cmdRigResume(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "gc rig resume: missing rig name") //nolint:errcheck // best-effort stderr
		return 1
	}
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc rig resume: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if c := apiClient(cityPath); c != nil {
		err := c.ResumeRig(args[0])
		if err == nil {
			fmt.Fprintf(stdout, "Resumed rig '%s'\n", args[0]) //nolint:errcheck // best-effort stdout
			return 0
		}
		if !api.ShouldFallback(err) {
			fmt.Fprintf(stderr, "gc rig resume: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		// Connection error — fall through to direct mutation.
	}
	return doRigResume(fsys.OSFS{}, cityPath, args[0], stdout, stderr)
}

// doRigResume clears suspended on the named rig in city.toml.
// Accepts an injected FS for testability.
func doRigResume(fs fsys.FS, cityPath, rigName string, stdout, stderr io.Writer) int {
	tomlPath := filepath.Join(cityPath, "city.toml")
	cfg, err := loadCityConfigForEditFS(fs, tomlPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc rig resume: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	found := false
	for i := range cfg.Rigs {
		if cfg.Rigs[i].Name == rigName {
			cfg.Rigs[i].Suspended = false
			found = true
			break
		}
	}
	if !found {
		fmt.Fprintln(stderr, rigNotFoundMsg("gc rig resume", rigName, cfg)) //nolint:errcheck // best-effort stderr
		return 1
	}

	content, err := cfg.Marshal()
	if err != nil {
		fmt.Fprintf(stderr, "gc rig resume: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := fs.WriteFile(tomlPath, content, 0o644); err != nil {
		fmt.Fprintf(stderr, "gc rig resume: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	fmt.Fprintf(stdout, "Resumed rig '%s'\n", rigName) //nolint:errcheck // best-effort stdout
	return 0
}
