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
	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/hooks"
	"github.com/gastownhall/gascity/internal/packman"
	"github.com/gastownhall/gascity/internal/rig"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/spf13/cobra"
)

const rigDeferredStoreInitWait = 30 * time.Second

var (
	rigReloadControllerConfig = reloadControllerConfig
	rigWaitForStoreAccessible = waitForRigStoreAccessible
	rigListSessionProvider    = newSessionProvider
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
	var gitURLFlag string
	var requestIDFlag string
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
			// Remote city: drive server-side provisioning over the control plane
			// before any local city/config/store work. The branch runs ahead of both
			// resolveCity() paths below so a resolved remote target never touches
			// local disk (gate G1). A remote error is non-fallbackable.
			remoteC, isRemote, target, rerr := resolveWriteTarget()
			if rerr != nil {
				if jsonOutput {
					if writeJSONError(stdout, stderr, "city_resolve_failed", fmt.Sprintf("gc rig add: %v", rerr), 1) != 0 {
						return errExit
					}
					return nil
				}
				fmt.Fprintf(stderr, "gc rig add: %v\n", rerr) //nolint:errcheck // best-effort stderr
				return errExit
			}
			if isRemote {
				if cmdRigAddRemote(remoteC, target, args, gitURLFlag, requestIDFlag, nameFlag, prefixFlag, defaultBranchFlag, includes, startSuspended, adoptFlag, jsonOutput, stdout, stderr) != 0 {
					return errExit
				}
				return nil
			}
			// LOCAL path (byte-identical to the pre-C7 behavior). --git-url is a
			// remote-only feature in C7: local clone semantics are un-specced, and
			// teaching the local path to clone would risk the byte-identical local
			// output the gate protects. Refuse it loudly.
			if strings.TrimSpace(gitURLFlag) != "" {
				msg := "gc rig add: --git-url requires a remote city target (--context/--city-url); for a local city run `git clone` then `gc rig add <path>`"
				if jsonOutput {
					if writeJSONError(stdout, stderr, "unsupported_local", msg, 1) != 0 {
						return errExit
					}
					return nil
				}
				fmt.Fprintln(stderr, msg) //nolint:errcheck // best-effort stderr
				return errExit
			}
			// --request-id is the idempotency key for a server-side --git-url
			// provision; it has no meaning locally. Refuse it loudly rather than
			// silently ignoring it (symmetric with the --git-url guard above).
			if strings.TrimSpace(requestIDFlag) != "" {
				msg := "gc rig add: --request-id requires a remote city target (--context/--city-url); it is the idempotency key for a server-side --git-url provision"
				if jsonOutput {
					if writeJSONError(stdout, stderr, "unsupported_local", msg, 1) != 0 {
						return errExit
					}
					return nil
				}
				fmt.Fprintln(stderr, msg) //nolint:errcheck // best-effort stderr
				return errExit
			}
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
				return writeManagementActionJSON(stdout, rigAddJSONSummary(rigPath, rig))
			}
			if cmdRigAdd(args, includes, nameFlag, prefixFlag, defaultBranchFlag, startSuspended, adoptFlag, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&includes, "include", nil, "pack source for rig agents (repeatable; writes canonical rig imports)")
	cmd.Flags().StringVar(&nameFlag, "name", "", "rig name (default: directory basename, or git URL basename for --git-url)")
	cmd.Flags().StringVar(&prefixFlag, "prefix", "", "bead ID prefix (default: derived from name)")
	cmd.Flags().StringVar(&defaultBranchFlag, "default-branch", "", "mainline branch (default: auto-detect from origin/HEAD or current branch)")
	cmd.Flags().BoolVar(&startSuspended, "start-suspended", false, "add rig in suspended state (dormant-by-default)")
	cmd.Flags().BoolVar(&adoptFlag, "adopt", false, "adopt existing .beads/ directory (skip init)")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output in JSONL format")
	cmd.Flags().StringVar(&gitURLFlag, "git-url", "", "git URL to clone into a new rig on a REMOTE city (server-side provisioning)")
	cmd.Flags().StringVar(&requestIDFlag, "request-id", "", "idempotency key for a remote --git-url add; reuse it to resume/retry a provision")
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
	return doRigAdd(fsys.OSFS{}, cityPath, rigPath, includes, nameOverride, prefixOverride, defaultBranchOverride, startSuspended, adopt, stdout, stderr)
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
	// Preflight the rig path before loading config so an invalid rig path is
	// reported ahead of a config-load failure (Provision re-checks it as
	// step 2). This preserves the original error ordering.
	if _, err := rig.StatRigPath(fs, rigPath, adopt); err != nil {
		fmt.Fprintf(stderr, "gc rig add: %v\n", err) //nolint:errcheck // best-effort stderr
		return config.Rig{}, 1
	}
	tomlPath := filepath.Join(cityPath, "city.toml")
	cfg, err := loadCityConfigForEditFS(fs, tomlPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc rig add: loading config: %v\n", err) //nolint:errcheck // best-effort stderr
		return config.Rig{}, 1
	}
	// Register the city dolt config for the duration of provisioning so the
	// beads-init path can read the process-global lifecycle fields. The
	// register/clear pair must stay in one lexical scope wrapping the whole
	// Provision call.
	if cityUsesBdStoreContract(cityPath) && cityDoltConfigHasLifecycleFields(cfg.Dolt) {
		registerCityDoltConfig(cityPath, cfg.Dolt)
		defer clearCityDoltConfig(cityPath)
	}
	name := nameOverride
	if name == "" {
		name = filepath.Base(rigPath)
	}

	deps := rig.Deps{
		FS:           fs,
		CityPath:     cityPath,
		Cfg:          cfg,
		InitStore:    initDirIfReady,
		InitAndHook:  initAndHookDir,
		ComposePacks: ensureBundledRigImportsInstalled,
		WriteRoutes: func(cp string, c *config.City) error {
			return writeAllRigRoutes(collectRigRoutes(cp, c))
		},
		ProbeBranch: func(p string) string { return git.New(p).ProbeDefaultBranch() },
		NormalizeScopes: func(cp string, c *config.City) error {
			return normalizeCanonicalBdScopeFiles(cp, c, io.Discard)
		},
		PrepareAdopt:  prepareRigAdoptProviderState,
		StoreContract: cityUsesBdStoreContract,
		DoltSkip:      gcDoltSkip,
		PostProvision: func(pc rig.ProvisionContext) error {
			if adopt {
				if err := installBeadHooks(rigPath, cityPath); err != nil {
					fmt.Fprintf(stderr, "gc rig add: installing bead hooks: %v\n", err) //nolint:errcheck // best-effort stderr
				}
			}
			if err := ensureGitignoreEntries(fs, rigPath, rigGitignoreEntries); err != nil {
				fmt.Fprintf(stderr, "gc rig add: writing .gitignore: %v\n", err) //nolint:errcheck // best-effort stderr
			}
			if ih := pc.Cfg.Workspace.InstallAgentHooks; len(ih) > 0 {
				resolver := func(name string) string { return config.BuiltinFamily(name, pc.Cfg.Providers) }
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

			if err := rigReloadControllerConfig(cityPath); err == nil && pc.Deferred && cityUsesBdStoreContract(cityPath) {
				if waitErr := rigWaitForStoreAccessible(cityPath, rigPath, rigDeferredStoreInitWait); waitErr != nil {
					fmt.Fprintf(stderr, "gc rig add: warning: controller init still pending for rig %q: %v\n", name, waitErr) //nolint:errcheck // best-effort stderr
				}
			}
			return nil
		},
		OnStep: func(s rig.ProvisionStep) {
			if s.Warn {
				fmt.Fprintf(stderr, "gc rig add: %s\n", s.Detail) //nolint:errcheck // best-effort stderr
			} else {
				fmt.Fprintln(stdout, s.Detail) //nolint:errcheck // best-effort stdout
			}
		},
	}

	r, _, err := rig.Provision(deps, rig.ProvisionRequest{
		Name:           name,
		Path:           rigPath,
		Prefix:         prefixOverride,
		DefaultBranch:  defaultBranchOverride,
		Includes:       includes,
		StartSuspended: startSuspended,
		Adopt:          adopt,
	})
	if err != nil {
		fmt.Fprintf(stderr, "gc rig add: %v\n", err) //nolint:errcheck // best-effort stderr
		return config.Rig{}, 1
	}
	return r, 0
}

