// gc is the Gas City CLI — an orchestration-builder for multi-agent workflows.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	beadsexec "github.com/gastownhall/gascity/internal/beads/exec"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/supervisor"
	"github.com/gastownhall/gascity/internal/telemetry"
	"github.com/spf13/cobra"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// errExit is a sentinel error returned by cobra RunE functions to signal
// non-zero exit. The command has already written its own error to stderr.
var errExit = errors.New("exit")

type commandExitError struct {
	code int
}

func (e *commandExitError) Error() string {
	if e == nil {
		return "exit"
	}
	return fmt.Sprintf("exit %d", e.code)
}

func (e *commandExitError) ExitCode() int {
	if e == nil || e.code == 0 {
		return 1
	}
	return e.code
}

func exitForCode(code int) error {
	if code == 0 {
		return nil
	}
	if code == 1 {
		return errExit
	}
	return &commandExitError{code: code}
}

func commandExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr interface{ ExitCode() int }
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	if errors.Is(err, errExit) {
		return 1
	}
	return 1
}

// cityFlag holds the value of the --city persistent flag.
// Empty means "discover from cwd."
var cityFlag string

// rigFlag holds the value of the --rig persistent flag.
// Empty means "discover from cwd or omit."
var rigFlag string

// run executes the gc CLI with the given args, writing output to stdout and
// errors to stderr. Returns the exit code.
func run(args []string, stdout, stderr io.Writer) int {
	prevCityFlag, prevRigFlag := cityFlag, rigFlag
	cityFlag, rigFlag = "", ""
	defer func() {
		cityFlag = prevCityFlag
		rigFlag = prevRigFlag
	}()

	// Initialize OTel telemetry (opt-in via GC_OTEL_METRICS_URL / GC_OTEL_LOGS_URL).
	provider, err := telemetry.Init(context.Background(), "gascity", version)
	if err != nil {
		fmt.Fprintf(stderr, "gc: telemetry init: %v\n", err) //nolint:errcheck // best-effort stderr
	}
	if provider != nil {
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = provider.Shutdown(ctx)
		}()
		telemetry.SetProcessOTELAttrs()
	}

	root := newRootCmd(stdout, stderr)
	if args == nil {
		args = []string{}
	}
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	if err := root.Execute(); err != nil {
		return commandExitCode(err)
	}
	return 0
}

// newRootCmd creates the root cobra command with all subcommands.
func newRootCmd(stdout, stderr io.Writer) *cobra.Command {
	root := &cobra.Command{
		Use:           "gc",
		Short:         "Gas City CLI — orchestration-builder for multi-agent workflows",
		SilenceErrors: true,
		SilenceUsage:  true,
		Args:          cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			// Lazy fallback: if eager discovery missed a pack command
			// (e.g. config changed after binary started), try one more time.
			if tryPackCommandFallback(args, stdout, stderr) {
				return nil
			}
			fmt.Fprintf(stderr, "gc: unknown command %q\n\n", args[0]) //nolint:errcheck // best-effort stderr
			printCommandUsage(stderr, cmd)
			return errExit
		},
	}
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		printCommandUsageError(stderr, cmd, err)
		return errExit
	})
	root.PersistentFlags().StringVar(&cityFlag, "city", "",
		"path to the city directory (default: walk up from cwd)")
	root.PersistentFlags().StringVar(&rigFlag, "rig", "",
		"rig name or path (default: discover from cwd)")
	root.AddCommand(
		newStartCmd(stdout, stderr),
		newInitCmd(stdout, stderr),
		newReloadCmd(stdout, stderr),
		newStopCmd(stdout, stderr),
		newRestartCmd(stdout, stderr),
		newStatusCmd(stdout, stderr),
		newServiceCmd(stdout, stderr),
		newSuspendCmd(stdout, stderr),
		newResumeCmd(stdout, stderr),
		newRigCmd(stdout, stderr),
		newMailCmd(stdout, stderr),
		newNudgeCmd(stdout, stderr),
		newWaitCmd(stdout, stderr),
		newAgentCmd(stdout, stderr),
		newEventCmd(stdout, stderr),
		newEventsCmd(stdout, stderr),
		newTraceCmd(stdout, stderr),
		newOrderCmd(stdout, stderr),
		newImportCmd(stdout, stderr),
		newConfigCmd(stdout, stderr),
		newPackCmd(stdout, stderr),
		newDoctorCmd(stdout, stderr),
		newHookCmd(stdout, stderr),
		newSlingCmd(stdout, stderr),
		newConvoyCmd(stdout, stderr),
		newWispCmd(stdout, stderr),
		newPrimeCmd(stdout, stderr),
		newHandoffCmd(stdout, stderr),
		newBeadsCmd(stdout, stderr),
		newBuildImageCmd(stdout, stderr),
		newSkillCmd(stdout, stderr),
		newMcpCmd(stdout, stderr),
		newInternalCmd(stdout, stderr),
		newVersionCmd(stdout),
		newDashboardCmd(stdout, stderr),
		newGraphCmd(stdout, stderr),
		newRegisterCmd(stdout, stderr),
		newUnregisterCmd(stdout, stderr),
		newCitiesCmd(stdout, stderr),
		newSupervisorCmd(stdout, stderr),
		newSessionCmd(stdout, stderr),
		newConvergeCmd(stdout, stderr),
		newWorkflowCmd(stdout, stderr),
		newRuntimeCmd(stdout, stderr),
		newFormulaCmd(stdout, stderr),
		newBdCmd(stdout, stderr),
		newBdStoreBridgeCmd(stdout, stderr),
		newDoltConfigCmd(stdout, stderr),
		newDoltStateCmd(stdout, stderr),
		newShellCmd(stdout, stderr),
	)
	// gen-doc needs the root command to walk the tree; add after construction.
	root.AddCommand(newGenDocCmd(stdout, stderr, root))

	// Best-effort: discover pack CLI commands if we're inside a city.
	registerPackCommands(root, stdout, stderr)

	installArgUsageErrors(root, stderr)

	return root
}

