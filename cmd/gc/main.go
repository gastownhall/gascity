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

// cityFlag holds the value of the --city persistent flag.
// Empty means "discover from cwd."
var cityFlag string

// rigFlag holds the value of the --rig persistent flag.
// Empty means "discover from cwd or omit."
var rigFlag string

// run executes the gc CLI with the given args, writing output to stdout and
// errors to stderr. Returns the exit code.
func run(args []string, stdout, stderr io.Writer) int {
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
		return 1
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
			fmt.Fprintf(stderr, "gc: unknown command %q\n", args[0]) //nolint:errcheck // best-effort stderr
			return errExit
		},
	}
	root.PersistentFlags().StringVar(&cityFlag, "city", "",
		"path to the city directory (default: walk up from cwd)")
	root.PersistentFlags().StringVar(&rigFlag, "rig", "",
		"rig name or path (default: discover from cwd)")
	root.CompletionOptions.DisableDefaultCmd = true
	root.AddCommand(
		newStartCmd(stdout, stderr),
		newInitCmd(stdout, stderr),
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
	)
	// gen-doc needs the root command to walk the tree; add after construction.
	root.AddCommand(newGenDocCmd(stdout, stderr, root))

	// Best-effort: discover pack CLI commands if we're inside a city.
	registerPackCommands(root, stdout, stderr)

	return root
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
	cliStoreCache.mu.Lock()
	if cliStoreCache.path != cityPath {
		cliStoreCache.store, _ = openCityStoreAt(cityPath)
		cliStoreCache.path = cityPath
	}
	store := cliStoreCache.store
	cliStoreCache.mu.Unlock()
	return sessionName(store, cityName, agentName, sessionTemplate)
}

// findCity walks dir upward looking for a directory containing city.toml.
// Falls back to legacy .gc/ markers for compatibility.
func findCity(dir string) (string, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	var legacy string
	for {
		if citylayout.HasCityConfig(dir) {
			return dir, nil
		}
		if legacy == "" && citylayout.HasRuntimeRoot(dir) {
			legacy = dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			if legacy != "" {
				return legacy, nil
			}
			return "", fmt.Errorf("not in a city directory (no city.toml or .gc/ found)")
		}
		dir = parent
	}
}

// resolvedContext holds the result of city+rig resolution.
type resolvedContext struct {
	CityPath string // absolute path to city root
	RigName  string // rig name (empty if not in a rig context)
}

