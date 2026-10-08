package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/spf13/cobra"
)

func newStopCmd(stdout, stderr io.Writer) *cobra.Command {
	var wallClockTimeout time.Duration
	var force bool
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "stop [path|name]",
		Short: "Stop all agent sessions in the city",
		Long: `Stop all agent sessions in the city with graceful shutdown.

Sends interrupt signals to running agents, waits for the configured
shutdown timeout, then force-kills any remaining sessions. Also stops
the Dolt server and cleans up orphan sessions. If a controller is
running, delegates shutdown to it.

If the city is registered with the machine-wide supervisor, stop also
unregisters it (equivalent to a following "gc unregister") — the city
will not be found by name or auto-started again until it is re-registered
with "gc register". Use "gc unregister" directly to remove a registration
without stopping sessions.

gc stop reports "City stopped." only when it could confirm that every
session stopped. If it could not list the runtime's sessions completely,
or could not check whether a session is still running, it names what it
could not verify, still stops every session it did see, and exits
non-zero; a supervisor registration is restored. Resolve the reported
error and run gc stop again.

Use --timeout=DURATION to cap the wall-clock time gc stop will spend
before giving up; the default budgets configured session interrupt and
stop waves, the configured shutdown grace wait, and a second orphan
cleanup pass. Use --force to skip the interrupt grace period and go
straight to kill.`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeCityNames,
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdStopJSON(args, stdout, stderr, wallClockTimeout, force, jsonOut) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().DurationVar(&wallClockTimeout, "timeout", 0, "wall-clock cap for the stop sequence (0 = derive from city config)")
	cmd.Flags().BoolVar(&force, "force", false, "skip the interrupt grace period and force-kill all sessions immediately")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSONL summary")
	return cmd
}

var sessionProviderForStopCity = newSessionProviderForCity

// cmdStop stops the city by terminating all configured agent sessions.
// If a path is given, operates there; otherwise uses cwd.
//
// wallClockTimeout caps how long cmdStop will wait for the shutdown
// sequence; if 0, a default derived from cfg.Daemon.ShutdownTimeoutDuration
// is used. force=true skips the interrupt grace period (gracefulStopAll
// runs with timeout=0, going straight to kill).
func cmdStop(args []string, stdout, stderr io.Writer, wallClockTimeout time.Duration, force bool) int {
	return cmdStopJSON(args, stdout, stderr, wallClockTimeout, force, false)
}

type stopCommandOutcome struct {
	code         int
	cityPath     string
	unregistered bool
}

func cmdStopJSON(args []string, stdout, stderr io.Writer, wallClockTimeout time.Duration, force bool, jsonOut bool) int {
	var outcome stopCommandOutcome
	if wallClockTimeout > 0 {
		unregisterTx := newSupervisorUnregisterTransaction()
		outcome = runStopWithWallClockCap(wallClockTimeout, stderr, unregisterTx, func() stopCommandOutcome {
			return cmdStopJSONSequence(args, stdout, stderr, force, jsonOut, true, unregisterTx)
		})
	} else {
		// The uncapped path holds the same pending-unregister transaction as
		// the capped one: a stop that fails after removing the registration —
		// a managed provider that refuses to shut down, an invalid config the
		// body cannot recover — must hand the entry back rather than leave a
		// live city unregistered. This mirrors the capped arm's accept/rollback
		// exactly; the deferred success message is part of the same contract.
		unregisterTx := newSupervisorUnregisterTransaction()
		outcome = cmdStopJSONSequence(args, stdout, stderr, force, jsonOut, false, unregisterTx)
		if outcome.code == 0 {
			unregisterTx.commit()
		} else {
			writeSupervisorUnregisterRollback(stderr, "gc stop", "stop failed after unregistering city", unregisterTx.rollback())
		}
	}
	if outcome.code != 0 {
		return outcome.code
	}
	if jsonOut {
		return writeCityStopSuccess(stdout, stderr, outcome.cityPath, force, outcome.unregistered)
	}
	fmt.Fprintln(stdout, "City stopped.") //nolint:errcheck // best-effort stdout
	return 0
}