func installArgUsageErrors(cmd *cobra.Command, stderr io.Writer) {
	if cmd.Args != nil {
		argsValidator := cmd.Args
		cmd.Args = func(cmd *cobra.Command, args []string) error {
			if err := argsValidator(cmd, args); err != nil {
				printCommandUsageError(stderr, cmd, err)
				return errExit
			}
			return nil
		}
	}
	for _, child := range cmd.Commands() {
		installArgUsageErrors(child, stderr)
	}
}

func printCommandUsageError(stderr io.Writer, cmd *cobra.Command, err error) {
	if err != nil {
		fmt.Fprintf(stderr, "gc: %v\n\n", err) //nolint:errcheck // best-effort stderr
	}
	printCommandUsage(stderr, cmd)
}

func printCommandUsage(stderr io.Writer, cmd *cobra.Command) {
	if cmd == nil {
		return
	}
	usage := strings.TrimRight(cmd.UsageString(), "\n")
	if usage == "" {
		return
	}
	fmt.Fprintln(stderr, usage) //nolint:errcheck // best-effort stderr
}

// sessionName returns the session name for a city agent.
// When a bead store is provided, it looks up the session bead first;
// otherwise falls back to the legacy SessionNameFor function.
// sessionTemplate is a Go text/template string (empty = default pattern).
//
// When running inside a container (Docker/K8s), the tmux session has a
// fixed name ("agent" or "main") that differs from the controller's
// session name. GC_TMUX_SESSION overrides the resolved name so agent-side
// commands (drain-check, drain-ack, request-restart) target the correct
// tmux session for metadata reads/writes.
func sessionName(store beads.Store, cityName, agentName, sessionTemplate string) string {
	if override := os.Getenv("GC_TMUX_SESSION"); override != "" {
		return override
	}
	return lookupSessionNameOrLegacy(store, cityName, agentName, sessionTemplate)
}

// cliStoreCache caches the bead store for CLI commands that call
// cliSessionName repeatedly with the same cityPath. This avoids
// opening the store on every call in loops over agents.
//
// Thread safety: CLI commands are single-threaded (cobra runs one command
// at a time). Tests that call cliSessionName should use resetCliStoreCache
// in cleanup to prevent state leaking between tests.
var cliStoreCache struct {
	mu    sync.Mutex
	path  string
	store beads.Store
}

