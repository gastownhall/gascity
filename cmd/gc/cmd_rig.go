package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/hooks"
	"github.com/gastownhall/gascity/internal/supervisor"
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
				fmt.Fprintln(stderr, "gc rig: missing subcommand (add, default, list, restart, resume, status, suspend)") //nolint:errcheck // best-effort stderr
			} else {
				fmt.Fprintf(stderr, "gc rig: unknown subcommand %q\n", args[0]) //nolint:errcheck // best-effort stderr
			}
			return errExit
		},
	}
	cmd.AddCommand(
		newRigAddCmd(stdout, stderr),
		newRigDefaultCmd(stdout, stderr),
		newRigListCmd(stdout, stderr),
		newRigRemoveCmd(stdout, stderr),
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
	var nameFlag string
	var prefixFlag string
	var adoptFlag bool
	cmd := &cobra.Command{
		Use:   "add <path>",
		Short: "Register a project as a rig",
		Long: `Register an external project directory as a rig.

Initializes beads database, installs agent hooks if configured,
generates cross-rig routes, and appends the rig to city.toml.
If the target directory doesn't exist, it is created. Use --include
to apply a pack directory that defines the rig's agent configuration.

Use --name to set the rig name explicitly (default: directory basename).
Use --prefix to set the bead ID prefix explicitly (default: derived from name).
Use --start-suspended to add the rig in a suspended state (dormant-by-default).
The rig's agents won't spawn until explicitly resumed with "gc rig resume".

Use --adopt to register a directory that already has a fully initialized
.beads/ directory. Skips beads init and downgrades the git repo check
from error to warning.`,
		Example: `  gc rig add /path/to/project
  gc rig add /path/to/project --name myrig
  gc rig add /path/to/project --prefix r1
  gc rig add ./my-project --include packs/gastown
  gc rig add ./my-project --include packs/gastown --start-suspended
  gc rig add /path/to/existing --adopt`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdRigAdd(args, include, nameFlag, prefixFlag, startSuspended, adoptFlag, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&include, "include", "", "pack directory for rig agents")
	cmd.Flags().StringVar(&nameFlag, "name", "", "rig name (default: directory basename)")
	cmd.Flags().StringVar(&prefixFlag, "prefix", "", "bead ID prefix (default: derived from name)")
	cmd.Flags().BoolVar(&startSuspended, "start-suspended", false, "add rig in suspended state (dormant-by-default)")
	cmd.Flags().BoolVar(&adoptFlag, "adopt", false, "adopt existing .beads/ directory (skip init)")
	return cmd
}

func newRigListCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonFlag bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List registered rigs",
		Long: `List all registered rigs with their paths, prefixes, and beads status.

Shows the HQ rig (the city itself) and all configured rigs. Each rig
displays its bead ID prefix and whether its beads database is initialized.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdRigList(args, jsonFlag, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "Output in JSON format")
	return cmd
}

// cmdRigAdd registers an external project directory as a rig in the city.
func cmdRigAdd(args []string, include, nameOverride, prefixOverride string, startSuspended, adopt bool, stdout, stderr io.Writer) int {
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
	return doRigAdd(fsys.OSFS{}, cityPath, rigPath, include, nameOverride, prefixOverride, startSuspended, adopt, stdout, stderr)
}

// doRigAdd is the pure logic for "gc rig add". Operations are ordered so that
// city.toml is written last — if any earlier step fails, config is unchanged.
// This prevents partial-state bugs where city.toml lists a rig but the rig's
// infrastructure (beads, routes) was never created.
func doRigAdd(fs fsys.FS, cityPath, rigPath, include, nameOverride, prefixOverride string, startSuspended, adopt bool, stdout, stderr io.Writer) int {
	// Validate prefix format: hyphens break beadPrefix() which splits on
	// the first '-' to extract the rig prefix from a bead ID.
	if prefixOverride != "" && strings.Contains(prefixOverride, "-") {
		fmt.Fprintf(stderr, "gc rig add: --prefix %q must not contain hyphens (conflicts with bead ID format)\n", prefixOverride) //nolint:errcheck // best-effort stderr
		return 1
	}

	fi, err := fs.Stat(rigPath)
	if err != nil {
		if adopt {
			fmt.Fprintf(stderr, "gc rig add: --adopt requires an existing directory: %s\n", rigPath) //nolint:errcheck // best-effort stderr
			return 1
		}
		// Directory doesn't exist — create it.
		if err := fs.MkdirAll(rigPath, 0o755); err != nil {
			fmt.Fprintf(stderr, "gc rig add: creating %s: %v\n", rigPath, err) //nolint:errcheck // best-effort stderr
			return 1
		}
	} else if !fi.IsDir() {
		fmt.Fprintf(stderr, "gc rig add: %s is not a directory\n", rigPath) //nolint:errcheck // best-effort stderr
		return 1
	}

	// When adopting, validate that .beads/metadata.json exists.
	if adopt {
		metaPath := filepath.Join(rigPath, ".beads", "metadata.json")
		if _, err := fs.Stat(metaPath); err != nil {
			fmt.Fprintf(stderr, "gc rig add: --adopt requires .beads/metadata.json in %s\n", rigPath) //nolint:errcheck // best-effort stderr
			return 1
		}
	}

	name := nameOverride
	if name == "" {
		name = filepath.Base(rigPath)
	}

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
	// An explicit --prefix flag takes precedence for new rigs.
	// Canonicalize to lowercase: runtime lookup (findRigByPrefix) is
	// case-insensitive, so validation must match.
	var prefix string
	switch {
	case reAdd:
		prefix = existingRig.EffectivePrefix()
	case prefixOverride != "":
		prefix = strings.ToLower(prefixOverride)
	default:
		prefix = config.DeriveBeadsPrefix(name)
	}

	// Guard: if the rig already has a .beads/config.yaml with a different
	// prefix, refuse to proceed. Silently overwriting would mix bead IDs
	// when the same rig is registered in multiple cities.
	if existingPrefix, ok := readBeadsPrefix(fs, rigPath); ok && existingPrefix != prefix {
		if reAdd {
			// On re-add, --prefix is ignored (we use the existing rig's
			// configured prefix). Direct the user to edit city.toml.
			fmt.Fprintf(stderr, "gc rig add: rig %q has bead prefix %q but city.toml has %q; "+ //nolint:errcheck // best-effort stderr
				"edit city.toml to set prefix = %q, or remove %s/.beads to reinitialize\n",
				name, existingPrefix, prefix, existingPrefix, rigPath)
		} else {
			fmt.Fprintf(stderr, "gc rig add: rig %q already has bead prefix %q (requested %q); "+ //nolint:errcheck // best-effort stderr
				"use --prefix %s to match, or remove %s/.beads to reinitialize\n",
				name, existingPrefix, prefix, existingPrefix, rigPath)
		}
		return 1
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
		if prefixOverride != "" && strings.ToLower(prefixOverride) != existingRig.EffectivePrefix() {
			fmt.Fprintf(stderr, "gc rig add: warning: --prefix=%s ignored (existing: %s); edit city.toml to change\n", //nolint:errcheck // best-effort stderr
				prefixOverride, existingRig.EffectivePrefix())
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

	// Initialize beads for the rig. When --adopt is set, skip init
	// entirely — the existing .beads/ directory is already valid.
	if adopt {
		w("  Adopted existing beads database")
	} else {
		// Probes the backing service first; if the probe fails (e.g.
		// Dolt not yet ready), falls back to direct init — the city is
		// likely already running and the probe script may just be
		// checking the wrong state.
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
	}

	// Write rig-scoped .gitignore entries.
	if err := ensureGitignoreEntries(fs, rigPath, rigGitignoreEntries); err != nil {
		fmt.Fprintf(stderr, "gc rig add: writing .gitignore: %v\n", err) //nolint:errcheck // best-effort stderr
		// Non-fatal — rig is still usable without .gitignore.
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
		// Store the canonicalized (lowercased) prefix, not the raw flag
		// value. EffectivePrefix() returns the stored value as-is, and
		// downstream consumers (findRigByPrefix, ValidateRigs) must agree
		// on casing with .beads/config.yaml (always lowercase).
		storedPrefix := ""
		if prefixOverride != "" {
			storedPrefix = strings.ToLower(prefixOverride)
		}
		rig := config.Rig{
			Name:      name,
			Path:      rigPath,
			Prefix:    storedPrefix,
			Suspended: startSuspended,
		}
		switch {
		case include != "":
			rig.Includes = []string{include}
		case len(cfg.Workspace.DefaultRigIncludes) > 0:
			rig.Includes = append([]string{}, cfg.Workspace.DefaultRigIncludes...)
		}
		cfg.Rigs = append(cfg.Rigs, rig)
		if err := config.ValidateRigs(cfg.Rigs, config.EffectiveHQPrefix(cfg)); err != nil {
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

	// --- Phase 4: Global registry (cities.toml) ---

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.RegisterRig(rigPath, name, cityPath); err != nil {
		// Non-fatal: global registry is a convenience index.
		fmt.Fprintf(stderr, "gc rig add: warning: updating global registry: %v\n", err) //nolint:errcheck // best-effort stderr
	} else {
		w("  Registered in global rig index")
	}

	// --- Phase 5: Write GT_ROOT to .beads/.env ---

	if err := writeBeadsEnvGTRoot(fs, rigPath, cityPath); err != nil {
		fmt.Fprintf(stderr, "gc rig add: warning: writing .beads/.env: %v\n", err) //nolint:errcheck // best-effort stderr
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
func cmdRigList(args []string, jsonOutput bool, stdout, stderr io.Writer) int {
	_ = args // no arguments used yet
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc rig list: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return doRigList(fsys.OSFS{}, cityPath, jsonOutput, stdout, stderr)
}

// RigListJSON is the JSON output format for "gc rig list --json".
type RigListJSON struct {
	CityPath string        `json:"city_path"`
	CityName string        `json:"city_name"`
	Rigs     []RigListItem `json:"rigs"`
}

// RigListItem is one rig entry in the JSON output.
type RigListItem struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Prefix    string `json:"prefix"`
	HQ        bool   `json:"hq"`
	Suspended bool   `json:"suspended"`
	Beads     string `json:"beads"`
}

// doRigList is the pure logic for "gc rig list". It reads rigs from city.toml
// and prints each with its prefix and beads status. Accepts an injected FS for
// testability.
func doRigList(fs fsys.FS, cityPath string, jsonOutput bool, stdout, stderr io.Writer) int {
	cfg, err := loadCityConfigFS(fs, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		fmt.Fprintf(stderr, "gc rig list: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	hqPrefix := config.EffectiveHQPrefix(cfg)
	cityName := cfg.EffectiveCityName()

	if jsonOutput {
		result := RigListJSON{
			CityPath: cityPath,
			CityName: cityName,
		}
		result.Rigs = append(result.Rigs, RigListItem{
			Name:   cityName,
			Path:   cityPath,
			Prefix: hqPrefix,
			HQ:     true,
			Beads:  rigBeadsStatus(fs, cityPath),
		})
		for i := range cfg.Rigs {
			result.Rigs = append(result.Rigs, RigListItem{
				Name:      cfg.Rigs[i].Name,
				Path:      cfg.Rigs[i].Path,
				Prefix:    cfg.Rigs[i].EffectivePrefix(),
				Suspended: cfg.Rigs[i].Suspended,
				Beads:     rigBeadsStatus(fs, cfg.Rigs[i].Path),
			})
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(result); err != nil {
			fmt.Fprintf(stderr, "gc rig list: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return 0
	}

	w := func(s string) { fmt.Fprintln(stdout, s) } //nolint:errcheck // best-effort stdout
	w("")
	w(fmt.Sprintf("Rigs in %s:", cityPath))

	// HQ rig (the city itself).
	hqBeads := rigBeadsStatus(fs, cityPath)
	displayName := cfg.Workspace.Name
	if displayName == "" {
		displayName = filepath.Base(cityPath)
	}
	w("")
	w(fmt.Sprintf("  %s (HQ):", displayName))
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
		Use:   "suspend [name]",
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
	ctx, err := resolveContext()
	if err != nil {
		fmt.Fprintf(stderr, "gc rig suspend: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	rigName := ctx.RigName
	if len(args) > 0 {
		rigName = args[0]
	}
	if rigName == "" {
		fmt.Fprintln(stderr, "gc rig suspend: missing rig name") //nolint:errcheck // best-effort stderr
		return 1
	}
	cityPath := ctx.CityPath
	if c := apiClient(cityPath); c != nil {
		err := c.SuspendRig(rigName)
		if err == nil {
			fmt.Fprintf(stdout, "Suspended rig '%s'\n", rigName) //nolint:errcheck // best-effort stdout
			return 0
		}
		if !api.ShouldFallback(err) {
			fmt.Fprintf(stderr, "gc rig suspend: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		// Connection error — fall through to direct mutation.
	}
	return doRigSuspend(fsys.OSFS{}, cityPath, rigName, stdout, stderr)
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
		Use:   "resume [name]",
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
	ctx, err := resolveContext()
	if err != nil {
		fmt.Fprintf(stderr, "gc rig resume: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	rigName := ctx.RigName
	if len(args) > 0 {
		rigName = args[0]
	}
	if rigName == "" {
		fmt.Fprintln(stderr, "gc rig resume: missing rig name") //nolint:errcheck // best-effort stderr
		return 1
	}
	cityPath := ctx.CityPath
	if c := apiClient(cityPath); c != nil {
		err := c.ResumeRig(rigName)
		if err == nil {
			fmt.Fprintf(stdout, "Resumed rig '%s'\n", rigName) //nolint:errcheck // best-effort stdout
			return 0
		}
		if !api.ShouldFallback(err) {
			fmt.Fprintf(stderr, "gc rig resume: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		// Connection error — fall through to direct mutation.
	}
	return doRigResume(fsys.OSFS{}, cityPath, rigName, stdout, stderr)
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

func newRigRemoveCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a rig from the city",
		Long: `Remove a rig from the current city's configuration.

Removes the rig entry from city.toml and updates the global rig index
in cities.toml. If the rig no longer belongs to any city, it is removed
from the global index entirely. If this city was the rig's default,
the default is cleared.`,
		Example: `  gc rig remove myrig`,
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdRigRemove(args[0], stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

// cmdRigRemove removes a rig from the current city and updates the global registry.
func cmdRigRemove(rigName string, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc rig remove: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	tomlPath := filepath.Join(cityPath, "city.toml")
	cfg, err := loadCityConfigForEditFS(fsys.OSFS{}, tomlPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc rig remove: loading config: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Find and remove the rig from config.
	var removedPath string
	found := false
	filtered := cfg.Rigs[:0]
	for _, r := range cfg.Rigs {
		if r.Name == rigName {
			removedPath = r.Path
			if !filepath.IsAbs(removedPath) {
				removedPath = filepath.Join(cityPath, removedPath)
			}
			removedPath = filepath.Clean(removedPath)
			found = true
			continue
		}
		filtered = append(filtered, r)
	}
	if !found {
		fmt.Fprintln(stderr, rigNotFoundMsg("gc rig remove", rigName, cfg)) //nolint:errcheck // best-effort stderr
		return 1
	}
	cfg.Rigs = filtered

	// Write updated config.
	content, err := cfg.Marshal()
	if err != nil {
		fmt.Fprintf(stderr, "gc rig remove: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := os.WriteFile(tomlPath, content, 0o644); err != nil {
		fmt.Fprintf(stderr, "gc rig remove: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Update global registry: check if rig is still in any other city.
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	remainingPaths := rigCityPaths(reg, removedPath)
	if len(remainingPaths) == 0 {
		// No other city has this rig — remove from global index.
		_ = reg.UnregisterRig(removedPath)
	} else {
		// Still in other cities — update default if it pointed to this city.
		if entry, ok := reg.LookupRigByName(rigName); ok && samePath(entry.DefaultCity, cityPath) {
			var newDefault string
			if len(remainingPaths) == 1 {
				newDefault = remainingPaths[0]
			}
			_ = reg.SetRigDefault(removedPath, newDefault)

			// Update .beads/.env and routes for the rig's new default city.
			if newDefault != "" {
				_ = writeBeadsEnvGTRoot(fsys.OSFS{}, removedPath, newDefault)
				if newCfg, err := loadCityConfig(newDefault); err == nil {
					resolveRigPaths(newDefault, newCfg.Rigs)
					newRigs := collectRigRoutes(newDefault, newCfg)
					_ = writeAllRoutes(newRigs)
				}
			}
		}
	}

	// Regenerate routes.
	resolveRigPaths(cityPath, cfg.Rigs)
	allRigs := collectRigRoutes(cityPath, cfg)
	if err := writeAllRoutes(allRigs); err != nil {
		fmt.Fprintf(stderr, "gc rig remove: warning: writing routes: %v\n", err) //nolint:errcheck // best-effort stderr
	}

	_ = pokeController(cityPath)
	fmt.Fprintf(stdout, "Removed rig '%s'\n", rigName) //nolint:errcheck // best-effort stdout
	return 0
}

func newRigDefaultCmd(stdout, stderr io.Writer) *cobra.Command {
	var cityName string
	cmd := &cobra.Command{
		Use:   "default <rig-name>",
		Short: "Set the default city for a rig",
		Long: `Set which city a rig resolves to when accessed from outside any city tree.

When a rig belongs to multiple cities, gc commands run from the rig
directory need to know which city to use. This command sets that default.
It also updates the rig's .beads/.env with GT_ROOT and rewrites
routes.jsonl from the new default city's rig set.`,
		Example: `  gc rig default myrig --city alpha
  gc rig default /path/to/myrig --city beta`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cityName == "" {
				fmt.Fprintln(stderr, "gc rig default: --city is required") //nolint:errcheck // best-effort stderr
				return errExit
			}
			if cmdRigDefault(args[0], cityName, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&cityName, "city", "", "city name or path to set as default (required)")
	return cmd
}

