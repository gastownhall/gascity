package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/hooks"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/telemetry"
	workdirutil "github.com/gastownhall/gascity/internal/workdir"
	"github.com/gastownhall/gascity/internal/workspacesvc"
	"github.com/spf13/cobra"
)

// computeSuspendedNames builds a set of session names for agents marked
// suspended in the config or belonging to suspended rigs. Also includes
// all agents when the city itself is suspended (workspace.suspended).
// Used by the reconciler to distinguish suspended agents from true orphans
// during Phase 2 cleanup.
func computeSuspendedNames(cfg *config.City, cityName, cityPath string) map[string]bool {
	names := make(map[string]bool)
	st := cfg.Workspace.SessionTemplate

	// City-level suspend: all agents are suspended.
	if cfg.Workspace.Suspended {
		for _, a := range cfg.Agents {
			names[cliSessionName(cityPath, cityName, a.QualifiedName(), st)] = true
		}
		return names
	}

	// Individually suspended agents.
	for _, a := range cfg.Agents {
		if a.Suspended {
			qn := a.QualifiedName()
			names[cliSessionName(cityPath, cityName, qn, st)] = true
		}
	}
	// Agents in suspended rigs.
	suspendedRigPaths := make(map[string]bool)
	for _, r := range cfg.Rigs {
		if r.Suspended {
			suspendedRigPaths[filepath.Clean(r.Path)] = true
		}
	}
	if len(suspendedRigPaths) > 0 {
		for _, a := range cfg.Agents {
			if a.Suspended || a.Dir == "" {
				continue // Already counted or no rig scope.
			}
			rigName := configuredRigName(cityPath, &a, cfg.Rigs)
			if rigName != "" && suspendedRigPaths[filepath.Clean(rigRootForName(rigName, cfg.Rigs))] {
				names[cliSessionName(cityPath, cityName, a.QualifiedName(), st)] = true
			}
		}
	}
	return names
}

// computePoolSessions builds the set of ALL possible pool session names
// (1..max for bounded pools, currently running for unlimited) for every
// multi-instance pool agent in the config, mapped to the pool's drain
// timeout. Used to distinguish excess pool members (drain) from true orphans
// (kill) during reconciliation, and to enforce drain timeouts.
func computePoolSessions(cfg *config.City, cityName, cityPath string, sp runtime.Provider) map[string]time.Duration {
	ps := make(map[string]time.Duration)
	st := cfg.Workspace.SessionTemplate
	for _, a := range cfg.Agents {
		pool := a.EffectivePool()
		if !a.IsPool() || !pool.IsMultiInstance() {
			continue
		}
		timeout := pool.DrainTimeoutDuration()
		for _, qualifiedInstance := range discoverPoolInstances(a.Name, a.Dir, pool, cityName, st, sp) {
			ps[cliSessionName(cityPath, cityName, qualifiedInstance, st)] = timeout
		}
	}
	return ps
}

// poolDeathInfo holds the on_death command and working directory for a pool instance.
type poolDeathInfo struct {
	Command string // on_death shell command (pre-baked with instance QN)
	Dir     string // working directory for bd commands
}

// computePoolDeathHandlers builds a map from session name to death handler
// for every pool instance (static for bounded pools, currently running for
// unlimited). Used to detect and handle pool deaths.
func computePoolDeathHandlers(cfg *config.City, cityName, cityPath string, sp runtime.Provider) map[string]poolDeathInfo {
	handlers := make(map[string]poolDeathInfo)
	st := cfg.Workspace.SessionTemplate
	for _, a := range cfg.Agents {
		if !a.IsPool() {
			continue
		}
		pool := a.EffectivePool()
		if !pool.IsMultiInstance() {
			continue
		}
		for _, qualifiedInstance := range discoverPoolInstances(a.Name, a.Dir, pool, cityName, st, sp) {
			_, instanceName := config.ParseQualifiedName(qualifiedInstance)
			instance := config.Agent{Name: instanceName, Dir: a.Dir, Pool: a.Pool, PoolName: a.QualifiedName()}
			cmd := instance.EffectiveOnDeath()
			if cmd == "" {
				continue
			}
			dir := agentCommandDir(cityPath, &a, cfg.Rigs)
			sn := cliSessionName(cityPath, cityName, qualifiedInstance, st)
			handlers[sn] = poolDeathInfo{Command: cmd, Dir: dir}
		}
	}
	return handlers
}

// extraConfigFiles holds paths from -f flags for CLI-level file layering.
var extraConfigFiles []string

// strictMode promotes composition collision warnings to errors.
// Defaults to true; use --no-strict to disable.
var strictMode bool

// noStrictMode disables strict config checking (opt-out).
var noStrictMode bool

// dryRunMode previews what agents would start without actually starting them.
var dryRunMode bool