// cliSessionName resolves a session name for CLI commands that don't already
// have a store open. Caches the bead store per cityPath so loops over
// agents don't open the store repeatedly. Silently falls back to legacy
// naming if the store is unavailable.
func cliSessionName(cityPath, cityName, agentName, sessionTemplate string) string {
	if strings.TrimSpace(cityPath) == "" {
		return sessionName(nil, cityName, agentName, sessionTemplate)
	}
	cliStoreCache.mu.Lock()
	if cliStoreCache.path != cityPath {
		cliStoreCache.store, _ = openCityStoreAt(cityPath)
		cliStoreCache.path = cityPath
	}
	store := cliStoreCache.store
	cliStoreCache.mu.Unlock()
	return sessionName(store, cityName, agentName, sessionTemplate)
}

// resolvedContext holds the result of city+rig resolution.
type resolvedContext struct {
	CityPath string // absolute path to city root
	RigName  string // rig name (empty if not in a rig context)
}

// resolveCommandContext resolves city+rig context for commands that accept an
// optional path argument. With no args, it uses the full flag/env/cwd resolver.
// With a path arg, it treats that path as either a city path or a rig path and
// resolves the containing city via the rig registry before falling back to
// walking up for city.toml.
func resolveCommandContext(args []string) (resolvedContext, error) {
	if len(args) == 0 {
		return resolveContext()
	}
	return resolveContextFromPath(args[0])
}

func resolveCommandCity(args []string) (string, error) {
	ctx, err := resolveCommandContext(args)
	if err != nil {
		return "", err
	}
	return ctx.CityPath, nil
}

// resolveContext resolves the city and optional rig context using the
// following priority chain:
//  1. --city + --rig flags (explicit both, validated)
//  2. --city only (explicit city, rig from cwd if applicable)
//  3. --rig only (rig from registered city site bindings)
//  4. Explicit city env (GC_CITY / GC_CITY_PATH / GC_CITY_ROOT) + GC_RIG
//  5. Explicit city env only (city set, rig from GC_DIR/cwd if applicable)
//  6. GC_RIG only (rig from registered city site bindings)
//  7. GC_DIR-derived city path
//  8. Registered rig binding lookup (cwd prefix match)
//  9. Walk up from cwd looking for city.toml
//  10. Fail
func resolveContext() (resolvedContext, error) {
	city := cityFlag
	rig := rigFlag
	gcRig := os.Getenv("GC_RIG")

	// Step 1: --city + --rig
	if city != "" && rig != "" {
		cp, err := validateCityPath(city)
		if err != nil {
			return resolvedContext{}, err
		}
		return resolvedContext{CityPath: cp, RigName: rig}, nil
	}

	// Step 2: --city only
	if city != "" {
		cp, err := validateCityPath(city)
		if err != nil {
			return resolvedContext{}, err
		}
		rn := rigFromCwd(cp)
		return resolvedContext{CityPath: cp, RigName: rn}, nil
	}

	// Step 3: --rig only
	if rig != "" {
		ctx, err := resolveRigToContext(rig)
		if err != nil {
			return resolvedContext{}, err
		}
		return ctx, nil
	}

	// Step 4: explicit city env + GC_RIG
	if gcCity, ok := resolveExplicitCityPathEnv(); ok && gcRig != "" {
		return resolvedContext{CityPath: gcCity, RigName: gcRig}, nil
	}

	// Step 5: explicit city env only
	if gcCity, ok := resolveExplicitCityPathEnv(); ok {
		rn := rigFromGCDirOrCwd(gcCity)
		return resolvedContext{CityPath: gcCity, RigName: rn}, nil
	}

	// Step 6: GC_RIG only
	if gcRig != "" {
		ctx, err := resolveRigToContext(gcRig)
		if err != nil {
			return resolvedContext{}, err
		}
		return ctx, nil
	}

	// Step 7: GC_DIR-derived city path.
	if gcDirCity, ok := resolveCityPathFromGCDir(); ok {
		rn := rigFromCwdDir(gcDirCity, strings.TrimSpace(os.Getenv("GC_DIR")))
		return resolvedContext{CityPath: gcDirCity, RigName: rn}, nil
	}

	// Step 8: Registered rig binding lookup (cwd prefix match).
	cwd, err := os.Getwd()
	if err != nil {
		return resolvedContext{}, err
	}
	if ctx, ok := lookupRigFromCwd(cwd); ok {
		return ctx, nil
	}

	// Step 9: Walk up from cwd looking for city.toml.
	cityPath, err := findCity(cwd)
	if err != nil {
		return resolvedContext{}, err
	}
	rn := rigFromCwdDir(cityPath, cwd)
	return resolvedContext{CityPath: cityPath, RigName: rn}, nil
}