func cmdStopJSONSequence(args []string, stdout, stderr io.Writer, force bool, jsonOut bool, wallClockCapApplied bool, unregisterTx *supervisorUnregisterTransaction) stopCommandOutcome {
	cityPath, err := resolveStopCityPath(args)
	if err != nil {
		fmt.Fprintf(stderr, "gc stop: %v\n", err) //nolint:errcheck // best-effort stderr
		return stopCommandOutcome{code: 1}
	}

	stopStdout := stdout
	if jsonOut {
		stopStdout = io.Discard
	}

	unregisteredFromSupervisor := false
	handled, code, ownership := unregisterCityFromSupervisorForStop(cityPath, stopStdout, stderr, "gc stop", force, unregisterTx)
	if ownership != nil {
		// The supervisor stopped the controller; hold its lock until the bead
		// store below is retired. It is released when this returns, before
		// the caller commits or rolls back the unregister, so a restored
		// registration never finds the lock still held by this stop.
		defer ownership.Close() //nolint:errcheck // releasing the flock cannot fail meaningfully
	}
	if handled {
		if code != 0 {
			return stopCommandOutcome{code: code, cityPath: cityPath}
		}
		unregisteredFromSupervisor = true
		// Retained ownership proves the supervisor path ran; re-probing the
		// supervisor could fall through to the standalone stop while this
		// process still holds the lock that path waits on.
		if ownership != nil || supervisorAliveHook() != 0 {
			if !stopCityManagedBeadsProviderAfterSuccessfulStop(cityPath, stderr) {
				return stopCommandOutcome{code: 1, cityPath: cityPath}
			}
			warnInvalidConfigAfterSuccessfulStop(cityPath, stderr)
			return stopCommandOutcome{cityPath: cityPath, unregistered: unregisteredFromSupervisor}
		}
	}

	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		if handled, code := stopManagedRuntimeWithoutConfig(cityPath, err, stopStdout, stderr, force); handled {
			return stopCommandOutcome{code: code, cityPath: cityPath, unregistered: unregisteredFromSupervisor}
		}
		fmt.Fprintf(stderr, "gc stop: %v\n", err) //nolint:errcheck // best-effort stderr
		return stopCommandOutcome{code: 1, cityPath: cityPath}
	}

	stopLoadedCity := func() stopCommandOutcome {
		return stopCommandOutcome{
			code:         cmdStopBodyWithoutSuccess(cityPath, cfg, force, stopStdout, stderr),
			cityPath:     cityPath,
			unregistered: unregisteredFromSupervisor,
		}
	}
	if wallClockCapApplied {
		return stopLoadedCity()
	}
	return runStopWithWallClockCap(defaultStopWallClockTimeout(cfg), stderr, nil, stopLoadedCity)
}

func runStopWithWallClockCap(wallClockCap time.Duration, stderr io.Writer, unregisterTx *supervisorUnregisterTransaction, stop func() stopCommandOutcome) stopCommandOutcome {
	doneCh := make(chan stopCommandOutcome, 1)
	bodyDone := make(chan struct{})
	go func() {
		defer close(bodyDone)
		doneCh <- stop()
	}()
	if h := stopBodyLifecycleHook; h != nil {
		h(bodyDone)
	}
	timer := time.NewTimer(wallClockCap)
	defer timer.Stop()

	select {
	case out := <-doneCh:
		if unregisterTx != nil {
			if out.code == 0 {
				unregisterTx.commit()
			} else {
				writeSupervisorUnregisterRollback(stderr, "gc stop", "stop failed after unregistering city", unregisterTx.rollback())
			}
		}
		return out
	case <-timer.C:
		var rollback supervisorUnregisterRollback
		if unregisterTx != nil {
			rollback = unregisterTx.rollback()
		}
		fmt.Fprintf(stderr, "gc stop: timed out after %s; some sessions may not have stopped — retry with --force if stop is wedged, or raise --timeout for large stop sets\n", wallClockCap) //nolint:errcheck // best-effort stderr
		writeSupervisorUnregisterRollback(stderr, "gc stop", "wall-clock timeout", rollback)
		return stopCommandOutcome{code: 1}
	}
}

// stopBodyLifecycleHook receives the bounded stop worker's done channel.
// Tests with providers or supervisor waits that block past the wall-clock
// cap register this hook so they can wait for the worker to finish,
// preventing the leaked goroutine from racing on package-level stop hooks
// against a later test.
var stopBodyLifecycleHook func(<-chan struct{})

func writeCityStopSuccess(stdout, stderr io.Writer, cityPath string, force, unregistered bool) int {
	return writeLifecycleActionJSONOrExit(stdout, stderr, "gc stop", lifecycleActionJSON{
		Command:      "stop",
		Action:       "stop",
		Message:      "City stopped.",
		CityPath:     cityPath,
		Force:        lifecycleBoolPtr(force),
		Unregistered: lifecycleBoolPtr(unregistered),
	})
}

func resolveStopCityPath(args []string) (string, error) {
	if len(args) == 0 {
		return resolveCommandCity(nil)
	}
	// A name-shaped positional may be a registered city name or a local rig
	// directory; route it through the shared name resolver so a slashless rig
	// dir still resolves to its owning city without reopening the bare-name
	// walk-up footgun. Path-shaped args keep the exact stop path resolver.
	if classifyCityRef(args[0]) == cityRefName {
		ctx, err := resolveCityNameContext(args[0], func(name string) (resolvedContext, error) {
			cp, perr := stopCityPathFromArg(name)
			return resolvedContext{CityPath: cp}, perr
		})
		if err != nil {
			return "", err
		}
		return ctx.CityPath, nil
	}
	return stopCityPathFromArg(args[0])
}

