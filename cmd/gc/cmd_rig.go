package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/hooks"
	"github.com/spf13/cobra"
)

const rigDeferredStoreInitWait = 30 * time.Second

var (
	rigReloadControllerConfig = reloadControllerConfig
	rigWaitForStoreAccessible = waitForRigStoreAccessible
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
				fmt.Fprintln(stderr, "gc rig: missing subcommand (add, list, remove, restart, resume, set-endpoint, status, suspend)") //nolint:errcheck // best-effort stderr
			} else {
				fmt.Fprintf(stderr, "gc rig: unknown subcommand %q\n", args[0]) //nolint:errcheck // best-effort stderr
			}
			return errExit
		},
	}
	cmd.AddCommand(
		newRigAddCmd(stdout, stderr),
		newRigListCmd(stdout, stderr),
		newRigRemoveCmd(stdout, stderr),
		newRigRestartCmd(stdout, stderr),
		newRigResumeCmd(stdout, stderr),
		newRigSetEndpointCmd(stdout, stderr),
		newRigStatusCmd(stdout, stderr),
		newRigSuspendCmd(stdout, stderr),
	)
	return cmd
}

func newRigAddCmd(stdout, stderr io.Writer) *cobra.Command {
	var includes []string
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
to apply a pack directory that defines the rig's agent configuration;
repeat the flag to compose multiple packs for one rig.

Use --name to set the rig name explicitly (default: directory basename).
Use --prefix to set the bead ID prefix explicitly (default: derived from name).
Use --start-suspended to add the rig in a suspended state (dormant-by-default).
The rig's agents won't spawn until explicitly resumed with "gc rig resume".

Use --adopt to register a directory that already has a fully initialized
.beads/ directory (must include both metadata.json and config.yaml).
Skips beads init; the git repo check remains informational.`,
		Example: `  gc rig add /path/to/project
  gc rig add /path/to/project --name myrig
  gc rig add /path/to/project --prefix r1
  gc rig add ./my-project --include packs/gastown
  gc rig add ./my-project --include packs/planner --include packs/architect
  gc rig add ./my-project --include packs/gastown --start-suspended
  gc rig add /path/to/existing --adopt`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdRigAdd(args, includes, nameFlag, prefixFlag, startSuspended, adoptFlag, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&includes, "include", nil, "pack directory for rig agents (repeatable)")
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
func cmdRigAdd(args []string, includes []string, nameOverride, prefixOverride string, startSuspended, adopt bool, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "gc rig add: missing path") //nolint:errcheck // best-effort stderr
		return 1
	}

	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc rig add: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	rigPath, err := resolveRigAddPath(cityPath, args[0])
	if err != nil {
		fmt.Fprintf(stderr, "gc rig add: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return doRigAdd(fsys.OSFS{}, cityPath, rigPath, includes, nameOverride, prefixOverride, startSuspended, adopt, stdout, stderr)
}

func resolveRigAddPath(cityPath, rigArg string) (string, error) {
	rigArg = strings.TrimSpace(rigArg)
	if rigArg == "" {
		return "", fmt.Errorf("missing path")
	}
	if filepath.IsAbs(rigArg) {
		return filepath.Clean(rigArg), nil
	}
	if strings.HasPrefix(rigArg, ".") {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		return filepath.Clean(filepath.Join(wd, rigArg)), nil
	}
	return filepath.Clean(filepath.Join(cityPath, rigArg)), nil
}

// doRigAdd is the pure logic for "gc rig add". Operations are ordered so that
// city.toml is written last — if any earlier step fails, config is unchanged.
// This prevents partial-state bugs where city.toml lists a rig but the rig's
// infrastructure (beads, routes) was never created.
func doRigAdd(fs fsys.FS, cityPath, rigPath string, includes []string, nameOverride, prefixOverride string, startSuspended, adopt bool, stdout, stderr io.Writer) int {
	// Validate prefix format: hyphens break beadPrefix() which splits on
	// the first '-' to extract the rig prefix from a bead ID.
	if prefixOverride != "" && strings.Contains(prefixOverride, "-") {
		fmt.Fprintf(stderr, "gc rig add: --prefix %q must not contain hyphens (conflicts with bead ID format)\n", prefixOverride) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Trim and drop empty --include entries so `--include=` or `--include " "`
	// doesn't persist a blank pack path that downstream resolution reads
	// as the city root.
	cleaned := includes[:0:0]
	for _, inc := range includes {
		if trimmed := strings.TrimSpace(inc); trimmed != "" {
			cleaned = append(cleaned, trimmed)
		}
	}
	includes = cleaned

	fi, err := fs.Stat(rigPath)
	if err != nil {
		if adopt {
			fmt.Fprintf(stderr, "gc rig add: --adopt requires an existing directory: %s\n", rigPath) //nolint:errcheck // best-effort stderr
			return 1
		}
		if err := fs.MkdirAll(rigPath, 0o755); err != nil {
			fmt.Fprintf(stderr, "gc rig add: creating %s: %v\n", rigPath, err) //nolint:errcheck // best-effort stderr
			return 1
		}
	} else if !fi.IsDir() {
		fmt.Fprintf(stderr, "gc rig add: %s is not a directory\n", rigPath) //nolint:errcheck // best-effort stderr
		return 1
	}

	if adopt {
		metaPath := filepath.Join(rigPath, ".beads", "metadata.json")
		if _, err := fs.Stat(metaPath); err != nil {
			fmt.Fprintf(stderr, "gc rig add: --adopt requires .beads/metadata.json in %s\n", rigPath) //nolint:errcheck // best-effort stderr
			return 1
		}
		if _, ok := readBeadsPrefix(fs, rigPath); !ok {
			fmt.Fprintf(stderr, "gc rig add: --adopt requires a valid issue_prefix in .beads/config.yaml in %s\n", rigPath) //nolint:errcheck // best-effort stderr
			return 1
		}
	}

	name := nameOverride
	if name == "" {
		name = filepath.Base(rigPath)
	}

	_, gitErr := fs.Stat(filepath.Join(rigPath, ".git"))
	hasGit := gitErr == nil

	tomlPath := filepath.Join(cityPath, "city.toml")
	cfg, err := loadCityConfigForEditFS(fs, tomlPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc rig add: loading config: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if cityUsesBdStoreContract(cityPath) && cityDoltConfigHasLifecycleFields(cfg.Dolt) {
		registerCityDoltConfig(cityPath, cfg.Dolt)
		defer clearCityDoltConfig(cityPath)
	}
	rootDefaultRigImports, err := config.LoadRootPackDefaultRigImports(fs, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc rig add: loading root pack defaults: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	defaultRigIncludes := append([]string{}, cfg.Workspace.DefaultRigIncludes...)

	var reAdd bool
	var reAddNeedsConfigWrite bool
	existingRigIdx := -1
	var existingRig *config.Rig
	for i, r := range cfg.Rigs {
		if r.Name != name {
			continue
		}
		existingRigIdx = i
		existingRig = &cfg.Rigs[i]
		existPath := r.Path
		if strings.TrimSpace(existPath) == "" {
			reAdd = true
			reAddNeedsConfigWrite = true
			break
		}
		if !filepath.IsAbs(existPath) {
			existPath = filepath.Join(cityPath, existPath)
		}
		if filepath.Clean(existPath) != filepath.Clean(rigPath) {
			fmt.Fprintf(stderr, "gc rig add: rig %q already registered at %s (not %s)\n", name, r.Path, rigPath) //nolint:errcheck // best-effort stderr
			return 1
		}
		reAdd = true
		break
	}

	var prefix string
	switch {
	case reAdd:
		prefix = existingRig.EffectivePrefix()
	case prefixOverride != "":
		prefix = strings.ToLower(prefixOverride)
	default:
		prefix = config.DeriveBeadsPrefix(name)
	}

	if existingPrefix, ok := readBeadsPrefix(fs, rigPath); ok && existingPrefix != prefix {
		switch {
		case reAdd:
			// On re-add, --prefix is ignored (we use the existing rig's
			// configured prefix). Direct the user to edit city.toml.
			fmt.Fprintf(stderr, "gc rig add: rig %q has bead prefix %q but city.toml has %q; "+ //nolint:errcheck // best-effort stderr
				"edit city.toml to set prefix = %q, or remove %s/.beads to reinitialize\n",
				name, existingPrefix, prefix, existingPrefix, rigPath)
		case adopt:
			// On --adopt, the user explicitly wants the existing store.
			// "Remove .beads to reinitialize" is the wrong recovery here:
			// nudge them toward matching the existing prefix instead.
			fmt.Fprintf(stderr, "gc rig add: --adopt: rig %q already has bead prefix %q (requested %q); "+ //nolint:errcheck // best-effort stderr
				"use --prefix %s (or omit --prefix) to match the existing store\n",
				name, existingPrefix, prefix, existingPrefix)
		default:
			fmt.Fprintf(stderr, "gc rig add: rig %q already has bead prefix %q (requested %q); "+ //nolint:errcheck // best-effort stderr
				"use --prefix %s to match, or remove %s/.beads to reinitialize\n",
				name, existingPrefix, prefix, existingPrefix, rigPath)
		}
		return 1
	}

	// Guard: on a fresh add (not a re-add) without --adopt, refuse to run
	// if .beads/ is already present. Without this, doRigAdd falls through
	// to bd init against an existing Dolt store and typically dies with
	// "bd init: signal: killed" after the probe times out — an unhelpful
	// failure mode for the common "register existing store" workflow.
	if !reAdd && !adopt {
		beadsPath := filepath.Join(rigPath, ".beads")
		fi, err := fs.Stat(beadsPath)
		if err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(stderr, "gc rig add: checking %s: %v\n", beadsPath, err) //nolint:errcheck // best-effort stderr
			return 1
		}
		if err == nil && fi.IsDir() {
			fmt.Fprintf(stderr, "gc rig add: %s/.beads already exists; "+ //nolint:errcheck // best-effort stderr
				"use --adopt to register the existing store, or remove %s/.beads to reinitialize\n",
				rigPath, rigPath)
			return 1
		}
	}

	// --- Phase 1: Infrastructure (all fallible, before touching city.toml) ---

	w := func(s string) { fmt.Fprintln(stdout, s) } //nolint:errcheck // best-effort stdout
	if reAdd {
		w(fmt.Sprintf("Re-initializing rig '%s'...", name))
		if startSuspended && startSuspended != existingRig.Suspended {
			fmt.Fprintf(stderr, "gc rig add: warning: --start-suspended ignored (existing: suspended=%v); edit city.toml to change\n", existingRig.Suspended) //nolint:errcheck // best-effort stderr
		}
		if len(includes) > 0 && !slices.Equal(existingRig.Includes, includes) {
			fmt.Fprintf(stderr, "gc rig add: warning: --include flags %v ignored (existing: %v); edit city.toml to change\n", includes, existingRig.Includes) //nolint:errcheck // best-effort stderr
		}
		if prefixOverride != "" && strings.ToLower(prefixOverride) != existingRig.EffectivePrefix() {
			fmt.Fprintf(stderr, "gc rig add: warning: --prefix=%s ignored (existing: %s); edit city.toml to change\n", prefixOverride, existingRig.EffectivePrefix()) //nolint:errcheck // best-effort stderr
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
		case len(includes) > 0:
			w(fmt.Sprintf("  Include: %s", strings.Join(includes, ", ")))
		default:
			if len(rootDefaultRigImports) > 0 {
				w(fmt.Sprintf("  Import: %s (default)", formatBoundImports(rootDefaultRigImports)))
			}
			if len(defaultRigIncludes) > 0 {
				w(fmt.Sprintf("  Include: %s (default)", strings.Join(defaultRigIncludes, ", ")))
			}
		}
	}

	if adopt {
		if err := prepareRigAdoptProviderState(cityPath, rigPath); err != nil {
			fmt.Fprintf(stderr, "gc rig add: prepare adopted rig store: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		w("  Adopted existing beads database")
	}

	deferred := false
	if !adopt {
		deferred, err = initDirIfReady(cityPath, rigPath, prefix)
		if err != nil {
			fmt.Fprintf(stderr, "gc rig add: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		if deferred {
			if cityUsesBdStoreContract(cityPath) && os.Getenv("GC_DOLT") == "skip" {
				w("  Beads init deferred to controller")
			} else if err := initAndHookDir(cityPath, rigPath, prefix); err != nil {
				w("  Beads init deferred to controller")
			} else {
				w("  Initialized beads database")
			}
		} else {
			w("  Initialized beads database")
		}
	}

	var nextCfg *config.City
	if reAdd {
		if reAddNeedsConfigWrite {
			next := *cfg
			next.Rigs = append([]config.Rig{}, cfg.Rigs...)
			next.Rigs[existingRigIdx].Path = rigPath
			nextCfg = &next
		} else {
			nextCfg = cfg
		}
	} else {
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
		case len(includes) > 0:
			rig.Includes = slices.Clone(includes)
		default:
			if len(rootDefaultRigImports) > 0 {
				rig.Imports = make(map[string]config.Import, len(rootDefaultRigImports))
				for _, bound := range rootDefaultRigImports {
					rig.Imports[bound.Binding] = bound.Import
				}
			}
			if len(defaultRigIncludes) > 0 {
				rig.Includes = slices.Clone(defaultRigIncludes)
			}
		}
		next := *cfg
		next.Rigs = append(append([]config.Rig{}, cfg.Rigs...), rig)
		if err := config.ValidateRigs(next.Rigs, config.EffectiveHQPrefix(&next)); err != nil {
			fmt.Fprintf(stderr, "gc rig add: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		nextCfg = &next
	}

	snapshots, err := snapshotRigAddTopologyFiles(fs, cityPath, nextCfg)
	if err != nil {
		fmt.Fprintf(stderr, "gc rig add: snapshot canonical files: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !reAdd || reAddNeedsConfigWrite {
		if err := normalizeCanonicalBdScopeFiles(cityPath, nextCfg); err != nil {
			writeRigAddRollbackError(fs, stderr, snapshots, "canonicalizing rig topology", err)
			return 1
		}

		if err := writeCityConfigForEditFS(fs, tomlPath, nextCfg); err != nil {
			writeRigAddRollbackError(fs, stderr, snapshots, "writing config", err)
			return 1
		}
	}
	cfg = nextCfg

	allRigs := collectRigRoutes(cityPath, cfg)
	if err := writeAllRigRoutes(allRigs); err != nil {
		writeRigAddRollbackError(fs, stderr, snapshots, "writing routes", err)
		return 1
	}
	w("  Generated routes.jsonl for cross-rig routing")

	if adopt {
		if err := installBeadHooks(rigPath); err != nil {
			fmt.Fprintf(stderr, "gc rig add: installing bead hooks: %v\n", err) //nolint:errcheck // best-effort stderr
		}
	}
	if err := ensureGitignoreEntries(fs, rigPath, rigGitignoreEntries); err != nil {
		fmt.Fprintf(stderr, "gc rig add: writing .gitignore: %v\n", err) //nolint:errcheck // best-effort stderr
	}
	if ih := cfg.Workspace.InstallAgentHooks; len(ih) > 0 {
		resolver := func(name string) string { return config.BuiltinFamily(name, cfg.Providers) }
		if err := hooks.InstallWithResolver(fs, cityPath, rigPath, ih, resolver); err != nil {
			fmt.Fprintf(stderr, "gc rig add: installing agent hooks: %v\n", err) //nolint:errcheck // best-effort stderr
		}
	}

	reloadedCfg, prov, _ := config.LoadWithIncludes(fsys.OSFS{}, tomlPath)
	emitLoadCityConfigWarnings(stderr, prov)
	if reloadedCfg != nil {
		layers, ok := reloadedCfg.FormulaLayers.Rigs[name]
		if !ok || len(layers) == 0 {
			layers = reloadedCfg.FormulaLayers.City
		}
		if len(layers) > 0 {
			if rfErr := ResolveFormulas(rigPath, layers); rfErr != nil {
				fmt.Fprintf(stderr, "gc rig add: resolving formulas: %v\n", rfErr) //nolint:errcheck // best-effort stderr
			}
		}
	}

	if err := writeBeadsEnvGTRoot(fs, rigPath, cityPath); err != nil {
		fmt.Fprintf(stderr, "gc rig add: warning: writing .beads/.env: %v\n", err) //nolint:errcheck // best-effort stderr
	}

	if err := rigReloadControllerConfig(cityPath); err == nil && deferred && cityUsesBdStoreContract(cityPath) {
		if waitErr := rigWaitForStoreAccessible(cityPath, rigPath, rigDeferredStoreInitWait); waitErr != nil {
			fmt.Fprintf(stderr, "gc rig add: warning: controller init still pending for rig %q: %v\n", name, waitErr) //nolint:errcheck // best-effort stderr
		}
	}

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

func formatBoundImports(imports []config.BoundImport) string {
	parts := make([]string, 0, len(imports))
	for _, bound := range imports {
		part := bound.Binding
		if source := strings.TrimSpace(bound.Import.Source); source != "" {
			part += "=" + source
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ", ")
}

func snapshotRigAddTopologyFiles(fs fsys.FS, cityPath string, cfg *config.City) ([]fileSnapshot, error) {
	snapshots := make([]fileSnapshot, 0, len(cfg.Rigs)*3+5)
	cityToml, err := snapshotOptionalFile(fs, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		return nil, err
	}
	snapshots = append(snapshots, cityToml)
	siteToml, err := snapshotOptionalFile(fs, config.SiteBindingPath(cityPath))
	if err != nil {
		return nil, err
	}
	snapshots = append(snapshots, siteToml)
	citySnapshots, err := snapshotRigCanonicalFiles(fs, cityPath)
	if err != nil {
		return nil, err
	}
	snapshots = append(snapshots, citySnapshots...)
	cityPort, err := snapshotOptionalFile(fs, filepath.Join(cityPath, ".beads", "dolt-server.port"))
	if err != nil {
		return nil, err
	}
	snapshots = append(snapshots, cityPort)
	seen := map[string]struct{}{}
	for _, rig := range cfg.Rigs {
		rigPath := rig.Path
		if !filepath.IsAbs(rigPath) {
			rigPath = filepath.Join(cityPath, rigPath)
		}
		rigPath = filepath.Clean(rigPath)
		if _, ok := seen[rigPath]; ok {
			continue
		}
		seen[rigPath] = struct{}{}
		rigSnapshots, err := snapshotRigCanonicalFiles(fs, rigPath)
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, rigSnapshots...)
		rigPort, err := snapshotOptionalFile(fs, filepath.Join(rigPath, ".beads", "dolt-server.port"))
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, rigPort)
	}
	return snapshots, nil
}

func writeRigAddRollbackError(fs fsys.FS, stderr io.Writer, snapshots []fileSnapshot, action string, cause error) {
	if restoreErr := restoreSnapshots(fs, snapshots); restoreErr != nil {
		fmt.Fprintf(stderr, "gc rig add: %s: %v (rollback failed: %v)\n", action, cause, restoreErr) //nolint:errcheck // best-effort stderr
		return
	}
	fmt.Fprintf(stderr, "gc rig add: %s: %v\n", action, cause) //nolint:errcheck // best-effort stderr
}

var writeAllRigRoutes = writeAllRoutes

func waitForRigStoreAccessible(cityPath, rigPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		store, err := openStoreAtForCity(rigPath, cityPath)
		if err == nil {
			pingErr := store.Ping()
			if pingErr == nil {
				return nil
			}
			lastErr = pingErr
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			if lastErr == nil {
				lastErr = fmt.Errorf("timed out waiting for rig store to become accessible")
			}
			return lastErr
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func prepareRigAdoptProviderState(cityPath, rigPath string) error {
	if rawBeadsProvider(cityPath) != "file" {
		return nil
	}
	if !fileStoreUsesScopedRoots(cityPath) {
		return nil
	}
	return ensurePersistedScopeLocalFileStore(rigPath)
}

// findEnclosingRig returns the rig whose path is a prefix of dir. It does
// prefix matching so that subdirectories of a rig are recognized.
func findEnclosingRig(dir string, rigs []config.Rig) (name, rigPath string, found bool) {
	cleanDir := normalizePathForCompare(dir)
	bestName, bestPath := "", ""
	for _, r := range rigs {
		if strings.TrimSpace(r.Path) == "" {
			continue
		}
		cleanRig := normalizePathForCompare(r.Path)
		if pathWithinScope(cleanDir, cleanRig) {
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
	Name string `json:"name"`
	// Path is the absolute filesystem path to the rig directory, resolved from
	// city.toml by resolveRigPaths. Always absolute in output, regardless of
	// the relative form stored in city.toml.
	Path      string `json:"path"`
	Prefix    string `json:"prefix"`
	HQ        bool   `json:"hq"`
	Suspended bool   `json:"suspended"`
	Beads     string `json:"beads"`
}

// doRigList is the pure logic for "gc rig list". It reads rigs from city.toml
// and prints each with its prefix and beads status. Accepts an injected FS for
// testability.
//
// Rig paths are resolved to absolute form via resolveRigPaths before output;
// both JSON and text output reflect the on-disk absolute path regardless of
// how the rig path is declared in city.toml. The cityPath parameter must be
// absolute.
func doRigList(fs fsys.FS, cityPath string, jsonOutput bool, stdout, stderr io.Writer) int {
	cfg, err := loadCityConfigFS(fs, filepath.Join(cityPath, "city.toml"), stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc rig list: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	resolveRigPaths(cityPath, cfg.Rigs)

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
	displayName := loadedCityName(cfg, cityPath)
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
		ValidArgsFunction: completeRigNames,
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

	if err := writeCityConfigForEditFS(fs, tomlPath, cfg); err != nil {
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
		ValidArgsFunction: completeRigNames,
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

	if err := writeCityConfigForEditFS(fs, tomlPath, cfg); err != nil {
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

Removes the rig entry from city.toml and removes its machine-local path
binding from .gc/site.toml.`,
		Example: `  gc rig remove myrig`,
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdRigRemove(args[0], stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
		ValidArgsFunction: completeRigNames,
	}
}

// cmdRigRemove removes a rig from the current city and its local site binding.
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
	found := false
	filtered := cfg.Rigs[:0]
	for _, r := range cfg.Rigs {
		if r.Name == rigName {
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
	if err := writeCityConfigForEditFS(fsys.OSFS{}, tomlPath, cfg); err != nil {
		fmt.Fprintf(stderr, "gc rig remove: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Regenerate routes.
	resolveRigPaths(cityPath, cfg.Rigs)
	allRigs := collectRigRoutes(cityPath, cfg)
	if err := writeAllRoutes(allRigs); err != nil {
		fmt.Fprintf(stderr, "gc rig remove: warning: writing routes: %v\n", err) //nolint:errcheck // best-effort stderr
	}

	_ = reloadControllerConfig(cityPath)
	fmt.Fprintf(stdout, "Removed rig '%s'\n", rigName) //nolint:errcheck // best-effort stdout
	return 0
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
	prefix, ok, err := contract.ReadIssuePrefix(fs, filepath.Join(rigPath, ".beads", "config.yaml"))
	if err != nil || !ok {
		return "", false
	}
	return strings.ToLower(prefix), true
}