// resolveCity returns the city root path. Thin wrapper over resolveContext
// for the many callers that only need the city path.
func resolveCity() (string, error) {
	return resolveCommandCity(nil)
}

func resolveContextFromPath(path string) (resolvedContext, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return resolvedContext{}, err
	}
	ctx, ok, err := resolveRigPathToContext(abs)
	if err != nil {
		return resolvedContext{}, err
	}
	if ok {
		return ctx, nil
	}
	if cityPath, err := validateCityPath(abs); err == nil {
		return resolvedContext{
			CityPath: cityPath,
			RigName:  rigFromCwdDir(cityPath, abs),
		}, nil
	}
	cityPath, err := findCity(abs)
	if err != nil {
		return resolvedContext{}, err
	}
	return resolvedContext{
		CityPath: cityPath,
		RigName:  rigFromCwdDir(cityPath, abs),
	}, nil
}

// validateCityPath resolves and validates a path as a city directory.
func validateCityPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if citylayout.HasCityConfig(abs) || citylayout.HasRuntimeRoot(abs) {
		return abs, nil
	}
	return "", fmt.Errorf("not a city directory: %s (no city.toml or .gc/ found)", abs)
}

// resolveRigToContext resolves a rig name or path to a full context by scanning
// registered cities and their machine-local .gc/site.toml rig bindings. This
// is an explicit rig-resolution path, so stale-sibling warnings are emitted
// to os.Stderr (deduped across the two registry scans below).
func resolveRigToContext(nameOrPath string) (resolvedContext, error) {
	var allStale []staleRegisteredCity
	defer func() { emitStaleRegisteredCityWarnings(os.Stderr, allStale) }()

	matches, stale, err := registeredRigBindingsByName(nameOrPath, true)
	allStale = append(allStale, stale...)
	if err != nil {
		return resolvedContext{}, err
	}
	if len(matches) > 0 {
		return resolveRigBindingMatches(nameOrPath, matches)
	}

	abs, err := filepath.Abs(nameOrPath)
	if err != nil {
		return resolvedContext{}, fmt.Errorf("rig %q: %w", nameOrPath, err)
	}
	matches, stale, err = registeredRigBindingsByPath(abs, true)
	allStale = append(allStale, stale...)
	if err != nil {
		return resolvedContext{}, err
	}
	if len(matches) > 0 {
		return resolveRigBindingMatches(abs, matches)
	}

	return resolvedContext{}, fmt.Errorf("rig %q is not registered in any city", nameOrPath)
}

// resolveRigPathToContext resolves an explicit path argument to a registered
// rig context. Stale-sibling warnings are emitted to os.Stderr because the
// caller is explicitly depending on the registry.
func resolveRigPathToContext(dir string) (resolvedContext, bool, error) {
	matches, stale, err := registeredRigBindingsByPath(dir, true)
	emitStaleRegisteredCityWarnings(os.Stderr, stale)
	if err != nil {
		return resolvedContext{}, false, err
	}
	if len(matches) == 0 {
		return resolvedContext{}, false, nil
	}
	ctx, err := resolveRigBindingMatches(dir, matches)
	if err != nil {
		return resolvedContext{}, true, err
	}
	return ctx, true, nil
}

// lookupRigFromCwd checks registered city site bindings for a rig matching cwd.
// Ambiguous bindings deliberately fall through to the city walk-up fallback.
// This is an opportunistic probe (failOnLoadError=false): stale-sibling
// warnings are intentionally dropped so unrelated commands stay quiet.
func lookupRigFromCwd(cwd string) (resolvedContext, bool) {
	matches, _, err := registeredRigBindingsByPath(cwd, false)
	if err != nil || len(matches) != 1 {
		return resolvedContext{}, false
	}
	return resolvedContext{CityPath: matches[0].City.Path, RigName: matches[0].Rig.Name}, true
}

// rigFromCwd attempts to derive a rig name from cwd when the city is known.
func rigFromCwd(cityPath string) string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return rigFromCwdDir(cityPath, cwd)
}

// rigFromCwdDir matches cwd against registered rigs in a city's config.
func rigFromCwdDir(cityPath, cwd string) string {
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		return ""
	}
	rig, ok := rigForDir(cfg, cityPath, cwd)
	if !ok {
		return ""
	}
	return rig.Name
}