// stopCityPathFromArg resolves a path-shaped stop argument (or a local city) to
// a city path, trying an exact city path, then a rig path, then an upward city
// walk — the original path-only behavior, now invoked as resolveCityRef's path
// resolver.
func stopCityPathFromArg(ref string) (string, error) {
	abs, err := filepath.Abs(ref)
	if err != nil {
		return "", err
	}
	if cityPath, err := validateCityPath(abs); err == nil {
		return cityPath, nil
	}
	ctx, ok, rigErr := resolveRigPathToContext(abs)
	if rigErr == nil && ok {
		return ctx.CityPath, nil
	}
	cityPath, findErr := findCity(abs)
	if findErr == nil {
		return cityPath, nil
	}
	if rigErr != nil {
		return "", rigErr
	}
	return "", findErr
}

// defaultStopWallClockTimeout returns the wall-clock cap used by cmdStop
// when --timeout is not set. Each pass budgets three sequential phases:
// interrupt provider dispatch, the configured post-interrupt grace wait, and
// bounded force-stop waves. A second pass covers orphan cleanup. Unknown extra
// live pool sessions or orphans can still require an explicit --timeout from
// the operator.
func defaultStopWallClockTimeout(cfg *config.City) time.Duration {
	base := 5 * time.Second
	if cfg != nil {
		if d := cfg.Daemon.ShutdownTimeoutDuration(); d > 0 {
			base = d
		}
	}
	targets := estimatedConfiguredStopTargets(cfg)
	interruptWaves := ceilDiv(targets, defaultMaxParallelInterrupts)
	stopWaves := ceilDiv(targets, defaultMaxParallelStopsPerWave)
	onePass := time.Duration(interruptWaves)*interruptPerTargetTimeout(cfg) +
		base +
		time.Duration(stopWaves)*stopPerTargetTimeoutDefault
	return 2*onePass + stopPerTargetTimeoutDefault
}

func estimatedConfiguredStopTargets(cfg *config.City) int {
	if cfg == nil || len(cfg.Agents) == 0 {
		return 1
	}
	total := 0
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		if len(agent.NamepoolNames) > 0 {
			total += len(agent.NamepoolNames)
			continue
		}
		if maxSessions := agent.EffectiveMaxActiveSessions(); maxSessions != nil {
			switch {
			case *maxSessions == 0:
				continue
			case *maxSessions > 0:
				total += *maxSessions
				continue
			}
		}
		if minSessions := agent.EffectiveMinActiveSessions(); minSessions > 1 {
			total += minSessions
			continue
		}
		total++
	}
	if total < 1 {
		return 1
	}
	return total
}

func ceilDiv(n, d int) int {
	if n <= 0 {
		return 0
	}
	if d <= 0 {
		return n
	}
	return (n + d - 1) / d
}

func cmdStopBody(cityPath string, cfg *config.City, force bool, stdout, stderr io.Writer) int { //nolint:unparam // compatibility wrapper preserves the production-shaped force seam for direct tests
	code := cmdStopBodyWithoutSuccess(cityPath, cfg, force, stdout, stderr)
	if code == 0 {
		fmt.Fprintln(stdout, "City stopped.") //nolint:errcheck // best-effort stdout
	}
	return code
}

