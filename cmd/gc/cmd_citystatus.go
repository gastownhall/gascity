package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/worker"
	"github.com/spf13/cobra"
)

// StatusJSON is the JSON output format for "gc status --json".
type StatusJSON struct {
	CityName   string            `json:"city_name"`
	CityPath   string            `json:"city_path"`
	Controller ControllerJSON    `json:"controller"`
	Suspended  bool              `json:"suspended"`
	Agents     []StatusAgentJSON `json:"agents"`
	Rigs       []StatusRigJSON   `json:"rigs"`
	Summary    StatusSummaryJSON `json:"summary"`
}

// ControllerJSON represents controller state in JSON output.
type ControllerJSON struct {
	Running bool   `json:"running"`
	PID     int    `json:"pid,omitempty"`
	Mode    string `json:"mode,omitempty"`
	Status  string `json:"status,omitempty"`
}

// StatusAgentJSON represents an agent in the JSON status output.
type StatusAgentJSON struct {
	Name          string    `json:"name"`
	QualifiedName string    `json:"qualified_name"`
	Scope         string    `json:"scope"`
	Running       bool      `json:"running"`
	Suspended     bool      `json:"suspended"`
	Pool          *PoolJSON `json:"pool,omitempty"`
}

// PoolJSON represents pool configuration in JSON output.
type PoolJSON struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

// StatusRigJSON represents a rig in the JSON status output.
type StatusRigJSON struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Suspended bool   `json:"suspended"`
}

// StatusSummaryJSON is the agent count summary in JSON output.
type StatusSummaryJSON struct {
	TotalAgents       int `json:"total_agents"`
	RunningAgents     int `json:"running_agents"`
	ActiveSessions    int `json:"active_sessions,omitempty"`
	SuspendedSessions int `json:"suspended_sessions,omitempty"`
}

var (
	observeSessionTargetForStatus = workerObserveSessionTargetWithConfig
	openCityStoreAtForStatus      = openCityStoreAt
)

var controllerStatusStandaloneFallbackTimeout = 250 * time.Millisecond