type registeredRigBinding struct {
	City supervisor.CityEntry
	Rig  config.Rig
	Path string
}

func registeredRigBindingsByName(name string, failOnLoadError bool) (matches []registeredRigBinding, stale []staleRegisteredCity, err error) {
	return registeredRigBindings(failOnLoadError, func(binding registeredRigBinding) bool {
		return binding.Rig.Name == name
	})
}

func registeredRigBindingsByPath(dir string, failOnLoadError bool) (matches []registeredRigBinding, stale []staleRegisteredCity, err error) {
	dir = normalizePathForCompare(dir)
	matches, stale, err = registeredRigBindings(failOnLoadError, func(binding registeredRigBinding) bool {
		rigPath := normalizePathForCompare(binding.Path)
		return pathWithinScope(dir, rigPath)
	})
	if err != nil {
		return nil, stale, err
	}
	return keepDeepestRigBindings(matches), stale, nil
}

// staleRegisteredCity identifies a registered city whose city.toml is
// missing on disk. registeredRigBindings returns these as structured data
// instead of emitting to stderr so callers that are explicitly resolving a
// registered rig can warn, while opportunistic probes stay quiet.
type staleRegisteredCity struct {
	Label string
	Path  string
}

// emitStaleRegisteredCityWarnings writes one `warning: ...` line per stale
// registry entry. Each Label is emitted at most once even if stale carries
// duplicates (e.g. from callers that invoke registeredRigBindings twice in
// one command).
func emitStaleRegisteredCityWarnings(w io.Writer, stale []staleRegisteredCity) {
	if w == nil || len(stale) == 0 {
		return
	}
	seen := make(map[string]struct{}, len(stale))
	for _, s := range stale {
		if _, already := seen[s.Label]; already {
			continue
		}
		seen[s.Label] = struct{}{}
		fmt.Fprintf(w, "warning: skipping stale registered city %q: city.toml missing at %s\n", //nolint:errcheck // best-effort stderr
			s.Label, s.Path)
	}
}

func registeredRigBindings(failOnLoadError bool, match func(registeredRigBinding) bool) (_ []registeredRigBinding, stale []staleRegisteredCity, _ error) {
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	cities, err := reg.List()
	if err != nil {
		return nil, nil, err
	}
	var matched []registeredRigBinding
	var loadErrors []string
	for _, c := range cities {
		cfg, err := loadCityConfigSuppressDeprecatedOrderWarnings(c.Path, io.Discard)
		if err != nil {
			// Tolerate stale registry entries whose city.toml has been
			// deleted out from under the registry, but keep missing includes
			// or other config dependencies as load errors.
			if cityTOML, ok := missingRootCityTOML(err, c.Path); ok {
				stale = append(stale, staleRegisteredCity{Label: registeredCityLabel(c), Path: cityTOML})
				continue
			}
			loadErrors = append(loadErrors, fmt.Sprintf("%s: %v", registeredCityLabel(c), err))
			continue
		}
		siteBinding, err := config.LoadSiteBinding(fsys.OSFS{}, c.Path)
		if err != nil {
			loadErrors = append(loadErrors, fmt.Sprintf("%s: %v", registeredCityLabel(c), err))
			continue
		}
		siteRigPaths := make(map[string]string, len(siteBinding.Rigs))
		for _, siteRig := range siteBinding.Rigs {
			name := strings.TrimSpace(siteRig.Name)
			path := strings.TrimSpace(siteRig.Path)
			if name == "" || path == "" {
				continue
			}
			siteRigPaths[name] = path
		}
		for _, rig := range cfg.Rigs {
			if strings.TrimSpace(rig.Name) == "" {
				continue
			}
			sitePath := strings.TrimSpace(siteRigPaths[rig.Name])
			if sitePath == "" {
				continue
			}
			rig.Path = sitePath
			binding := registeredRigBinding{
				City: c,
				Rig:  rig,
				Path: resolveStoreScopeRoot(c.Path, sitePath),
			}
			if match(binding) {
				matched = append(matched, binding)
			}
		}
	}
	if len(loadErrors) > 0 && (failOnLoadError || len(matched) > 0) {
		return nil, stale, fmt.Errorf("loading registered city rig bindings: %s", strings.Join(loadErrors, "; "))
	}
	return matched, stale, nil
}

