package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/suspensionstate"
	"github.com/spf13/cobra"
)

// newSuspendCmd creates the "gc suspend [path]" command.
func newSuspendCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "suspend [path|name]",
		Short: "Suspend the city (all agents effectively suspended)",
		Long: `Suspends the city by recording an explicit "suspended" preference
in .gc/runtime/suspension-state.json (per-clone runtime state, not
committed).

This inherits downward — when the city is suspended, all agents are
effectively suspended regardless of their individual suspended fields.
The reconciler won't spawn agents, gc hook/prime return empty.

Use "gc resume" to restore.`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeCityNames,
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdSuspend(args, jsonOut, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSONL summary")
	return cmd
}

// newResumeCmd creates the "gc resume [path]" command.
func newResumeCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "resume [path|name]",
		Short: "Resume a suspended city",
		Long: `Resume a suspended city by recording an explicit "resumed" preference
in .gc/runtime/suspension-state.json. The override sticks across city
restarts even when [workspace] declares suspended_on_start = true.

Restores normal operation: the reconciler will spawn agents again and
gc hook/prime will return work. Use "gc agent resume" to resume
individual agents, or "gc rig resume" for rigs.`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeCityNames,
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdResume(args, jsonOut, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSONL summary")
	return cmd
}

// cmdSuspend is the CLI entry point for suspending the city.
func cmdSuspend(args []string, jsonOut bool, stdout, stderr io.Writer) int {
	// The brake must work when the API is wedged, so everything before the
	// direct write shares one deadline from here: resolving the city, its API
	// route and the request.
	ctx, cancel := context.WithTimeout(context.Background(), suspendAPITimeout)
	defer cancel()
	cityPath, err := resolveSuspendDir(args)
	if err != nil {
		fmt.Fprintf(stderr, "gc suspend: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	select {
	case a := <-suspendThroughAPIHook(ctx, cityPath):
		if a.client != nil {
			if a.err == nil {
				return writeCitySuspensionSuccess(stdout, stderr, cityPath, true, jsonOut)
			}
			if !api.ShouldFallback(a.client, a.err) {
				fmt.Fprintf(stderr, "gc suspend: %v\n", a.err) //nolint:errcheck // best-effort stderr
				return 1
			}
		}
		// No API serves the city, or a connection error: write directly.
	case <-ctx.Done():
		// The route or the request did not answer in time, which is a
		// connection error too.
	}
	code := doSuspendCity(fsys.OSFS{}, cityPath, true, jsonOut, stdout, stderr)
	if code == 0 {
		// Best effort: a running controller re-reads the file on its next
		// pass anyway; the poke only makes that pass come now.
		_, _ = sendControllerCommandWithTimeouts(cityPath, "poke", suspendPokeTimeout, suspendPokeTimeout, suspendPokeTimeout)
	}
	return code
}

// suspendAPITimeout bounds everything `gc suspend` does before it writes the
// suspension file itself: resolving the city and its API route, and the API
// request.
const suspendAPITimeout = 3 * time.Second

// suspendAPIAnswer is how gc suspend's attempt through the city's API ended:
// client is nil when no API serves the city, and err is the request's error.
type suspendAPIAnswer struct {
	client *api.Client
	err    error
}

// suspendThroughAPIHook starts gc suspend's attempt through the city's API.
// Indirected for tests, which keep the answer channel to wait out an attempt
// the command stopped waiting for.
var suspendThroughAPIHook = suspendThroughAPI

// suspendThroughAPI starts suspending the city through its API, when one
// serves it, and returns the channel its answer arrives on. Resolving the
// route (apiClient) takes no context: it probes the controller socket and,
// for a supervisor-hosted city, asks the supervisor which cities it runs, and
// either can hang as long as the request. So the attempt runs on its own
// goroutine and the caller stops waiting at ctx's deadline, which also ends
// the request. The channel holds the one answer and is then closed: the
// attempt never blocks on a caller that stopped waiting, and a receive after
// the caller took the answer returns once the attempt has ended. The CLI
// exits with an attempt it stopped waiting for still running.
func suspendThroughAPI(ctx context.Context, cityPath string) <-chan suspendAPIAnswer {
	answer := make(chan suspendAPIAnswer, 1)
	go func() {
		defer close(answer)
		c := apiClient(cityPath)
		if c == nil {
			answer <- suspendAPIAnswer{}
			return
		}
		answer <- suspendAPIAnswer{client: c, err: c.SuspendCityContext(ctx)}
	}()
	return answer
}

// suspendPokeTimeout bounds each step of `gc suspend`'s controller poke after
// a direct write.
const suspendPokeTimeout = time.Second

// cmdResume is the CLI entry point for resuming the city.
func cmdResume(args []string, jsonOut bool, stdout, stderr io.Writer) int {
	cityPath, err := resolveSuspendDir(args)
	if err != nil {
		fmt.Fprintf(stderr, "gc resume: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if c := apiClient(cityPath); c != nil {
		err := c.ResumeCity()
		if err == nil {
			return writeCitySuspensionSuccess(stdout, stderr, cityPath, false, jsonOut)
		}
		if !api.ShouldFallback(c, err) {
			fmt.Fprintf(stderr, "gc resume: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		// Connection error — fall through to direct mutation.
	}
	return doSuspendCity(fsys.OSFS{}, cityPath, false, jsonOut, stdout, stderr)
}

// resolveSuspendDir resolves the city directory from args or the current city.
func resolveSuspendDir(args []string) (string, error) {
	return resolveCommandCity(args)
}

// doSuspendCity records the explicit city suspension preference in
// .gc/runtime/suspension-state.json. The committable
// workspace.suspended_on_start flag is left untouched: callers
// explicit-suspend or explicit-resume via runtime state, and that
// override beats the committed default at every read.
func doSuspendCity(fs fsys.FS, cityPath string, suspend bool, jsonOut bool, stdout, stderr io.Writer) int {
	tomlPath := filepath.Join(cityPath, "city.toml")
	cmd := "gc suspend"
	if !suspend {
		cmd = "gc resume"
	}
	// Validate city.toml parses so an unrelated config error surfaces
	// clearly instead of being masked by the runtime-state write. Suspend is
	// the brake, so a broken config only warns there.
	if _, err := loadCityConfigForEditFS(fs, tomlPath); err != nil {
		if !suspend {
			fmt.Fprintf(stderr, "%s: %v (fix city.toml, then run %s again)\n", cmd, err, cmd) //nolint:errcheck // best-effort stderr
			return 1
		}
		fmt.Fprintf(stderr, "%s: warning: %v; suspending anyway\n", cmd, err) //nolint:errcheck // best-effort stderr
	}

	stateFile := citylayout.SuspensionStateFile(cityPath)
	if !suspend {
		want := false
		if err := suspensionstate.SetCitySuspended(fs, cityPath, &want); err != nil {
			fmt.Fprintf(stderr, "%s: %s: %v (fix the JSON or remove the file, then run %s again)\n", cmd, stateFile, err, cmd) //nolint:errcheck // best-effort stderr
			return 1
		}
		recordCitySuspension(cityPath, events.CityResumed, "", stderr)
		return writeCitySuspensionSuccess(stdout, stderr, cityPath, false, jsonOut)
	}

	discarded, err := suspensionstate.ForceCitySuspended(fs, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "%s: writing %s: %v\n", cmd, stateFile, err) //nolint:errcheck // best-effort stderr
		return 1
	}
	var note string
	if discarded != nil {
		note = replacedSuspensionNote(cityPath, discarded)
		fmt.Fprintf(stderr, "%s: %s\n", cmd, note) //nolint:errcheck // best-effort stderr
	}
	// The brake is recorded: report it before the steps below, which wait on
	// the city's event log and stores, not on the brake.
	code := writeCitySuspensionSuccess(stdout, stderr, cityPath, true, jsonOut)
	recordCitySuspension(cityPath, events.CitySuspended, note, stderr)
	retireSuspendedScopesWithoutController(cityPath, nil, true, stderr)
	return code
}

// recordCitySuspension records a city.suspended or city.resumed event in the
// city whose state changed, not the ambient one. openCityRecorder re-resolves
// the city from cwd/env, which is not cityPath whenever the target was named
// explicitly (`gc suspend <dir>`) from inside a different, live city — that
// leaked city.suspended/city.resumed into an unrelated event log (ga-41g9gr).
func recordCitySuspension(cityPath, eventType, message string, stderr io.Writer) {
	openCityRecorderAt(cityPath, stderr).Record(events.Event{
		Type:    eventType,
		Actor:   eventActor(),
		Message: message,
	})
}

// replacedSuspensionNote reports a city suspend that replaced the city's
// suspension file because its content did not decode: the file and the
// discarded error.
func replacedSuspensionNote(cityPath string, discarded error) string {
	return fmt.Sprintf("replaced unreadable %s (%v)", citylayout.SuspensionStateFile(cityPath), discarded)
}

func writeCitySuspensionSuccess(stdout, stderr io.Writer, cityPath string, suspend bool, jsonOut bool) int {
	if jsonOut {
		action := "resume"
		message := "City resumed."
		if suspend {
			action = "suspend"
			message = "City suspended."
		}
		return writeLifecycleActionJSONOrExit(stdout, stderr, "gc "+action, lifecycleActionJSON{
			Command:  action,
			Action:   action,
			Message:  message,
			CityPath: cityPath,
		})
	}
	if suspend {
		fmt.Fprintf(stdout, "City suspended (%s)\n", cityPath) //nolint:errcheck // best-effort stdout
		return 0
	}
	fmt.Fprintf(stdout, "City resumed (%s)\n", cityPath) //nolint:errcheck // best-effort stdout
	return 0
}

// citySuspended is the canonical predicate for "is the city suspended
// right now?". It loads the runtime suspension state from the
// ambient city (resolveCity) and merges it with the workspace's
// effective suspended_on_start. The deprecated `[workspace] suspended`
// field is honored as an alias for `suspended_on_start` via
// [config.Workspace.EffectiveSuspendedOnStart], so existing city.toml
// files keep their behavior on upgrade.
//
// Callers that already have a pre-loaded [suspensionstate.State]
// (e.g. the reconciler or snapshot builder) should call
// [citySuspendedWithState] instead to avoid the extra read.
func citySuspended(cfg *config.City) bool {
	cityPath, _ := resolveCity()
	return citySuspendedWithState(cfg, loadSuspensionStateBestEffort(cityPath))
}

// citySuspendedWithState is the pure form for callers that already
// loaded the runtime suspension state.
func citySuspendedWithState(cfg *config.City, st suspensionstate.State) bool {
	return effectiveCitySuspended(cfg, st)
}

// effectiveCitySuspended is the canonical "is the city suspended
// right now" predicate. It honors the GC_SUSPENDED=1 escape hatch
// (used by integration tests and ops to override without touching
// files), then the runtime state file, then falls back to the
// workspace's effective suspended_on_start (which honors the
// deprecated `suspended` field as an alias).
func effectiveCitySuspended(cfg *config.City, st suspensionstate.State) bool {
	if os.Getenv("GC_SUSPENDED") == "1" {
		return true
	}
	if cfg == nil {
		return suspensionstate.EffectiveCitySuspended(st, false)
	}
	return suspensionstate.EffectiveCitySuspended(st, cfg.Workspace.EffectiveSuspendedOnStart())
}

// isAgentEffectivelySuspended reports whether an agent is suspended.
// True if any of: city is suspended, agent is individually suspended,
// or the agent's rig is effectively suspended (runtime override or
// SuspendedOnStart). Suspension inherits downward.
//
// Callers that already have pre-loaded runtime state should call
// [isAgentEffectivelySuspendedWith] to avoid the per-call disk read.
func isAgentEffectivelySuspended(cfg *config.City, a *config.Agent) bool {
	cityPath, _ := resolveCity()
	return isAgentEffectivelySuspendedWith(cfg, cityPath, a, loadSuspensionStateBestEffort(cityPath))
}

// isAgentEffectivelySuspendedWith is like isAgentEffectivelySuspended
// but takes a pre-loaded runtime state so callers in hot paths don't
// re-read the file.
//
// The agent's rig is resolved path-aware via configuredRigName — the same
// resolver the desired-state build uses (agentInSuspendedRig). Matching the
// rig by name only (a.Dir == rig.Name) missed rig-bound agents whose Dir is a
// filesystem path rather than the bare rig name — notably third-party-pack
// agents bound through a dir override. For those, the desired-state build
// (path-aware) dropped the session while this gate (name-only) reported the
// agent awake, so a suspended rig never quiesced them: it drained and re-woke
// each tick. Keeping the two gates on the same resolver closes that gap.
func isAgentEffectivelySuspendedWith(cfg *config.City, cityPath string, a *config.Agent, st suspensionstate.State) bool {
	_, _, suspended := agentSuspensionCauseWith(cfg, cityPath, a, st)
	return suspended
}

func agentSuspensionCauseWith(cfg *config.City, cityPath string, a *config.Agent, st suspensionstate.State) (scope, name string, suspended bool) {
	if effectiveCitySuspended(cfg, st) {
		return "city", "", true
	}
	if a == nil {
		return "", "", false
	}
	if a.Suspended {
		return "agent", a.QualifiedName(), true
	}
	if cfg == nil {
		return "", "", false
	}
	rigName := configuredRigName(cityPath, a, cfg.Rigs)
	if rigName == "" {
		return "", "", false
	}
	for i := range cfg.Rigs {
		if cfg.Rigs[i].Name != rigName {
			continue
		}
		if suspensionstate.EffectiveRigSuspended(st, cfg.Rigs[i].Name, cfg.Rigs[i].EffectiveSuspendedOnStart()) {
			return "rig", rigName, true
		}
		return "", "", false
	}
	return "", "", false
}
