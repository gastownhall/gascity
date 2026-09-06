// gc is the Gas City CLI — an orchestration-builder for multi-agent workflows.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
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
	"github.com/gastownhall/gascity/internal/pathutil"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/supervisor"
	"github.com/gastownhall/gascity/internal/telemetry"
	"github.com/spf13/cobra"
)

func main() {
	os.Exit(mainExitCode(os.Args[1:], os.Stdout, os.Stderr))
}

// mainExitCode is the central process-entry funnel. main's body is required by
// the exit-bypass census to be nothing but the os.Exit around this call, so any
// work that must happen before dispatch belongs here rather than there.
func mainExitCode(args []string, stdout, stderr io.Writer) int {
	// Before any dispatch: a closed stdout/stderr must surface as an EPIPE the
	// command can handle, not as a signal that kills gc mid-write. The claim
	// path's delivery unwind depends on surviving that write.
	ignoreSIGPIPE()
	if handled, code := privateProductMetricsEntrypoint(args); handled {
		return code
	}
	return run(args, stdout, stderr)
}

// errExit is a sentinel error returned by cobra RunE functions to signal
// non-zero exit. The command has already written its own error to stderr.
var errExit = errors.New("exit")

type commandExitError struct {
	code int
}

type switchableWriter struct {
	target io.Writer
}

func (w *switchableWriter) Write(p []byte) (int, error) {
	if w == nil || w.target == nil {
		return 0, io.ErrClosedPipe
	}
	return w.target.Write(p)
}

type countingWriter struct {
	target io.Writer
	mu     sync.Mutex
	n      int64
}

func (w *countingWriter) Write(p []byte) (int, error) {
	if w == nil || w.target == nil {
		return 0, io.ErrClosedPipe
	}
	n, err := w.target.Write(p)
	w.mu.Lock()
	w.n += int64(n)
	w.mu.Unlock()
	return n, err
}