// The following one-line aliases keep cmd/gc test files compiling against the
// helpers extracted into internal/rig (C2.3). They are removed once the tests
// are repointed at the rig package.
var (
	readBeadsPrefix             = rig.ReadBeadsPrefix
	mergeBoundImports           = rig.MergeBoundImports
	snapshotRigAddTopologyFiles = rig.SnapshotTopologyFiles
)

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
	existing, err := collectAllImportsFS(cityPath)
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
		if !api.ShouldFallbackForRead(c, err) {
			logRoute(stderr, cmdName, "api", "error")
			fmt.Fprintf(stderr, "gc rig list: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		logRoute(stderr, cmdName, "fallback", api.FallbackReason(c, err))
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
			var defaultSlingTargets []string
			if cfgRig, ok := rigsByName[rig.Name]; ok {
				path = cfgRig.Path
				prefix = cfgRig.EffectivePrefix()
				defaultBranch = cfgRig.EffectiveDefaultBranch()
				defaultSlingTarget = cfgRig.DefaultSlingTarget
				defaultSlingTargets = cfgRig.DefaultSlingTargets
			}
			result.Rigs = append(result.Rigs, RigListItem{
				Name:                rig.Name,
				Path:                path,
				Prefix:              prefix,
				DefaultBranch:       defaultBranch,
				Suspended:           rig.Suspended,
				Running:             rig.RunningCount > 0,
				DefaultSlingTarget:  defaultSlingTarget,
				DefaultSlingTargets: defaultSlingTargets,
				Beads:               rigBeadsStatus(fs, path),
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
	Path                string   `json:"path"`
	Prefix              string   `json:"prefix"`
	DefaultBranch       string   `json:"default_branch,omitempty"`
	HQ                  bool     `json:"hq"`
	Suspended           bool     `json:"suspended"`
	Running             bool     `json:"running"`
	DefaultSlingTarget  string   `json:"default_sling_target,omitempty"`
	DefaultSlingTargets []string `json:"default_sling_targets,omitempty"`
	Beads               string   `json:"beads"`
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
		// Build the session provider once and share it across rigs:
		// constructing it per rig reopened the session store and re-forked
		// tmux probes, making --json scale O(rigs) in subprocesses (~7x
		// slower than the text path, which skips running-status detection).
		var sp runtime.Provider
		if len(cfg.Rigs) > 0 {
			sp, err = rigListSessionProvider()
			if err != nil {
				return writeJSONError(stdout, stderr, "session_provider_failed", fmt.Sprintf("gc rig list: %v", err), 1)
			}
		}
		for i := range cfg.Rigs {
			running := rigHasRunningAgent(cfg, cfg.Rigs[i].Name, sp)
			result.Rigs = append(result.Rigs, RigListItem{
				Name:                cfg.Rigs[i].Name,
				Path:                cfg.Rigs[i].Path,
				Prefix:              cfg.Rigs[i].EffectivePrefix(),
				DefaultBranch:       cfg.Rigs[i].EffectiveDefaultBranch(),
				Suspended:           suspNames[cfg.Rigs[i].Name],
				Running:             running,
				DefaultSlingTarget:  cfg.Rigs[i].DefaultSlingTarget,
				DefaultSlingTargets: cfg.Rigs[i].DefaultSlingTargets,
				Beads:               rigBeadsStatus(fs, cfg.Rigs[i].Path),
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

// rigHasRunningAgent reports whether any agent scoped to rigName has a live
// session. The caller supplies the session provider so a single provider can
// be reused across rigs instead of reconstructed per rig (see doRigList).
func rigHasRunningAgent(cfg *config.City, rigName string, sp runtime.Provider) bool {
	if cfg == nil || rigName == "" || sp == nil {
		return false
	}
	cityName := cfg.EffectiveCityName()
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
		if !api.ShouldFallback(c, err) {
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
		if !api.ShouldFallback(c, err) {
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

	// Drop config blocks that reference the removed rig; left dangling they
	// break a later load of the city (#3666). The three [[patches.*]] kinds and
	// a [[github.pr_monitor]] hard-fail config.LoadWithIncludes once their
	// now-absent rig can no longer be resolved (ApplyPatches "not found in
	// merged config" / ValidateGitHubPRMonitors "rig is not declared"); an
	// [[orders.overrides]] instead dangles at order-scan time. The issue named
	// [[patches.agent]] and [[orders.overrides]]; the rest fail the same way on
	// rig removal, so sweep them all.
	cfg.Patches.Agents = slices.DeleteFunc(cfg.Patches.Agents,
		func(p config.AgentPatch) bool { return p.Dir == rigName })
	cfg.Patches.NamedSessions = slices.DeleteFunc(cfg.Patches.NamedSessions,
		func(p config.NamedSessionPatch) bool { return p.Dir == rigName })
	cfg.Patches.Rigs = slices.DeleteFunc(cfg.Patches.Rigs,
		func(p config.RigPatch) bool { return p.Name == rigName })
	// Capture rig-scoped PR monitor names before deleting them so we can also
	// drop any [[patches.github_pr_monitor]] that targets them by name —
	// otherwise the patch dangles and ApplyPatches fails ("github pr monitor
	// %q not found in merged config") on the next compose, the same #3666 class.
	removedMonitors := map[string]bool{}
	for _, m := range cfg.GitHub.PRMonitors {
		if m.Rig == rigName {
			removedMonitors[m.Name] = true
		}
	}
	cfg.GitHub.PRMonitors = slices.DeleteFunc(cfg.GitHub.PRMonitors,
		func(m config.GitHubPRMonitor) bool { return m.Rig == rigName })
	cfg.Patches.GitHubPRMonitors = slices.DeleteFunc(cfg.Patches.GitHubPRMonitors,
		func(p config.GitHubPRMonitorPatch) bool { return removedMonitors[p.Name] })
	cfg.Orders.Overrides = slices.DeleteFunc(cfg.Orders.Overrides,
		func(o config.OrderOverride) bool { return o.Rig == rigName })

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
