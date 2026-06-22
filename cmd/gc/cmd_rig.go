package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/hooks"
	"github.com/gastownhall/gascity/internal/packman"
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
	var defaultBranchFlag string
	var adoptFlag bool
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "add <path>",
		Short: "Register a project as a rig",
		Long: `Register an external project directory as a rig.

Initializes beads database, installs agent hooks if configured,
generates cross-rig routes, and appends the rig to city.toml.
If the target directory doesn't exist, it is created. Use --include
to apply a pack source that defines the rig's agent configuration;
repeat the flag to compose multiple packs for one rig. The flag is
compatibility sugar: gc rig add writes canonical rig imports.

Use --name to set the rig name explicitly (default: directory basename).
Use --prefix to set the bead ID prefix explicitly (default: derived from name).
Use --default-branch to set the rig's mainline branch explicitly. By default,
gc rig add probes the repo's origin/HEAD (and falls back to the currently
checked-out branch) and stores the result in city.toml so polecats and the
refinery target the right branch without manual metadata patching.
Use --start-suspended to add the rig in a suspended state (dormant-by-default).
The rig's agents won't spawn until explicitly resumed with "gc rig resume".

Use --adopt to register a directory that already has a fully initialized
.beads/ directory (must include both metadata.json and config.yaml).
For managed-Dolt rigs, runs an idempotent config sync (registers types.custom
and other config into the DB, never destructively reinitializes). The git repo
check remains informational.`,
		Example: `  gc rig add /path/to/project
  gc rig add /path/to/project --name myrig
  gc rig add /path/to/project --prefix r1
  gc rig add /path/to/master-repo --default-branch master
  gc rig add ./my-project --include gastown
  gc rig add ./my-project --include packs/planner --include packs/architect
  gc rig add ./my-project --include gastown --start-suspended
  gc rig add /path/to/existing --adopt`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if jsonOutput {
				cityPath, err := resolveCity()
				if err != nil {
					fmt.Fprintf(stderr, "gc rig add: %v\n", err) //nolint:errcheck // best-effort stderr
					return errExit
				}
				if len(args) < 1 {
					fmt.Fprintln(stderr, "gc rig add: missing path") //nolint:errcheck // best-effort stderr
					return errExit
				}
				rigPath, err := resolveRigAddPath(cityPath, args[0])
				if err != nil {
					fmt.Fprintf(stderr, "gc rig add: %v\n", err) //nolint:errcheck // best-effort stderr
					return errExit
				}
				rig, code := doRigAddWithResult(fsys.OSFS{}, cityPath, rigPath, includes, nameFlag, prefixFlag, defaultBranchFlag, startSuspended, adoptFlag, io.Discard, stderr)
				if code != 0 {
					return errExit
				}
				// Same immediate materialization as the human path (see
				// cmdRigAdd) so `gc rig add --json` — the programmatic entry
				// point most likely to drive out-of-tree codex rigs — also
				// delivers skill sinks now, not on the next supervisor tick.
				// Logs go to stderr so the JSONL on stdout stays clean.
				materializeSkillsForCity(cityPath, stderr)
				return writeManagementActionJSON(stdout, rigAddJSONSummary(rigPath, rig))
			}
			if cmdRigAdd(args, includes, nameFlag, prefixFlag, defaultBranchFlag, startSuspended, adoptFlag, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&includes, "include", nil, "pack source for rig agents (repeatable; writes canonical rig imports)")
	cmd.Flags().StringVar(&nameFlag, "name", "", "rig name (default: directory basename)")
	cmd.Flags().StringVar(&prefixFlag, "prefix", "", "bead ID prefix (default: derived from name)")
	cmd.Flags().StringVar(&defaultBranchFlag, "default-branch", "", "mainline branch (default: auto-detect from origin/HEAD or current branch)")
	cmd.Flags().BoolVar(&startSuspended, "start-suspended", false, "add rig in suspended state (dormant-by-default)")
	cmd.Flags().BoolVar(&adoptFlag, "adopt", false, "adopt existing .beads/ directory (skip init)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output in JSONL format")
	return cmd
}

func newRigListCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonFlag bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List registered rigs",
		Long: `List all registered rigs with their paths, prefixes, default branches, and beads status.

Shows the HQ rig (the city itself) and all configured rigs. Each rig
displays its bead ID prefix, recorded default branch when set, and whether
its beads database is initialized.`,
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
func cmdRigAdd(args []string, includes []string, nameOverride, prefixOverride, defaultBranchOverride string, startSuspended, adopt bool, stdout, stderr io.Writer) int {
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
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, includes, nameOverride, prefixOverride, defaultBranchOverride, startSuspended, adopt, stdout, stderr)
	if code == 0 {
		// Materialize skills now so the new rig's provider skill sinks
		// (codex's .agents/skills, claude's .claude/skills, …) exist as soon
		// as `gc rig add` returns — without waiting for the next supervisor
		// tick, even for an out-of-tree rig, and even when no supervisor is
		// running. Best-effort; never undoes a successful add.
		materializeSkillsForCity(cityPath, stderr)
	}
	return code
}