// cmdStopBodyWithoutSuccess performs the stop flow without emitting the final
// success record. The command writes that record only after the bounded worker
// returns, so a timed-out worker cannot report a late success.
func cmdStopBodyWithoutSuccess(cityPath string, cfg *config.City, force bool, stdout, stderr io.Writer) int {
	cityName := loadedCityName(cfg, cityPath)

	// If a controller is running, ask it to shut down (it stops agents).
	stopResult := tryStopControllerWithForce(cityPath, stdout, force)
	switch stopResult.outcome {
	case controllerStopAcknowledged:
		ownership, err := acquireStoppedControllerOwnership(cityPath, cfg.Daemon.ShutdownTimeoutDuration()+15*time.Second)
		if err != nil {
			fmt.Fprintf(stderr, "gc stop: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		defer ownership.Close() //nolint:errcheck // releasing the flock cannot fail meaningfully
		// Controller handled the shutdown — still stop the bead store, and
		// only after waiting for the controller to be gone: a live reader
		// restarts a provider-owned proxy the moment it is retired. The
		// controller lock stays held until this returns so a restarted or
		// second controller cannot come up against a provider being retired.
		if err := shutdownBeadsProviderForStop(cityPath); err != nil {
			fmt.Fprintf(stderr, "gc stop: bead store: %v\n", err) //nolint:errcheck // best-effort stderr
		}
		return 0
	case controllerStopDefinitePreEntryUnavailable:
		// No stop request entered a controller, so direct cleanup may proceed.
	case controllerStopMayHaveEntered, controllerStopOutcomeInvalid:
		fmt.Fprintf(stderr, "gc stop: %v\n", stopResult.failClosedError()) //nolint:errcheck // best-effort stderr
		return 1
	default:
		fmt.Fprintf(stderr, "gc stop: %v\n", stopResult.failClosedError()) //nolint:errcheck // best-effort stderr
		return 1
	}

	// A city that is already stopped has nothing for this stop to do, and on
	// a provider-owned proxied store the rest of this flow is not free: the
	// session provider's snapshot and every session lookup read the store,
	// and each bd read restarts the scope's proxy and Dolt child, only for the
	// final provider stop to retire them again. Answer from the runtime and
	// the proxy records instead, without touching the store.
	if stopped, sp := cityAlreadyStoppedForStop(cfg, cityPath); stopped {
		teardownServerForStop(sp, stderr, "gc stop")
		return 0
	}

	store, _ := openCityStoreForStop(cityPath)
	// Every store consumer in this stop flow is session-class (sleep-reason marks,
	// session-name lookups, session-runtime stop, orphan cleanup), so route the
	// whole flow through the session coordination-class store for relocation-safety.
	sessStore := cliSessionStore(store, cfg, cityPath)
	markCityStopSessionSleepReason(sessionFrontDoor(sessStore), stderr)

	sp, err := sessionProviderForStopCity(cfg, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc stop: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	st := cfg.Workspace.SessionTemplate
	var sessionNames []string
	desired := make(map[string]bool, len(cfg.Agents))
	for _, a := range cfg.Agents {
		sp0 := scaleParamsFor(&a)
		qn := a.QualifiedName()
		if !a.SupportsInstanceExpansion() {
			// Non-expanding template.
			sn := lookupSessionNameOrLegacy(sessStore, cityName, qn, st)
			sessionNames = append(sessionNames, sn)
			desired[sn] = true
		} else {
			// Pool agent: resolve runtime session names from beads first, then legacy discovery.
			for _, ref := range resolvePoolSessionRefs(sessStore, cfg, a.Name, a.Dir, sp0, &a, cityName, st, sp, stderr) {
				sessionNames = append(sessionNames, ref.sessionName)
				desired[ref.sessionName] = true
			}
		}
	}
	recorder := openCityRecorderAt(cityPath, stderr)

	graceTimeout := cfg.Daemon.ShutdownTimeoutDuration()
	if force {
		// gracefulStopAll treats timeout=0 as "skip interrupt pass, kill immediately".
		graceTimeout = 0
	}

	code := doStopWithoutSuccess(sessionNames, sp, cfg, sessStore, graceTimeout, recorder, stdout, stderr)

	// Clean up orphan sessions (sessions with the city prefix that are
	// not in the current config). An orphan sweep that could not see the
	// whole runtime leaves the stop unconfirmed, but the remaining cleanup
	// below still runs: it only retires things this stop owns.
	if !stopOrphans(sp, desired, cfg, sessionFrontDoor(sessStore), graceTimeout, recorder, stdout, stderr) && code == 0 {
		fmt.Fprintln(stderr, "gc stop: stop not confirmed: the runtime inventory was incomplete, so sessions outside the configuration may still be running; resolve the runtime error and run gc stop again") //nolint:errcheck // best-effort stderr
		code = 1
	}

	teardownServerForStop(sp, stderr, "gc stop")

	// Stop the bead store's backing service LAST, and only here. The order
	// this function runs in is load-bearing for a provider-owned proxied city:
	//
	//   controller (agents drain with it) -> sessions -> orphan sessions ->
	//   runtime server teardown -> bd dolt stop per provider-owned scope
	//
	// bd restarts a proxied scope's proxy and Dolt child on ANY read (beads
	// cmd/bd/main.go:1758 — BEADS_DOLT_AUTO_START does not reach that path),
	// so a single surviving reader after this point resurrects the processes
	// this call just retired. Retiring first and stopping readers afterwards
	// would leave the city up with a live proxy every time.
	//
	// The call is re-runnable: `bd dolt stop` is idempotent on rc.2 (exit 0,
	// stopped/verified true, with or without a live proxy). A second `gc stop`
	// of a fully stopped proxied city does not get this far: it returns at
	// cityAlreadyStoppedForStop above without reading the store.
	if err := shutdownBeadsProviderForStop(cityPath); err != nil {
		fmt.Fprintf(stderr, "gc stop: bead store: %v\n", err) //nolint:errcheck // best-effort stderr
		// Non-fatal warning.
	}

	return code
}

// openCityStoreForStop opens the store the full stop flow reads sessions
// from. A variable so tests can prove the already-stopped path never opens it.
var openCityStoreForStop = openCityStoreAt

// sessionProviderForStopProbe builds the session provider
// cityAlreadyStoppedForStop lists the runtime through. A variable so tests can
// substitute a fake runtime.
var sessionProviderForStopProbe = newStopProbeSessionProvider

// newStopProbeSessionProvider is the city's session provider without the
// session-bead snapshot sessionProviderForStopCity loads to seed ACP routes.
// Listing what is running needs no routes: the composite provider lists every
// backend. Loading the snapshot would read the store, which is the one thing
// the already-stopped check must not do.
func newStopProbeSessionProvider(cfg *config.City, cityPath string) (runtime.Provider, error) {
	ctx := sessionProviderContextForCity(cfg, cityPath, os.Getenv("GC_SESSION"))
	sp, err := newSessionProviderFromContext(ctx, nil)
	if err != nil {
		return nil, err
	}
	return sp, nil
}

// cityAlreadyStoppedForStop reports whether the standalone stop flow, once it
// has proven no controller serves the city, would find nothing to stop: the
// bead store's backing service is already retired and no runtime session is
// running. It reads only the proxy records, the process table and the runtime
// inventory; it never opens the store or runs bd. When it answers true it also
// returns the provider it listed through, for the runtime server teardown.
//
// It answers false whenever it cannot prove the city is stopped, so the full
// flow still handles a partially stopped city: a live session, a live or
// SIGKILLed proxy, a stray Dolt child, a gc-managed Dolt, a scope that is not
// provider-owned proxied, or a runtime it could not list completely.
//
// The city's nudge pollers are stopped first. They are readers the city
// spawns, and one that outlived the city would restart the proxy this check
// found retired; the full flow stops them for the same reason.
//
// What this path does not do is mark session beads asleep with the city-stop
// reason: that needs the store. A clean stop already marked them, so the only
// beads left unmarked are those of a city whose controller died without
// stopping, whose sessions are gone and whose proxy has since retired.
func cityAlreadyStoppedForStop(cfg *config.City, cityPath string) (bool, runtime.Provider) {
	if err := stopCityNudgePollers(cityPath); err != nil {
		return false, nil
	}
	if !stopCityStoreRetired(cityPath) {
		return false, nil
	}
	sp, err := sessionProviderForStopProbe(cfg, cityPath)
	if err != nil {
		return false, nil
	}
	running, complete := listRunningForStop(sp, io.Discard)
	if !complete || len(running) > 0 {
		return false, nil
	}
	return true, sp
}

// stopCityStoreRetired is a variable so stop-flow tests can model a retired
// or a live store without a real bd proxy.
var stopCityStoreRetired = cityStoreRetiredForStop

// cityStoreRetiredForStop reports whether every bead-store scope this city
// would retire on stop is a provider-owned proxied scope whose proxy exited
// cleanly (bd removes proxy.pid on an orderly exit), with no Dolt server left
// running under any of their proxy roots. Any other shape, or any read it
// cannot complete, answers false.
func cityStoreRetiredForStop(cityPath string) bool {
	if !cityUsesBdStoreContract(cityPath) {
		return false
	}
	if currentResolvableManagedDoltPort(cityPath) != "" {
		return false
	}
	scopes, err := providerOwnedLifecycleScopeRoots(cityPath, "stop")
	if err != nil || len(scopes) == 0 {
		return false
	}
	table := proxyendpoint.DefaultProcessTable()
	proxyRoots := make([]string, 0, len(scopes))
	for _, scopeRoot := range scopes {
		owned, err := scopeProviderOwned(cityPath, scopeRoot)
		if err != nil || !owned || !doctor.ProxiedStoreNotRunning(scopeRoot) {
			return false
		}
		root, err := proxyendpoint.ProviderRoot(scopeRoot)
		if err != nil {
			return false
		}
		if proxyendpoint.Inspect(root, table).Verdict != proxyendpoint.VerdictNoRecord {
			return false
		}
		proxyRoots = append(proxyRoots, normalizePathForCompare(root))
	}
	procs, err := discoverDoltProcesses()
	if err != nil {
		return false
	}
	for _, proc := range procs {
		configPath := extractConfigPath(proc.Argv)
		if configPath == "" {
			continue
		}
		for _, root := range proxyRoots {
			if pathUnderRoot(configPath, root) {
				return false
			}
		}
	}
	return true
}

// teardownServerForStop terminates a provider's shared server after every
// session has been stopped. logPrefix identifies the caller in the error
// line: the standalone path passes "gc stop", the supervisor-managed path
// passes its own "<logPrefix>: city '<name>'" so a teardown warning reads
// like every other managed-shutdown error.
func teardownServerForStop(sp runtime.Provider, stderr io.Writer, logPrefix string) {
	lifecycle, ok := sp.(runtime.ServerLifecycleProvider)
	if !ok {
		return
	}
	if err := lifecycle.TeardownServer(); err != nil {
		fmt.Fprintf(stderr, "%s: teardown server: %v\n", logPrefix, err) //nolint:errcheck // best-effort stderr
	}
}

func markCityStopSessionSleepReason(sessFront *session.Store, stderr io.Writer) {
	if !sessFront.Backed() {
		return
	}
	// The label-only, closed-excluded, IsSessionBeadOrRepairable-UNfiltered Info
	// lister is byte-identical to the former ListByLabel("gc:session") + closed-skip
	// sweep: it keeps damaged gc:session-labeled beads with a non-"session" type (which
	// the narrowing Store.List would drop) and reads each row's classifier through the
	// typed twin (sessionMetadataStateInfo) + the Info.SleepReason mirror.
	sessions, err := sessFront.ListLabeledSessionInfosUnfiltered()
	if err != nil {
		fmt.Fprintf(stderr, "gc stop: marking sessions: %v\n", err) //nolint:errcheck // best-effort warning
		return
	}
	for _, info := range sessions {
		if sessionMetadataStateInfo(info) != "active" {
			continue
		}
		if strings.TrimSpace(info.SleepReason) != "" {
			continue
		}
		if err := sessFront.SetMarker(info.ID, "sleep_reason", string(session.SleepReasonCityStop)); err != nil {
			fmt.Fprintf(stderr, "gc stop: marking session %s: %v\n", info.ID, err) //nolint:errcheck // best-effort warning
		}
	}
}

func stopCityManagedBeadsProviderAfterSuccessfulStop(cityPath string, stderr io.Writer) bool {
	_, err := stopCityManagedBeadsProvider(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc stop: bead store: %v\n", err) //nolint:errcheck // best-effort stderr
		return false
	}
	return true
}

// stopCityManagedBeadsProvider retires the city's bead-store backend on the
// stop paths that do not run the full stop body (supervisor-unregistered, and
// a city whose config will not load).
//
// A provider-owned scope is not gated on a managed Dolt port. bd owns the
// process for those scopes and publishes no GC-managed port, so the port probe
// — which is the right question for the legacy managed-Dolt lifecycle — would
// answer "nothing to stop" for every proxied city and leave its proxy and Dolt
// child resident.
func stopCityManagedBeadsProvider(cityPath string) (bool, error) {
	if rawBeadsProvider(cityPath) != "bd" {
		return false, nil
	}
	providerOwned, err := cityHasProviderOwnedScope(cityPath)
	if err != nil {
		return false, err
	}
	if !providerOwned && currentResolvableManagedDoltPort(cityPath) == "" {
		return false, nil
	}
	return true, shutdownBeadsProviderForStop(cityPath)
}

var shutdownBeadsProviderForStop = shutdownBeadsProvider

func stopManagedRuntimeWithoutConfig(cityPath string, cfgErr error, stdout, stderr io.Writer, force bool) (bool, int) {
	controllerStopped, ownership, controllerErr := stopStandaloneControllerWithoutConfig(cityPath, stdout, force)
	if controllerErr != nil {
		fmt.Fprintf(stderr, "gc stop: %v\n", controllerErr) //nolint:errcheck // best-effort stderr
		return true, 1
	}
	if ownership != nil {
		// Keep the controller lock through provider shutdown; see
		// acquireStoppedControllerOwnership.
		defer ownership.Close() //nolint:errcheck // releasing the flock cannot fail meaningfully
	}
	stopped, stopErr := stopCityManagedBeadsProvider(cityPath)
	if stopErr != nil {
		fmt.Fprintf(stderr, "gc stop: bead store: %v\n", stopErr) //nolint:errcheck // best-effort stderr
		return true, 1
	}
	if !controllerStopped && !stopped {
		return false, 0
	}
	warnInvalidConfigStopSuccess(cfgErr, stderr)
	return true, 0
}

// stopStandaloneControllerWithoutConfig stops (or proves the absence of) a
// standalone controller for a city whose config does not load. On success the
// returned lock, when non-nil, is the held controller lock: the caller must
// keep it through provider shutdown and then Close it.
func stopStandaloneControllerWithoutConfig(cityPath string, stdout io.Writer, force bool) (bool, *os.File, error) {
	stopResult := tryStopControllerWithForce(cityPath, stdout, force)
	switch stopResult.outcome {
	case controllerStopAcknowledged:
		ownership, err := acquireStoppedControllerOwnership(cityPath, supervisorCityStopTimeout(cityPath))
		if err != nil {
			return true, nil, err
		}
		return true, ownership, nil
	case controllerStopDefinitePreEntryUnavailable:
		// No stop request entered a controller, so the lock probe may proceed.
	case controllerStopMayHaveEntered, controllerStopOutcomeInvalid:
		return true, nil, stopResult.failClosedError()
	default:
		return true, nil, stopResult.failClosedError()
	}
	if _, err := os.Stat(filepath.Join(cityPath, ".gc")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil, nil
		}
		return false, nil, fmt.Errorf("probing standalone controller runtime dir: %w", err)
	}
	ownership, err := acquireStoppedControllerOwnership(cityPath, 0)
	if err != nil {
		return false, nil, err
	}
	return false, ownership, nil
}

func warnInvalidConfigAfterSuccessfulStop(cityPath string, stderr io.Writer) {
	if _, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard); err != nil {
		warnInvalidConfigStopSuccess(err, stderr)
	}
}

func warnInvalidConfigStopSuccess(err error, stderr io.Writer) {
	if err == nil {
		return
	}
	fmt.Fprintf(stderr, "gc stop: stopped city despite invalid config: %v\n", err) //nolint:errcheck // best-effort stderr
}

// stopOrphans stops sessions that are not in the desired set. Used by gc stop
// to clean up orphans after stopping config agents. With per-city socket
// isolation, all sessions on the socket belong to this city.
//
// It reports whether its runtime inventory was complete. False means an
// orphan may have been invisible to the sweep and may still be running.
func stopOrphans(sp runtime.Provider, desired map[string]bool, cfg *config.City, sessFront *session.Store,
	timeout time.Duration, rec events.Recorder, stdout, stderr io.Writer,
) bool {
	running, complete := listRunningForStop(sp, stderr)
	var orphans []string
	for _, name := range running {
		if desired[name] {
			continue
		}
		orphans = append(orphans, name)
	}
	gracefulStopAll(orphans, sp, timeout, rec, cfg, sessFront.Store(), stdout, stderr)
	return complete
}

// listRunningForStop lists the provider's running sessions for gc stop and
// reports whether the answer is a complete inventory. An incomplete answer
// still returns every name the provider did observe, so the caller can stop
// those, but it must not report the city as stopped: a backend it could not
// see may still be running sessions.
//
// A runtime server that is not running at all is the one listing failure that
// counts as complete. Sessions cannot outlive their server, so an absent server
// holds none. This is what keeps a repeated gc stop idempotent after the first
// stop tore the server down.
func listRunningForStop(sp runtime.Provider, stderr io.Writer) ([]string, bool) {
	names, err := sp.ListRunning("")
	switch {
	case err == nil:
		return names, true
	case listFailureIsOnlyServerAbsence(err):
		return names, true
	case runtime.IsPartialListError(err):
		fmt.Fprintf(stderr, "gc stop: listing sessions partially failed: %v\n", err) //nolint:errcheck // best-effort stderr
		return names, false
	default:
		fmt.Fprintf(stderr, "gc stop: listing sessions: %v\n", err) //nolint:errcheck // best-effort stderr
		return nil, false
	}
}

// listFailureIsOnlyServerAbsence reports whether every backend failure behind
// a ListRunning error is an absent runtime server.
//
// [runtime.IsRuntimeServerAbsent] deliberately does not unwrap, because a
// composite provider joins its backends' errors and one absent backend says
// nothing about its siblings. This walk keeps that guarantee by requiring
// every joined failure to be an absence: a composite whose other backends all
// answered (their names are in the result) and whose failing backends are all
// absent has a complete inventory. Any other failure anywhere in the tree
// makes the answer false.
func listFailureIsOnlyServerAbsence(err error) bool {
	if err == nil {
		return false
	}
	if runtime.IsRuntimeServerAbsent(err) {
		return true
	}
	switch wrapped := err.(type) { //nolint:errorlint // walks the error tree by hand: every branch of a join must be an absence, which errors.As cannot express
	case interface{ Unwrap() []error }:
		children := wrapped.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !listFailureIsOnlyServerAbsence(child) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		return listFailureIsOnlyServerAbsence(wrapped.Unwrap())
	default:
		return false
	}
}

// tryStopController connects to the controller socket and sends "stop".
// Returns true if a controller acknowledged the shutdown. If no controller
// is running (socket doesn't exist or connection refused), returns false.
func tryStopController(cityPath string, stdout io.Writer) bool {
	return tryStopControllerWithForce(cityPath, stdout, false).outcome == controllerStopAcknowledged
}

func tryStopControllerWithForce(cityPath string, stdout io.Writer, force bool) controllerStopResult {
	result := sendControllerStop(cityPath, force)
	if result.outcome == controllerStopAcknowledged {
		fmt.Fprintln(stdout, "Controller stopping...") //nolint:errcheck // best-effort stdout
	}
	return result
}

func waitForSupervisorControllerStop(cityPath string, timeout time.Duration) error {
	return waitForControllerStop(cityPath, timeout)
}

// waitForControllerStop waits until no controller serves the city and then
// releases the controller lock again. Use it only where nothing that needs
// the controller to stay down follows; stop paths that go on to retire the
// bead store use acquireStoppedControllerOwnership and hold the lock.
func waitForControllerStop(cityPath string, timeout time.Duration) error {
	lock, err := acquireStoppedControllerOwnership(cityPath, timeout)
	if err != nil {
		return err
	}
	lock.Close() //nolint:errcheck // best-effort probe cleanup
	return nil
}

// claimStoppedControllerOwnership takes the controller lock once, right after
// a wait has already proven the controller stopped, and returns it held (nil
// when the city has no runtime dir, so no controller can have run there).
// Losing the lock here means another controller started in between, so the
// caller must not go on to retire the bead store underneath it.
func claimStoppedControllerOwnership(cityPath string) (*os.File, error) {
	if _, err := os.Stat(filepath.Join(cityPath, ".gc")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("probing controller runtime dir: %w", err)
	}
	lock, err := acquireControllerLock(cityPath)
	if errors.Is(err, errControllerAlreadyRunning) {
		return nil, errors.New("a controller started for the city before stop could retire the bead store")
	}
	if err != nil {
		return nil, fmt.Errorf("claiming controller lock: %w", err)
	}
	return lock, nil
}

// acquireStoppedControllerOwnership waits until no controller answers on the
// city's controller socket and the controller lock is free, then returns the
// held lock. The caller owns the lock and must Close it on every path.
//
// Holding the lock is what makes "the controller is gone" stay true: both a
// standalone controller (runController) and a supervisor-hosted city take this
// lock before they serve, so while it is held neither a supervisor restart nor
// a second `gc start` can bring a controller up against state the caller is
// still tearing down (the bead-store provider in particular). The controller
// being stopped released the lock itself as its last shutdown step, so holding
// it here cannot block that controller's own shutdown.
//
// The lock is a non-blocking flock on a close-on-exec descriptor: it is
// released by Close or process exit and is never inherited by provider
// subprocesses. Callers must not call waitForControllerStop, ensureNoStandaloneController,
// or anything else that probes acquireControllerLock while holding it — a
// second flock on a separate descriptor conflicts even within this process.
func acquireStoppedControllerOwnership(cityPath string, timeout time.Duration) (*os.File, error) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		pid := controllerAlive(cityPath)
		lock, err := acquireControllerLock(cityPath)
		switch {
		case err == nil && pid == 0:
			return lock, nil
		case err == nil:
			lock.Close() //nolint:errcheck // a controller still answers; retry after it exits
		case !errors.Is(err, errControllerAlreadyRunning):
			return nil, fmt.Errorf("probing controller: %w", err)
		}
		if time.Now().After(deadline) {
			if pid != 0 {
				identity := probeControllerIdentity(cityPath)
				if identity.PID == 0 {
					identity.PID = pid
				}
				return nil, controllerStopTimeoutError(identity, false)
			}
			return nil, controllerStopTimeoutError(controllerIdentityReply{}, true)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func controllerStopTimeoutError(identity controllerIdentityReply, waitingForLock bool) error {
	authority := "controller"
	switch identity.HostingMode {
	case controllerHostingStandalone:
		authority = "standalone controller"
	case controllerHostingSupervisor:
		authority = "supervisor-hosted controller"
	}
	if identity.PID != 0 {
		return fmt.Errorf("timed out waiting for %s (PID %d) to stop", authority, identity.PID)
	}
	if waitingForLock {
		return fmt.Errorf("timed out waiting for controller to release its lock")
	}
	return fmt.Errorf("timed out waiting for %s to stop", authority)
}

// doStop is the pure logic for "gc stop". Filters to running sessions and
// performs graceful shutdown (interrupt → wait → kill). Accepts session names,
// provider, timeout, and recorder for testability.
func doStop(sessionNames []string, sp runtime.Provider, cfg *config.City, store beads.Store, timeout time.Duration, //nolint:unparam // compatibility wrapper preserves the production-shaped store seam for direct tests
	rec events.Recorder, stdout, stderr io.Writer,
) int {
	code := doStopWithoutSuccess(sessionNames, sp, cfg, store, timeout, rec, stdout, stderr)
	if code == 0 {
		fmt.Fprintln(stdout, "City stopped.") //nolint:errcheck // best-effort stdout
	}
	return code
}

func doStopWithoutSuccess(sessionNames []string, sp runtime.Provider, cfg *config.City, store beads.Store, timeout time.Duration,
	rec events.Recorder, stdout, stderr io.Writer,
) int {
	visible := map[string]bool{}
	inventoryComplete := true
	if sp != nil {
		var names []string
		names, inventoryComplete = listRunningForStop(sp, stderr)
		for _, name := range names {
			if name = strings.TrimSpace(name); name != "" {
				visible[name] = true
			}
		}
	}
	var running, unverified []string
	for _, sn := range sessionNames {
		sn = strings.TrimSpace(sn)
		if sn == "" {
			continue
		}
		alive, err := workerSessionTargetRunningWithConfig("", store, sp, cfg, sn)
		switch {
		case err == nil && alive:
			running = append(running, sn)
			continue
		case err != nil && !errors.Is(err, session.ErrSessionNotFound):
			// The session's own observation failed, so it is unknown, not
			// absent. Report it against its name and withhold success; it is
			// still stopped below if the runtime inventory witnessed it.
			fmt.Fprintf(stderr, "gc stop: observing session %s: %v\n", sn, err) //nolint:errcheck // best-effort stderr
			if !slices.Contains(unverified, sn) {
				unverified = append(unverified, sn)
			}
		}
		if visible[sn] {
			running = append(running, sn)
		}
	}
	gracefulStopAll(running, sp, timeout, rec, cfg, beads.SessionStore{Store: store}, stdout, stderr)
	if len(unverified) > 0 {
		fmt.Fprintf(stderr, "gc stop: stop not confirmed: could not verify that session(s) %s stopped, so they may still be running; resolve the error above and run gc stop again\n", strings.Join(unverified, ", ")) //nolint:errcheck // best-effort stderr
	}
	if !inventoryComplete {
		fmt.Fprintln(stderr, "gc stop: stop not confirmed: the runtime inventory was incomplete, so sessions it could not see may still be running; resolve the runtime error and run gc stop again") //nolint:errcheck // best-effort stderr
	}
	if len(unverified) > 0 || !inventoryComplete {
		return 1
	}
	return 0
}