// newStatusCmd creates the "gc status [path]" command.
func newStatusCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonFlag bool
	cmd := &cobra.Command{
		Use:   "status [path]",
		Short: "Show city-wide status overview",
		Long: `Shows a city-wide overview: controller state, suspension,
all agents with running status, rigs, and a summary count.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdCityStatus(args, jsonFlag, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "Output in JSON format")
	return cmd
}

// cmdCityStatus is the CLI entry point for the city status overview.
func cmdCityStatus(args []string, jsonOutput bool, stdout, stderr io.Writer) int {
	cityPath, err := resolveCommandCity(args)
	if err != nil {
		fmt.Fprintf(stderr, "gc status: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc status: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	store, code := openCityStatusStore(cityPath, stderr)
	if code != 0 {
		return code
	}
	statusSnapshot := loadStatusSessionSnapshot(store)
	sp := newStatusSessionProviderForCityWithSnapshot(cfg, cityPath, statusSnapshot)
	dops := newDrainOps(sp)
	if jsonOutput {
		return doCityStatusJSONWithStoreAndSnapshot(sp, cfg, cityPath, store, statusSnapshot, stdout, stderr)
	}
	return doCityStatusWithStoreAndSnapshot(sp, dops, cfg, cityPath, store, statusSnapshot, stdout, stderr)
}

func observeSessionTargetWithWarning(
	cmdName string,
	cityPath string,
	store beads.Store,
	sp runtime.Provider,
	cfg *config.City,
	target statusObservationTarget,
	stderr io.Writer,
) worker.LiveObservation {
	if store != nil && target.sessionID != "" {
		handle, err := workerHandleForSessionWithConfig(cityPath, store, sp, cfg, target.sessionID)
		if err == nil {
			obs, err := worker.ObserveHandle(context.Background(), handle)
			if err == nil {
				return obs
			}
		}
	}

	// Status already passes a concrete runtime session name. Resolving that
	// string back through the bead store turns stopped pool instances such as
	// "dog-1" into invalid bd show lookups, which can block the overview.
	obs, err := observeSessionTargetForStatus(cityPath, nil, sp, cfg, target.runtimeSessionName)
	if err != nil && stderr != nil {
		fmt.Fprintf(stderr, "%s: observing %q: %v\n", cmdName, target.runtimeSessionName, err) //nolint:errcheck // best-effort stderr
	}
	return obs
}

type statusObservationTarget struct {
	runtimeSessionName string
	sessionID          string
}

func loadStatusSessionSnapshot(store beads.Store) *sessionBeadSnapshot {
	snapshot, err := loadSessionBeadSnapshot(store)
	if err != nil {
		return nil
	}
	return snapshot
}

func statusObservationTargetForIdentity(
	snapshot *sessionBeadSnapshot,
	cityName string,
	identity string,
	sessionTemplate string,
) statusObservationTarget {
	if snapshot != nil {
		if bead, ok := snapshot.FindSessionBeadByTemplate(identity); ok {
			if sessionName := strings.TrimSpace(bead.Metadata["session_name"]); sessionName != "" {
				return statusObservationTarget{
					runtimeSessionName: sessionName,
					sessionID:          bead.ID,
				}
			}
		}
	}
	return statusObservationTarget{
		runtimeSessionName: sessionName(nil, cityName, identity, sessionTemplate),
	}
}

func namedSessionBlockedBySuspension(cfg *config.City, agentCfg *config.Agent, suspendedRigs map[string]bool) bool {
	if cfg == nil {
		return false
	}
	if citySuspended(cfg) {
		return true
	}
	if agentCfg == nil {
		return false
	}
	return agentCfg.Suspended || (agentCfg.Dir != "" && suspendedRigs[agentCfg.Dir])
}

// doCityStatus prints the city-wide status overview. Accepts injected
// runtime.Provider for testability.
func doCityStatus(
	sp runtime.Provider,
	dops drainOps,
	cfg *config.City,
	cityPath string,
	stdout, stderr io.Writer,
) int {
	store, code := openCityStatusStore(cityPath, stderr)
	if code != 0 {
		return code
	}
	return doCityStatusWithStoreAndSnapshot(sp, dops, cfg, cityPath, store, loadStatusSessionSnapshot(store), stdout, stderr)
}

func doCityStatusWithStoreAndSnapshot(
	sp runtime.Provider,
	dops drainOps,
	cfg *config.City,
	cityPath string,
	store beads.Store,
	statusSnapshot *sessionBeadSnapshot,
	stdout, stderr io.Writer,
) int {
	snapshot := collectCityStatusSnapshotFromStoreSnapshot(sp, cfg, cityPath, store, statusSnapshot, stderr)
	renderCityStatusText(snapshot, dops, stdout)

	if store != nil {
		sessions, err := collectCitySessionCounts(cityPath, store, sp, cfg)
		if err != nil {
			fmt.Fprintf(stderr, "gc status: building session catalog: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		if sessions.ActiveSessions > 0 || sessions.SuspendedSessions > 0 {
			fmt.Fprintln(stdout)                                                                                            //nolint:errcheck // best-effort stdout
			fmt.Fprintf(stdout, "Sessions: %d active, %d suspended\n", sessions.ActiveSessions, sessions.SuspendedSessions) //nolint:errcheck // best-effort stdout
		}
	}

	return 0
}

// doCityStatusJSON outputs city status as JSON. Accepts injected providers
// for testability.
func doCityStatusJSON(
	sp runtime.Provider,
	cfg *config.City,
	cityPath string,
	stdout, stderr io.Writer,
) int {
	store, code := openCityStatusStore(cityPath, stderr)
	if code != 0 {
		return code
	}
	return doCityStatusJSONWithStoreAndSnapshot(sp, cfg, cityPath, store, loadStatusSessionSnapshot(store), stdout, stderr)
}

func doCityStatusJSONWithStoreAndSnapshot(
	sp runtime.Provider,
	cfg *config.City,
	cityPath string,
	store beads.Store,
	statusSnapshot *sessionBeadSnapshot,
	stdout, stderr io.Writer,
) int {
	snapshot := collectCityStatusSnapshotFromStoreSnapshot(sp, cfg, cityPath, store, statusSnapshot, stderr)
	if store != nil {
		sessions, err := collectCitySessionCounts(cityPath, store, sp, cfg)
		if err != nil {
			fmt.Fprintf(stderr, "gc status: building session catalog: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		snapshot.Summary.ActiveSessions = sessions.ActiveSessions
		snapshot.Summary.SuspendedSessions = sessions.SuspendedSessions
	}

	status := cityStatusJSONFromSnapshot(snapshot, snapshot.Summary)
	data, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "gc status: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintln(stdout, string(data)) //nolint:errcheck // best-effort stdout
	return 0
}

func controllerStatusForCity(cityPath string) ControllerJSON {
	_, registered, err := registeredCityEntry(cityPath)
	supervisorWasAlive := false
	if err == nil && registered {
		ctrl := ControllerJSON{Mode: "supervisor"}
		if pid := supervisorAliveHook(); pid != 0 {
			supervisorWasAlive = true
			ctrl.PID = pid
			if running, status, known := supervisorCityRunningHook(cityPath); known {
				ctrl.Running = running
				ctrl.Status = status
				return ctrl
			}
			if supervisorAliveHook() != 0 {
				ctrl.Status = "unknown"
				return ctrl
			}
		}
	}
	if supervisorWasAlive {
		if pid := controllerAliveWithin(cityPath, controllerStatusStandaloneFallbackTimeout); pid != 0 {
			return ControllerJSON{Running: true, PID: pid, Mode: "supervisor"}
		}
	}
	if pid := controllerAlive(cityPath); pid != 0 {
		return ControllerJSON{Running: true, PID: pid, Mode: "standalone"}
	}
	if err == nil && registered {
		return ControllerJSON{Mode: "supervisor"}
	}
	return ControllerJSON{}
}

func controllerAliveWithin(cityPath string, timeout time.Duration) int {
	if timeout <= 0 {
		return controllerAlive(cityPath)
	}
	deadline := time.Now().Add(timeout)
	for {
		if pid := controllerAlive(cityPath); pid != 0 {
			return pid
		}
		if time.Now().After(deadline) {
			return 0
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func controllerSupervisorStatusText(status string) string {
	switch status {
	case "":
		return "city stopped"
	case "loading_config":
		return "loading configuration"
	case "starting_bead_store":
		return "starting bead store"
	case "resolving_formulas":
		return "resolving formulas"
	case "adopting_sessions":
		return "adopting sessions"
	case "starting_agents":
		return "starting agents"
	case "init_failed":
		return "init failed"
	default:
		return strings.ReplaceAll(status, "_", " ")
	}
}

func controllerStatusLine(ctrl ControllerJSON) string {
	switch ctrl.Mode {
	case "supervisor":
		if ctrl.Running {
			return fmt.Sprintf("supervisor-managed (PID %d)", ctrl.PID)
		}
		if ctrl.PID != 0 {
			return fmt.Sprintf("supervisor-managed (PID %d, %s)", ctrl.PID, controllerSupervisorStatusText(ctrl.Status))
		}
		return "supervisor-managed (supervisor not running)"
	case "standalone":
		if ctrl.Running {
			return fmt.Sprintf("standalone-managed (PID %d)", ctrl.PID)
		}
	}
	return "stopped"
}

func controllerStatusGuidance(ctrl ControllerJSON, cityPath string) []string {
	quotedPath := shellQuotePath(cityPath)
	startCommand := "gc start " + quotedPath

	switch ctrl.Mode {
	case "standalone":
		if !ctrl.Running {
			return nil
		}
		authority := "Authority: standalone controller"
		if ctrl.PID != 0 {
			authority = fmt.Sprintf("Authority: standalone controller PID %d", ctrl.PID)
		}
		return []string{
			authority,
			"Next: gc stop " + quotedPath + " && " + startCommand + " to hand ownership to the supervisor",
		}
	case "supervisor":
		if ctrl.PID == 0 {
			return []string{
				"Authority: supervisor registry; no supervisor process is running",
				"Next: " + startCommand + " to start the supervisor and reconcile this city",
			}
		}
		lines := []string{fmt.Sprintf("Authority: supervisor process PID %d", ctrl.PID)}
		if ctrl.Running {
			return lines
		}
		if ctrl.Status == "" || ctrl.Status == "unknown" {
			return append(lines, "Next: "+startCommand+" to ask the supervisor to start this city")
		}
		if ctrl.Status == "init_failed" {
			return append(lines, "Next: gc supervisor logs to see the init failure")
		}
		return append(lines, "Next: gc supervisor logs to inspect startup progress")
	}
	return nil
}