// materializeSkillsForCity runs one best-effort stage-1 skill materialization
// pass for the whole city, using the same compose+materialize path the
// supervisor runs each tick. It exists so `gc rig add` delivers a rig's skill
// sinks immediately rather than leaving a window until the next reconcile —
// the difference that makes skills available in a freshly added rig's
// directory, including an out-of-tree rig.
//
// Idempotent: a converged city creates nothing new. Non-fatal by design: a
// config-load error here must not fail an already-persisted rig add, and
// runStage1SkillMaterialization logs per-agent issues inline and returns nil.
func materializeSkillsForCity(cityPath string, stderr io.Writer) {
	cfg, _, err := loadCityConfigWithBuiltinPacks(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc rig add: skipping immediate skill materialization (rig added; the supervisor will materialize on its next tick): %v\n", err) //nolint:errcheck // best-effort stderr
		return
	}
	// Honor the same collision gate gc start and the supervisor run before
	// materializing: a (scope-root, vendor) collision would otherwise let this
	// pass write conflicting symlinks. On collision, skip — the supervisor
	// surfaces the error on its next tick — rather than materialize a
	// half-resolved sink.
	if cerr := checkSkillCollisions(cfg, cityPath); cerr != nil {
		fmt.Fprintf(stderr, "gc rig add: skipping immediate skill materialization due to a skill collision (the supervisor reports this on its next tick):\n%v\n", cerr) //nolint:errcheck // best-effort stderr
		return
	}
	_ = runStage1SkillMaterialization(cityPath, cfg, stderr)
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
func doRigAdd(fs fsys.FS, cityPath, rigPath string, includes []string, nameOverride, prefixOverride, defaultBranchOverride string, startSuspended, adopt bool, stdout, stderr io.Writer) int {
	_, code := doRigAddWithResult(fs, cityPath, rigPath, includes, nameOverride, prefixOverride, defaultBranchOverride, startSuspended, adopt, stdout, stderr)
	return code
}

func doRigAddWithResult(fs fsys.FS, cityPath, rigPath string, includes []string, nameOverride, prefixOverride, defaultBranchOverride string, startSuspended, adopt bool, stdout, stderr io.Writer) (config.Rig, int) {
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

	rigPathExists := false
	if fi, err := fs.Stat(rigPath); err != nil {
		if adopt {
			fmt.Fprintf(stderr, "gc rig add: --adopt requires an existing directory: %s\n", rigPath) //nolint:errcheck // best-effort stderr
			return config.Rig{}, 1
		}
		if !os.IsNotExist(err) {
			fmt.Fprintf(stderr, "gc rig add: checking %s: %v\n", rigPath, err) //nolint:errcheck // best-effort stderr
			return config.Rig{}, 1
		}
	} else if !fi.IsDir() {
		fmt.Fprintf(stderr, "gc rig add: %s is not a directory\n", rigPath) //nolint:errcheck // best-effort stderr
		return config.Rig{}, 1
	} else {
		rigPathExists = true
	}

	name := nameOverride
	if name == "" {
		name = filepath.Base(rigPath)
	}

	_, gitErr := fs.Stat(filepath.Join(rigPath, ".git"))
	hasGit := gitErr == nil
	defaultBranchOverride = strings.TrimSpace(defaultBranchOverride)
	resolvedDefaultBranch := defaultBranchOverride
	if resolvedDefaultBranch == "" && hasGit {
		resolvedDefaultBranch = git.New(rigPath).ProbeDefaultBranch()
	}

	tomlPath := filepath.Join(cityPath, "city.toml")
	cfg, err := loadCityConfigForEditFS(fs, tomlPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc rig add: loading config: %v\n", err) //nolint:errcheck // best-effort stderr
		return config.Rig{}, 1
	}

	// Canonicalize --include tokens that name a materialized builtin pack so the
	// flag honors its --help promise of "canonical rig imports". Done after the
	// config load (so [packs] references are honored) but before the imports are
	// built and the re-add comparison below, so both the written city.toml and
	// that comparison use the resolvable path (gascity#3137).
	includes = canonicalizeBuiltinPackIncludes(fs, cityPath, includes, cfg.Packs)

	explicitRigImports, commitRigImports, err := ensureBundledRigImportsInstalled(cityPath, boundImportsFromLegacySources(includes, cfg.Packs))
	if err != nil {
		fmt.Fprintf(stderr, "gc rig add: installing bundled rig imports: %v\n", err) //nolint:errcheck // best-effort stderr
		return config.Rig{}, 1
	}
	if cityUsesBdStoreContract(cityPath) && cityDoltConfigHasLifecycleFields(cfg.Dolt) {
		registerCityDoltConfig(cityPath, cfg.Dolt)
		defer clearCityDoltConfig(cityPath)
	}
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
			return config.Rig{}, 1
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

	if !reAdd {
		prefixKey := strings.ToLower(prefix)
		if prefixKey == strings.ToLower(config.EffectiveHQPrefix(cfg)) {
			fmt.Fprintf(stderr, "gc rig add: rig %q: prefix %q collides with HQ. Use --prefix to specify a different prefix.\n", name, prefixKey) //nolint:errcheck // best-effort stderr
			return config.Rig{}, 1
		}
		for _, rig := range cfg.Rigs {
			if prefixKey == strings.ToLower(rig.EffectivePrefix()) {
				fmt.Fprintf(stderr, "gc rig add: rig %q: prefix %q collides with %s. Use --prefix to specify a different prefix.\n", name, prefixKey, rig.Name) //nolint:errcheck // best-effort stderr
				return config.Rig{}, 1
			}
		}
	}
	if reAdd && existingRig != nil && existingRig.EffectiveDefaultBranch() == "" && resolvedDefaultBranch != "" {
		reAddNeedsConfigWrite = true
	}

	nextCfg := cfg
	var defaultRigImports []config.BoundImport
	needsValidation := !reAdd || reAddNeedsConfigWrite
	if reAddNeedsConfigWrite {
		next := *cfg
		next.Rigs = append([]config.Rig{}, cfg.Rigs...)
		if strings.TrimSpace(next.Rigs[existingRigIdx].Path) == "" {
			next.Rigs[existingRigIdx].Path = rigPath
		}
		if next.Rigs[existingRigIdx].EffectiveDefaultBranch() == "" && resolvedDefaultBranch != "" {
			next.Rigs[existingRigIdx].DefaultBranch = resolvedDefaultBranch
		}
		nextCfg = &next
	} else if !reAdd {
		storedPrefix := ""
		if prefixOverride != "" {
			storedPrefix = strings.ToLower(prefixOverride)
		}
		rig := config.Rig{
			Name:             name,
			Path:             rigPath,
			Prefix:           storedPrefix,
			DefaultBranch:    resolvedDefaultBranch,
			SuspendedOnStart: startSuspended,
		}
		switch {
		case len(explicitRigImports) > 0:
			rig.Imports = boundImportsMap(explicitRigImports)
		default:
			rootDefaultRigImports, err := config.LoadRootPackDefaultRigImports(fs, cityPath)
			if err != nil {
				fmt.Fprintf(stderr, "gc rig add: loading root pack defaults: %v\n", err) //nolint:errcheck // best-effort stderr
				return config.Rig{}, 1
			}
			// Default-rig imports take the same pin/cache hardening as
			// explicit --include imports: a version-less bundled source
			// arriving from root-pack defaults or legacy
			// default_rig_includes must not persist version-less.
			defaultRigImports, commitRigImports, err = ensureBundledRigImportsInstalled(cityPath, composeDefaultRigImports(rootDefaultRigImports, cfg.Workspace.LegacyDefaultRigIncludes(), cfg.Packs))
			if err != nil {
				fmt.Fprintf(stderr, "gc rig add: installing bundled rig imports: %v\n", err) //nolint:errcheck // best-effort stderr
				return config.Rig{}, 1
			}
			if len(defaultRigImports) > 0 {
				rig.Imports = boundImportsMap(defaultRigImports)
			}
		}
		next := *cfg
		next.Rigs = append(append([]config.Rig{}, cfg.Rigs...), rig)
		nextCfg = &next
	}
	if needsValidation {
		if err := config.ValidateRigs(nextCfg.Rigs, config.EffectiveHQPrefix(nextCfg)); err != nil {
			fmt.Fprintf(stderr, "gc rig add: %v\n", err) //nolint:errcheck // best-effort stderr
			return config.Rig{}, 1
		}
	}

	if !rigPathExists {
		if err := fs.MkdirAll(rigPath, 0o755); err != nil {
			fmt.Fprintf(stderr, "gc rig add: creating %s: %v\n", rigPath, err) //nolint:errcheck // best-effort stderr
			return config.Rig{}, 1
		}
	}

	if adopt {
		metaPath := filepath.Join(rigPath, ".beads", "metadata.json")
		if _, err := fs.Stat(metaPath); err != nil {
			fmt.Fprintf(stderr, "gc rig add: --adopt requires .beads/metadata.json in %s\n", rigPath) //nolint:errcheck // best-effort stderr
			return config.Rig{}, 1
		}
		if _, ok := readBeadsPrefix(fs, rigPath); !ok {
			fmt.Fprintf(stderr, "gc rig add: --adopt requires a valid issue_prefix in .beads/config.yaml in %s\n", rigPath) //nolint:errcheck // best-effort stderr
			return config.Rig{}, 1
		}
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
		return config.Rig{}, 1
	}

	// Guard: on a fresh add (not a re-add) without --adopt, refuse to run
	// if .beads/ already holds a beads store. Without this, doRigAdd falls
	// through to bd init against an existing Dolt store and typically dies
	// with "bd init: signal: killed" after the probe times out.
	//
	// We treat .beads/ as a store only when metadata.json or config.yaml is
	// present. A directory that happens to be named .beads/ but contains
	// only unrelated content (e.g. the beads project's own .beads/formulas/
	// convention for formula source files) is not a store, so the init path
	// decides how to create the missing store files in place.
	if !reAdd && !adopt {
		beadsPath := filepath.Join(rigPath, ".beads")
		fi, err := fs.Stat(beadsPath)
		if err != nil && !os.IsNotExist(err) {
			fmt.Fprintf(stderr, "gc rig add: checking %s: %v\n", beadsPath, err) //nolint:errcheck // best-effort stderr
			return config.Rig{}, 1
		}
		if err == nil && fi.IsDir() {
			containsStore, containsErr := beadsDirContainsStore(fs, beadsPath)
			if containsErr != nil {
				fmt.Fprintf(stderr, "gc rig add: %v\n", containsErr) //nolint:errcheck // best-effort stderr
				return config.Rig{}, 1
			}
			if containsStore {
				fmt.Fprintf(stderr, "gc rig add: %s/.beads already contains a beads store; "+ //nolint:errcheck // best-effort stderr
					"use --adopt to register it, or remove %s/.beads to reinitialize\n",
					rigPath, rigPath)
				return config.Rig{}, 1
			}
		}
	}

	// --- Phase 1: Infrastructure (all fallible, before touching city.toml) ---

	w := func(s string) { fmt.Fprintln(stdout, s) } //nolint:errcheck // best-effort stdout
	if reAdd {
		w(fmt.Sprintf("Re-initializing rig '%s'...", name))
		if startSuspended && startSuspended != existingRig.EffectiveSuspendedOnStart() {
			fmt.Fprintf(stderr, "gc rig add: warning: --start-suspended ignored (existing: suspended_on_start=%v); edit city.toml to change\n", existingRig.EffectiveSuspendedOnStart()) //nolint:errcheck // best-effort stderr
		}
		if len(explicitRigImports) > 0 {
			existingRigImports, err := effectiveRigBoundImports(existingRig, cfg.Packs)
			if err != nil {
				fmt.Fprintf(stderr, "gc rig add: warning: --include flags %v ignored; existing rig imports could not be normalized (%v). Edit city.toml to change\n", includes, err) //nolint:errcheck // best-effort stderr
			} else if !slices.Equal(existingRigImports, explicitRigImports) {
				fmt.Fprintf(stderr, "gc rig add: warning: --include flags %v ignored (existing imports: %s); edit city.toml to change\n", includes, formatBoundImports(existingRigImports)) //nolint:errcheck // best-effort stderr
			}
		}
		if prefixOverride != "" && strings.ToLower(prefixOverride) != existingRig.EffectivePrefix() {
			fmt.Fprintf(stderr, "gc rig add: warning: --prefix=%s ignored (existing: %s); edit city.toml to change\n", prefixOverride, existingRig.EffectivePrefix()) //nolint:errcheck // best-effort stderr
		}
		if defaultBranchOverride != "" &&
			defaultBranchOverride != existingRig.EffectiveDefaultBranch() &&
			(existingRig.EffectiveDefaultBranch() != "" || resolvedDefaultBranch != defaultBranchOverride) {
			fmt.Fprintf(stderr, "gc rig add: warning: --default-branch=%s ignored (existing: %s); edit city.toml to change\n", defaultBranchOverride, existingRig.EffectiveDefaultBranch()) //nolint:errcheck // best-effort stderr
		}
	} else {
		w(fmt.Sprintf("Adding rig '%s'...", name))
	}
	if hasGit {
		w(fmt.Sprintf("  Detected git repo at %s", rigPath))
	}
	w(fmt.Sprintf("  Prefix: %s", prefix))
	if !reAdd && resolvedDefaultBranch != "" {
		w(fmt.Sprintf("  Default branch: %s", resolvedDefaultBranch))
	}
	if !reAdd {
		switch {
		case len(explicitRigImports) > 0:
			w(fmt.Sprintf("  Import: %s", formatBoundImports(explicitRigImports)))
		default:
			if len(defaultRigImports) > 0 {
				w(fmt.Sprintf("  Import: %s (default)", formatBoundImports(defaultRigImports)))
			}
		}
	}

	deferred := false
	if adopt {
		if err := prepareRigAdoptProviderState(cityPath, rigPath); err != nil {
			fmt.Fprintf(stderr, "gc rig add: prepare adopted rig store: %v\n", err) //nolint:errcheck // best-effort stderr
			return config.Rig{}, 1
		}
		if cityUsesBdStoreContract(cityPath) {
			deferred, err = initDirIfReady(cityPath, rigPath, prefix)
			if err != nil {
				fmt.Fprintf(stderr, "gc rig add: %v\n", err) //nolint:errcheck // best-effort stderr
				return config.Rig{}, 1
			}
		}
		w("  Adopted existing beads database")
	} else {
		deferred, err = initDirIfReady(cityPath, rigPath, prefix)
		if err != nil {
			fmt.Fprintf(stderr, "gc rig add: %v\n", err) //nolint:errcheck // best-effort stderr
			return config.Rig{}, 1
		}
		if deferred {
			if cityUsesBdStoreContract(cityPath) && gcDoltSkip() {
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

	snapshots, err := snapshotRigAddTopologyFiles(fs, cityPath, nextCfg)
	if err != nil {
		fmt.Fprintf(stderr, "gc rig add: snapshot canonical files: %v\n", err) //nolint:errcheck // best-effort stderr
		return config.Rig{}, 1
	}
	if !reAdd || reAddNeedsConfigWrite {
		if err := normalizeCanonicalBdScopeFiles(cityPath, nextCfg, io.Discard); err != nil {
			writeRigAddRollbackError(fs, stderr, snapshots, "canonicalizing rig topology", err)
			return config.Rig{}, 1
		}

		var writeErr error
		if !reAdd {
			// Surgical append: preserve existing comments by appending only the
			// new [[rigs]] block instead of re-serializing the whole file.
			newRig := nextCfg.Rigs[len(nextCfg.Rigs)-1]
			writeErr = config.AppendRigAndWriteSiteBindingsForEdit(fs, tomlPath, nextCfg, newRig)
		} else {
			writeErr = writeCityConfigForEditFS(fs, tomlPath, nextCfg)
		}
		if writeErr != nil {
			writeRigAddRollbackError(fs, stderr, snapshots, "writing config", writeErr)
			return config.Rig{}, 1
		}
	}

	// Persist packs.lock and materialize bundled rig imports only after the
	// city config write succeeds, so the lockfile honors the same
	// "city.toml written last" contract: any earlier failure leaves
	// packs.lock untouched, and a failure here rolls back through the
	// snapshot (which now covers packs.lock).
	if commitRigImports != nil {
		if err := commitRigImports(); err != nil {
			writeRigAddRollbackError(fs, stderr, snapshots, "installing bundled rig imports", err)
			return config.Rig{}, 1
		}
	}
	cfg = nextCfg

	allRigs := collectRigRoutes(cityPath, cfg)
	if err := writeAllRigRoutes(allRigs); err != nil {
		writeRigAddRollbackError(fs, stderr, snapshots, "writing routes", err)
		return config.Rig{}, 1
	}
	w("  Generated routes.jsonl for cross-rig routing")

	if adopt {
		if err := installBeadHooks(rigPath, cityPath); err != nil {
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
	for _, rig := range cfg.Rigs {
		if rig.Name == name {
			return rig, 0
		}
	}
	return config.Rig{
		Name:          name,
		Path:          rigPath,
		Prefix:        strings.ToLower(prefixOverride),
		DefaultBranch: resolvedDefaultBranch,
		Suspended:     startSuspended,
	}, 0
}

func formatBoundImports(imports []config.BoundImport) string {
	parts := make([]string, 0, len(imports))
	for _, bound := range sortedBoundImports(imports) {
		part := bound.Binding
		if source := strings.TrimSpace(bound.Import.Source); source != "" {
			part += "=" + source
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ", ")
}

// canonicalizeBuiltinPackIncludes rewrites --include tokens that name a
// bundled pack to its canonical remote source. Builtin packs compose from
// the user-global repo cache and are not registered in [packs], so a bare
// "<name>" or "packs/<name>" token (the form documented in `gc rig add
// --help`) would otherwise be persisted as the non-resolvable literal
// "./<token>", breaking pack expansion citywide (gascity#3137). A token
// whose raw form or derived single-segment name is a key in packs, or
// that resolves to a real local pack directory in the city, is left
// unchanged so explicit references keep their configured/local source
// rather than being shadowed by the builtin.
func canonicalizeBuiltinPackIncludes(fs fsys.FS, cityPath string, includes []string, packs map[string]config.PackSource) []string {
	out := make([]string, len(includes))
	for i, inc := range includes {
		out[i] = inc
		tok := strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(inc)), "./")
		name := tok
		if rest, ok := strings.CutPrefix(tok, "packs/"); ok {
			name = rest
		}
		// Only accept a single-segment pack name; arbitrary nested paths are
		// treated as real local imports, not builtin-pack references.
		if name == "" || strings.Contains(name, "/") {
			continue
		}
		// Don't shadow an explicitly configured [packs] reference: a token
		// that names a registered pack keeps its configured source.
		if _, ok := packs[tok]; ok {
			continue
		}
		if _, ok := packs[name]; ok {
			continue
		}
		// A token that resolves to a real local pack in the city is a local
		// import, not a builtin-pack reference.
		if !filepath.IsAbs(tok) {
			if _, err := fs.Stat(filepath.Join(cityPath, filepath.FromSlash(tok), "pack.toml")); err == nil {
				continue
			}
		}
		if source, ok := builtinpacks.CanonicalImportSource(name); ok {
			out[i] = source
		}
	}
	return out
}

// ensureBundledRigImportsInstalled pins any bundled-source rig imports so
// the new rig composes offline without a manual "gc import install". It
// returns a copy of imports with version-less bundled entries pinned at the
// canonical bundled version, plus a commit function that persists packs.lock
// and materializes the imports into the cache. The commit is deferred — and
// is nil when there are no bundled imports to persist — so the packs.lock
// write obeys the same "city.toml written last" atomicity contract as the
// rest of rig add: the lockfile is mutated only after the city config write
// succeeds, and the rig-add rollback snapshot covers it. Resolution (which
// only reads packs.lock and hydrates the shared repo cache) still happens
// eagerly here so any resolution error is surfaced before mutation begins.
//
// The input slice is not modified, and callers must persist the returned
// slice so the city.toml rig import carries the same pin the lockfile
// records: a version-less import resolves as "latest" if packs.lock is
// regenerated or lost, and "gc import upgrade" treats it as unconstrained —
// either path silently replaces the builtin the user asked for.
func ensureBundledRigImportsInstalled(cityPath string, imports []config.BoundImport) ([]config.BoundImport, func() error, error) {
	pinned := append([]config.BoundImport(nil), imports...)
	declared := make(map[string]config.Import)
	for i := range pinned {
		if !builtinpacks.IsSource(pinned[i].Import.Source) {
			continue
		}
		if strings.TrimSpace(pinned[i].Import.Version) == "" {
			pinned[i].Import.Version = bundledSourcePinnedVersion(pinned[i].Import.Source)
		}
		declared[pinned[i].Binding] = pinned[i].Import
	}
	if len(declared) == 0 {
		return pinned, nil, nil
	}
	existing, err := collectAllImportsFS(fsys.OSFS{}, cityPath)
	if err != nil {
		return nil, nil, err
	}
	for name, imp := range declared {
		existing[name] = imp
	}
	lock, err := syncImports(cityPath, existing, packman.InstallResolveIfNeeded)
	if err != nil {
		return nil, nil, err
	}
	commit := func() error {
		if err := writeImportLockfile(fsys.OSFS{}, cityPath, lock); err != nil {
			return err
		}
		if _, err := installLockedImports(cityPath); err != nil {
			return err
		}
		return nil
	}
	return pinned, commit, nil
}

func boundImportsFromLegacySources(sources []string, packs map[string]config.PackSource) []config.BoundImport {
	return config.BoundImportsFromLegacySources(sources, packs)
}

func boundImportsFromImportMap(imports map[string]config.Import) []config.BoundImport {
	if len(imports) == 0 {
		return nil
	}
	bindings := make([]string, 0, len(imports))
	for binding := range imports {
		bindings = append(bindings, binding)
	}
	slices.Sort(bindings)
	bound := make([]config.BoundImport, 0, len(bindings))
	for _, binding := range bindings {
		bound = append(bound, config.BoundImport{
			Binding: binding,
			Import:  imports[binding],
		})
	}
	return bound
}

func effectiveRigBoundImports(rig *config.Rig, packs map[string]config.PackSource) ([]config.BoundImport, error) {
	if rig == nil {
		return nil, nil
	}
	legacy := boundImportsFromLegacySources(rig.Includes, packs)
	return mergeBoundImports(boundImportsFromImportMap(rig.Imports), legacy)
}

func composeDefaultRigImports(root []config.BoundImport, legacyIncludes []string, packs map[string]config.PackSource) []config.BoundImport {
	if len(root) == 0 {
		return boundImportsFromLegacySources(legacyIncludes, packs)
	}
	target := make(map[string]config.Import, len(root)+len(legacyIncludes))
	order := make([]string, 0, len(root)+len(legacyIncludes))
	for _, bound := range root {
		if _, exists := target[bound.Binding]; !exists {
			order = append(order, bound.Binding)
		}
		target[bound.Binding] = bound.Import
	}
	order, _ = config.AddOrderedLegacyImports(target, order, legacyIncludes, packs)
	out := make([]config.BoundImport, 0, len(order))
	for _, binding := range order {
		imp, ok := target[binding]
		if !ok {
			continue
		}
		out = append(out, config.BoundImport{Binding: binding, Import: imp})
	}
	return out
}

func sortedBoundImports(imports []config.BoundImport) []config.BoundImport {
	if len(imports) == 0 {
		return nil
	}
	sorted := append([]config.BoundImport(nil), imports...)
	slices.SortFunc(sorted, func(a, b config.BoundImport) int {
		if a.Binding != b.Binding {
			return strings.Compare(a.Binding, b.Binding)
		}
		return strings.Compare(a.Import.Source, b.Import.Source)
	})
	return sorted
}

// mergeBoundImports is for already-bound import sets. Legacy default-rig
// includes use composeDefaultRigImports so binding collisions can be
// uniquified with the migration policy.
func mergeBoundImports(primary, secondary []config.BoundImport) ([]config.BoundImport, error) {
	if len(primary) == 0 && len(secondary) == 0 {
		return nil, nil
	}
	merged := make([]config.BoundImport, 0, len(primary)+len(secondary))
	seenByBinding := make(map[string]config.Import, len(primary)+len(secondary))
	appendImport := func(bound config.BoundImport) error {
		if prior, exists := seenByBinding[bound.Binding]; exists {
			if prior.Source == bound.Import.Source {
				return nil
			}
			return fmt.Errorf("binding %q maps to both %q and %q", bound.Binding, prior.Source, bound.Import.Source)
		}
		seenByBinding[bound.Binding] = bound.Import
		merged = append(merged, bound)
		return nil
	}
	for _, bound := range primary {
		if err := appendImport(bound); err != nil {
			return nil, err
		}
	}
	for _, bound := range secondary {
		if err := appendImport(bound); err != nil {
			return nil, err
		}
	}
	return sortedBoundImports(merged), nil
}

func boundImportsMap(imports []config.BoundImport) map[string]config.Import {
	if len(imports) == 0 {
		return nil
	}
	out := make(map[string]config.Import, len(imports))
	for _, bound := range imports {
		out[bound.Binding] = bound.Import
	}
	return out
}

func snapshotRigAddTopologyFiles(fs fsys.FS, cityPath string, cfg *config.City) ([]fileSnapshot, error) {
	snapshots := make([]fileSnapshot, 0, len(cfg.Rigs)*3+6)
	cityToml, err := snapshotResolvedFile(fs, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		return nil, err
	}
	snapshots = append(snapshots, cityToml)
	// packs.lock is written by the deferred bundled-rig-import commit after
	// the city config write, so it must be covered by the rollback snapshot
	// to keep rig add atomic across the lockfile.
	packsLock, err := snapshotOptionalFile(fs, filepath.Join(cityPath, "packs.lock"))
	if err != nil {
		return nil, err
	}
	snapshots = append(snapshots, packsLock)
	siteToml, err := snapshotResolvedFile(fs, config.SiteBindingPath(cityPath))
	if err != nil {
		return nil, err
	}
	snapshots = append(snapshots, siteToml)
	citySnapshots, err := snapshotRigCanonicalFiles(fs, cityPath)
	if err != nil {
		return nil, err
	}
	snapshots = append(snapshots, citySnapshots...)
	cityPort, err := snapshotResolvedFile(fs, filepath.Join(cityPath, ".beads", "dolt-server.port"))
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
		rigPort, err := snapshotResolvedFile(fs, filepath.Join(rigPath, ".beads", "dolt-server.port"))
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
		if jsonOutput {
			return writeJSONError(stdout, stderr, "city_resolve_failed", fmt.Sprintf("gc rig list: %v", err), 1)
		}
		fmt.Fprintf(stderr, "gc rig list: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	c, reason := rigListAPIClient(cityPath)
	return routeRigList(cityPath, c, reason, jsonOutput, stdout, stderr)
}

// rigListAPIClient returns (client, "") when the API path is available, or
// (nil, reason) when the caller should fall back. Indirected through a var
// so tests inject a client pointed at httptest.Server or force a specific
// fallback reason without spinning up a real controller.
var rigListAPIClient = func(cityPath string) (*api.Client, string) {
	if c := apiClient(cityPath); c != nil {
		return c, ""
	}
	return nil, apiClientFallbackReason(cityPath)
}

// routeRigList dispatches the `rig list` read to the supervisor API when
// available, falling back to doRigList when the controller is down, the
// escape hatch is set, or the API returns a fallbackable error. Emits
// exactly one route=... log line per exit path (gated on GC_DEBUG).
func routeRigList(cityPath string, c *api.Client, nilReason string, jsonOutput bool, stdout, stderr io.Writer) int {
	const cmdName = "rig list"
	if c != nil {
		cr, err := c.ListRigs()
		if err == nil {
			logRoute(stderr, cmdName, "api", "")
			return renderRigListFromAPI(fsys.OSFS{}, cityPath, cr, jsonOutput, stdout, stderr)
		}
		if !api.ShouldFallbackForRead(err) {
			logRoute(stderr, cmdName, "api", "error")
			fmt.Fprintf(stderr, "gc rig list: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		logRoute(stderr, cmdName, "fallback", api.FallbackReason(err))
	} else {
		logRoute(stderr, cmdName, "fallback", nilReason)
	}
	return doRigList(fsys.OSFS{}, cityPath, jsonOutput, stdout, stderr)
}

// renderRigListFromAPI formats the API-sourced rig list to match doRigList
// output. HQ info and per-rig beads status are derived locally (neither
// lives on the API response); configured rigs come from the API with an
// _cache_age_s envelope field (JSON) or staleness banner (human).
func renderRigListFromAPI(fs fsys.FS, cityPath string, cr api.CachedRead[[]api.RigView], jsonOutput bool, stdout, stderr io.Writer) int {
	cfg, err := loadCityConfigFS(fs, filepath.Join(cityPath, "city.toml"), stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc rig list: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	hqPrefix := config.EffectiveHQPrefix(cfg)
	cityName := cfg.EffectiveCityName()
	resolveRigPaths(cityPath, cfg.Rigs)
	rigsByName := make(map[string]config.Rig, len(cfg.Rigs))
	for i := range cfg.Rigs {
		rigsByName[cfg.Rigs[i].Name] = cfg.Rigs[i]
	}

	if jsonOutput {
		cacheAgeS := cr.AgeSeconds
		result := RigListJSON{
			SchemaVersion: "1",
			CityPath:      cityPath,
			CityName:      cityName,
			CacheAgeS:     &cacheAgeS,
			Rigs: []RigListItem{{
				Name:    cityName,
				Path:    cityPath,
				Prefix:  hqPrefix,
				HQ:      true,
				Running: true,
				Beads:   rigBeadsStatus(fs, cityPath),
			}},
		}
		for _, rig := range cr.Body {
			path := rig.Path
			prefix := rig.Prefix
			defaultBranch := rig.DefaultBranch
			defaultSlingTarget := ""
			if cfgRig, ok := rigsByName[rig.Name]; ok {
				path = cfgRig.Path
				prefix = cfgRig.EffectivePrefix()
				defaultBranch = cfgRig.EffectiveDefaultBranch()
				defaultSlingTarget = cfgRig.DefaultSlingTarget
			}
			result.Rigs = append(result.Rigs, RigListItem{
				Name:               rig.Name,
				Path:               path,
				Prefix:             prefix,
				DefaultBranch:      defaultBranch,
				Suspended:          rig.Suspended,
				Running:            rig.RunningCount > 0,
				DefaultSlingTarget: defaultSlingTarget,
				Beads:              rigBeadsStatus(fs, path),
			})
		}
		result.Summary.Total = len(result.Rigs)
		for _, rig := range result.Rigs {
			if rig.Suspended {
				result.Summary.Suspended++
			}
			if rig.Running {
				result.Summary.Running++
			}
		}
		if err := writeCLIJSONLine(stdout, result); err != nil {
			fmt.Fprintf(stderr, "gc rig list: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return 0
	}

	w := func(s string) { fmt.Fprintln(stdout, s) } //nolint:errcheck // best-effort stdout
	w("")
	w(fmt.Sprintf("Rigs in %s:", cityPath))

	hqBeads := rigBeadsStatus(fs, cityPath)
	displayName := loadedCityName(cfg, cityPath)
	w("")
	w(fmt.Sprintf("  %s (HQ):", displayName))
	w(fmt.Sprintf("    Prefix: %s", hqPrefix))
	w(fmt.Sprintf("    Beads:  %s", hqBeads))

	for _, rig := range cr.Body {
		path := rig.Path
		prefix := rig.Prefix
		defaultBranch := rig.DefaultBranch
		if cfgRig, ok := rigsByName[rig.Name]; ok {
			path = cfgRig.Path
			prefix = cfgRig.EffectivePrefix()
			defaultBranch = cfgRig.EffectiveDefaultBranch()
		}
		beads := rigBeadsStatus(fs, path)
		header := rig.Name
		if rig.Suspended {
			header += " (suspended)"
		}
		w("")
		w(fmt.Sprintf("  %s:", header))
		w(fmt.Sprintf("    Path:   %s", path))
		w(fmt.Sprintf("    Prefix: %s", prefix))
		if defaultBranch != "" {
			w(fmt.Sprintf("    Default branch: %s", defaultBranch))
		}
		w(fmt.Sprintf("    Beads:  %s", beads))
	}

	if cr.AgeSeconds > cacheAgeBannerThresholdSeconds {
		w("")
		w(fmt.Sprintf("(cache age: %.0fs — reconciler may be lagging)", cr.AgeSeconds))
	}
	return 0
}

// cacheAgeBannerThresholdSeconds is the cache-age cutoff above which human
// output appends the "reconciler may be lagging" banner. Matches the
// enabler contract D5 documented in the ga-h6w plan.
const cacheAgeBannerThresholdSeconds = 30.0

// RigListJSON is the JSON output format for "gc rig list --json".
type RigListJSON struct {
	SchemaVersion string         `json:"schema_version"`
	CityPath      string         `json:"city_path"`
	CityName      string         `json:"city_name"`
	Rigs          []RigListItem  `json:"rigs"`
	Summary       RigListSummary `json:"summary"`
	CacheAgeS     *float64       `json:"_cache_age_s,omitempty"`
}

// RigListItem is one rig entry in the JSON output.
type RigListItem struct {
	Name string `json:"name"`
	// Path is the absolute filesystem path to the rig directory, resolved from
	// city.toml by resolveRigPaths. Always absolute in output, regardless of
	// the relative form stored in city.toml.
	Path               string `json:"path"`
	Prefix             string `json:"prefix"`
	DefaultBranch      string `json:"default_branch,omitempty"`
	HQ                 bool   `json:"hq"`
	Suspended          bool   `json:"suspended"`
	Running            bool   `json:"running"`
	DefaultSlingTarget string `json:"default_sling_target,omitempty"`
	Beads              string `json:"beads"`
}

type RigListSummary struct {
	Total     int `json:"total"`
	Suspended int `json:"suspended"`
	Running   int `json:"running"`
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
	configStderr := stderr
	if jsonOutput {
		configStderr = io.Discard
	}
	cfg, err := loadCityConfigFS(fs, filepath.Join(cityPath, "city.toml"), configStderr)
	if err != nil {
		if jsonOutput {
			return writeJSONError(stdout, stderr, "config_load_failed", fmt.Sprintf("gc rig list: %v", err), 1)
		}
		fmt.Fprintf(stderr, "gc rig list: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	resolveRigPaths(cityPath, cfg.Rigs)

	suspState, _ := loadSuspensionState(fs, cityPath)
	suspNames := buildEffectiveSuspendedRigNames(cfg, suspState)

	hqPrefix := config.EffectiveHQPrefix(cfg)
	cityName := cfg.EffectiveCityName()

	if jsonOutput {
		result := RigListJSON{
			SchemaVersion: "1",
			CityPath:      cityPath,
			CityName:      cityName,
		}
		hqRunning := controllerAlive(cityPath) != 0
		result.Rigs = append(result.Rigs, RigListItem{
			Name:    cityName,
			Path:    cityPath,
			Prefix:  hqPrefix,
			HQ:      true,
			Running: hqRunning,
			Beads:   rigBeadsStatus(fs, cityPath),
		})
		for i := range cfg.Rigs {
			running := rigHasRunningAgent(cfg, cfg.Rigs[i].Name)
			result.Rigs = append(result.Rigs, RigListItem{
				Name:               cfg.Rigs[i].Name,
				Path:               cfg.Rigs[i].Path,
				Prefix:             cfg.Rigs[i].EffectivePrefix(),
				DefaultBranch:      cfg.Rigs[i].EffectiveDefaultBranch(),
				Suspended:          suspNames[cfg.Rigs[i].Name],
				Running:            running,
				DefaultSlingTarget: cfg.Rigs[i].DefaultSlingTarget,
				Beads:              rigBeadsStatus(fs, cfg.Rigs[i].Path),
			})
		}
		result.Summary.Total = len(result.Rigs)
		for _, rig := range result.Rigs {
			if rig.Suspended {
				result.Summary.Suspended++
			}
			if rig.Running {
				result.Summary.Running++
			}
		}
		if err := writeCLIJSONLine(stdout, result); err != nil {
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
		if suspNames[cfg.Rigs[i].Name] {
			header += " (suspended)"
		}
		w("")
		w(fmt.Sprintf("  %s:", header))
		w(fmt.Sprintf("    Path:   %s", cfg.Rigs[i].Path))
		w(fmt.Sprintf("    Prefix: %s", prefix))
		if branch := cfg.Rigs[i].EffectiveDefaultBranch(); branch != "" {
			w(fmt.Sprintf("    Default branch: %s", branch))
		}
		w(fmt.Sprintf("    Beads:  %s", beads))
	}
	return 0
}

func rigHasRunningAgent(cfg *config.City, rigName string) bool {
	if cfg == nil || rigName == "" {
		return false
	}
	cityName := cfg.EffectiveCityName()
	sp := newSessionProvider()
	for i := range cfg.Agents {
		a := cfg.Agents[i]
		if a.Dir != rigName {
			continue
		}
		sp0 := scaleParamsFor(&a)
		if isMultiSessionCfgAgent(&a) {
			for _, qualifiedInstance := range discoverPoolInstances(a.Name, a.Dir, sp0, &a, cityName, cfg.Workspace.SessionTemplate, sp) {
				if sp.IsRunning(sessionName(nil, cityName, qualifiedInstance, cfg.Workspace.SessionTemplate)) {
					return true
				}
			}
			continue
		}
		if sp.IsRunning(sessionName(nil, cityName, a.QualifiedName(), cfg.Workspace.SessionTemplate)) {
			return true
		}
	}
	return false
}

// rigBeadsStatus returns a human-readable beads status for a directory.
// It reports only fully initialized stores; the rig-add guard below uses a
// broader "dangerous to initialize over" check for partial store evidence.
func rigBeadsStatus(fs fsys.FS, dir string) string {
	metaPath := filepath.Join(dir, ".beads", "metadata.json")
	if _, err := fs.Stat(metaPath); err == nil {
		return "initialized"
	}
	return "not initialized"
}

func newRigSuspendCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "suspend [name]",
		Short: "Suspend a rig (reconciler will skip its agents)",
		Long: `Suspend a rig by recording the suspension in the runtime state file
(.gc/runtime/suspension-state.json).

All agents scoped to the suspended rig are effectively suspended —
the reconciler skips them and gc hook returns empty. The rig's beads
database remains accessible. Use "gc rig resume" to restore.

Suspension state is stored in the runtime directory, not city.toml,
so it is local to this machine and does not need to be committed.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if jsonOutput {
				rigName := ""
				if len(args) > 0 {
					rigName = args[0]
				} else if ctx, err := resolveContext(); err == nil {
					rigName = ctx.RigName
				}
				if cmdRigSuspend(args, io.Discard, stderr) != 0 {
					return errExit
				}
				return writeManagementActionJSON(stdout, managementActionResult{
					Command:   commandName("rig", "suspend"),
					Action:    "suspend",
					Name:      rigName,
					Rig:       rigName,
					Suspended: managementBoolPtr(true),
				})
			}
			if cmdRigSuspend(args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
		ValidArgsFunction: completeRigNames,
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output in JSONL format")
	return cmd
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

// doRigSuspend records rig suspension in the runtime state file.
// Accepts an injected FS for testability.
func doRigSuspend(fs fsys.FS, cityPath, rigName string, stdout, stderr io.Writer) int {
	tomlPath := filepath.Join(cityPath, "city.toml")
	cfg, err := loadCityConfigForEditFS(fs, tomlPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc rig suspend: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	found := false
	for _, r := range cfg.Rigs {
		if r.Name == rigName {
			found = true
			break
		}
	}
	if !found {
		fmt.Fprintln(stderr, rigNotFoundMsg("gc rig suspend", rigName, cfg)) //nolint:errcheck // best-effort stderr
		return 1
	}

	st, err := loadSuspensionState(fs, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc rig suspend: reading state: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if !suspendRigInState(&st, rigName) {
		fmt.Fprintf(stdout, "Rig '%s' is already suspended\n", rigName) //nolint:errcheck // best-effort stdout
		return 0
	}

	if err := saveSuspensionState(fs, cityPath, st); err != nil {
		fmt.Fprintf(stderr, "gc rig suspend: writing state: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	fmt.Fprintf(stdout, "Suspended rig '%s'\n", rigName) //nolint:errcheck // best-effort stdout
	return 0
}

func newRigResumeCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "resume [name]",
		Short: "Resume a suspended rig",
		Long: `Resume a suspended rig by recording an explicit "resumed" preference
in .gc/runtime/suspension-state.json. The override sticks across city restarts
even when the rig declares suspended_on_start = true.

The reconciler will start the rig's agents on its next tick.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if jsonOutput {
				rigName := ""
				if len(args) > 0 {
					rigName = args[0]
				} else if ctx, err := resolveContext(); err == nil {
					rigName = ctx.RigName
				}
				if cmdRigResume(args, io.Discard, stderr) != 0 {
					return errExit
				}
				return writeManagementActionJSON(stdout, managementActionResult{
					Command:   commandName("rig", "resume"),
					Action:    "resume",
					Name:      rigName,
					Rig:       rigName,
					Suspended: managementBoolPtr(false),
				})
			}
			if cmdRigResume(args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
		ValidArgsFunction: completeRigNames,
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output in JSONL format")
	return cmd
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

// doRigResume removes rig suspension from the runtime state file.
// Records an explicit "resumed" preference in .gc/runtime/suspension-state.json.
// The legacy `suspended` field in city.toml is left untouched — `gc doctor`
// flags it as a deprecated-field warning and users migrate by renaming
// it to suspended_on_start (or removing it).
// Accepts an injected FS for testability.
func doRigResume(fs fsys.FS, cityPath, rigName string, stdout, stderr io.Writer) int {
	tomlPath := filepath.Join(cityPath, "city.toml")
	cfg, err := loadCityConfigForEditFS(fs, tomlPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc rig resume: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	found := false
	for _, r := range cfg.Rigs {
		if r.Name == rigName {
			found = true
			break
		}
	}
	if !found {
		fmt.Fprintln(stderr, rigNotFoundMsg("gc rig resume", rigName, cfg)) //nolint:errcheck // best-effort stderr
		return 1
	}

	st, err := loadSuspensionState(fs, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc rig resume: reading state: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if !resumeRigInState(&st, rigName) {
		fmt.Fprintf(stdout, "Rig '%s' is not suspended\n", rigName) //nolint:errcheck // best-effort stdout
		return 0
	}

	if err := saveSuspensionState(fs, cityPath, st); err != nil {
		fmt.Fprintf(stderr, "gc rig resume: writing state: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	fmt.Fprintf(stdout, "Resumed rig '%s'\n", rigName) //nolint:errcheck // best-effort stdout
	return 0
}

func newRigRemoveCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a rig from the city",
		Long: `Remove a rig from the current city's configuration.

Removes the rig entry from city.toml and removes its machine-local path
binding from .gc/site.toml.`,
		Example: `  gc rig remove myrig`,
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if jsonOutput {
				if cmdRigRemove(args[0], io.Discard, stderr) != 0 {
					return errExit
				}
				return writeManagementActionJSON(stdout, managementActionResult{
					Command: commandName("rig", "remove"),
					Action:  "remove",
					Name:    args[0],
					Rig:     args[0],
				})
			}
			if cmdRigRemove(args[0], stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
		ValidArgsFunction: completeRigNames,
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output in JSONL format")
	return cmd
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
	if err := config.WriteCityAndRigSiteBindingsForEditRemovingRigs(fsys.OSFS{}, tomlPath, cfg, rigName); err != nil {
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

// beadsDirContainsStore reports whether beadsPath contains evidence that it
// would be dangerous to initialize over. Either canonical marker is enough to
// stop fresh initialization because partial stores should fail closed; only
// missing marker files are ignored.
func beadsDirContainsStore(fs fsys.FS, beadsPath string) (bool, error) {
	for _, name := range [...]string{"metadata.json", "config.yaml"} {
		path := filepath.Join(beadsPath, name)
		if _, err := fs.Stat(path); err == nil {
			return true, nil
		} else if !os.IsNotExist(err) {
			return false, fmt.Errorf("checking %s: %w", path, err)
		}
	}
	return false, nil
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