// resolveContext resolves the city and optional rig context using the
// following priority chain:
//  1. --city + --rig flags (explicit both, validated)
//  2. --city only (explicit city, rig from cwd if applicable)
//  3. --rig only (rig from cities.toml, city from default_city)
//  4. GC_CITY + GC_RIG env vars
//  5. GC_CITY only (city set, rig from cwd if applicable)
//  6. GC_RIG only (rig from cities.toml, city from default_city)
//  7. Rig index lookup (cwd prefix match in cities.toml)
//  8. Walk up from cwd looking for city.toml
//  9. Fail
func resolveContext() (resolvedContext, error) {
	city := cityFlag
	rig := rigFlag
	gcCity := os.Getenv("GC_CITY")
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

	// Step 4: GC_CITY + GC_RIG
	if gcCity != "" && gcRig != "" {
		if cp, err := validateCityPath(gcCity); err == nil {
			return resolvedContext{CityPath: cp, RigName: gcRig}, nil
		}
	}

	// Step 5: GC_CITY only
	if gcCity != "" {
		if cp, err := validateCityPath(gcCity); err == nil {
			rn := rigFromCwd(cp)
			return resolvedContext{CityPath: cp, RigName: rn}, nil
		}
	}

	// Step 6: GC_RIG only
	if gcRig != "" {
		ctx, err := resolveRigToContext(gcRig)
		if err == nil {
			return ctx, nil
		}
	}

	// Step 7: Rig index lookup (cwd prefix match in cities.toml).
	cwd, err := os.Getwd()
	if err != nil {
		return resolvedContext{}, err
	}
	if ctx, ok := lookupRigFromCwd(cwd); ok {
		return ctx, nil
	}

	// Step 8: Walk up from cwd looking for city.toml.
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
	ctx, err := resolveContext()
	if err != nil {
		return "", err
	}
	return ctx.CityPath, nil
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

// resolveRigToContext resolves a rig name or path to a full context via
// the global registry in cities.toml.
func resolveRigToContext(nameOrPath string) (resolvedContext, error) {
	reg := supervisor.NewRegistry(supervisor.RegistryPath())

	// Try by name first.
	if entry, ok := reg.LookupRigByName(nameOrPath); ok {
		ctx, err := resolveRigEntryCity(reg, entry)
		if err != nil {
			return resolvedContext{}, err
		}
		return ctx, nil
	}

	// Try by path.
	abs, err := filepath.Abs(nameOrPath)
	if err != nil {
		return resolvedContext{}, fmt.Errorf("rig %q: %w", nameOrPath, err)
	}
	if entry, ok := reg.LookupRigByPath(abs); ok {
		ctx, err := resolveRigEntryCity(reg, entry)
		if err != nil {
			return resolvedContext{}, err
		}
		return ctx, nil
	}

	return resolvedContext{}, fmt.Errorf("rig %q is not registered in any city", nameOrPath)
}

// resolveRigEntryCity resolves a rig entry to a city. Uses default_city if
// set, otherwise auto-resolves if exactly one city contains the rig.
func resolveRigEntryCity(reg *supervisor.Registry, entry supervisor.RigEntry) (resolvedContext, error) {
	if entry.DefaultCity != "" {
		return resolvedContext{CityPath: entry.DefaultCity, RigName: entry.Name}, nil
	}
	// No default — check how many cities actually contain this rig.
	paths := rigCityPaths(reg, entry.Path)
	switch len(paths) {
	case 1:
		return resolvedContext{CityPath: paths[0], RigName: entry.Name}, nil
	case 0:
		return resolvedContext{}, fmt.Errorf("rig %q is registered but not found in any city", entry.Name)
	default:
		cities := rigCityList(reg, entry.Path)
		return resolvedContext{}, fmt.Errorf(
			"rig %q is registered in multiple cities: %s\n  Set a default:  gc rig default %s --city <name>\n  Or specify now:  gc --city <name> <command>",
			entry.Name, strings.Join(cities, ", "), entry.Name)
	}
}

// lookupRigFromCwd checks the global registry for a rig matching the cwd.
func lookupRigFromCwd(cwd string) (resolvedContext, bool) {
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	entry, ok := reg.LookupRigByPath(cwd)
	if !ok {
		return resolvedContext{}, false
	}
	if entry.DefaultCity == "" {
		// Ambiguous — can't auto-resolve. Fall through to walk-up.
		return resolvedContext{}, false
	}
	return resolvedContext{CityPath: entry.DefaultCity, RigName: entry.Name}, true
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
	cfg, err := loadCityConfig(cityPath)
	if err != nil {
		return ""
	}
	for _, rig := range cfg.Rigs {
		rigPath := rig.Path
		if !filepath.IsAbs(rigPath) {
			rigPath = filepath.Join(cityPath, rigPath)
		}
		rigPath = filepath.Clean(rigPath)
		if cwd == rigPath || (len(cwd) > len(rigPath) && cwd[len(rigPath)] == '/' && cwd[:len(rigPath)] == rigPath) {
			return rig.Name
		}
	}
	return ""
}

// rigCityList scans all registered cities to find which ones contain a rig.
// Returns display names for error messages.
func rigCityList(reg *supervisor.Registry, rigPath string) []string {
	var names []string
	for _, c := range rigCityEntries(reg, rigPath) {
		name := c.EffectiveName()
		if name == "" {
			name = c.Path
		}
		names = append(names, name)
	}
	return names
}

// rigCityPaths returns the paths of cities that contain the given rig.
func rigCityPaths(reg *supervisor.Registry, rigPath string) []string {
	var paths []string
	for _, c := range rigCityEntries(reg, rigPath) {
		paths = append(paths, c.Path)
	}
	return paths
}

// rigCityEntries returns CityEntry values for all cities that contain the given rig.
func rigCityEntries(reg *supervisor.Registry, rigPath string) []supervisor.CityEntry {
	cities, err := reg.List()
	if err != nil {
		return nil
	}
	var matched []supervisor.CityEntry
	for _, c := range cities {
		cfg, err := loadCityConfig(c.Path)
		if err != nil {
			continue
		}
		for _, rig := range cfg.Rigs {
			rp := rig.Path
			if !filepath.IsAbs(rp) {
				rp = filepath.Join(c.Path, rp)
			}
			if filepath.Clean(rp) == rigPath {
				matched = append(matched, c)
			}
		}
	}
	return matched
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
	// Ensure GC_DOLT_PORT is in the environment so bd subprocesses can
	// connect to the managed dolt server.
	readDoltPort(cityPath)
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err)                   //nolint:errcheck // best-effort stderr
		fmt.Fprintln(stderr, "hint: run \"gc doctor\" for diagnostics") //nolint:errcheck // best-effort stderr
		return nil, 1
	}
	return store, 0
}

// resolveHQPrefixForPath loads the city config and returns the HQ beads prefix.
// Returns empty string on any error (best-effort).
func resolveHQPrefixForPath(cityPath string) string {
	tomlPath := filepath.Join(cityPath, "city.toml")
	data, err := os.ReadFile(tomlPath)
	if err != nil {
		return ""
	}
	cfg, err := config.Parse(data)
	if err != nil {
		return ""
	}
	return config.EffectiveHQPrefix(cfg)
}

// openCityStoreAt opens a bead store at the given city path.
// Used by the controller (which already knows the city path) and by
// openCityStore (which resolves the path first).
func openCityStoreAt(cityPath string) (beads.Store, error) {
	return openStoreAtForCity(cityPath, cityForStoreDir(cityPath))
}

func openStoreAtForCity(storePath, cityPath string) (beads.Store, error) {
	runtimeCityPath := cityPath
	if runtimeCityPath == "" {
		runtimeCityPath = cityForStoreDir(storePath)
	}
	provider := rawBeadsProvider(runtimeCityPath)
	if strings.HasPrefix(provider, "exec:") {
		store := beadsexec.NewStore(strings.TrimPrefix(provider, "exec:"))
		env := citylayout.CityRuntimeEnvMap(runtimeCityPath)
		// Resolve HQ prefix so the exec script targets the correct store.
		if prefix := resolveHQPrefixForPath(runtimeCityPath); prefix != "" {
			env["GC_BEADS_PREFIX"] = prefix
		}
		store.SetEnv(env)
		return store, nil
	}
	switch provider {
	case "file":
		beadsPath := filepath.Join(runtimeCityPath, ".gc", "beads.json")
		store, err := beads.OpenFileStore(fsys.OSFS{}, beadsPath)
		if err != nil {
			return nil, err
		}
		store.SetLocker(beads.NewFileFlock(beadsPath + ".lock"))
		return store, nil
	default: // "bd" or unrecognized → use bd
		if _, err := exec.LookPath("bd"); err != nil {
			return nil, fmt.Errorf("bd not found in PATH (install beads or set GC_BEADS=file)")
		}
		return bdStoreForCity(storePath, runtimeCityPath), nil
	}
}
