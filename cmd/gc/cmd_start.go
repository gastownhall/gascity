package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
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

func startupSessionName(cityName, agentName, sessionTemplate string) string {
	return agent.SessionNameFor(cityName, agentName, sessionTemplate)
}

func standaloneBuildAgentsFnWithSessionBeads(
	cityName, cityPath string,
	beaconTime time.Time,
	stderr io.Writer,
) func(*config.City, runtime.Provider, beads.Store, map[string]beads.Store, *sessionBeadSnapshot, *sessionReconcilerTraceCycle) DesiredStateResult {
	return func(
		c *config.City,
		currentSP runtime.Provider,
		store beads.Store,
		rigStores map[string]beads.Store,
		sessionBeads *sessionBeadSnapshot,
		trace *sessionReconcilerTraceCycle,
	) DesiredStateResult {
		return buildDesiredStateWithSessionBeads(cityName, cityPath, beaconTime, c, currentSP, store, rigStores, sessionBeads, trace, stderr)
	}
}

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
			names[startupSessionName(cityName, a.QualifiedName(), st)] = true
		}
		return names
	}

	// Individually suspended agents.
	for _, a := range cfg.Agents {
		if a.Suspended {
			qn := a.QualifiedName()
			names[startupSessionName(cityName, qn, st)] = true
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
				names[startupSessionName(cityName, a.QualifiedName(), st)] = true
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
func computePoolSessions(cfg *config.City, cityName, _ string, sp runtime.Provider) map[string]time.Duration {
	ps := make(map[string]time.Duration)
	st := cfg.Workspace.SessionTemplate
	for _, a := range cfg.Agents {
		sp0 := scaleParamsFor(&a)
		if !a.SupportsInstanceExpansion() {
			continue
		}
		timeout := a.DrainTimeoutDuration()
		for _, qualifiedInstance := range discoverPoolInstances(a.Name, a.Dir, sp0, &a, cityName, st, sp) {
			ps[startupSessionName(cityName, qualifiedInstance, st)] = timeout
		}
	}
	return ps
}

// poolDeathInfo holds the pre-expanded on_death command and working
// directory for a pool instance.
type poolDeathInfo struct {
	Command string            // on_death shell command pre-expanded for the instance
	Dir     string            // working directory for bd commands
	Env     map[string]string // canonical runtime env for the agent scope
}

// computePoolDeathHandlers builds a map from session name to death handler
// for every pool instance (static for bounded pools, currently running for
// unlimited). Used to detect and handle pool deaths.
func computePoolDeathHandlers(cfg *config.City, cityName, cityPath string, sp runtime.Provider, stderr io.Writer) map[string]poolDeathInfo {
	handlers := make(map[string]poolDeathInfo)
	st := cfg.Workspace.SessionTemplate
	for _, a := range cfg.Agents {
		sp0 := scaleParamsFor(&a)
		if !a.SupportsInstanceExpansion() {
			continue
		}
		agentEnv := controllerQueryRuntimeEnv(cityPath, cfg, &a)
		for _, qualifiedInstance := range discoverPoolInstances(a.Name, a.Dir, sp0, &a, cityName, st, sp) {
			_, instanceName := config.ParseQualifiedName(qualifiedInstance)
			instance := deepCopyAgent(&a, instanceName, a.Dir)
			cmd := instance.EffectiveOnDeath()
			if cmd == "" {
				continue
			}
			cmd = expandAgentCommandTemplate(cityPath, cityName, &instance, cfg.Rigs, "on_death", cmd, stderr)
			dir := agentCommandDir(cityPath, &a, cfg.Rigs)
			sn := startupSessionName(cityName, qualifiedInstance, st)
			handlers[sn] = poolDeathInfo{Command: cmd, Dir: dir, Env: agentEnv}
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
func buildIdleTracker(cfg *config.City, cityName, _ string, sp runtime.Provider) idleTracker {
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
	var registeredAny bool
	for _, a := range cfg.Agents {
		timeout := a.IdleTimeoutDuration()
		if timeout <= 0 {
			continue
		}
		named := config.FindNamedSession(cfg, a.QualifiedName())
		if named != nil {
			// Configured named sessions own the canonical runtime session for
			// direct configured identities. mode="always" must never be subject
			// to idle timeout.
			if named.ModeOrDefault() != "always" {
				it.setTimeout(config.NamedSessionRuntimeName(cityName, cfg.Workspace, a.QualifiedName()), timeout)
				registeredAny = true
			}
			if !a.SupportsInstanceExpansion() {
				continue
			}
		}
		sp0 := scaleParamsFor(&a)
		if a.SupportsInstanceExpansion() {
			// Register each pool instance (worker-1, worker-2, ...).
			for _, qualifiedInstance := range discoverPoolInstances(a.Name, a.Dir, sp0, &a, cityName, st, sp) {
				sn := startupSessionName(cityName, qualifiedInstance, st)
				it.setTimeout(sn, timeout)
				registeredAny = true
			}
			continue
		}
		sn := startupSessionName(cityName, a.QualifiedName(), st)
		it.setTimeout(sn, timeout)
		registeredAny = true
	}
	if !registeredAny {
		return nil
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
	return doStartWithNameOverride(args, controllerMode, stdout, stderr, "")
}

func doStartWithNameOverride(args []string, controllerMode bool, stdout, stderr io.Writer, nameOverride string) int {
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
	if code := registerCityWithSupervisorNamed(cityPath, nameOverride, stdout, stderr, "gc start", true); code != 0 {
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
	ctx, err := resolveContextFromPath(dir)
	if err != nil {
		absDir, absErr := filepath.Abs(dir)
		if absErr == nil {
			return "", fmt.Errorf("%w; run \"gc init %s\" first", err, absDir)
		}
		return "", fmt.Errorf("%w; run \"gc init\" first", err)
	}
	cityPath := ctx.CityPath
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

	allIncludes := make([]string, 0, len(extraConfigFiles)+3)
	allIncludes = append(allIncludes, extraConfigFiles...)
	allIncludes = append(allIncludes, builtinPackIncludes(cityPath)...)
	cfg, prov, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"), allIncludes...)
	if err != nil {
		fmt.Fprintf(stderr, "gc start: %v\n", err)                      //nolint:errcheck // best-effort stderr
		fmt.Fprintln(stderr, "hint: run \"gc doctor\" for diagnostics") //nolint:errcheck // best-effort stderr
		return 1
	}
	applyFeatureFlags(cfg)
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
	if err := config.ValidateRigs(cfg.Rigs, config.EffectiveHQPrefix(cfg)); err != nil {
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

	// Materialize builtin packs (bd + dolt) so doctor checks, commands,
	// and the bd pack's gc-beads-bd script are available.
	if err := MaterializeBuiltinPacks(cityPath); err != nil {
		fmt.Fprintf(stderr, "gc start: materializing builtin packs: %v\n", err) //nolint:errcheck // best-effort stderr
		// Non-fatal: only needed if provider = "bd".
	}
	// Built-in prompts and formulas now arrive via the core bootstrap pack.
	ensureInitArtifacts(cityPath, cfg, stderr, "gc start")

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

	// Materialize formula symlinks before agent startup.
	// System formulas/orders now arrive via the core bootstrap pack.
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

	// Materialize script symlinks before agent startup.
	if len(cfg.ScriptLayers.City) > 0 {
		if err := ResolveScripts(cityPath, cfg.ScriptLayers.City); err != nil {
			fmt.Fprintf(stderr, "gc start: city scripts: %v\n", err) //nolint:errcheck // best-effort stderr
		}
	}
	for _, r := range cfg.Rigs {
		if layers, ok := cfg.ScriptLayers.Rigs[r.Name]; ok && len(layers) > 0 {
			if err := ResolveScripts(r.Path, layers); err != nil {
				fmt.Fprintf(stderr, "gc start: rig %q scripts: %v\n", r.Name, err) //nolint:errcheck // best-effort stderr
			}
		}
	}

	// Validate agents.
	if err := config.ValidateAgents(cfg.Agents); err != nil {
		fmt.Fprintf(stderr, "gc start: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Skill collision validator — hard gate. Two agents sharing a
	// (scope-root, vendor) sink cannot both provide an agent-local
	// skill under the same name; the materialiser below would write
	// conflicting symlinks. Block start so the operator fixes the
	// collision before any half-written sink state lands. Per
	// engdocs/proposals/skill-materialization.md § "Collision
	// validation (startup validator)".
	if err := checkSkillCollisions(cfg, cityPath); err != nil {
		fmt.Fprintf(stderr, "gc start: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Stage-1 skill materialization — runs for every eligible agent
	// at its scope root before sessions spawn. Non-fatal: per-agent
	// errors are logged inline by runStage1SkillMaterialization
	// itself; it never returns a non-nil error to its caller.
	_ = runStage1SkillMaterialization(cityPath, cfg, stderr)

	// Stage-1 MCP projection is a hard gate because it mutates the provider's
	// active runtime config surface. Conflicting shared targets or projection
	// write failures must block startup before sessions launch against stale or
	// ambiguous MCP state.
	if err := runStage1MCPProjection(cityPath, cfg, exec.LookPath, stderr); err != nil {
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
	buildAgents := func(c *config.City, currentSP runtime.Provider, store beads.Store) DesiredStateResult {
		return buildDesiredState(cityName, cityPath, beaconTime, c, currentSP, store, stderr)
	}
	buildAgentsWithSessionBeads := standaloneBuildAgentsFnWithSessionBeads(cityName, cityPath, beaconTime, stderr)

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
		printDryRunPreview(agents.State, cfg, cityName, stdout)
		return 0
	}

	tomlPath := filepath.Join(cityPath, "city.toml")
	if controllerMode {
		poolSessions := computePoolSessions(cfg, cityName, cityPath, sp)
		poolDeathHandlers := computePoolDeathHandlers(cfg, cityName, cityPath, sp, stderr)
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

	runPoolOnBoot(cfg, cityPath, shellRunHook, stderr)

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
	dsResult := buildDesiredStateWithSessionBeads(cityName, cityPath, beaconTime, cfg, sp, oneShotStore, nil, sessionBeads, nil, stderr)
	ds := dsResult.State
	cfgNames := configuredSessionNamesWithSnapshot(cfg, cityName, sessionBeads)
	_, sessionBeads = syncSessionBeadsWithSnapshot(
		cityPath, oneShotStore, ds, sp, cfgNames, cfg, clock.Real{}, stderr, true, sessionBeads,
	)

	open := sessionBeads.Open()
	dt := newDrainTracker()
	poolDesired := PoolDesiredCounts(ComputePoolDesiredStates(
		cfg, nil, sessionBeads.Open(), dsResult.ScaleCheckCounts))
	if poolDesired == nil {
		poolDesired = make(map[string]int)
	}
	mergeNamedSessionDemand(poolDesired, dsResult.NamedSessionDemand, cfg)
	reconcileSessionBeadsAtPath(
		sigCtx, cityPath, open, ds, cfgNames, cfg, sp, oneShotStore,
		nil, nil, nil, nil, dt, poolDesired,
		dsResult.StoreQueryPartial,
		nil, cityName,
		nil, clock.Real{}, recorder, cfg.Session.StartupTimeoutDuration(), 0,
		stdout, stderr,
	)

	// Post-reconcile sync: update bead state to reflect post-start reality.
	sessionBeads, err = loadSessionBeadSnapshot(oneShotStore)
	if err != nil {
		fmt.Fprintf(stderr, "gc start: loading session beads: %v\n", err) //nolint:errcheck
		sessionBeads = nil
	}
	dsResult = buildDesiredStateWithSessionBeads(cityName, cityPath, beaconTime, cfg, sp, oneShotStore, nil, sessionBeads, nil, stderr)
	ds = dsResult.State
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
// if settings.json exists for this city. Uses the absolute city-root path so
// it resolves correctly regardless of the session's working directory. The K8s
// provider remaps city-root references to /workspace automatically.
// Returns empty string for non-Claude providers or if no settings file is present.
//
// Note: this uses Stat-level existence only. It does NOT verify the file is
// readable. Use settingsArgsIfReadable in best-effort fallback paths where
// pointing Claude at an unreadable file would be worse than no --settings.
func settingsArgs(cityPath, providerName string) string {
	if providerName != "claude" {
		return ""
	}
	settingsPath, _ := claudeSettingsSource(cityPath)
	if settingsPath == "" {
		return ""
	}
	return fmt.Sprintf("--settings %q", settingsPath)
}

// settingsArgsIfReadable is the stricter variant used by best-effort fallback
// paths (e.g. buildResumeCommand on projection failure). It returns "--settings
// <path>" only if the discovered file is actually readable — not just present.
// This prevents `gc session attach` from pointing Claude at a 0o000 or
// otherwise-unreadable .gc/settings.json that a failed projection could not
// repair this tick.
func settingsArgsIfReadable(cityPath, providerName string) string {
	if providerName != "claude" {
		return ""
	}
	settingsPath, _ := claudeSettingsSource(cityPath)
	if settingsPath == "" {
		return ""
	}
	if _, err := os.ReadFile(settingsPath); err != nil {
		return ""
	}
	return fmt.Sprintf("--settings %q", settingsPath)
}

// ensureClaudeSettingsArgs projects managed Claude settings to
// .gc/settings.json (idempotent: no-op when bytes match) and returns the
// "--settings <path>" arg for the resolved Claude command. This is the
// single chokepoint that guarantees every Claude launch path — reconciler
// or session attach/submit — sees the projected file before settingsArgs
// probes for it. Returns empty string and nil error for non-Claude providers.
//
// Returns a non-nil error when projection fails. Strict callers
// (resolveTemplate) should propagate so that a malformed preferred override
// fails loudly at agent creation rather than silently running with stale
// bytes from a prior tick. Best-effort callers (buildResumeCommand) may
// choose to log-and-continue so a `gc session attach` still succeeds when
// projection is transiently broken.
//
// fs may be nil; in that case OSFS is used. stderr may be nil; in that
// case projection errors are only returned, not written.
func ensureClaudeSettingsArgs(fs fsys.FS, cityPath, providerName string, stderr io.Writer) (string, error) {
	if providerName != "claude" || cityPath == "" {
		return "", nil
	}
	if fs == nil {
		fs = fsys.OSFS{}
	}
	if stderr == nil {
		stderr = io.Discard
	}
	if err := hooks.Install(fs, cityPath, cityPath, []string{"claude"}); err != nil {
		fmt.Fprintf(stderr, "claude hooks: %v\n", err) //nolint:errcheck // best-effort stderr
		return "", fmt.Errorf("projecting Claude settings: %w", err)
	}
	return settingsArgs(cityPath, providerName), nil
}

func claudeSettingsSource(cityPath string) (src, rel string) {
	candidates := []struct {
		src string
		rel string
	}{
		{src: filepath.Join(cityPath, ".gc", "settings.json"), rel: path.Join(".gc", "settings.json")},
		{src: citylayout.ClaudeHookFilePath(cityPath), rel: path.Clean(strings.ReplaceAll(citylayout.ClaudeHookFile, string(filepath.Separator), "/"))},
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate.src); err == nil {
			return candidate.src, candidate.rel
		}
	}
	return "", ""
}

// stageHookFiles adds hook files to the copy_files list so container
// providers (K8s) can stage them into pods. Docker doesn't need this
// (bind-mount), but the extra entries are harmless.
//
// Claude's city-level .gc/settings.json is staged here because settingsArgs
// points --settings at the city-root path. All other provider hook files
// ship via the core pack overlay and flow through PackOverlayDirs staging,
// so they are not handled here.
func stageHookFiles(copyFiles []runtime.CopyEntry, cityPath, workDir string) []runtime.CopyEntry {
	// Compute the relative path from cityPath to workDir so that
	// container-side RelDst places files under the agent's WorkingDir
	// (/workspace/<relWorkDir>/), not always at /workspace/.
	// When workDir == cityPath, relWorkDir is "." and path.Join collapses it.
	relWorkDir := "."
	if workDir != cityPath {
		if r, err := filepath.Rel(cityPath, workDir); err == nil {
			relWorkDir = r
		}
	}

	// workDir-based hooks: gemini, codex, opencode, copilot, cursor, pi, omp.
	for _, rel := range []string{
		path.Join(".gemini", "settings.json"),
		path.Join(".codex", "hooks.json"),
		path.Join(".opencode", "plugins", "gascity.js"),
		path.Join(".github", "hooks", "gascity.json"),
		path.Join(".github", "copilot-instructions.md"),
		path.Join(".cursor", "hooks.json"),
		path.Join(".pi", "extensions", "gc-hooks.js"),
		path.Join(".omp", "hooks", "gc-hook.ts"),
	} {
		abs := filepath.Join(workDir, rel)
		if _, err := os.Stat(abs); err == nil {
			copyFiles = append(copyFiles, runtime.CopyEntry{
				Src: abs, RelDst: path.Join(relWorkDir, rel),
				Probed: true, ContentHash: runtime.HashPathContent(abs),
			})
		}
	}

	// Stage Claude skills directory (if materialized).
	skillsDir := filepath.Join(workDir, ".claude", "skills")
	if info, err := os.Stat(skillsDir); err == nil && info.IsDir() {
		copyFiles = append(copyFiles, runtime.CopyEntry{
			Src: skillsDir, RelDst: path.Join(relWorkDir, ".claude", "skills"),
			Probed: true, ContentHash: runtime.HashPathContent(skillsDir),
		})
	}
	// cityDir-based hooks: claude (.gc/settings.json).
	// Skip if settingsArgs already added it.
	// These are city-root relative, so no relWorkDir prefix needed.
	settingsAbs, settingsRel := claudeSettingsSource(cityPath)
	if settingsAbs != "" {
		alreadyStaged := false
		for _, cf := range copyFiles {
			if cf.RelDst == settingsRel {
				alreadyStaged = true
				break
			}
		}
		if !alreadyStaged {
			copyFiles = append(copyFiles, runtime.CopyEntry{
				Src: settingsAbs, RelDst: settingsRel,
				Probed: true, ContentHash: runtime.HashPathContent(settingsAbs),
			})
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

func resolveConfiguredWorkDir(cityPath, cityName, qualifiedName string, a *config.Agent, rigs []config.Rig) (string, error) {
	if a == nil {
		return resolveAgentDir(cityPath, "")
	}
	if strings.TrimSpace(qualifiedName) == "" {
		qualifiedName = a.QualifiedName()
	}
	workDir, err := workdirutil.ResolveWorkDirPathStrict(cityPath, cityName, qualifiedName, *a, rigs)
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
			return resolveAgentDirPath(cityPath, rigRoot)
		}
	}
	if dir, err := resolveAgentDir(cityPath, a.Dir); err == nil {
		return dir
	}
	return resolveAgentDirPath(cityPath, a.Dir)
}

// passthroughEnv returns environment variables from the parent process that
// agent sessions should inherit. Agents need PATH to find tools (including gc),
// GC_BEADS/GC_DOLT so they use the same bead store as the parent,
// GC_DOLT_HOST/PORT/USER/PASSWORD so agents can connect to remote Dolt servers,
// and Claude auth/home context so managed sessions can launch reliably under
// shell and supervisor-driven flows.
func passthroughEnv() map[string]string {
	m := make(map[string]string)
	// Pass through PATH so managed sessions can find tools, and preserve the
	// minimum user/home context Claude Code needs to resolve stored credentials.
	if v := os.Getenv("PATH"); v != "" {
		m["PATH"] = v
	}
	if v := os.Getenv("HOME"); v != "" {
		m["HOME"] = v
	}
	// USER/LOGNAME are required on macOS for Keychain access — without them
	// providers like Claude Code cannot read stored OAuth credentials.
	// CLAUDE_CONFIG_DIR and CLAUDE_CODE_OAUTH_TOKEN let managed Claude
	// sessions find stored credentials and token-based auth.
	for _, key := range []string{"USER", "LOGNAME", "CLAUDE_CONFIG_DIR", "CLAUDE_CODE_OAUTH_TOKEN"} {
		if v := os.Getenv(key); v != "" {
			m[key] = v
		}
	}
	// XDG directories are needed for providers to locate config files
	// (e.g. ~/.config/opencode/opencode.jsonc). When not set, compute
	// defaults from HOME so spawned sessions always find user config.
	if v := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); v != "" {
		m["XDG_CONFIG_HOME"] = v
	} else if home := os.Getenv("HOME"); home != "" {
		m["XDG_CONFIG_HOME"] = filepath.Join(home, ".config")
	}
	if v := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); v != "" {
		m["XDG_STATE_HOME"] = v
	} else if home := os.Getenv("HOME"); home != "" {
		m["XDG_STATE_HOME"] = filepath.Join(home, ".local", "state")
	}
	// Pass through all GC_* and ANTHROPIC_* vars. Agent credentials are
	// included in the global baseline because the SDK cannot know which
	// agent uses which provider (zero hardcoded roles); the trust boundary
	// is the managed session itself.
	for _, entry := range os.Environ() {
		key, val, ok := strings.Cut(entry, "=")
		if !ok || val == "" {
			continue
		}
		if strings.HasPrefix(key, "GC_") || strings.HasPrefix(key, "ANTHROPIC_") {
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
func countRunningPoolInstances(agentName, agentDir string, sp0 scaleParams, a *config.Agent, cityName, sessionTemplate string, sp runtime.Provider) int { //nolint:unparam // agentName varies in production use
	isUnlimited := sp0.Max < 0
	if isUnlimited {
		// Unlimited: count by prefix matching.
		instances := discoverPoolInstances(agentName, agentDir, sp0, a, cityName, sessionTemplate, sp)
		count := 0
		for _, qn := range instances {
			sn := sessionName(nil, cityName, qn, sessionTemplate)
			if running, err := workerSessionTargetRunningWithConfig("", nil, sp, nil, sn); err == nil && running {
				count++
			}
		}
		return count
	}

	// Bounded: build the set of expected pool instance session names.
	expected := make(map[string]bool, sp0.Max)
	for i := 1; i <= sp0.Max; i++ {
		instanceName := poolInstanceName(agentName, i, a)
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
			if running, err := workerSessionTargetRunningWithConfig("", nil, sp, nil, sn); err == nil && running {
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
//
// Note on pool.check omission: the default EffectiveScaleCheck string bakes
// the agent's QualifiedName into the shell expression. Different code paths
// in buildDesiredState resolve the same session bead with sometimes a base
// agent ("pool-name") and sometimes a deep-copied instance agent
// ("pool-name-1"), producing different pool.check strings and a different
// fingerprint for the same session bead on different ticks. The constant
// oscillation drives config-drift drain on every live pool/named session
// (minutes-into-work reaps — see gascity ga-00f). scale_check is a runtime
// probe for demand, not a behavioral-identity field; changes to ScaleCheck
// don't need to reap live sessions. pool.min / pool.max / depends_on /
// wake_mode continue to contribute since those are genuinely identity.
func buildFingerprintExtra(a *config.Agent) map[string]string {
	m := make(map[string]string)
	if a.MinActiveSessions != nil || a.MaxActiveSessions != nil || a.ScaleCheck != "" || a.DrainTimeout != "" {
		sp := scaleParamsFor(a)
		m["pool.min"] = strconv.Itoa(sp.Min)
		m["pool.max"] = strconv.Itoa(sp.Max)
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