// cmdRigDefault sets the default city for a rig.
func cmdRigDefault(rigNameOrPath, cityNameOrPath string, stdout, stderr io.Writer) int {
	reg := supervisor.NewRegistry(supervisor.RegistryPath())

	// Resolve rig.
	entry, ok := reg.LookupRigByName(rigNameOrPath)
	if !ok {
		abs, err := filepath.Abs(rigNameOrPath)
		if err != nil {
			fmt.Fprintf(stderr, "gc rig default: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		entry, ok = reg.LookupRigByPath(abs)
		if !ok {
			fmt.Fprintf(stderr, "gc rig default: rig %q is not registered\n", rigNameOrPath) //nolint:errcheck // best-effort stderr
			return 1
		}
	}

	// Resolve city.
	cityPath, err := resolveCityByNameOrPath(reg, cityNameOrPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc rig default: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Validate rig belongs to this city.
	cfg, err := loadCityConfig(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc rig default: loading city config: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	found := false
	for _, r := range cfg.Rigs {
		rp := r.Path
		if !filepath.IsAbs(rp) {
			rp = filepath.Join(cityPath, rp)
		}
		if samePath(rp, entry.Path) {
			found = true
			break
		}
	}
	if !found {
		fmt.Fprintf(stderr, "gc rig default: rig %q is not registered in city %q\n", entry.Name, cityNameOrPath) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Update default in cities.toml.
	if err := reg.SetRigDefault(entry.Path, cityPath); err != nil {
		fmt.Fprintf(stderr, "gc rig default: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Update .beads/.env.
	if err := writeBeadsEnvGTRoot(fsys.OSFS{}, entry.Path, cityPath); err != nil {
		fmt.Fprintf(stderr, "gc rig default: warning: writing .beads/.env: %v\n", err) //nolint:errcheck // best-effort stderr
	}

	// Rewrite routes.jsonl from the new default city's rig set.
	resolveRigPaths(cityPath, cfg.Rigs)
	allRigs := collectRigRoutes(cityPath, cfg)
	if err := writeAllRoutes(allRigs); err != nil {
		fmt.Fprintf(stderr, "gc rig default: warning: writing routes: %v\n", err) //nolint:errcheck // best-effort stderr
	}

	cityName := cfg.Workspace.Name
	if cityName == "" {
		cityName = filepath.Base(cityPath)
	}
	fmt.Fprintf(stdout, "Set default city for rig '%s' to '%s'\n", entry.Name, cityName) //nolint:errcheck // best-effort stdout
	return 0
}

// resolveCityByNameOrPath resolves a city argument that can be either a name
// (looked up in the registry) or a filesystem path.
func resolveCityByNameOrPath(reg *supervisor.Registry, nameOrPath string) (string, error) {
	// Try as a path first.
	abs, err := filepath.Abs(nameOrPath)
	if err == nil {
		if citylayout.HasCityConfig(abs) || citylayout.HasRuntimeRoot(abs) {
			return normalizePathForCompare(abs), nil
		}
	}

	// Try as a name in the registry.
	cities, err := reg.List()
	if err != nil {
		return "", fmt.Errorf("listing cities: %w", err)
	}
	for _, c := range cities {
		if c.EffectiveName() == nameOrPath {
			return c.Path, nil
		}
	}

	return "", fmt.Errorf("city %q not found (not a path or registered name)", nameOrPath)
}

// writeBeadsEnvGTRoot writes or updates GT_ROOT in <rigPath>/.beads/.env.
// Preserves existing entries, only replaces the GT_ROOT line.
func writeBeadsEnvGTRoot(fs fsys.FS, rigPath, cityPath string) error {
	envPath := filepath.Join(rigPath, ".beads", ".env")

	// Read existing .env content (may not exist).
	existing, err := fs.ReadFile(envPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading %s: %w", envPath, err)
	}

	// Parse existing lines, replacing GT_ROOT if found.
	var lines []string
	found := false
	if len(existing) > 0 {
		for _, line := range strings.Split(string(existing), "\n") {
			if strings.HasPrefix(line, "GT_ROOT=") {
				lines = append(lines, fmt.Sprintf("GT_ROOT=%s", cityPath))
				found = true
			} else {
				lines = append(lines, line)
			}
		}
	}
	if !found {
		// Remove trailing empty line before appending.
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		lines = append(lines, fmt.Sprintf("GT_ROOT=%s", cityPath))
	}

	content := strings.Join(lines, "\n")
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}

	if err := ensureBeadsDir(fs, filepath.Join(rigPath, ".beads")); err != nil {
		return fmt.Errorf("creating .beads dir: %w", err)
	}
	return fs.WriteFile(envPath, []byte(content), 0o644)
}

// readBeadsPrefix reads the issue_prefix from an existing .beads/config.yaml
// in the given rig directory. Returns the prefix and true if found, or empty
// string and false if the file doesn't exist or has no prefix. Checks both
// the underscore form (issue_prefix) and dash form (issue-prefix) since the
// lifecycle code writes both.
func readBeadsPrefix(fs fsys.FS, rigPath string) (string, bool) {
	data, err := fs.ReadFile(filepath.Join(rigPath, ".beads", "config.yaml"))
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		for _, key := range []string{"issue_prefix:", "issue-prefix:"} {
			if strings.HasPrefix(trimmed, key) {
				val := strings.TrimSpace(strings.TrimPrefix(trimmed, key))
				if val != "" {
					return strings.ToLower(val), true
				}
			}
		}
	}
	return "", false
}