func missingRootCityTOML(err error, cityPath string) (string, bool) {
	if !errors.Is(err, os.ErrNotExist) {
		return "", false
	}
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) {
		return "", false
	}
	cityTOML := filepath.Clean(filepath.Join(cityPath, "city.toml"))
	return cityTOML, samePath(pathErr.Path, cityTOML)
}

func keepDeepestRigBindings(matches []registeredRigBinding) []registeredRigBinding {
	var bestLen int
	for _, binding := range matches {
		if l := len(normalizePathForCompare(binding.Path)); l > bestLen {
			bestLen = l
		}
	}
	if bestLen == 0 {
		return matches
	}
	filtered := matches[:0]
	for _, binding := range matches {
		if len(normalizePathForCompare(binding.Path)) == bestLen {
			filtered = append(filtered, binding)
		}
	}
	return filtered
}

func resolveRigBindingMatches(value string, matches []registeredRigBinding) (resolvedContext, error) {
	if len(matches) == 1 {
		return resolvedContext{CityPath: matches[0].City.Path, RigName: matches[0].Rig.Name}, nil
	}
	return resolvedContext{}, fmt.Errorf(
		"rig %q is registered in multiple cities: %s\n  Specify now:  gc --city <name> <command>",
		value,
		strings.Join(registeredRigBindingCityNames(matches), ", "))
}

func registeredRigBindingCityNames(matches []registeredRigBinding) []string {
	seen := make(map[string]struct{}, len(matches))
	var names []string
	for _, binding := range matches {
		name := registeredCityLabel(binding.City)
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func registeredCityLabel(city supervisor.CityEntry) string {
	name := strings.TrimSpace(city.EffectiveName())
	if name == "" {
		name = city.Path
	}
	return name
}

// openCityRecorder returns a Recorder that appends to .gc/events.jsonl in the
// current city. Returns events.Discard on any error — commands always get a
// valid recorder.
func openCityRecorder(stderr io.Writer) events.Recorder {
	cityPath, err := resolveCity()
	if err != nil {
		return events.Discard
	}
	return openCityRecorderAt(cityPath, stderr)
}

func openCityRecorderAt(cityPath string, stderr io.Writer) events.Recorder {
	rec, err := events.NewFileRecorder(
		filepath.Join(cityPath, ".gc", "events.jsonl"), stderr)
	if err != nil {
		return events.Discard
	}
	return rec
}

// eventActor returns the public actor identity for events.
// Prefer the session alias when present, but preserve GC_AGENT fallback for
// managed-session hooks and older event-emitting contexts.
func eventActor() string {
	if alias := strings.TrimSpace(os.Getenv("GC_ALIAS")); alias != "" {
		return alias
	}
	if agent := strings.TrimSpace(os.Getenv("GC_AGENT")); agent != "" {
		return agent
	}
	if sessionID := strings.TrimSpace(os.Getenv("GC_SESSION_ID")); sessionID != "" {
		return sessionID
	}
	return "human"
}

// openCityStore locates the city root from the current directory and opens a
// Store using the configured provider. On error it writes to stderr and returns
// nil plus an exit code.
func openCityStore(stderr io.Writer, cmdName string) (beads.Store, int) {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
		return nil, 1
	}
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err)                   //nolint:errcheck // best-effort stderr
		fmt.Fprintln(stderr, "hint: run \"gc doctor\" for diagnostics") //nolint:errcheck // best-effort stderr
		return nil, 1
	}
	return store, 0
}

// openCityStoreAt opens a bead store at the given city path.
// Used by the controller (which already knows the city path) and by
// openCityStore (which resolves the path first).
func openCityStoreAt(cityPath string) (beads.Store, error) {
	return openStoreAtForCity(cityPath, cityForStoreDir(cityPath))
}

const fileStoreLayoutScopedV1 = "scope-local-v1"

func fileStoreLayoutMarkerPath(cityPath string) string {
	return filepath.Join(cityPath, ".gc", "file-beads-layout")
}

func fileStoreUsesScopedRoots(cityPath string) bool {
	data, err := os.ReadFile(fileStoreLayoutMarkerPath(cityPath))
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(data)) == fileStoreLayoutScopedV1
}

func ensureScopedFileStoreLayout(cityPath string) error {
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		return err
	}
	return os.WriteFile(fileStoreLayoutMarkerPath(cityPath), []byte(fileStoreLayoutScopedV1+"\n"), 0o644)
}