// buildIdleTracker creates an idleTracker from the config, populating
// timeouts for agents that have idle_timeout set. Returns nil if no
// agents use idle timeout (disabled).
func buildIdleTracker(cfg *config.City, cityName, cityPath string, sp runtime.Provider) idleTracker {
	var hasAny bool
	st := cfg.Workspace.SessionTemplate
	for _, a := range cfg.Agents {
		if a.IdleTimeoutDuration() > 0 {
			hasAny = true
			break
		}
	}
	if !hasAny {
		return nil
	}
	it := newIdleTracker()
	for _, a := range cfg.Agents {
		timeout := a.IdleTimeoutDuration()
		if timeout <= 0 {
			continue
		}
		pool := a.EffectivePool()
		if a.IsPool() && pool.IsMultiInstance() {
			// Register each pool instance (worker-1, worker-2, ...).
			for _, qualifiedInstance := range discoverPoolInstances(a.Name, a.Dir, pool, cityName, st, sp) {
				sn := cliSessionName(cityPath, cityName, qualifiedInstance, st)
				it.setTimeout(sn, timeout)
			}
		} else {
			sn := cliSessionName(cityPath, cityName, a.QualifiedName(), st)
			it.setTimeout(sn, timeout)
		}
	}
	return it
}

func newStartCmd(stdout, stderr io.Writer) *cobra.Command {
	var foregroundMode bool
	cmd := &cobra.Command{
		Use:   "start [path]",
		Short: "Start the city under the machine-wide supervisor",
		Long: `Start the city under the machine-wide supervisor.

Requires an existing city bootstrapped by "gc init". Fetches remote
packs as needed, registers the city with the machine-wide supervisor,
ensures the supervisor is running, and triggers immediate reconciliation.
Use "gc supervisor run" for foreground operation.`,
		Example: `  gc start
  gc start ~/my-city
  gc start --dry-run
  gc supervisor run`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if doStart(args, foregroundMode, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&foregroundMode, "foreground", false,
		"run the legacy per-city controller loop")
	cmd.Flags().BoolVar(&foregroundMode, "controller", false,
		"alias for --foreground")
	cmd.Flags().MarkHidden("foreground") //nolint:errcheck // flag always exists
	cmd.Flags().MarkHidden("controller") //nolint:errcheck // flag always exists
	cmd.Flags().StringArrayVarP(&extraConfigFiles, "file", "f", nil,
		"additional config files to layer (can be repeated)")
	cmd.Flags().BoolVar(&noStrictMode, "no-strict", false,
		"disable strict config collision checking (strict is on by default)")
	cmd.Flags().MarkHidden("file")      //nolint:errcheck // flag always exists
	cmd.Flags().MarkHidden("no-strict") //nolint:errcheck // flag always exists
	cmd.Flags().BoolVarP(&dryRunMode, "dry-run", "n", false,
		"preview what agents would start without starting them")
	return cmd
}

func doStart(args []string, controllerMode bool, stdout, stderr io.Writer) int {
	if controllerMode || dryRunMode {
		return doStartStandalone(args, controllerMode, stdout, stderr)
	}
	if len(extraConfigFiles) > 0 || noStrictMode {
		fmt.Fprintln(stderr, "gc start: --file and --no-strict only apply to the legacy standalone controller; use --foreground or remove those flags") //nolint:errcheck // best-effort stderr
		return 1
	}

	dir, err := resolveStartDir(args)
	if err != nil {
		fmt.Fprintf(stderr, "gc start: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	cityPath, err := requireBootstrappedCity(dir)
	if err != nil {
		fmt.Fprintf(stderr, "gc start: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := ensureCityScaffold(cityPath); err != nil {
		fmt.Fprintf(stderr, "gc start: runtime scaffold: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if missing := checkHardDependencies(cityPath); len(missing) > 0 {
		fmt.Fprintf(stderr, "gc start: missing required dependencies:\n\n") //nolint:errcheck // best-effort stderr
		for _, dep := range missing {
			fmt.Fprintf(stderr, "  - %s", dep.name) //nolint:errcheck // best-effort stderr
			if dep.installHint != "" {
				fmt.Fprintf(stderr, "\n    Install: %s", dep.installHint) //nolint:errcheck // best-effort stderr
			}
			fmt.Fprintln(stderr) //nolint:errcheck // best-effort stderr
		}
		fmt.Fprintln(stderr)                                                               //nolint:errcheck // best-effort stderr
		fmt.Fprintln(stderr, "gc start: install the missing dependencies, then try again") //nolint:errcheck // best-effort stderr
		return 1
	}
	if code := registerCityWithSupervisor(cityPath, stdout, stderr, "gc start", true); code != 0 {
		return code
	}
	fmt.Fprintln(stdout, "City started under supervisor.") //nolint:errcheck // best-effort stdout
	return 0
}

func resolveStartDir(args []string) (string, error) {
	switch {
	case len(args) > 0:
		return filepath.Abs(args[0])
	case cityFlag != "":
		return filepath.Abs(cityFlag)
	default:
		return os.Getwd()
	}
}

func requireBootstrappedCity(dir string) (string, error) {
	cityPath, err := findCity(dir)
	if err != nil {
		absDir, absErr := filepath.Abs(dir)
		if absErr == nil {
			return "", fmt.Errorf("%w; run \"gc init %s\" first", err, absDir)
		}
		return "", fmt.Errorf("%w; run \"gc init\" first", err)
	}
	if !citylayout.HasRuntimeRoot(cityPath) {
		return "", fmt.Errorf("city runtime not bootstrapped at %s; run \"gc init %s\" first", cityPath, cityPath)
	}
	return cityPath, nil
}

// doStartStandalone boots an existing city in the legacy per-city mode.
// If a path is given, operates there; otherwise uses cwd. When controllerMode
// is true, enters a persistent reconciliation loop instead of one-shot start.
func doStartStandalone(args []string, controllerMode bool, stdout, stderr io.Writer) int {
	// Strict mode is on by default; --no-strict disables it.
	strictMode = !noStrictMode

	dir, err := resolveStartDir(args)
	if err != nil {
		fmt.Fprintf(stderr, "gc start: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	cityPath, err := requireBootstrappedCity(dir)
	if err != nil {
		fmt.Fprintf(stderr, "gc start: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if controllerMode {
		_, registered, err := registeredCityEntry(cityPath)
		if err != nil {
			fmt.Fprintf(stderr, "gc start: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		if registered {
			fmt.Fprintf(stderr, "gc start: city is registered with the supervisor; run \"gc unregister %s\" before using --foreground\n", cityPath) //nolint:errcheck // best-effort stderr
			return 1
		}
	}
	if err := ensureCityScaffold(cityPath); err != nil {
		fmt.Fprintf(stderr, "gc start: runtime scaffold: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	// Quick-parse city.toml (without includes) for pre-load tasks.
	quickCfg, qErr := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))

	// Materialize gastown packs before full config load if the city
	// references them. Covers the case where gc init wrote city.toml
	// but failed before MaterializeGastownPacks ran.
	if qErr == nil && usesGastownPack(quickCfg) {
		if err := MaterializeGastownPacks(cityPath); err != nil {
			fmt.Fprintf(stderr, "gc start: materializing gastown packs: %v\n", err) //nolint:errcheck // best-effort stderr
		}
	}

	// Auto-fetch remote packs before full config load.
	if qErr == nil && len(quickCfg.Packs) > 0 {
		if fErr := config.FetchPacks(quickCfg.Packs, cityPath); fErr != nil {
			fmt.Fprintf(stderr, "gc start: fetching packs: %v\n", fErr) //nolint:errcheck // best-effort stderr
			return 1
		}
	}

	cfg, prov, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"), extraConfigFiles...)
	if err != nil {
		fmt.Fprintf(stderr, "gc start: %v\n", err)                      //nolint:errcheck // best-effort stderr
		fmt.Fprintln(stderr, "hint: run \"gc doctor\" for diagnostics") //nolint:errcheck // best-effort stderr
		return 1
	}
	// Strict mode (default) promotes composition warnings to errors.
	if strictMode && len(prov.Warnings) > 0 {
		for _, w := range prov.Warnings {
			fmt.Fprintf(stderr, "gc start: strict: %s\n", w) //nolint:errcheck // best-effort stderr
		}
		fmt.Fprintln(stderr, "gc start: use --no-strict to disable strict checking") //nolint:errcheck // best-effort stderr
		return 1
	}
	for _, w := range prov.Warnings {
		fmt.Fprintf(stderr, "gc start: warning: %s\n", w) //nolint:errcheck // best-effort stderr
	}

	cityName := cfg.Workspace.Name
	if cityName == "" {
		cityName = filepath.Base(cityPath)
	}

	// Validate rigs (prefix collisions, missing fields).
	if err := config.ValidateRigs(cfg.Rigs, cityName); err != nil {
		fmt.Fprintf(stderr, "gc start: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := config.ValidateServices(cfg.Services); err != nil {
		fmt.Fprintf(stderr, "gc start: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := workspacesvc.ValidateRuntimeSupport(cfg.Services); err != nil {
		fmt.Fprintf(stderr, "gc start: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Materialize the gc-beads-bd script so the exec: provider can use it.
	if _, err := MaterializeBeadsBdScript(cityPath); err != nil {
		fmt.Fprintf(stderr, "gc start: materializing gc-beads-bd: %v\n", err) //nolint:errcheck // best-effort stderr
		// Non-fatal: only needed if provider = "bd".
	}

	// Materialize builtin packs (bd + dolt) so doctor checks and commands are available.
	if err := MaterializeBuiltinPacks(cityPath); err != nil {
		fmt.Fprintf(stderr, "gc start: materializing builtin packs: %v\n", err) //nolint:errcheck // best-effort stderr
		// Non-fatal: only needed if provider = "bd".
	}
	injectBuiltinPacks(cfg, cityPath)

	// Materialize builtin prompts and formulas to stay in sync with binary.
	if err := materializeBuiltinPrompts(cityPath); err != nil {
		fmt.Fprintf(stderr, "gc start: builtin prompts: %v\n", err) //nolint:errcheck // best-effort stderr
	}
	if err := materializeBuiltinFormulas(cityPath); err != nil {
		fmt.Fprintf(stderr, "gc start: builtin formulas: %v\n", err) //nolint:errcheck // best-effort stderr
	}
	ensureInitArtifacts(cityPath, cfg, stderr, "gc start")

	// Wire [dolt] config to process env before the bead store lifecycle.
	// This lets gc-beads-bd's is_remote() detect external hosts and skip
	// local Dolt startup. Env vars set by the user take precedence.
	applyDoltConfig(cfg)

	// Resolve rig paths and run the full bead store lifecycle:
	// probe → init+hooks(city) → init+hooks(rigs) → routes.
	resolveRigPaths(cityPath, cfg.Rigs)
	if err := startBeadsLifecycle(cityPath, cityName, cfg, stderr); err != nil {
		fmt.Fprintf(stderr, "gc start: %v\n", err)                      //nolint:errcheck // best-effort stderr
		fmt.Fprintln(stderr, "hint: run \"gc doctor\" for diagnostics") //nolint:errcheck // best-effort stderr
		return 1
	}

	// Post-startup health check: baseline probe of the beads provider.
	// The gc-beads-bd script's health operation validates server liveness
	// (TCP + query probe). Recovery is attempted on failure.
	if err := healthBeadsProvider(cityPath); err != nil {
		fmt.Fprintf(stderr, "gc start: beads health check: %v\n", err) //nolint:errcheck // best-effort stderr
		// Non-fatal warning — server may recover by the time agents need it.
	}

	// Materialize system formulas into the city formulas/ directory.
	if _, sysErr := MaterializeSystemFormulas(systemFormulasFS, "system_formulas", cityPath); sysErr != nil {
		fmt.Fprintf(stderr, "gc start: system formulas: %v\n", sysErr) //nolint:errcheck // best-effort stderr
	}

	// Materialize formula symlinks before agent startup.
	if len(cfg.FormulaLayers.City) > 0 {
		if err := ResolveFormulas(cityPath, cfg.FormulaLayers.City); err != nil {
			fmt.Fprintf(stderr, "gc start: city formulas: %v\n", err) //nolint:errcheck // best-effort stderr
		}
	}
	for _, r := range cfg.Rigs {
		layers, ok := cfg.FormulaLayers.Rigs[r.Name]
		if !ok || len(layers) == 0 {
			layers = cfg.FormulaLayers.City
		}
		if len(layers) > 0 {
			if err := ResolveFormulas(r.Path, layers); err != nil {
				fmt.Fprintf(stderr, "gc start: rig %q formulas: %v\n", r.Name, err) //nolint:errcheck // best-effort stderr
			}
		}
	}

	// Materialize Claude skill stubs (after formulas, before agent startup).
	if cfg.Workspace.Provider == "claude" {
		dirs := []string{cityPath}
		for _, r := range cfg.Rigs {
			if r.Path != "" {
				dirs = append(dirs, r.Path)
			}
		}
		if err := materializeSkillStubs(dirs...); err != nil {
			fmt.Fprintf(stderr, "gc start: skill stubs: %v\n", err) //nolint:errcheck // best-effort stderr
			// Non-fatal.
		}
	}

	// Validate agents.
	if err := config.ValidateAgents(cfg.Agents); err != nil {
		fmt.Fprintf(stderr, "gc start: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Validate install_agent_hooks (workspace + all agents).
	if ih := cfg.Workspace.InstallAgentHooks; len(ih) > 0 {
		if err := hooks.Validate(ih); err != nil {
			fmt.Fprintf(stderr, "gc start: workspace: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}
	for _, a := range cfg.Agents {
		if len(a.InstallAgentHooks) > 0 {
			if err := hooks.Validate(a.InstallAgentHooks); err != nil {
				fmt.Fprintf(stderr, "gc start: agent %q: %v\n", a.QualifiedName(), err) //nolint:errcheck // best-effort stderr
				return 1
			}
		}
	}

	sp := newSessionProvider()

	// beaconTime is captured once so the beacon timestamp remains stable
	// across reconcile ticks. Without this, FormatBeacon(time.Now()) would
	// produce a different command string each tick, causing
	// ConfigFingerprint to detect spurious drift and restart all agents.
	beaconTime := time.Now()

	// buildAgents constructs the desired agent list from the given config.
	// Called once for one-shot, or on each tick for controller mode.
	// Pool check commands are re-evaluated each call. Accepts a *config.City
	// parameter so the controller loop can pass freshly-reloaded config.
	buildAgents := func(c *config.City, currentSP runtime.Provider, store beads.Store) map[string]TemplateParams {
		return buildDesiredState(cityName, cityPath, beaconTime, c, currentSP, store, stderr)
	}
	buildAgentsWithSessionBeads := func(c *config.City, currentSP runtime.Provider, store beads.Store, sessionBeads *sessionBeadSnapshot) map[string]TemplateParams {
		return buildDesiredStateWithSessionBeads(cityName, cityPath, beaconTime, c, currentSP, store, sessionBeads, stderr)
	}

	recorder := events.Discard
	var eventProv events.Provider // nil when events disabled or FileRecorder fails
	if fr, err := events.NewFileRecorder(
		filepath.Join(cityPath, ".gc", "events.jsonl"), stderr); err == nil {
		recorder = fr
		eventProv = fr
	}

	// Pre-check container images once (fail fast before N serial starts).
	if err := checkAgentImages(sp, cfg.Agents, stderr); err != nil {
		fmt.Fprintf(stderr, "gc start: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// --dry-run: build agents and print preview without starting.
	if dryRunMode {
		agents := buildAgents(cfg, sp, nil)
		printDryRunPreview(agents, cfg, cityName, stdout)
		return 0
	}

	tomlPath := filepath.Join(cityPath, "city.toml")
	if controllerMode {
		poolSessions := computePoolSessions(cfg, cityName, cityPath, sp)
		poolDeathHandlers := computePoolDeathHandlers(cfg, cityName, cityPath, sp)
		watchDirs := config.WatchDirs(prov, cfg, cityPath)
		configRev := config.Revision(fsys.OSFS{}, prov, cfg, cityPath)
		return runController(cityPath, tomlPath, cfg, configRev, buildAgents, buildAgentsWithSessionBeads, sp,
			newDrainOps(sp), poolSessions, poolDeathHandlers, watchDirs, recorder, eventProv, stdout, stderr)
	}

	// One-shot reconciliation (default): no drain (kill is fine).
	// Create a signal-aware context so Ctrl-C cancels in-flight starts.
	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Enforce restrictive permissions on .gc/ and its subdirectories.
	enforceGCPermissions(cityPath, stderr)

	runPoolOnBoot(cfg, cityPath, shellScaleCheck, stderr)

	var oneShotStore beads.Store
	if store, err := openCityStoreAt(cityPath); err == nil {
		oneShotStore = store

		// Run adoption barrier before sync.
		result, passed := runAdoptionBarrier(store, sp, cfg, cityName, clock.Real{}, stderr, false)
		if result.Adopted > 0 {
			fmt.Fprintf(stdout, "Adopted %d running session(s) into bead store.\n", result.Adopted) //nolint:errcheck
		}
		if !passed && result.Skipped > 0 {
			fmt.Fprintf(stderr, "adoption barrier: %d session(s) failed bead creation\n", result.Skipped) //nolint:errcheck
		}
	} else {
		// No persistent store — use in-memory store for one-shot reconciliation.
		// Beads won't be persisted, but the reconciler still manages lifecycle.
		oneShotStore = beads.NewMemStore()
	}

	// One-shot bead reconciliation: same code path as the daemon.
	sessionBeads, err := loadSessionBeadSnapshot(oneShotStore)
	if err != nil {
		fmt.Fprintf(stderr, "gc start: loading session beads: %v\n", err) //nolint:errcheck
		sessionBeads = nil
	}
	ds := buildDesiredStateWithSessionBeads(cityName, cityPath, beaconTime, cfg, sp, oneShotStore, sessionBeads, stderr)
	cfgNames := configuredSessionNamesWithSnapshot(cfg, cityName, sessionBeads)
	_, sessionBeads = syncSessionBeadsWithSnapshot(
		cityPath, oneShotStore, ds, sp, cfgNames, cfg, clock.Real{}, stderr, true, sessionBeads,
	)

	open := sessionBeads.Open()
	dt := newDrainTracker()
	poolDesired := derivePoolDesired(ds, cfg)
	reconcileSessionBeads(
		sigCtx, open, ds, cfgNames, cfg, sp, oneShotStore,
		nil, nil, nil, dt, poolDesired, cityName,
		nil, clock.Real{}, recorder, cfg.Session.StartupTimeoutDuration(), 0,
		stdout, stderr,
	)

	// Post-reconcile sync: update bead state to reflect post-start reality.
	sessionBeads, err = loadSessionBeadSnapshot(oneShotStore)
	if err != nil {
		fmt.Fprintf(stderr, "gc start: loading session beads: %v\n", err) //nolint:errcheck
		sessionBeads = nil
	}
	ds = buildDesiredStateWithSessionBeads(cityName, cityPath, beaconTime, cfg, sp, oneShotStore, sessionBeads, stderr)
	cfgNames = configuredSessionNamesWithSnapshot(cfg, cityName, sessionBeads)
	syncSessionBeadsWithSnapshot(cityPath, oneShotStore, ds, sp, cfgNames, cfg, clock.Real{}, stderr, false, sessionBeads)

	fmt.Fprintln(stdout, "City started.") //nolint:errcheck // best-effort stdout
	return 0
}

// printDryRunPreview prints what agents would be started without starting them.
func printDryRunPreview(desiredState map[string]TemplateParams, cfg *config.City, cityName string, stdout io.Writer) {
	fmt.Fprintf(stdout, "Dry-run: %d agent(s) would start in city %q\n\n", len(desiredState), cityName) //nolint:errcheck // best-effort stdout

	if len(desiredState) == 0 {
		fmt.Fprintln(stdout, "  (no agents to start)") //nolint:errcheck // best-effort stdout
		return
	}

	sortedNames := make([]string, 0, len(desiredState))
	for sn := range desiredState {
		sortedNames = append(sortedNames, sn)
	}
	sort.Strings(sortedNames)
	for _, sn := range sortedNames {
		tp := desiredState[sn]
		fmt.Fprintf(stdout, "  %-30s  session=%s\n", tp.DisplayName(), sn) //nolint:errcheck // best-effort stdout
	}

	// Summary by suspension.
	var suspended int
	for _, a := range cfg.Agents {
		if a.Suspended {
			suspended++
		}
	}
	fmt.Fprintln(stdout) //nolint:errcheck // best-effort stdout
	if suspended > 0 {
		fmt.Fprintf(stdout, "  %d agent(s) suspended (not shown above)\n", suspended) //nolint:errcheck // best-effort stdout
	}
	fmt.Fprintln(stdout, "No side effects executed (--dry-run).") //nolint:errcheck // best-effort stdout
}

// settingsArgs returns "--settings <path>" to append to a Claude command
// if settings.json exists for this city. Uses a path relative to the session
// working directory so it works for both local and remote providers (the
// .gc directory is staged via CopyFiles).
// Returns empty string for non-Claude providers or if no settings file is present.
func settingsArgs(cityPath, providerName string) string {
	if providerName != "claude" {
		return ""
	}
	settingsPath := citylayout.ClaudeHookFilePath(cityPath)
	if _, err := os.Stat(settingsPath); err != nil {
		return ""
	}
	return "--settings .gc/settings.json"
}

// stageHookFiles adds hook files installed by hooks.Install() to the
// copy_files list so container providers (K8s) can stage them into pods.
// Docker doesn't need this (bind-mount), but the extra entries are harmless.
// Avoids duplicating .gc/settings.json if settingsArgs already added it.
func stageHookFiles(copyFiles []runtime.CopyEntry, cityPath, workDir string) []runtime.CopyEntry {
	// workDir-based hooks: gemini, codex, opencode, copilot, pi, omp.
	for _, rel := range []string{
		filepath.Join(".gemini", "settings.json"),
		filepath.Join(".codex", "hooks.json"),
		filepath.Join(".opencode", "plugins", "gascity.js"),
		filepath.Join(".github", "hooks", "gascity.json"),
		filepath.Join(".github", "copilot-instructions.md"),
		filepath.Join(".pi", "extensions", "gc-hooks.js"),
		filepath.Join(".omp", "hooks", "gc-hook.ts"),
	} {
		abs := filepath.Join(workDir, rel)
		if _, err := os.Stat(abs); err == nil {
			copyFiles = append(copyFiles, runtime.CopyEntry{Src: abs, RelDst: rel})
		}
	}
	// Stage Claude skills directory (if materialized).
	skillsDir := filepath.Join(workDir, ".claude", "skills")
	if info, err := os.Stat(skillsDir); err == nil && info.IsDir() {
		copyFiles = append(copyFiles, runtime.CopyEntry{
			Src: skillsDir, RelDst: filepath.Join(".claude", "skills"),
		})
	}
	// cityDir-based hooks: claude (.gc/settings.json).
	// Skip if settingsArgs already added it.
	settingsRel := filepath.Join(".gc", "settings.json")
	settingsAbs := citylayout.ClaudeHookFilePath(cityPath)
	if _, err := os.Stat(settingsAbs); err == nil {
		alreadyStaged := false
		for _, cf := range copyFiles {
			if cf.RelDst == settingsRel {
				alreadyStaged = true
				break
			}
		}
		if !alreadyStaged {
			copyFiles = append(copyFiles, runtime.CopyEntry{Src: settingsAbs, RelDst: settingsRel})
		}
	}
	return copyFiles
}

// resolveAgentDirPath returns the absolute filesystem path for an agent dir
// spec. Empty dir defaults to cityPath. Relative paths resolve against
// cityPath. This helper is pure and does not create directories.
func resolveAgentDirPath(cityPath, dir string) string {
	if dir == "" {
		return cityPath
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(cityPath, dir)
	}
	return dir
}

// resolveAgentDir returns the absolute working directory for an agent.
// Empty dir defaults to cityPath. Relative paths resolve against cityPath.
// Creates the directory if it doesn't exist.
func resolveAgentDir(cityPath, dir string) (string, error) {
	dir = resolveAgentDirPath(cityPath, dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating agent dir %q: %w", dir, err)
	}
	return dir, nil
}

func sessionSetupContextForAgent(cityPath, cityName, qualifiedName string, a *config.Agent, rigs []config.Rig) SessionSetupContext {
	ctx := workdirutil.PathContextForQualifiedName(cityPath, cityName, qualifiedName, *a, rigs)
	return SessionSetupContext{
		Agent:     qualifiedName,
		AgentBase: ctx.AgentBase,
		Rig:       ctx.Rig,
		RigRoot:   ctx.RigRoot,
		CityRoot:  cityPath,
		CityName:  cityName,
	}
}

func resolveConfiguredWorkDir(cityPath, cityName string, a *config.Agent, rigs []config.Rig) (string, error) {
	if a == nil {
		return resolveAgentDir(cityPath, "")
	}
	workDir, err := workdirutil.ResolveWorkDirPathStrict(cityPath, cityName, a.QualifiedName(), *a, rigs)
	if err != nil {
		return "", err
	}
	return resolveAgentDir(cityPath, workDir)
}

// configuredRigName returns the rig associated with an agent, preferring the
// legacy dir-as-rig convention and falling back to path matching for inline
// configs that point directly at a rig path.
func configuredRigName(cityPath string, a *config.Agent, rigs []config.Rig) string {
	if a == nil || a.Dir == "" {
		return ""
	}
	return workdirutil.ConfiguredRigName(cityPath, *a, rigs)
}

// rigRootForName returns the configured rig root path.
func rigRootForName(rigName string, rigs []config.Rig) string {
	return workdirutil.RigRootForName(rigName, rigs)
}

// agentCommandDir returns the directory used for controller-side shell
// commands such as work_query, scale_check, on_boot, and on_death. These
// commands operate against the canonical rig repository, not an individual
// agent's isolated work_dir.
func agentCommandDir(cityPath string, a *config.Agent, rigs []config.Rig) string {
	if a == nil {
		return cityPath
	}
	if rigName := configuredRigName(cityPath, a, rigs); rigName != "" {
		if rigRoot := rigRootForName(rigName, rigs); rigRoot != "" {
			return rigRoot
		}
	}
	if dir, err := resolveAgentDir(cityPath, a.Dir); err == nil {
		return dir
	}
	return resolveAgentDirPath(cityPath, a.Dir)
}

// passthroughEnv returns environment variables from the parent process that
// agent sessions should inherit. Agents need PATH to find tools (including gc),
// GC_BEADS/GC_DOLT so they use the same bead store as the parent, and
// GC_DOLT_HOST/PORT/USER/PASSWORD so agents can connect to remote Dolt servers.
func passthroughEnv() map[string]string {
	m := make(map[string]string)
	// Pass through PATH and all GC_* environment variables so provider
	// configs (Docker, K8s, beads, dolt, etc.) propagate to agents.
	if v := os.Getenv("PATH"); v != "" {
		m["PATH"] = v
	}
	for _, entry := range os.Environ() {
		if key, val, ok := strings.Cut(entry, "="); ok && strings.HasPrefix(key, "GC_") && val != "" {
			m[key] = val
		}
	}
	// Propagate OTel env vars so agent subprocesses emit telemetry.
	for k, v := range telemetry.OTELEnvMap() {
		m[k] = v
	}
	// Always clear Claude nesting-detection vars so agents don't refuse to
	// start when gc is run from inside a Claude Code session. Set
	// unconditionally so the fingerprint is stable regardless of whether
	// the supervisor or a user shell created the session bead.
	m["CLAUDECODE"] = ""
	m["CLAUDE_CODE_ENTRYPOINT"] = ""
	return m
}

// expandEnvMap returns a copy of m with os.ExpandEnv applied to each value.
// This allows TOML-sourced env blocks to reference the controller's environment,
// e.g. DOLTHUB_TOKEN = "$DOLTHUB_TOKEN".
func expandEnvMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = os.ExpandEnv(v)
	}
	return out
}

// mergeEnv combines multiple env maps into one. Later maps override earlier
// ones for the same key. Returns nil if all inputs are empty.
func mergeEnv(maps ...map[string]string) map[string]string {
	size := 0
	for _, m := range maps {
		size += len(m)
	}
	if size == 0 {
		return nil
	}
	out := make(map[string]string, size)
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// resolveRigForAgent returns the rig name for an agent based on its working
// directory. Returns empty string if the agent is not scoped to any rig.
// Paths are cleaned before comparison to handle trailing slashes and
// redundant separators.
func resolveRigForAgent(workDir string, rigs []config.Rig) string {
	cleanWork := filepath.Clean(workDir)
	for _, r := range rigs {
		if cleanWork == filepath.Clean(r.Path) {
			return r.Name
		}
	}
	return ""
}

// resolveOverlayDir resolves an overlay_dir path relative to cityPath.
// Returns the path unchanged if already absolute, or empty if not set.
func resolveOverlayDir(dir, cityPath string) string {
	if dir == "" || filepath.IsAbs(dir) {
		return dir
	}
	return filepath.Join(cityPath, dir)
}

// imageChecker is implemented by session providers that support pre-checking
// container images (e.g., exec provider for Docker). Providers that don't
// support it simply don't implement this interface — checkAgentImages is a
// no-op for them.
type imageChecker interface {
	CheckImage(image string) error
}

// checkAgentImages verifies that all unique container images referenced by
// agents exist locally. Called once before the reconcile loop to fail fast
// instead of discovering a missing image after N serial start timeouts.
// Returns nil if the provider doesn't support image checking.
func checkAgentImages(sp runtime.Provider, agents []config.Agent, _ io.Writer) error {
	ic, ok := sp.(imageChecker)
	if !ok {
		return nil
	}
	seen := make(map[string]bool)
	for _, a := range agents {
		img := a.Env["GC_DOCKER_IMAGE"]
		if img == "" || seen[img] {
			continue
		}
		seen[img] = true
		if err := ic.CheckImage(img); err != nil {
			return fmt.Errorf("image pre-check: %w", err)
		}
	}
	return nil
}

// countRunningPoolInstances counts how many pool instances are currently
// running for a given pool agent. For bounded pools, checks static names
// (1..max). For unlimited pools, discovers via prefix matching.
//
// Uses ListRunning with the city prefix for a single batch call instead
// of N individual IsRunning calls. For exec providers (K8s), this reduces
// N subprocess spawns to 1.
func countRunningPoolInstances(agentName, agentDir string, pool config.PoolConfig, cityName, sessionTemplate string, sp runtime.Provider) int { //nolint:unparam // agentName varies in production use
	if pool.IsUnlimited() {
		// Unlimited: count by prefix matching.
		instances := discoverPoolInstances(agentName, agentDir, pool, cityName, sessionTemplate, sp)
		count := 0
		for _, qn := range instances {
			sn := sessionName(nil, cityName, qn, sessionTemplate)
			if sp.IsRunning(sn) {
				count++
			}
		}
		return count
	}

	// Bounded: build the set of expected pool instance session names.
	expected := make(map[string]bool, pool.Max)
	for i := 1; i <= pool.Max; i++ {
		instanceName := poolInstanceName(agentName, i, pool)
		qualifiedInstance := instanceName
		if agentDir != "" {
			qualifiedInstance = agentDir + "/" + instanceName
		}
		expected[sessionName(nil, cityName, qualifiedInstance, sessionTemplate)] = true
	}

	// Single ListRunning call, then intersect with expected set.
	// Per-city socket isolation: all sessions belong to this city.
	running, err := sp.ListRunning("")
	if err != nil {
		// Fallback: individual IsRunning calls (original behavior).
		count := 0
		for sn := range expected {
			if sp.IsRunning(sn) {
				count++
			}
		}
		return count
	}

	count := 0
	for _, name := range running {
		if expected[name] {
			count++
		}
	}
	return count
}

// buildFingerprintExtra builds the fpExtra map for an agent's fingerprint
// from its config. Returns nil if no extra fields are present.
func buildFingerprintExtra(a *config.Agent) map[string]string {
	m := make(map[string]string)
	if a.Pool != nil {
		m["pool.min"] = strconv.Itoa(a.Pool.Min)
		m["pool.max"] = strconv.Itoa(a.Pool.Max)
		if a.Pool.Check != "" {
			m["pool.check"] = a.Pool.Check
		}
	}
	if len(a.DependsOn) > 0 {
		m["depends_on"] = strings.Join(a.DependsOn, ",")
	}
	if a.WakeMode != "" && a.WakeMode != "resume" {
		m["wake_mode"] = a.WakeMode
	}
	if len(m) == 0 {
		return nil
	}
	return m
}
