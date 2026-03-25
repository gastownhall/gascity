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
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
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
		newTranscriptCmd(stdout, stderr),
		newNudgeCmd(stdout, stderr),
		newWaitCmd(stdout, stderr),
		newAgentCmd(stdout, stderr),
		newEventCmd(stdout, stderr),
		newEventsCmd(stdout, stderr),
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

// resolveCity returns the city root path. If --city was provided, it
// verifies city.toml exists there (or falls back to legacy .gc/).
// Otherwise falls back to os.Getwd() →
// findCity().
func resolveCity() (string, error) {
	if cityFlag != "" {
		p, err := filepath.Abs(cityFlag)
		if err != nil {
			return "", err
		}
		if citylayout.HasCityConfig(p) || citylayout.HasRuntimeRoot(p) {
			return p, nil
		}
		return "", fmt.Errorf("not a city directory: %s (no city.toml or .gc/ found)", p)
	}
	if gcCity := os.Getenv("GC_CITY"); gcCity != "" {
		p, err := filepath.Abs(gcCity)
		if err == nil && (citylayout.HasCityConfig(p) || citylayout.HasRuntimeRoot(p)) {
			return p, nil
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return findCity(cwd)
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
		store.SetEnv(citylayout.CityRuntimeEnvMap(runtimeCityPath))
		return store, nil
	}
	switch provider {
	case "file":
		store, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(runtimeCityPath, ".gc", "beads.json"))
		if err != nil {
			return nil, err
		}
		return store, nil
	default: // "bd" or unrecognized → use bd
		if _, err := exec.LookPath("bd"); err != nil {
			return nil, fmt.Errorf("bd not found in PATH (install beads or set GC_BEADS=file)")
		}
		return bdStoreForCity(storePath, runtimeCityPath), nil
	}
}
