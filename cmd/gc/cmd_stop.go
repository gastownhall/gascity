package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/spf13/cobra"
)

func newStopCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stop [path]",
		Short: "Stop all agent sessions in the city",
		Long: `Stop all agent sessions in the city with graceful shutdown.

Sends interrupt signals to running agents, waits for the configured
shutdown timeout, then force-kills any remaining sessions. Also stops
the Dolt server and cleans up orphan sessions. If a controller is
running, delegates shutdown to it.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdStop(args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	return cmd
}

var sessionProviderForStopCity = newSessionProviderForCity

const sleepReasonCityStop = "city-stop"

// cmdStop stops the city by terminating all configured agent sessions.
// If a path is given, operates there; otherwise uses cwd.
func cmdStop(args []string, stdout, stderr io.Writer) int {
	cityPath, err := resolveCommandCity(args)
	if err != nil {
		fmt.Fprintf(stderr, "gc stop: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc stop: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cityName := loadedCityName(cfg, cityPath)

	if handled, code := unregisterCityFromSupervisor(cityPath, stdout, stderr, "gc stop"); handled {
		if code != 0 {
			return code
		}
		if supervisorAliveHook() != 0 {
			stopCityManagedBeadsProviderIfRunning(cityPath, stderr)
			fmt.Fprintln(stdout, "City stopped.") //nolint:errcheck // best-effort stdout
			return 0
		}
	}

	store, _ := openCityStoreAt(cityPath)
	markCityStopSessionSleepReason(store, stderr)

	// If a controller is running, ask it to shut down (it stops agents).
	if tryStopController(cityPath, stdout) {
		if err := waitForStandaloneControllerStop(cityPath, cfg.Daemon.ShutdownTimeoutDuration()+15*time.Second); err != nil {
			fmt.Fprintf(stderr, "gc stop: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		// Controller handled the shutdown — still stop bead store below.
		if err := shutdownBeadsProvider(cityPath); err != nil {
			fmt.Fprintf(stderr, "gc stop: bead store: %v\n", err) //nolint:errcheck // best-effort stderr
		}
		fmt.Fprintln(stdout, "City stopped.") //nolint:errcheck // best-effort stdout
		return 0
	}

	sp := sessionProviderForStopCity(cfg, cityPath)
	st := cfg.Workspace.SessionTemplate
	var sessionNames []string
	desired := make(map[string]bool, len(cfg.Agents))
	for _, a := range cfg.Agents {
		sp0 := scaleParamsFor(&a)
		qn := a.QualifiedName()
		if !a.SupportsInstanceExpansion() {
			// Non-expanding template.
			sn := lookupSessionNameOrLegacy(store, cityName, qn, st)
			sessionNames = append(sessionNames, sn)
			desired[sn] = true
		} else {
			// Pool agent: resolve runtime session names from beads first, then legacy discovery.
			for _, ref := range resolvePoolSessionRefs(store, cfg, a.Name, a.Dir, sp0, &a, cityName, st, sp, stderr) {
				sessionNames = append(sessionNames, ref.sessionName)
				desired[ref.sessionName] = true
			}
		}
	}
	recorder := events.Discard
	if fr, err := events.NewFileRecorder(
		filepath.Join(cityPath, ".gc", "events.jsonl"), stderr); err == nil {
		recorder = fr
	}

	code := doStop(sessionNames, sp, cfg, store, cfg.Daemon.ShutdownTimeoutDuration(), recorder, stdout, stderr)

	// Clean up orphan sessions (sessions with the city prefix that are
	// not in the current config).
	stopOrphans(sp, desired, cfg, store, cfg.Daemon.ShutdownTimeoutDuration(), recorder, stdout, stderr)

	// Stop bead store's backing service after agents.
	if err := shutdownBeadsProvider(cityPath); err != nil {
		fmt.Fprintf(stderr, "gc stop: bead store: %v\n", err) //nolint:errcheck // best-effort stderr
		// Non-fatal warning.
	}

	return code
}

func markCityStopSessionSleepReason(store beads.Store, stderr io.Writer) {
	if store == nil {
		return
	}
	sessions, err := store.ListByLabel("gc:session", 0)
	if err != nil {
		fmt.Fprintf(stderr, "gc stop: marking sessions: %v\n", err) //nolint:errcheck // best-effort warning
		return
	}
	for _, session := range sessions {
		state := sessionMetadataState(session)
		if state != "active" && state != "creating" {
			continue
		}
		if strings.TrimSpace(session.Metadata["sleep_reason"]) != "" {
			continue
		}
		if err := store.SetMetadata(session.ID, "sleep_reason", sleepReasonCityStop); err != nil {
			fmt.Fprintf(stderr, "gc stop: marking session %s: %v\n", session.ID, err) //nolint:errcheck // best-effort warning
		}
	}
}

func stopCityManagedBeadsProviderIfRunning(cityPath string, stderr io.Writer) {
	if rawBeadsProvider(cityPath) != "bd" {
		return
	}
	if currentManagedDoltPort(cityPath) == "" {
		return
	}
	if err := shutdownBeadsProvider(cityPath); err != nil {
		fmt.Fprintf(stderr, "gc stop: bead store: %v\n", err) //nolint:errcheck // best-effort warning
	}
}

// stopOrphans stops sessions that are not in the desired set. Used by gc stop
// to clean up orphans after stopping config agents. With per-city socket
// isolation, all sessions on the socket belong to this city.
func stopOrphans(sp runtime.Provider, desired map[string]bool, cfg *config.City, store beads.Store,
	timeout time.Duration, rec events.Recorder, stdout, stderr io.Writer,
) {
	running, err := sp.ListRunning("")
	partialList := runtime.IsPartialListError(err)
	if err != nil && !partialList {
		fmt.Fprintf(stderr, "gc stop: listing sessions: %v\n", err) //nolint:errcheck // best-effort stderr
		return
	}
	if partialList {
		fmt.Fprintf(stderr, "gc stop: listing sessions partially failed: %v\n", err) //nolint:errcheck // best-effort stderr
	}
	var orphans []string
	for _, name := range running {
		if desired[name] {
			continue
		}
		orphans = append(orphans, name)
	}
	gracefulStopAll(orphans, sp, timeout, rec, cfg, store, stdout, stderr)
}

// tryStopController connects to the controller socket and sends "stop".
// Returns true if a controller acknowledged the shutdown. If no controller
// is running (socket doesn't exist or connection refused), returns false.
func tryStopController(cityPath string, stdout io.Writer) bool {
	sockPath := controllerSocketPath(cityPath)
	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		return false
	}
	defer conn.Close()                                     //nolint:errcheck // best-effort cleanup
	conn.Write([]byte("stop\n"))                           //nolint:errcheck // best-effort
	conn.SetReadDeadline(time.Now().Add(10 * time.Second)) //nolint:errcheck // best-effort
	buf := make([]byte, 64)
	n, readErr := conn.Read(buf)
	if readErr != nil || !strings.Contains(string(buf[:n]), "ok") {
		return false // controller did not acknowledge — fall through to direct cleanup
	}
	fmt.Fprintln(stdout, "Controller stopping...") //nolint:errcheck // best-effort stdout
	return true
}

func waitForStandaloneControllerStop(cityPath string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		pid := controllerAlive(cityPath)
		lock, err := acquireControllerLock(cityPath)
		switch {
		case err == nil && pid == 0:
			lock.Close() //nolint:errcheck // best-effort probe cleanup
			return nil
		case err == nil:
			lock.Close() //nolint:errcheck // best-effort probe cleanup
		case !errors.Is(err, errControllerAlreadyRunning):
			return fmt.Errorf("probing standalone controller: %w", err)
		}
		if time.Now().After(deadline) {
			if pid != 0 {
				return fmt.Errorf("timed out waiting for standalone controller (PID %d) to stop", pid)
			}
			return fmt.Errorf("timed out waiting for standalone controller to release its lock")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// doStop is the pure logic for "gc stop". Filters to running sessions and
// performs graceful shutdown (interrupt → wait → kill). Accepts session names,
// provider, timeout, and recorder for testability.
func doStop(sessionNames []string, sp runtime.Provider, cfg *config.City, store beads.Store, timeout time.Duration,
	rec events.Recorder, stdout, stderr io.Writer,
) int {
	visible := map[string]bool{}
	if sp != nil {
		names, err := sp.ListRunning("")
		partialList := runtime.IsPartialListError(err)
		if err != nil && !partialList {
			fmt.Fprintf(stderr, "gc stop: listing sessions: %v\n", err) //nolint:errcheck // best-effort stderr
			names = nil
		}
		if partialList {
			fmt.Fprintf(stderr, "gc stop: listing sessions partially failed: %v\n", err) //nolint:errcheck // best-effort stderr
		}
		for _, name := range names {
			if name = strings.TrimSpace(name); name != "" {
				visible[name] = true
			}
		}
	}
	var running []string
	for _, sn := range sessionNames {
		sn = strings.TrimSpace(sn)
		if sn == "" {
			continue
		}
		if alive, err := workerSessionTargetRunningWithConfig("", store, sp, cfg, sn); err == nil && alive {
			running = append(running, sn)
			continue
		}
		if visible[sn] {
			running = append(running, sn)
		}
	}
	gracefulStopAll(running, sp, timeout, rec, cfg, store, stdout, stderr)
	fmt.Fprintln(stdout, "City stopped.") //nolint:errcheck // best-effort stdout
	return 0
}