func (w *countingWriter) BytesWritten() int64 {
	if w == nil {
		return 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.n
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

type cliTelemetryShutdowner interface {
	Shutdown(context.Context) error
}

var initializeCLITelemetry = func(ctx context.Context, serviceName, serviceVersion string) (cliTelemetryShutdowner, error) {
	provider, err := telemetry.Init(ctx, serviceName, serviceVersion)
	if provider == nil {
		return nil, err
	}
	return provider, err
}

var setCLIProcessOTELAttrs = telemetry.SetProcessOTELAttrs

// run executes the gc CLI with the given args, writing output to stdout and
// errors to stderr. Returns the exit code.
func run(args []string, stdout, stderr io.Writer) int {
	if args == nil {
		args = []string{}
	}
	lifecycle := openProductMetricsInvocationLifecycle(args)
	defer lifecycle.Close()
	return runWithRootCommandOptionsAndLifecycle(args, stdout, stderr, rootCommandOptionsForArgs(args), lifecycle)
}

// runWithRootCommandOptions preserves an explicit eager/lazy construction
// seam for package tests while production always derives options from its
// injected args. It must never fill options from ambient os.Args.
func runWithRootCommandOptions(args []string, stdout, stderr io.Writer, options rootCommandOptions) int {
	if args == nil {
		args = []string{}
	}
	lifecycle := openProductMetricsInvocationLifecycle(args)
	defer lifecycle.Close()
	return runWithRootCommandOptionsAndLifecycle(args, stdout, stderr, options, lifecycle)
}

func runWithRootCommandOptionsAndLifecycle(args []string, stdout, stderr io.Writer, options rootCommandOptions, lifecycle *productMetricsInvocationLifecycle) int {
	// Whatever this invocation opened for one-shot storage routing closes here,
	// after the command has run and before the process reports its code.
	defer func() { _ = closeCLIStorageRoutes() }()

	prevCityFlag, prevRigFlag := cityFlag, rigFlag
	prevContextFlag, prevCityURLFlag, prevCityNameFlag := contextFlag, cityURLFlag, cityNameFlag
	cityFlag, rigFlag = "", ""
	contextFlag, cityURLFlag, cityNameFlag = "", "", ""
	defer func() {
		cityFlag = prevCityFlag
		rigFlag = prevRigFlag
		contextFlag, cityURLFlag, cityNameFlag = prevContextFlag, prevCityURLFlag, prevCityNameFlag
	}()

	// Initialize OTel telemetry (opt-in via GC_OTEL_METRICS_URL / GC_OTEL_LOGS_URL).
	provider, err := initializeCLITelemetry(context.Background(), "gascity", version)
	if err != nil {
		fmt.Fprintf(stderr, "gc: telemetry init: %v\n", err) //nolint:errcheck // best-effort stderr
	}
	if provider != nil {
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = provider.Shutdown(ctx)
		}()
		setCLIProcessOTELAttrs()
	}
	execStdout := &switchableWriter{target: stdout}
	var jsonStdout bytes.Buffer
	var observedStdout *countingWriter
	options.invocationArgs = append([]string(nil), args...)
	root := newRootCmdWithOptions(execStdout, stderr, options)
	root.SetArgs(args)
	root.SetOut(execStdout)
	root.SetErr(stderr)
	if options.discoverPackCommands {
		materializePackCommandTreeForArgs(root, args, execStdout, stderr)
	}
	lifecycleBinding := bindProductMetricsInvocationLifecycle(root, args, lifecycle)
	classification := lifecycleBinding.classification
	lifecycle.prepareNotice(classification, stderr)
	bufferJSONExecution := shouldBufferJSONExecution(root, args)
	reportJSONFailure := shouldReportJSONExecutionError(root, args)
	if bufferJSONExecution {
		execStdout.target = &jsonStdout
	} else if reportJSONFailure {
		observedStdout = &countingWriter{target: stdout}
		execStdout.target = observedStdout
	}
	if earlyAction, ok := prepareJSONEarlyAction(root, args); ok {
		earlyOutcome := resolveProductMetricsEarlyOutcome(earlyAction, classification)
		lifecycle.attemptEarlyOutcome(earlyOutcome)
		if handled, code := executeProductMetricsEarlyOutcome(earlyOutcome, earlyAction, stdout, stderr); handled {
			return code
		}
	}
	executedCommand, executeErr := root.ExecuteC()
	lifecycle.attemptFinalOutcome(resolveProductMetricsFinalOutcome(executedCommand, classification))
	if executeErr != nil {
		code := commandExitCode(executeErr)
		if bufferJSONExecution {
			if len(bytes.TrimSpace(jsonStdout.Bytes())) > 0 {
				if _, copyErr := io.Copy(stdout, &jsonStdout); copyErr != nil {
					return 1
				}
			} else {
				_ = writeJSONFailure(stdout, "command_failed", commandFailureMessage(executeErr), code)
			}
		} else if reportJSONFailure && observedStdout.BytesWritten() == 0 {
			_ = writeJSONFailure(stdout, "command_failed", commandFailureMessage(executeErr), code)
		}
		return code
	}
	if bufferJSONExecution {
		if _, err := io.Copy(stdout, &jsonStdout); err != nil {
			return 1
		}
	}
	return 0
}

func commandFailureMessage(err error) string {
	if err == nil {
		return "command failed"
	}
	msg := strings.TrimSpace(err.Error())
	if msg == "" || errors.Is(err, errExit) {
		return "command failed; see stderr for diagnostics"
	}
	return msg
}

// newRootCmd creates the root cobra command with all subcommands.
func newRootCmd(stdout, stderr io.Writer) *cobra.Command {
	return newRootCmdWithOptions(stdout, stderr, rootCommandOptions{
		discoverPackCommands:      true,
		eagerPackCommandDiscovery: true,
	})
}

// newRootCmdWithOptions constructs the built-in command tree and optionally
// performs city/pack discovery selected from injected invocation arguments.
func newRootCmdWithOptions(stdout, stderr io.Writer, options rootCommandOptions) *cobra.Command {
	root := &cobra.Command{
		Use:           "gc",
		Short:         "Gas City CLI — orchestration-builder for multi-agent workflows",
		SilenceErrors: true,
		SilenceUsage:  true,
		Args:          cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if packCommandFlagsHaveEmptyExplicitScope(cmd) {
				attemptProductMetricsForCommand(cmd)
				fmt.Fprintln(stderr, "gc: --city and --rig require non-empty values") //nolint:errcheck // best-effort stderr
				printCommandUsage(stderr, cmd)
				return errExit
			}
			if len(args) == 0 {
				return cmd.Help()
			}
			// Lazy fallback: if eager discovery missed a pack command
			// (e.g. config changed after binary started), try one more time.
			packAction := resolvePackCommandFallback(args, stdout, stderr)
			packOutcome := executeProductMetricsPackAction(cmd, packAction)
			if packAction.selected {
				return packOutcome.err()
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
	root.PersistentFlags().StringVar(&contextFlag, "context", "",
		"operate the REMOTE city named by this context (~/.gc/contexts.toml)")
	root.PersistentFlags().StringVar(&cityURLFlag, "city-url", "",
		"operate a REMOTE city at this base URL (https; requires --city-name)")
	root.PersistentFlags().StringVar(&cityNameFlag, "city-name", "",
		"remote city name for --city-url (does not overload --city)")
	configureJSONSchemaFlag(root)
	_ = root.RegisterFlagCompletionFunc("rig", completeRigFlagNames)
	root.AddCommand(
		newStartCmd(stdout, stderr),
		newInitCmd(stdout, stderr),
		newReloadCmd(stdout, stderr),
		newStopCmd(stdout, stderr),
		newRestartCmd(stdout, stderr),
		newStatusCmd(stdout, stderr),
		newStorageCmd(stdout, stderr),
		newServiceCmd(stdout, stderr),
		newSuspendCmd(stdout, stderr),
		newResumeCmd(stdout, stderr),
		newRigCmd(stdout, stderr),
		newMailCmd(stdout, stderr),
		newMaintenanceCmd(stdout, stderr),
		newNudgeCmd(stdout, stderr),
		newWaitCmd(stdout, stderr),
		newAgentCmd(stdout, stderr),
		newAgentScriptCmd(stdout, stderr),
		newGitHubCmd(stdout, stderr),
		newEventCmd(stdout, stderr),
		newEventsCmd(stdout, stderr),
		newExtMsgCmd(stdout, stderr),
		newTraceCmd(stdout, stderr),
		newOrderCmd(stdout, stderr),
		newImportCmd(stdout, stderr),
		newConfigCmd(stdout, stderr),
		newPackCmd(stdout, stderr),
		newLintCmd(stdout, stderr),
		newDoctorCmd(stdout, stderr),
		newHookCmd(stdout, stderr),
		newReadyCmd(stdout, stderr),
		newSlingCmd(stdout, stderr),
		newConvoyCmd(stdout, stderr),
		newWispCmd(stdout, stderr),
		newMoleculeCmd(stdout, stderr),
		newPrimeCmd(stdout, stderr),
		newPromptCmd(stdout, stderr),
		newHandoffCmd(stdout, stderr),
		newBeadsCmd(stdout, stderr),
		newBuildImageCmd(stdout, stderr),
		newSkillCmd(stdout, stderr),
		newMcpCmd(stdout, stderr),
		newInternalCmd(stdout, stderr),
		newMetricsCmd(stdout, stderr),
		newPerfCmd(stdout, stderr),
		newVersionCmd(stdout, stderr),
		newDashboardCmd(stdout, stderr),
		newGraphCmd(stdout, stderr),
		newRegisterCmd(stdout, stderr),
		newUnregisterCmd(stdout, stderr),
		newCitiesCmd(stdout, stderr),
		newContextCmd(stdout, stderr),
		newSupervisorCmd(stdout, stderr),
		newSessionCmd(stdout, stderr),
		newConvergeCmd(stdout, stderr),
		newWorkflowCmd(stdout, stderr),
		newWorktreeCmd(stdout, stderr),
		newRuntimeCmd(stdout, stderr),
		newFormulaCmd(stdout, stderr),
		newBdCmd(stdout, stderr),
		newBdStoreBridgeCmd(stdout, stderr),
		newDoltCleanupCmd(stdout, stderr),
		newDoltConfigCmd(stdout, stderr),
		newDoltStateCmd(stdout, stderr),
		newShellCmd(stdout, stderr),
		newAnalyzeCmd(stdout, stderr),
		newCostsCmd(stdout, stderr),
		newGitCredentialCmd(stdout, stderr),
		newLoginCmd(stdout, stderr),
		newWhoamiCmd(stdout, stderr),
		newLogoutCmd(stdout, stderr),
	)
	// gen-doc needs the root command to walk the tree; add after construction.
	root.AddCommand(newGenDocCmd(stdout, stderr, root))

	// Cobra materializes its public help and completion commands lazily. Force
	// them while pack discovery is still disabled so the finite built-in
	// product-metrics census sees the same tree on every machine. Set the
	// writers first: Cobra captures them in the generated handlers.
	root.SetOut(stdout)
	root.SetErr(stderr)
	materializeProductMetricsCobraDefaults(root)
	applyProductionProductMetricsCommandCensus(root)

	// Best-effort: discover pack CLI commands if we're inside a city.
	if options.discoverPackCommands && options.eagerPackCommandDiscovery {
		registerPackCommands(root, options.invocationArgs, stdout, stderr)
	}

	installArgUsageErrors(root, stderr)
	installFlagGroupUsageErrors(root, stderr)

	return root
}

func installArgUsageErrors(cmd *cobra.Command, stderr io.Writer) {
	if cmd.Args != nil && cmd.Annotations[cobraForcedDefaultAnnotation] != "true" {
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

// installFlagGroupUsageErrors wraps PreRunE on every command so mutually
// exclusive / required-together / one-required flag violations surface as
// readable usage errors. Without this, cobra's own ValidateFlagGroups error
// returns through RunE and is swallowed by the root's SilenceErrors, causing
// `gc <cmd> --a --b` (with --a/--b mutex) to exit 1 with no output.
func installFlagGroupUsageErrors(cmd *cobra.Command, stderr io.Writer) {
	prev := cmd.PreRunE
	prevRun := cmd.PreRun
	cmd.PreRunE = func(cmd *cobra.Command, args []string) error {
		if err := cmd.ValidateFlagGroups(); err != nil {
			printCommandUsageError(stderr, cmd, err)
			return errExit
		}
		if prev != nil {
			return prev(cmd, args)
		}
		if prevRun != nil {
			prevRun(cmd, args)
		}
		return nil
	}
	for _, child := range cmd.Commands() {
		installFlagGroupUsageErrors(child, stderr)
	}
}

func printCommandUsageError(stderr io.Writer, cmd *cobra.Command, err error) {
	attemptProductMetricsForCommand(cmd)
	if err != nil {
		fmt.Fprintf(stderr, "gc: %v\n\n", err) //nolint:errcheck // best-effort stderr
	}
	printCommandUsage(stderr, cmd)
}

func printCommandUsage(stderr io.Writer, cmd *cobra.Command) {
	if cmd == nil {
		return
	}
	fmt.Fprintf(stderr, "Run %q for usage.\n", cmd.CommandPath()+" --help") //nolint:errcheck // best-effort stderr
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

// contextResolutionMode distinguishes the two kinds of caller the resolution
// chain serves. The zero value is the authoritative one, so every caller that
// does not opt in keeps waiting for whatever it needs.
type contextResolutionMode struct {
	// advisory marks a resolution nobody asked for: eager pack-command
	// discovery and shell completion, which run on every gc invocation
	// whatever the user typed. Such a caller would rather have no answer than
	// wait out another process's network clone, so an advisory resolution
	// never blocks on the machine-wide repo-cache lock, skips the rig
	// decoration its caller discards anyway, and stays off the terminal.
	//
	// It does not trade accuracy for speed. An advisory resolution either
	// names the same city an authoritative one would, or fails: whatever it
	// declines to wait for it reports as an error rather than resolving
	// around. Eager discovery captures the city it picks into the
	// pack-command closures the user later runs, so a cheaper-but-different
	// answer would end up executing pack code against the wrong city.
	advisory bool
}

// authoritativeResolution is the mode for anything the user actually typed: it
// waits for whatever it needs. Named rather than spelled as a bare
// contextResolutionMode{} so the choice reads as deliberate at the call site.
var authoritativeResolution contextResolutionMode

// resolvedContext holds the result of city+rig resolution.
type resolvedContext struct {
	CityPath string        // absolute path to city root (empty when Remote is set)
	RigName  string        // rig name (empty if not in a rig context)
	Remote   *remoteTarget // non-nil => a REMOTE city over the control plane; CityPath is empty
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
	// A positional city/rig argument targets a LOCAL city; combined with a remote
	// FLAG (--city-url/--context) — the same explicit tier — it must not silently
	// shadow the requested remote city, so reject that loudly. A remote ENV
	// selector is lower precedence than the positional (flag > env), so it is
	// shadowed rather than conflicting (Decision 4).
	if remoteFlagPresent() {
		return resolvedContext{}, remotePositionalConflictErr(args[0])
	}
	// A name-shaped positional may be a registered city name or a local rig
	// directory. Route it through the shared name resolver, which consults the
	// registry and the rig-path resolver before failing and never feeds a bare
	// name to resolveContextFromPath's upward city walk (which would silently
	// target an ambient ancestor city).
	if classifyCityRef(args[0]) == cityRefName {
		return resolveCityNameContext(args[0], resolveContextFromPath)
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

// resolveContext resolves the city and optional rig context using a fixed
// priority chain, each stage delegated to a helper that reports whether it
// handled the request so the chain stops at the first match:
//  0. remote target: --city-url/--context flag or GC_CITY_URL/GC_CITY_CONTEXT
//     (resolveRemoteTarget)
//  1. --city / --rig flags                  (resolveContextFromFlags)
//  2. explicit city env + GC_RIG            (resolveContextFromCityEnv)
//  3. GC_DIR / cwd discovery and walk-up    (resolveContextFromDir)
//  4. sticky default context                (resolveStickyDefaultTarget)
//
// Steps 0 and 4 select a REMOTE city (Decision 4): an explicit remote flag/env
// beats every local tier, while the sticky default is subordinate to local
// discovery.
//
// resolveContext is the LOCAL-ONLY entry point: it applies the capability gate,
// erroring on a remote target. Every command that only operates a local city
// (via resolveCity/resolveCommandCity or a direct call) uses it and is therefore
// refused loudly under a remote target — it can never silently fall back to a
// local store. Remote-capable READ commands call resolveContextAllowRemote
// directly and route through the remote transport (resolveReadRoute).
func resolveContext() (resolvedContext, error) {
	return resolveContextMode(authoritativeResolution)
}

func resolveContextMode(mode contextResolutionMode) (resolvedContext, error) {
	ctx, err := resolveContextAllowRemoteMode(mode)
	if err != nil {
		return resolvedContext{}, err
	}
	if ctx.Remote != nil {
		return resolvedContext{}, errRemoteNotSupportedYet()
	}
	return ctx, nil
}

// resolveCityForDiscovery returns the city root for a resolution nobody asked
// for — eager pack-command discovery, shell completion. It is resolveCity that
// refuses to wait on the machine-wide repo-cache lock: a single `gc import
// install` holds that lock for the whole of its network clone, and blocking
// discovery would make every unrelated gc command on the host hang for the
// duration. Callers get "no city right now" and degrade; anything the user
// actually typed still resolves through resolveCity and waits.
func resolveCityForDiscovery() (string, error) {
	ctx, err := resolveContextMode(contextResolutionMode{advisory: true})
	if err != nil {
		return "", err
	}
	return ctx.CityPath, nil
}

// resolveContextAllowRemote is the raw priority-chain resolver. It returns a
// remote target (resolvedContext.Remote) when one is selected, WITHOUT the
// capability gate — so only a remote-aware caller that routes through the remote
// transport should use it. Every other caller uses resolveContext, which gates.
func resolveContextAllowRemote() (resolvedContext, error) {
	return resolveContextAllowRemoteMode(authoritativeResolution)
}

func resolveContextAllowRemoteMode(mode contextResolutionMode) (resolvedContext, error) {
	// Step 0: explicit remote target. A conflict (remote+local or remote+remote)
	// surfaces here regardless.
	if target, handled, err := resolveRemoteTarget(); err != nil {
		return resolvedContext{}, err
	} else if handled {
		return resolvedContext{Remote: target}, nil
	}
	if ctx, handled, err := resolveContextFromFlags(mode); handled {
		return ctx, err
	}
	if ctx, handled, err := resolveContextFromCityEnv(mode); handled {
		return ctx, err
	}
	ctx, err := resolveContextFromDir(mode)
	if err == nil {
		return ctx, nil
	}
	// A busy repo cache means local discovery could not look, not that it
	// looked and found nothing. Falling through would answer with a remote
	// sticky default because a clone happened to be running in another
	// terminal — a different city for the same cwd, decided by timing.
	if errors.Is(err, config.ErrRepoCacheBusy) {
		return resolvedContext{}, err
	}
	// Step 4: no local city discoverable — fall back to the sticky default
	// context, if any (subordinate to local discovery, per Decision 4).
	if target, ok, derr := resolveStickyDefaultTarget(); derr != nil {
		return resolvedContext{}, derr
	} else if ok {
		// Honor GC_NO_API on the sticky-default tier too: the explicit flag/env
		// tiers guard it inside resolveRemoteSelection, and the escape hatch
		// ("never route through the API") must apply consistently rather than be
		// silently ignored for a sticky-default remote target.
		if gerr := guardNoAPI(readRemoteSelection()); gerr != nil {
			return resolvedContext{}, gerr
		}
		return resolvedContext{Remote: target}, nil
	}
	return resolvedContext{}, err
}

// resolveContextFromFlags resolves context from the explicit --city and --rig
// flags (priority steps 1-3). handled is false with a nil error when neither
// flag is set, so the caller falls through to env/cwd resolution.
func resolveContextFromFlags(mode contextResolutionMode) (resolvedContext, bool, error) {
	city := cityFlag
	rig := rigFlag
	switch {
	case city != "" && rig != "": // Step 1: --city + --rig
		cp, err := resolveCityFlagValue(city)
		if err != nil {
			return resolvedContext{}, true, err
		}
		return resolvedContext{CityPath: cp, RigName: rig}, true, nil
	case city != "": // Step 2: --city only
		cp, err := resolveCityFlagValue(city)
		if err != nil {
			return resolvedContext{}, true, err
		}
		return resolvedContext{CityPath: cp, RigName: rigFromCwd(cp, mode)}, true, nil
	case rig != "": // Step 3: --rig only
		ctx, err := resolveRigToContext(rig, mode)
		return ctx, true, err
	default:
		return resolvedContext{}, false, nil
	}
}

// resolveContextFromCityEnv resolves context from the explicit city env
// (GC_CITY / GC_CITY_PATH / GC_CITY_ROOT) and GC_RIG (priority steps 4-6).
// handled is false with a nil error when neither resolves.
func resolveContextFromCityEnv(mode contextResolutionMode) (resolvedContext, bool, error) {
	gcRig := os.Getenv("GC_RIG")
	gcCity, ok := resolveExplicitCityPathEnv()
	switch {
	case ok && gcRig != "": // Step 4: explicit city env + GC_RIG
		return resolvedContext{CityPath: gcCity, RigName: gcRig}, true, nil
	case ok: // Step 5: explicit city env only
		return resolvedContext{CityPath: gcCity, RigName: rigFromGCDirOrCwd(gcCity, mode)}, true, nil
	case gcRig != "": // Step 6: GC_RIG only
		ctx, err := resolveRigToContext(gcRig, mode)
		return ctx, true, err
	default:
		return resolvedContext{}, false, nil
	}
}

// resolveContextFromDir resolves context from GC_DIR and the cwd (priority
// steps 7-11): a GC_DIR rig binding, a GC_DIR-derived city, a cwd rig binding,
// and finally a walk up from cwd for city.toml. This is the terminal stage, so
// it always returns a result or an error.
func resolveContextFromDir(mode contextResolutionMode) (resolvedContext, error) {
	// Step 7: Registered rig binding lookup using GC_DIR. Must run before
	// the GC_DIR walkup (step 8) so that a rig dir with a leftover ".gc/"
	// runtime artifact does not get mistaken for a legacy city via
	// findCity's HasRuntimeRoot fallback. Spawned rig agents have GC_DIR
	// set to the rig path; when that path is a sibling of the city
	// (e.g. rig at /Code/rigname and city at /Code/cityname), the walkup
	// never reaches the real city and the stale ".gc/" inside the rig
	// would otherwise win.
	//
	// Guard: only run the (potentially expensive) registry scan when GC_DIR
	// actually shows the legacy-fallback misfire shape — a .gc/ directory
	// without a sibling city.toml. When GC_DIR carries its own city.toml
	// the walkup at step 8 finds the right city in O(1) and we don't pay
	// for a full registry scan. When GC_DIR has neither, step 9 (cwd-based
	// rig lookup) covers it.
	if gcDir := strings.TrimSpace(os.Getenv("GC_DIR")); gcDir != "" &&
		citylayout.HasRuntimeRoot(gcDir) && !citylayout.HasCityConfig(gcDir) {
		ctx, ok, err := lookupRigFromCwd(gcDir, mode)
		if err != nil {
			return resolvedContext{}, err
		}
		if ok {
			return ctx, nil
		}
	}

	// Step 8: GC_DIR-derived city path.
	if gcDirCity, ok := resolveCityPathFromGCDir(); ok {
		rn := rigFromCwdDir(gcDirCity, strings.TrimSpace(os.Getenv("GC_DIR")), mode)
		return resolvedContext{CityPath: gcDirCity, RigName: rn}, nil
	}

	// Step 9: Registered rig binding lookup (cwd prefix match).
	cwd, err := os.Getwd()
	if err != nil {
		return resolvedContext{}, err
	}
	ctx, ok, err := lookupRigFromCwd(cwd, mode)
	if err != nil {
		return resolvedContext{}, err
	}
	if ok {
		return ctx, nil
	}

	// Step 10: Walk up from cwd looking for city.toml.
	if isTestBinary() {
		return resolvedContext{}, fmt.Errorf(
			"not in a city directory (ambient upward discovery from %q is refused in "+
				"test binaries; set GC_CITY, GC_CITY_PATH, or GC_CITY_ROOT to an explicit "+
				"synthetic city)", cwd)
	}
	cityPath, err := findCity(cwd)
	if err != nil {
		return resolvedContext{}, err
	}
	return resolvedContext{CityPath: cityPath, RigName: rigFromCwdDir(cityPath, cwd, mode)}, nil
}

// resolveCity returns the city root path. Thin wrapper over resolveContext
// for the many callers that only need the city path.
func resolveCity() (string, error) {
	return resolveCommandCity(nil)
}

func resolveContextFromPath(path string) (resolvedContext, error) {
	abs := normalizePathForCompare(path)
	// Validate the explicit target directly before scanning the registry for
	// rig bindings. An unrelated registered city with a broken/stale config
	// must not abort resolution of a perfectly healthy explicit target
	// (#4364) -- this mirrors resolveCityNameContext's f.localIsCity-first
	// ordering for named refs.
	//
	// Deliberately narrower than validateCityPath: only a real city.toml
	// qualifies here, not validateCityPath's HasRuntimeRoot fallback. A rig
	// directory can carry a leftover ".gc/" runtime artifact with no
	// city.toml of its own (the same shape resolveContextFromDir's step-7
	// comment already guards against for a different code path); accepting
	// that shape here would misread the rig dir as its own city and
	// short-circuit before rig resolution ever runs, silently losing the
	// real city+rig binding.
	if citylayout.HasCityConfig(abs) {
		return resolvedContext{
			CityPath: abs,
			RigName:  rigFromCwdDir(abs, abs, authoritativeResolution),
		}, nil
	}
	ctx, ok, err := resolveRigPathToContext(abs)
	if err != nil {
		return resolvedContext{}, err
	}
	if ok {
		return ctx, nil
	}
	cityPath, err := findCity(abs)
	if err != nil {
		return resolvedContext{}, err
	}
	return resolvedContext{
		CityPath: cityPath,
		RigName:  rigFromCwdDir(cityPath, abs, authoritativeResolution),
	}, nil
}

// validateCityPath resolves and validates a path as a city directory.
func validateCityPath(p string) (string, error) {
	abs := normalizePathForCompare(p)
	if citylayout.HasCityConfig(abs) || citylayout.HasRuntimeRoot(abs) {
		return abs, nil
	}
	return "", fmt.Errorf("not a city directory: %s (no city.toml or .gc/ found)", abs)
}

// resolveRigToContext resolves a rig name or path to a full context by scanning
// registered cities and their machine-local .gc/site.toml rig bindings. This
// is an explicit rig-resolution path, so stale-sibling warnings are emitted
// to os.Stderr (deduped across the two registry scans below).
func resolveRigToContext(nameOrPath string, mode contextResolutionMode) (resolvedContext, error) {
	var allStale []staleRegisteredCity
	defer func() { emitStaleRegisteredCityWarnings(os.Stderr, allStale) }()

	var deferredRegisteredLoadErr error
	matches, stale, err, loadErr := registeredRigBindingsByNameWithDeferredLoadError(nameOrPath, false, mode)
	allStale = append(allStale, stale...)
	if err != nil {
		return resolvedContext{}, err
	}
	if len(matches) > 0 {
		return resolveRigBindingMatches(nameOrPath, matches)
	}
	deferredRegisteredLoadErr = loadErr

	abs, err := filepath.Abs(nameOrPath)
	if err != nil {
		return resolvedContext{}, fmt.Errorf("rig %q: %w", nameOrPath, err)
	}
	matches, stale, err, loadErr = registeredRigBindingsByPathWithDeferredLoadError(abs, false, mode)
	allStale = append(allStale, stale...)
	if err != nil {
		return resolvedContext{}, err
	}
	if len(matches) > 0 {
		return resolveRigBindingMatches(abs, matches)
	}
	if deferredRegisteredLoadErr == nil {
		deferredRegisteredLoadErr = loadErr
	}

	// Fallback: a city declared locally but not yet handed to the
	// supervisor (cities.toml does not list it) is invisible to the
	// registry walks above. Honor explicit local city resolution by checking
	// the resolved city for a site-bound rig of this name. Site binding is
	// required: legacy city.toml-only paths remain rejected so the existing
	// legacy_city_toml_path_is_not_registered_binding test continues to pass.
	if ctx, ok, err := lookupRigFromLocalCity(nameOrPath, mode); err != nil {
		return resolvedContext{}, err
	} else if ok {
		return ctx, nil
	}
	if deferredRegisteredLoadErr != nil {
		return resolvedContext{}, deferredRegisteredLoadErr
	}
	return resolvedContext{}, fmt.Errorf("rig %q is not registered in any city", nameOrPath)
}

func resolveLocalCityForRigFallback() (string, error) {
	if cityFlag != "" {
		return resolveCityFlagValue(cityFlag)
	}
	if gcCity, ok := resolveExplicitCityPathEnv(); ok {
		return gcCity, nil
	}
	if gcDir := strings.TrimSpace(os.Getenv("GC_DIR")); gcDir != "" {
		gcDirCity, err := findCity(gcDir)
		if err != nil {
			if !isCityDiscoveryNotFound(err) {
				return "", err
			}
		} else {
			return gcDirCity, nil
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	cityPath, err := findCity(cwd)
	if err != nil {
		if isCityDiscoveryNotFound(err) {
			return "", nil
		}
		return "", err
	}
	return cityPath, nil
}

func isCityDiscoveryNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not in a city directory")
}

// lookupRigFromLocalCity resolves the local city without consulting --rig or
// GC_RIG, then builds declared rig candidates from city.toml plus
// .gc/site.toml, matching the registered resolver's binding semantics.
// Legacy city.toml-only paths are still rejected so this fallback preserves
// the invariant pinned by legacy_city_toml_path_is_not_registered_binding.
func lookupRigFromLocalCity(nameOrPath string, mode contextResolutionMode) (resolvedContext, bool, error) {
	cityPath, err := resolveLocalCityForRigFallback()
	if err != nil {
		return resolvedContext{}, false, err
	}
	if cityPath == "" {
		return resolvedContext{}, false, nil
	}
	bindings, err := localCityRigBindings(cityPath, mode)
	if err != nil {
		return resolvedContext{}, false, err
	}

	var nameMatches []registeredRigBinding
	for _, binding := range bindings {
		if binding.Rig.Name == nameOrPath {
			nameMatches = append(nameMatches, binding)
		}
	}
	if len(nameMatches) > 0 {
		ctx, err := resolveRigBindingMatches(nameOrPath, nameMatches)
		return ctx, true, err
	}

	requestPath := normalizePathForCompare(nameOrPath)
	var pathMatches []registeredRigBinding
	for _, binding := range bindings {
		if pathWithinScope(requestPath, normalizePathForCompare(binding.Path)) {
			pathMatches = append(pathMatches, binding)
		}
	}
	pathMatches = keepDeepestRigBindings(pathMatches)
	if len(pathMatches) > 0 {
		ctx, err := resolveRigBindingMatches(requestPath, pathMatches)
		return ctx, true, err
	}

	return resolvedContext{}, false, nil
}

func localCityRigBindings(cityPath string, mode contextResolutionMode) ([]registeredRigBinding, error) {
	cfg, err := loadRegisteredCityConfig(cityPath, mode)
	if err != nil {
		if _, ok := missingRootCityTOML(err, cityPath); ok {
			return nil, nil
		}
		return nil, fmt.Errorf("loading local city rig bindings: %s: %w", cityPath, err)
	}
	siteBinding, err := config.LoadSiteBinding(fsys.OSFS{}, cityPath)
	if err != nil {
		return nil, fmt.Errorf("loading local city rig bindings: %s: %w", cityPath, err)
	}
	city := supervisor.CityEntry{Path: cityPath, Name: cfg.ResolvedWorkspaceName}
	return siteBoundRigBindings(city, cfg, siteBinding), nil
}

func siteBoundRigBindings(city supervisor.CityEntry, cfg *config.City, siteBinding *config.SiteBinding) []registeredRigBinding {
	candidates := make(map[string]rigCandidate, len(siteBinding.Rigs))
	for _, candidate := range siteRigCandidates(city, siteBinding) {
		candidates[candidate.Name] = candidate
	}

	var bindings []registeredRigBinding
	for _, rig := range cfg.Rigs {
		if strings.TrimSpace(rig.Name) == "" {
			continue
		}
		candidate, ok := candidates[rig.Name]
		if !ok {
			continue
		}
		rig.Path = candidate.Path
		bindings = append(bindings, registeredRigBinding{
			City: city,
			Rig:  rig,
			Path: candidate.ScopeRoot,
		})
	}
	return bindings
}

// siteRigCandidates enumerates the machine-local rig bindings a city's
// .gc/site.toml declares.
//
// This is the single derivation both siteBoundRigBindings and the pre-filter
// in registeredRigBindings run, which is what makes the pre-filter's superset
// property structural rather than a claim two functions have to keep agreeing
// on independently.
func siteRigCandidates(city supervisor.CityEntry, siteBinding *config.SiteBinding) []rigCandidate {
	candidates := make([]rigCandidate, 0, len(siteBinding.Rigs))
	for _, rig := range siteBinding.Rigs {
		name := strings.TrimSpace(rig.Name)
		path := strings.TrimSpace(rig.Path)
		if name == "" || path == "" {
			continue
		}
		candidates = append(candidates, rigCandidate{
			City:      city,
			Name:      name,
			Path:      path,
			ScopeRoot: resolveStoreScopeRoot(city.Path, path),
		})
	}
	return candidates
}

// resolveRigPathToContext resolves an explicit path argument to a registered
// rig context. Stale-sibling warnings are emitted to os.Stderr because the
// caller is explicitly depending on the registry.
func resolveRigPathToContext(dir string) (resolvedContext, bool, error) {
	matches, stale, err := registeredRigBindingsByPath(dir, true, authoritativeResolution)
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
// lookupRigFromCwd maps cwd onto the single registered rig that contains it.
//
// A scan error normally means "no answer from the registry", and the caller
// falls back to walking up from cwd. A busy repo cache is different: the
// registry might well have named a city, and falling back could settle on a
// different one just because an unrelated clone was running. That case is
// returned as an error so the caller fails closed instead.
func lookupRigFromCwd(cwd string, mode contextResolutionMode) (resolvedContext, bool, error) {
	matches, _, err := registeredRigBindingsByPath(cwd, false, mode)
	if err != nil {
		if errors.Is(err, config.ErrRepoCacheBusy) {
			return resolvedContext{}, false, err
		}
		return resolvedContext{}, false, nil
	}
	if len(matches) != 1 {
		return resolvedContext{}, false, nil
	}
	return resolvedContext{CityPath: matches[0].City.Path, RigName: matches[0].Rig.Name}, true, nil
}

// rigFromCwd attempts to derive a rig name from cwd when the city is known.
func rigFromCwd(cityPath string, mode contextResolutionMode) string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return rigFromCwdDir(cityPath, cwd, mode)
}

// rigFromCwdDir matches cwd against registered rigs in a city's config.
//
// This is pure decoration: it loads the whole city config to produce a rig
// name, and an advisory resolution's caller only wants the city path. Skipping
// it there is what keeps discovery off the repo-cache lock entirely, rather
// than merely making its acquisition non-blocking.
func rigFromCwdDir(cityPath, cwd string, mode contextResolutionMode) string {
	if mode.advisory {
		return ""
	}
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

// rigCandidate is the part of a registered rig binding that a city's
// .gc/site.toml alone determines — everything siteRigCandidates can produce
// without loading city.toml.
//
// Match predicates take this rather than a whole registeredRigBinding so the
// compiler forbids a predicate from reading a field the pre-filter cannot
// populate. That failure would be silent: the pre-filter would stop admitting
// a city the real scan would have matched, and the scan would just return
// fewer bindings.
type rigCandidate struct {
	City      supervisor.CityEntry
	Name      string // rig name as recorded in site.toml
	Path      string // machine-local rig path as recorded in site.toml
	ScopeRoot string // resolveStoreScopeRoot(City.Path, Path)
}

// candidate projects a fully resolved binding back onto the fields the
// pre-filter can see, so both sides hand the predicate the same shape.
func (b registeredRigBinding) candidate() rigCandidate {
	return rigCandidate{City: b.City, Name: b.Rig.Name, Path: b.Rig.Path, ScopeRoot: b.Path}
}

func registeredRigBindingsByName(name string, failOnLoadError bool, mode contextResolutionMode) (matches []registeredRigBinding, stale []staleRegisteredCity, err error) {
	matches, stale, err, _ = registeredRigBindingsByNameWithDeferredLoadError(name, failOnLoadError, mode)
	return matches, stale, err
}

func registeredRigBindingsByNameWithDeferredLoadError(name string, failOnLoadError bool, mode contextResolutionMode) (matches []registeredRigBinding, stale []staleRegisteredCity, err error, deferredLoadErr error) {
	return registeredRigBindings(failOnLoadError, mode, func(c rigCandidate) bool {
		return c.Name == name
	})
}

func registeredRigBindingsByPath(dir string, failOnLoadError bool, mode contextResolutionMode) (matches []registeredRigBinding, stale []staleRegisteredCity, err error) {
	matches, stale, err, _ = registeredRigBindingsByPathWithDeferredLoadError(dir, failOnLoadError, mode)
	return matches, stale, err
}

func registeredRigBindingsByPathWithDeferredLoadError(dir string, failOnLoadError bool, mode contextResolutionMode) (matches []registeredRigBinding, stale []staleRegisteredCity, err error, deferredLoadErr error) {
	dir = normalizePathForCompare(dir)
	matches, stale, err, deferredLoadErr = registeredRigBindings(failOnLoadError, mode, func(c rigCandidate) bool {
		return pathWithinScope(dir, normalizePathForCompare(c.ScopeRoot))
	})
	if err != nil {
		return nil, stale, err, nil
	}
	return keepDeepestRigBindings(matches), stale, nil, deferredLoadErr
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

func registeredRigBindings(failOnLoadError bool, mode contextResolutionMode, match func(rigCandidate) bool) (_ []registeredRigBinding, stale []staleRegisteredCity, _ error, deferredLoadErr error) {
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	cities, err := reg.List()
	if err != nil {
		return nil, nil, err, nil
	}
	var matched []registeredRigBinding
	var loadErrors []string
	for _, c := range cities {
		siteBinding, siteErr := config.LoadSiteBinding(fsys.OSFS{}, c.Path)
		// A match-only scan skips the config load for cities that cannot
		// contribute a match.
		//
		// This is what keeps the fail-closed behavior above from turning one
		// clone into a machine-wide outage. Every registered city's load is a
		// chance to hit a busy repo cache, and a busy cache now aborts the whole
		// scan — so without this, `gc import install` in any one city would
		// break rig resolution in all of them. Filtering first narrows that
		// exposure to the cities that could actually answer the question.
		//
		// site.toml names a superset of the rigs siteBoundRigBindings keeps —
		// it records where a rig lives, not whether city.toml still declares
		// it — so a city with no candidate here has none after pruning either.
		// Cities that do have one are loaded and pruned exactly as before.
		//
		// The condition deliberately does not mention mode. It once read
		// `mode.advisory && ...`, which left the two modes disagreeing about a
		// city that cannot match but can still fail to load: the unfiltered
		// scan collected that load error, and one load error fails any scan
		// that also matched something (below). Same registry, same rig, a
		// different answer depending on who asked — which is what the advisory
		// contract forbids, and eager discovery captures the city it picks into
		// pack-command closures the user later runs. With mode out of the
		// condition the two traversals are identical by construction.
		//
		// failOnLoadError is a different request: report everything wrong with
		// the registry, not just what matched. A caller asking for that is
		// asking about the cities this filter would skip — a vanished city.toml
		// or an unloadable include is exactly the diagnostic they came for — so
		// it turns the filter off and pays the full scan for it.
		//
		// A malformed site.toml also disables the filter for that city: it is
		// not evidence of anything, so the city is loaded and its error
		// reported below.
		if !failOnLoadError && siteErr == nil && !siteBindingHasCandidate(c, siteBinding, match) {
			continue
		}
		cfg, err := loadRegisteredCityConfig(c.Path, mode)
		if err != nil {
			// A busy repo cache is the one failure an advisory scan must not
			// absorb: quietly skipping the city would let resolution settle on
			// a different one purely because a clone happened to be running.
			if errors.Is(err, config.ErrRepoCacheBusy) {
				return nil, stale, fmt.Errorf("loading registered city rig bindings: %s: %w", registeredCityLabel(c), err), nil
			}
			// Tolerate stale registry entries whose city.toml has been
			// deleted out from under the registry, but keep missing includes
			// or other config dependencies as load errors.
			if cityTOML, ok := missingRootCityTOML(err, c.Path); ok {
				stale = append(stale, staleRegisteredCity{Label: registeredCityLabel(c), Path: cityTOML})
				continue
			}
			loadErrors = append(loadErrors, registeredCityLoadError(c, err))
			continue
		}
		if siteErr != nil {
			loadErrors = append(loadErrors, registeredCityLoadError(c, siteErr))
			continue
		}
		for _, binding := range siteBoundRigBindings(c, cfg, siteBinding) {
			if match(binding.candidate()) {
				matched = append(matched, binding)
			}
		}
	}
	if len(loadErrors) > 0 && (failOnLoadError || len(matched) > 0) {
		return nil, stale, fmt.Errorf("loading registered city rig bindings: %s", strings.Join(loadErrors, "; ")), nil
	}
	if len(loadErrors) > 0 {
		return matched, stale, nil, fmt.Errorf("loading registered city rig bindings: %s", strings.Join(loadErrors, "; "))
	}
	return matched, stale, nil, nil
}

// siteBindingHasCandidate reports whether any machine-local binding in the
// city's .gc/site.toml could satisfy match. It is a pre-filter, not an answer:
// site.toml records where a rig lives, not whether city.toml still declares it,
// so a candidate here is only a reason to load the config and check properly.
//
// It runs match over exactly the candidates siteBoundRigBindings prunes from,
// so "no candidate here" implies "no binding after pruning" by construction.
func siteBindingHasCandidate(city supervisor.CityEntry, siteBinding *config.SiteBinding, match func(rigCandidate) bool) bool {
	return slices.ContainsFunc(siteRigCandidates(city, siteBinding), match)
}

// loadRegisteredCityConfig loads one registered city's config for a rig-binding
// scan. An advisory scan refuses to wait on the repo-cache lock, and takes
// builtin packs as they already are on disk — the same staleness window shell
// completion has always accepted.
func loadRegisteredCityConfig(cityPath string, mode contextResolutionMode) (*config.City, error) {
	if mode.advisory {
		return loadCityConfigAdvisory(cityPath)
	}
	return loadCityConfig(cityPath, io.Discard)
}

func registeredCityLoadError(city supervisor.CityEntry, err error) string {
	label := registeredCityLabel(city)
	base := fmt.Sprintf("%s: %v", label, err)
	if strings.Contains(err.Error(), "unsupported PackV1 order path") {
		return base + fmt.Sprintf(
			" (registered city %q still has a legacy order layout; run `gc --city %s doctor` for migration diagnostics, then rename legacy orders to flat orders/<name>.toml)",
			label, label)
	}
	return base
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
	eventsCfg := config.EventsConfig{}
	if cfg, err := loadCityConfig(cityPath, io.Discard); err == nil {
		eventsCfg = cfg.Events
	}
	rec, err := newFileEventsRecorder(
		filepath.Join(cityPath, ".gc", "events.jsonl"), eventsCfg, stderr)
	if err != nil {
		return events.Discard
	}
	return rec
}

// eventActor returns the public actor identity for events.
// Prefer the session alias when present, but preserve GC_AGENT fallback for
// managed-session hooks and older event-emitting contexts. BEADS_ACTOR is
// the cross-process identity signal shared with bd; falling through to it
// before "human" lets supervisor-spawned hooks (e.g., bd on_close →
// `gc event emit`) be attributed correctly to the controller or the order
// that triggered the close.
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
	if beadsActor := strings.TrimSpace(os.Getenv("BEADS_ACTOR")); beadsActor != "" {
		return beadsActor
	}
	return "human"
}

// openCityStore locates the city root from the current directory and opens a
// Store using the configured provider. On error it writes to stderr and returns
// nil plus an exit code.
func openCityStore(stderr io.Writer, cmdName string) (beads.Store, int) {
	store, _, code := openCityStoreWithPath(stderr, cmdName)
	return store, code
}

// openCityStoreWithPath locates the city root and opens its Store, returning
// the resolved city path used for the store.
func openCityStoreWithPath(stderr io.Writer, cmdName string) (beads.Store, string, int) {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err) //nolint:errcheck // best-effort stderr
		return nil, "", 1
	}
	store, err := openCityStoreAt(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", cmdName, err)                   //nolint:errcheck // best-effort stderr
		fmt.Fprintln(stderr, "hint: run \"gc doctor\" for diagnostics") //nolint:errcheck // best-effort stderr
		return nil, "", 1
	}
	return store, cityPath, 0
}

// openCityStoreAt opens a bead store at the given city path.
// Used by the controller (which already knows the city path) and by
// openCityStore (which resolves the path first). Keep the passed city path
// authoritative; rerouting through cityForStoreDir would let inherited
// GC_CITY override an explicit --city resolution.
func openCityStoreAt(cityPath string) (beads.Store, error) {
	result, err := openCityStoreResultAt(cityPath)
	if err != nil {
		return nil, err
	}
	return result.Store, nil
}

func openCityStoreResultAt(cityPath string) (beads.StoreOpenResult, error) {
	return openStoreResultAtForCity(cityPath, cityPath)
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
	return openStoreAtForCityWithAuthority(storePath, cityPath, false)
}

// openStoreAtForCityWithConfig is openStoreAtForCity for a caller that already
// holds this city's config. Opening a store resolves the conditional-writes
// mode from config, which otherwise means loading the whole city config —
// builtin-cache readiness and pack expansion included — again inside the open.
// A nil config keeps the loading behavior, matching nativeDoltOpenEnvForScope.
func openStoreAtForCityWithConfig(storePath, cityPath string, cfg *config.City) (beads.Store, error) {
	result, err := openStoreResultAtForCityWithConfig(storePath, cityPath, cfg, gate.ModeUnset, false, false)
	if err != nil {
		return nil, err
	}
	return result.Store, nil
}

func openAuthoritativeStoreAtForCity(storePath, cityPath string) (beads.Store, error) {
	return openStoreAtForCityWithAuthority(storePath, cityPath, true)
}

func openStoreAtForCityWithAuthority(storePath, cityPath string, authoritative bool) (beads.Store, error) {
	result, err := openStoreResultAtForCityWithAuthority(storePath, cityPath, gate.ModeUnset, false, authoritative)
	if err != nil {
		return nil, err
	}
	return result.Store, nil
}

func openStoreResultAtForCity(storePath, cityPath string) (beads.StoreOpenResult, error) {
	return openStoreResultAtForCityWithMode(storePath, cityPath, gate.ModeUnset, false)
}

// openStoreResultAtForCityWithMode is openStoreResultAtForCity with the
// conditional-writes mode supplied by the caller instead of re-resolved from
// the on-disk config. Controller-owned reopens use it to carry the
// boot-latched mode: re-resolving from disk on a reload would flip the city
// store's write discipline mid-process while rig stores keep the boot mode —
// exactly the mixed-writer state the process latch exists to prevent.
func openStoreResultAtForCityWithMode(storePath, cityPath string, modeOverride gate.Mode, haveMode bool) (beads.StoreOpenResult, error) {
	return openStoreResultAtForCityWithAuthority(storePath, cityPath, modeOverride, haveMode, false)
}

func openStoreResultAtForCityWithAuthority(storePath, cityPath string, modeOverride gate.Mode, haveMode, authoritative bool) (beads.StoreOpenResult, error) {
	return openStoreResultAtForCityWithConfig(storePath, cityPath, nil, modeOverride, haveMode, authoritative)
}

// openStoreResultAtForCityWithConfig is openStoreResultAtForCityWithAuthority
// with the city config supplied by a caller that already loaded it. A nil
// config is loaded here, which is what every caller outside the bd scope
// resolution path passes.
func openStoreResultAtForCityWithConfig(storePath, cityPath string, cfg *config.City, modeOverride gate.Mode, haveMode, authoritative bool) (beads.StoreOpenResult, error) {
	runtimeCityPath := cityPath
	if runtimeCityPath == "" {
		runtimeCityPath = cityForStoreDir(storePath)
	}
	if cfg == nil {
		cfg, _ = loadCityConfig(runtimeCityPath, io.Discard)
	} else {
		// Loading the config would have run the builtin-cache readiness pass.
		// Reusing one must not skip that self-heal for a city this process has
		// never readied.
		_ = ensureBuiltinRuntimeAssetsForSuppliedConfig(runtimeCityPath, io.Discard)
	}
	scopeRoot := resolveStoreScopeRoot(runtimeCityPath, storePath)
	provider := rawBeadsProviderForScope(scopeRoot, runtimeCityPath)
	if authoritative {
		provider = authoritativeBeadsProviderForScope(scopeRoot, runtimeCityPath)
	}
	switch strings.TrimSpace(provider) {
	case "sqlite", "sqlite-cgo", "coordstore":
		return beads.StoreOpenResult{}, fmt.Errorf(
			"beads provider %q is no longer supported: the sqlite coordination-store experiment has been removed; "+
				"update provider in city.toml to a supported value such as %q, or remove the setting to use the default",
			provider, "doltlite")
	}
	mode := resolvedConditionalWritesMode(cfg)
	if haveMode {
		mode = modeOverride
	}
	result, err := beads.OpenStoreAtForCity(context.Background(), beads.StoreOpenOptions{
		ScopeRoot:         scopeRoot,
		CityPath:          runtimeCityPath,
		Provider:          provider,
		PreflightChecker:  newBeadsPreflightChecker(runtimeCityPath, provider),
		Logger:            slog.Default(),
		ConditionalWrites: mode,
		OnConditionalWritesDegraded: func() func(beads.ConditionalWritesDegrade) {
			flags, resolved := resolvedConditionalWritesFlags(cfg)
			return lazyConditionalWritesDegradeEmitter(
				runtimeCityPath, conditionalWritesStoreID(scopeRoot, runtimeCityPath), flags, resolved)
		}(),
		OpenFileStore: func() (beads.Store, error) {
			return openCompatibleFileStore(scopeRoot, runtimeCityPath)
		},
		OpenBdStore: func() (beads.Store, error) {
			if err := requireBdBinaryForCity(runtimeCityPath); err != nil {
				return nil, err
			}
			return openBdStoreAtWithConfig(scopeRoot, runtimeCityPath, cfg)
		},
		OpenExecStore: func() (beads.Store, error) {
			return openExecStoreAtForCityWithConfig(provider, scopeRoot, runtimeCityPath, cfg)
		},
		OpenNativeStore: func() (beads.Store, error) {
			// Reuse the config this call already loaded. Passing nil made the
			// rig-scoped projection load the whole city config a second time,
			// pack expansion included, for the same city at the same moment.
			// The reopen hook below deliberately keeps re-loading: it fires long
			// after this open, where re-reading current state is the point.
			env, err := nativeDoltOpenEnvForScope(runtimeCityPath, cfg, scopeRoot)
			if err != nil {
				return nil, fmt.Errorf("project native store env %s: %w", scopeRoot, err)
			}
			// Reopen hook for the native read-path reconnect: the store's cached
			// open env pins the managed Dolt port as of open time, which is dead
			// after a hard-kill/rebind. Re-resolve the CURRENT env on every
			// reconnect — nativeDoltOpenEnvForScope re-reads the live port and
			// triggers managed-Dolt recovery/restart when the server is down
			// (allowRecovery=true), mirroring how each bd subprocess re-resolves
			// the port per command — then re-open against the live server via the
			// direct native path (which bypasses the factory preflight/identity
			// gate, so an absent scope project_id cannot block the reconnect).
			reopen := func(ctx context.Context) (beads.NativeStorage, error) {
				freshEnv, rerr := nativeDoltOpenEnvForScopeContext(ctx, runtimeCityPath, nil, scopeRoot)
				if rerr != nil {
					return nil, fmt.Errorf("re-resolve native store env %s: %w", scopeRoot, rerr)
				}
				return beads.OpenNativeStorage(ctx, scopeRoot, freshEnv)
			}
			return beads.OpenNativeDoltStoreAt(context.Background(), scopeRoot, env, beads.WithNativeReopen(reopen))
		},
	})
	if err != nil {
		return beads.StoreOpenResult{}, err
	}
	result.Store = wrapStoreWithBeadPolicies(result.Store, cfg)
	return result, nil
}

// requireBdBinaryForCity verifies that the logical bd command has either an
// ambient executable or the city-configured, absolute workspace pin. The
// runner keeps the logical command name as "bd" so its timeout, telemetry,
// and backup policy still apply while executing that pin.
func requireBdBinaryForCity(cityPath string) error {
	_, err := resolveBdBinaryForScope(cityPath, cityPath)
	if errors.Is(err, errBdNotOnPath) {
		return fmt.Errorf("bd not found in PATH (install beads or set GC_BEADS=file)")
	}
	return err
}

// openExecStoreAtForCityWithConfig opens the exec-provider store for a city.
// A caller that already holds this city's config passes it to avoid reloading
// it; a nil config is loaded here.
func openExecStoreAtForCityWithConfig(provider, scopeRoot, runtimeCityPath string, cfg *config.City) (beads.Store, error) {
	target, err := resolveConfiguredExecStoreTargetWithConfig(runtimeCityPath, scopeRoot, cfg)
	if err != nil {
		return nil, err
	}
	env := gcExecStoreEnv(runtimeCityPath, target, provider)
	if execProviderNeedsScopedDoltStoreEnv(provider) {
		if target.ScopeKind == "rig" {
			rigCfg := cfg
			if rigCfg == nil {
				loaded, err := loadCityConfig(runtimeCityPath, io.Discard)
				if err != nil {
					return nil, err
				}
				rigCfg = loaded
			}
			projected, err := bdRuntimeEnvForRigWithError(runtimeCityPath, rigCfg, target.ScopeRoot)
			if err != nil {
				return nil, err
			}
			copyExecProjectedBackendEnv(env, projected)
		} else {
			projected, err := bdRuntimeEnvWithError(runtimeCityPath)
			if err != nil {
				return nil, err
			}
			copyExecProjectedBackendEnv(env, projected)
		}
	}
	store := beadsexec.NewStore(strings.TrimPrefix(provider, "exec:"))
	store.SetEnv(env)
	return store, nil
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
	// Resolve symlinks so a city reached through a linked path (e.g. ~/gc ->
	// /real/city) yields the same scope root as the real path. Without this the
	// native-store identity gate sees an unregistered scope and rejects it
	// ("database project_id could not be confirmed"), silently degrading to the
	// bd-subprocess fallback.
	//
	// Normalize through pathutil, not bare EvalSymlinks: pathutil also collapses
	// the darwin /private alias. Resolving without that collapse maps an
	// already-canonical /var city path to a /private/var scope root, so the
	// scope no longer matches the city it was derived from.
	if resolved := pathutil.NormalizePathForCompare(scopeRoot); resolved != "" {
		scopeRoot = resolved
	}
	return scopeRoot
}

// openBdStoreAtWithConfig opens the bd-backed store at storePath for a city.
// A caller that already holds this city's config passes it to avoid reloading
// it; a nil config is loaded here.
func openBdStoreAtWithConfig(storePath, cityPath string, cfg *config.City) (beads.Store, error) {
	if filepath.Clean(storePath) == filepath.Clean(cityPath) {
		store := bdStoreForCity(storePath, cityPath)
		if optimized, ok := openOptimizedDoltliteStore(storePath, store); ok {
			return optimized, nil
		}
		return store, nil
	}
	if cfg == nil {
		loaded, err := loadCityConfig(cityPath, io.Discard)
		if err != nil {
			loaded = nil
		}
		cfg = loaded
	}
	store := bdStoreForRig(storePath, cityPath, cfg)
	if optimized, ok := openOptimizedDoltliteStore(storePath, store); ok {
		return optimized, nil
	}
	return store, nil
}