func openScopeLocalFileStore(scopeRoot string) (*beads.FileStore, error) {
	beadsPath := filepath.Join(scopeRoot, ".gc", "beads.json")
	store, err := beads.OpenFileStore(fsys.OSFS{}, beadsPath)
	if err != nil {
		return nil, err
	}
	store.SetLocker(beads.NewFileFlock(beadsPath + ".lock"))
	return store, nil
}

func ensurePersistedScopeLocalFileStore(scopeRoot string) error {
	beadsPath := filepath.Join(scopeRoot, ".gc", "beads.json")
	if _, err := os.Stat(beadsPath); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(beadsPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(beadsPath, []byte("{\"seq\":0,\"beads\":[]}\n"), 0o644)
}

func openExistingScopeLocalFileStore(scopeRoot string) (*beads.FileStore, error) {
	beadsPath := filepath.Join(scopeRoot, ".gc", "beads.json")
	if _, err := os.Stat(beadsPath); err != nil {
		return nil, err
	}
	return openScopeLocalFileStore(scopeRoot)
}

func openCompatibleFileStore(scopeRoot, cityPath string) (*beads.FileStore, error) {
	scopeRoot = resolveStoreScopeRoot(cityPath, scopeRoot)
	if !samePath(scopeRoot, cityPath) && scopeUsesFileStoreContract(scopeRoot) {
		return openExistingScopeLocalFileStore(scopeRoot)
	}
	if fileStoreUsesScopedRoots(cityPath) {
		return openExistingScopeLocalFileStore(scopeRoot)
	}
	return openScopeLocalFileStore(cityPath)
}

func openStoreAtForCity(storePath, cityPath string) (beads.Store, error) {
	runtimeCityPath := cityPath
	if runtimeCityPath == "" {
		runtimeCityPath = cityForStoreDir(storePath)
	}
	scopeRoot := resolveStoreScopeRoot(runtimeCityPath, storePath)
	provider := rawBeadsProviderForScope(scopeRoot, runtimeCityPath)
	if strings.HasPrefix(provider, "exec:") {
		target, err := resolveConfiguredExecStoreTarget(runtimeCityPath, scopeRoot)
		if err != nil {
			return nil, err
		}
		env := gcExecStoreEnv(runtimeCityPath, target, provider)
		if execProviderNeedsScopedDoltStoreEnv(provider) {
			if target.ScopeKind == "rig" {
				cfg, err := loadCityConfig(runtimeCityPath, io.Discard)
				if err != nil {
					return nil, err
				}
				copyExecProjectedDoltEnv(env, bdRuntimeEnvForRig(runtimeCityPath, cfg, target.ScopeRoot))
			} else {
				copyExecProjectedDoltEnv(env, bdRuntimeEnv(runtimeCityPath))
			}
		}
		store := beadsexec.NewStore(strings.TrimPrefix(provider, "exec:"))
		store.SetEnv(env)
		return store, nil
	}
	switch provider {
	case "file":
		return openCompatibleFileStore(scopeRoot, runtimeCityPath)
	default: // "bd" or unrecognized → use bd
		if _, err := exec.LookPath("bd"); err != nil {
			return nil, fmt.Errorf("bd not found in PATH (install beads or set GC_BEADS=file)")
		}
		return openBdStoreAt(scopeRoot, runtimeCityPath)
	}
}

// resolveStoreScopeRoot resolves a store's scope root under cityPath.
// An empty storePath falls back to cityPath — this is the "city scope"
// default used by callers that don't have a specific rig context. Callers
// that need to distinguish an unbound rig from the city scope must check
// rig.Path themselves before calling (see rig_scope_resolution.go and
// beads_provider_lifecycle.go for the `if rig.Path == "" { continue }`
// pattern).
func resolveStoreScopeRoot(cityPath, storePath string) string {
	scopeRoot := strings.TrimSpace(storePath)
	if scopeRoot == "" {
		scopeRoot = cityPath
	}
	if !filepath.IsAbs(scopeRoot) {
		scopeRoot = filepath.Join(cityPath, scopeRoot)
	}
	return filepath.Clean(scopeRoot)
}

func openBdStoreAt(storePath, cityPath string) (beads.Store, error) {
	if filepath.Clean(storePath) == filepath.Clean(cityPath) {
		return bdStoreForCity(storePath, cityPath), nil
	}
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		cfg = nil
	}
	return bdStoreForRig(storePath, cityPath, cfg), nil
}
