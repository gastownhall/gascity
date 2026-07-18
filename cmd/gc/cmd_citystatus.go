package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/suspensionstate"
	"github.com/gastownhall/gascity/internal/worker"
	"github.com/spf13/cobra"
)

// StatusJSON is the JSON output format for gc status.
type StatusJSON struct {
	SchemaVersion string                 `json:"schema_version"`
	OK            bool                   `json:"ok"`
	CityName      string                 `json:"city_name"`
	Workspace     WorkspaceJSON          `json:"workspace"`
	CityPath      string                 `json:"city_path"`
	Controller    ControllerJSON         `json:"controller"`
	Running       bool                   `json:"running"`
	Suspended     bool                   `json:"suspended"`
	Health        HealthJSON             `json:"health"`
	Beads         *beads.BeadsDiagnostic `json:"beads,omitempty"`
	// ConditionalWrites mirrors the API status block verbatim (§12.5).
	ConditionalWrites *api.StatusConditionalWrites `json:"conditional_writes,omitempty"`
	Agents            []StatusAgentJSON            `json:"agents"`
	Rigs              []StatusRigJSON              `json:"rigs"`
	Summary           StatusSummaryJSON            `json:"summary"`
}

type WorkspaceJSON struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type HealthJSON struct {
	Usable   bool     `json:"usable"`
	Degraded bool     `json:"degraded"`
	Signals  []string `json:"signals,omitempty"`
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
	Name                string   `json:"name"`
	Path                string   `json:"path"`
	Prefix              string   `json:"prefix,omitempty"`
	Suspended           bool     `json:"suspended"`
	DefaultSlingTarget  string   `json:"default_sling_target,omitempty"`
	DefaultSlingTargets []string `json:"default_sling_targets,omitempty"`
}

// StatusSummaryJSON is the agent count summary in JSON output.
type StatusSummaryJSON struct {
	TotalAgents       int          `json:"total_agents"`
	RunningAgents     int          `json:"running_agents"`
	ActiveSessions    int          `json:"active_sessions,omitempty"`
	SuspendedSessions int          `json:"suspended_sessions,omitempty"`
	StoreHealth       *StoreHealth `json:"store_health,omitempty"`
}

// StoreHealth is the JSON shape of the Dolt bead store health block
// surfaced by gc status. See ADR 0002 / bead ga-d5y design D9.
type StoreHealth struct {
	Path         string  `json:"path"`
	SizeBytes    int64   `json:"size_bytes"`
	LiveRows     int     `json:"live_rows"`
	RatioMB      float64 `json:"ratio_mb_per_row"`
	Warning      bool    `json:"warning"`
	ThresholdMB  float64 `json:"threshold_mb_per_row"`
	LastGCAt     string  `json:"last_gc_at,omitempty"`
	LastGCStatus string  `json:"last_gc_status,omitempty"`
}

var (
	observeSessionTargetForStatus = workerObserveSessionTargetWithConfig
	openCityStoreAtForStatus      = openCityStoreResultAt
)

var (
	controllerStatusStandaloneFallbackTimeout = 250 * time.Millisecond
	statusObservationTimeout                  = 750 * time.Millisecond
	statusSessionSnapshotTimeout              = 3 * time.Second
)

// newStatusCmd creates the "gc status [path]" command.
func newStatusCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonFlag bool
	var formatFlag string
	cmd := &cobra.Command{
		Use:   "status [path|name]",
		Short: "Show city-wide status overview",
		Long: `Shows a city-wide overview: controller state, suspension,
all agents with running status, rigs, and a summary count.`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeCityNames,
		RunE: func(_ *cobra.Command, args []string) error {
			format := strings.ToLower(strings.TrimSpace(formatFlag))
			switch format {
			case "", "text", "json":
			default:
				fmt.Fprintf(stderr, "gc status: unsupported format %q\n", formatFlag) //nolint:errcheck // best-effort stderr
				return errExit
			}
			if cmdCityStatus(args, jsonFlag || format == "json", stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "Output in JSON format")
	cmd.Flags().StringVar(&formatFlag, "format", "", "Output format: text or json")
	return cmd
}

// cmdCityStatus is the CLI entry point for the city status overview.
// Routes through the supervisor API when a controller is up and falls
// back to the local snapshot builder otherwise.
func cmdCityStatus(args []string, jsonOutput bool, stdout, stderr io.Writer) int {
	cityPath, err := resolveCommandCity(args)
	if err != nil {
		if jsonOutput {
			return writeJSONError(stdout, stderr, "city_resolve_failed", fmt.Sprintf("gc status: %v", err), 1)
		}
		fmt.Fprintf(stderr, "gc status: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	configStderr := stderr
	if jsonOutput {
		configStderr = io.Discard
	}
	cfg, err := loadCityConfig(cityPath, configStderr)
	if err != nil {
		if jsonOutput {
			return writeJSONError(stdout, stderr, "config_load_failed", fmt.Sprintf("gc status: %v", err), 1)
		}
		fmt.Fprintf(stderr, "gc status: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	storeStderr := stderr
	if jsonOutput {
		storeStderr = io.Discard
	}
	store, _, code := openCityStatusStore(cityPath, storeStderr)
	if code != 0 {
		if jsonOutput {
			return writeJSONError(stdout, stderr, "store_open_failed", "gc status: opening bead store failed", code)
		}
		return code
	}
	statusSnapshot := loadStatusSessionSnapshot(cityPath, cfg, cliSessionStore(store, cfg, cityPath), stderr)
	sp, err := newStatusSessionProviderForCityWithSnapshot(cfg, cityPath, statusSnapshot)
	if err != nil {
		message := fmt.Sprintf("gc status: %v", err)
		if jsonOutput {
			return writeJSONError(stdout, stderr, "session_provider_failed", message, 1)
		}
		fmt.Fprintln(stderr, message) //nolint:errcheck // best-effort stderr
		return 1
	}
	dops := newDrainOps(sp)
	c, reason := cityStatusAPIClient(cityPath)
	return routeCityStatus(cityPath, cfg, sp, dops, c, reason, jsonOutput, stdout, stderr)
}

// cityStatusAPIClient returns (client, "") when the API path is available,
// or (nil, reason) when the caller should fall back. Indirected through a
// var so tests inject a client pointed at httptest.Server or force a
// specific fallback reason without spinning up a real controller.
var cityStatusAPIClient = func(cityPath string) (*api.Client, string) {
	if c := apiClient(cityPath); c != nil {
		return c, ""
	}
	return nil, apiClientFallbackReason(cityPath)
}

// routeCityStatus dispatches `gc status` to the supervisor API when a
// controller is up; otherwise falls back to the local snapshot builder.
// Emits exactly one route=... log line per exit path (gated on GC_DEBUG).
func routeCityStatus(
	cityPath string,
	cfg *config.City,
	sp runtime.Provider,
	dops drainOps,
	c *api.Client,
	nilReason string,
	jsonOutput bool,
	stdout, stderr io.Writer,
) int {
	var cr api.CachedRead[api.StatusView]
	return routeRead(c, "status", nilReason, stderr,
		func() error {
			var err error
			cr, err = c.GetStatus()
			return err
		},
		func() int { return renderCityStatusFromAPI(cityPath, cr, dops, jsonOutput, stdout) },
		func() int {
			store, diagnostic, code := openCityStatusStore(cityPath, stderr)
			if code != 0 {
				return code
			}
			statusSnapshot := loadStatusSessionSnapshot(cityPath, cfg, cliSessionStore(store, cfg, cityPath), stderr)
			if jsonOutput {
				return doCityStatusJSONWithDiagnosticAndSnapshot(sp, cfg, cityPath, store, diagnostic, statusSnapshot, stdout, stderr)
			}
			return doCityStatusWithStoreAndSnapshot(sp, dops, cfg, cityPath, store, statusSnapshot, stdout, stderr)
		},
	)
}

// renderCityStatusFromAPI renders the server's StatusView using the same
// text and JSON formatters as the fallback path. The API path adds
// _cache_age_s on --json output and a staleness banner on human output
// when cache age > 30 s.
//
// Controller authority is not surfaced through the API response (the
// server is the controller, so the CLI resolves that locally via
// controllerStatusForCity — same call the fallback path makes).
func renderCityStatusFromAPI(cityPath string, cr api.CachedRead[api.StatusView], dops drainOps, jsonOutput bool, stdout io.Writer) int {
	snapshot := snapshotFromStatusView(cityPath, cr.Body)
	if jsonOutput {
		writeCityStatusJSONWithCache(snapshot, snapshot.Summary, cr.AgeSeconds, stdout)
		return 0
	}
	renderCityStatusText(snapshot, dops, stdout)
	if cr.Body.SessionCounts.Active > 0 || cr.Body.SessionCounts.Suspended > 0 {
		fmt.Fprintln(stdout)                                                                                                      //nolint:errcheck // best-effort stdout
		fmt.Fprintf(stdout, "Sessions: %d active, %d suspended\n", cr.Body.SessionCounts.Active, cr.Body.SessionCounts.Suspended) //nolint:errcheck // best-effort stdout
	}
	if cr.AgeSeconds > cacheAgeBannerThresholdSeconds {
		fmt.Fprintf(stdout, "(cache age: %.0fs — reconciler may be lagging)\n", cr.AgeSeconds) //nolint:errcheck // best-effort stdout
	}
	return 0
}

// snapshotFromStatusView builds a cityStatusSnapshot from the API's
// StatusView so the existing renderCityStatusText + cityStatusJSONFromSnapshot
// helpers produce identical output on the API path.
func snapshotFromStatusView(cityPath string, v api.StatusView) cityStatusSnapshot {
	snapshot := cityStatusSnapshot{
		CityName:          v.CityName,
		CityPath:          v.CityPath,
		Suspended:         v.Suspended,
		Controller:        controllerStatusForCity(cityPath),
		Beads:             v.Beads,
		ConditionalWrites: v.ConditionalWrites,
		Summary: StatusSummaryJSON{
			TotalAgents:       v.Summary.TotalAgents,
			RunningAgents:     v.Summary.RunningAgents,
			ActiveSessions:    v.SessionCounts.Active,
			SuspendedSessions: v.SessionCounts.Suspended,
		},
	}
	for _, a := range v.Agents {
		snapshot.Agents = append(snapshot.Agents, cityStatusAgentRow{
			Agent: StatusAgentJSON{
				Name:          a.Name,
				QualifiedName: a.QualifiedName,
				Scope:         a.Scope,
				Running:       a.Running,
				Suspended:     a.Suspended,
			},
			SessionName: a.SessionName,
			GroupName:   a.GroupName,
			ScaleLabel:  a.ScaleLabel,
			Expanded:    a.Expanded,
		})
	}
	for _, r := range v.Rigs {
		snapshot.Rigs = append(snapshot.Rigs, StatusRigJSON{
			Name:      r.Name,
			Path:      r.Path,
			Suspended: r.Suspended,
		})
	}
	for _, ns := range v.NamedSessions {
		snapshot.NamedSessions = append(snapshot.NamedSessions, cityStatusNamedSession{
			Identity: ns.Identity,
			Status:   ns.Status,
			Mode:     ns.Mode,
		})
	}
	if v.StoreHealth != nil {
		snapshot.Summary.StoreHealth = &StoreHealth{
			Path:         v.StoreHealth.Path,
			SizeBytes:    v.StoreHealth.SizeBytes,
			LiveRows:     v.StoreHealth.LiveRows,
			RatioMB:      v.StoreHealth.RatioMB,
			Warning:      v.StoreHealth.Warning,
			ThresholdMB:  v.StoreHealth.ThresholdMB,
			LastGCAt:     v.StoreHealth.LastGCAt,
			LastGCStatus: v.StoreHealth.LastGCStatus,
		}
	}
	return snapshot
}

// writeCityStatusJSONWithCache writes the snapshot's JSON form with a
// leading _cache_age_s field inserted at the envelope level. Mirrors the
// envelope shape other routed read commands emit on the API path.
func writeCityStatusJSONWithCache(
	snapshot cityStatusSnapshot,
	summary StatusSummaryJSON,
	ageSeconds float64,
	stdout io.Writer,
) {
	status := cityStatusJSONFromSnapshot(snapshot, summary)
	envelope := struct {
		CacheAgeS  float64 `json:"_cache_age_s"`
		StatusJSON         // inline
	}{CacheAgeS: ageSeconds, StatusJSON: status}
	data, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		fmt.Fprintf(stdout, "{\"error\": %q}\n", err.Error()) //nolint:errcheck // best-effort stdout
		return
	}
	fmt.Fprintln(stdout, string(data)) //nolint:errcheck // best-effort stdout
}

func observeSessionTargetWithWarning(
	cmdName string,
	cityPath string,
	_ beads.Store,
	sp runtime.Provider,
	cfg *config.City,
	target statusObservationTarget,
	stderr io.Writer,
) worker.LiveObservation {
	// Status already passes a concrete runtime session name. Resolving that
	// string back through the bead store turns stopped pool instances such as
	// "dog-1" into invalid bd show lookups, which can block the overview.
	type observeResult struct {
		observation worker.LiveObservation
		err         error
	}
	done := make(chan observeResult, 1)
	go func() {
		obs, err := observeSessionTargetForStatus(cityPath, nil, sp, cfg, target.runtimeSessionName)
		done <- observeResult{observation: obs, err: err}
	}()

	select {
	case result := <-done:
		if result.err != nil && stderr != nil {
			fmt.Fprintf(stderr, "%s: observing %q: %v\n", cmdName, target.runtimeSessionName, result.err) //nolint:errcheck // best-effort stderr
		}
		return result.observation
	case <-time.After(statusObservationTimeout):
		if stderr != nil {
			fmt.Fprintf(stderr, "%s: observing %q timed out after %s\n", cmdName, target.runtimeSessionName, statusObservationTimeout) //nolint:errcheck // best-effort stderr
		}
		return worker.LiveObservation{}
	}
}

type statusObservationTarget struct {
	runtimeSessionName string
	sessionID          string
	suspended          bool
}

func loadStatusSessionSnapshot(cityPath string, cfg *config.City, store beads.Store, stderr io.Writer) *sessionBeadSnapshot {
	if store == nil {
		return newSessionBeadSnapshotFromInfos(nil)
	}
	// Callers pass the session coordination-class store (cliSessionStore) so a
	// [beads.classes.sessions] relocation reaches this snapshot; the guard in
	// frontdoor_di_guard_test.go pins that seam at each read-site file.

	// A throwaway, ctx-bound clone of store when it's bd-CLI-backed: on
	// timeout below, canceling reqCtx kills an in-flight bd child instead
	// of abandoning it to run past this function's return (gascity
	// ga-cdmx6x). scopedStoreLike answers (nil, nil) for non-bd-CLI
	// backends, which have no subprocess to leak — those keep reading
	// through store directly, unchanged.
	//
	// SPLIT CAVEAT (must fix in cmd/gc/scoped_store.go before enabling the
	// domain/infra split): scopedStoreLike is CLASS-BLIND — it unwraps to the
	// backing *beads.BdStore and rebuilds a scoped clone from cityPath / the
	// backing Dir(), never re-consulting resolveClassStore. Today that is
	// byte-identical (cliSessionStore is identity, so store's backing Dir() ==
	// cityPath). Once [beads.classes.sessions] relocates to a bd-CLI-backed
	// store, this clone would silently re-point the session read at the WORK
	// store, defeating the cliSessionStore seam above (the DI guard cannot catch
	// it — the cliSessionStore( needle is still present). scopedStoreLike must be
	// made class-preserving (clone what it unwrapped, or refuse to unwrap a
	// relocated store so this keeps reading through the routed store) as part of
	// the split, where a real infra store makes the fix testable.
	reqCtx, cancel := context.WithTimeout(context.Background(), statusSessionSnapshotTimeout)
	defer cancel()
	readStore := store
	if scoped, err := scopedStoreLike(reqCtx, cityPath, cfg, store); err != nil {
		if stderr != nil {
			fmt.Fprintf(stderr, "gc status: loading session snapshot: resolving store: %v\n", err) //nolint:errcheck // best-effort stderr
		}
		return newSessionBeadSnapshotWithError(fmt.Errorf("loading session snapshot: resolving store: %w", err))
	} else if scoped != nil {
		readStore = scoped
	}

	type snapshotResult struct {
		snapshot *sessionBeadSnapshot
		err      error
	}
	done := make(chan snapshotResult, 1)
	go func() {
		snapshot, err := loadSessionBeadSnapshot(readStore)
		done <- snapshotResult{snapshot: snapshot, err: err}
	}()

	select {
	case result := <-done:
		if result.err != nil {
			if stderr != nil {
				fmt.Fprintf(stderr, "gc status: loading session snapshot: %v\n", result.err) //nolint:errcheck // best-effort stderr
			}
			return newSessionBeadSnapshotWithError(fmt.Errorf("loading session snapshot: %w", result.err))
		}
		if result.snapshot == nil {
			return newSessionBeadSnapshotFromInfos(nil)
		}
		return result.snapshot
	case <-time.After(statusSessionSnapshotTimeout):
		if stderr != nil {
			fmt.Fprintf(stderr, "gc status: loading session snapshot timed out after %s; continuing with runtime-only status\n", statusSessionSnapshotTimeout) //nolint:errcheck // best-effort stderr
		}
		return newSessionBeadSnapshotWithError(fmt.Errorf("loading session snapshot timed out after %s", statusSessionSnapshotTimeout))
	}
}

func statusObservationTargetForIdentity(
	snapshot *sessionBeadSnapshot,
	cityName string,
	identity string,
	sessionTemplate string,
) statusObservationTarget {
	if snapshot != nil {
		if info, ok := snapshot.FindInfoByTemplate(identity); ok {
			if sessionName := strings.TrimSpace(info.SessionNameMetadata); sessionName != "" {
				return statusObservationTarget{
					runtimeSessionName: sessionName,
					sessionID:          info.ID,
					suspended:          sessionMetadataStateInfo(info) == string(session.StateSuspended),
				}
			}
		}
		if info, ok := snapshot.FindInfoByNamedIdentity(identity); ok {
			if sessionName := strings.TrimSpace(info.SessionNameMetadata); sessionName != "" {
				return statusObservationTarget{
					runtimeSessionName: sessionName,
					sessionID:          info.ID,
					suspended:          sessionMetadataStateInfo(info) == string(session.StateSuspended),
				}
			}
		}
	}
	return statusObservationTarget{
		runtimeSessionName: sessionName(nil, cityName, identity, sessionTemplate),
	}
}

func namedSessionBlockedBySuspension(cfg *config.City, agentCfg *config.Agent, suspState suspensionstate.State, suspendedRigs map[string]bool) bool {
	if cfg == nil {
		return false
	}
	if citySuspendedWithState(cfg, suspState) {
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
	store, _, code := openCityStatusStore(cityPath, stderr)
	if code != 0 {
		return code
	}
	return doCityStatusWithStoreAndSnapshot(sp, dops, cfg, cityPath, store, loadStatusSessionSnapshot(cityPath, cfg, cliSessionStore(store, cfg, cityPath), stderr), stdout, stderr)
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

	// Track session-snapshot degradation so we can render the textual report
	// AND signal the failure via exit code. Restores the pre-#2005 contract
	// that monitoring callers rely on (see #2147).
	snapshotDegraded := statusSnapshot.LoadError() != nil

	if store != nil {
		sessions, err := collectCitySessionCounts(cityPath, store, sp, cfg, statusSnapshot)
		if err != nil {
			fmt.Fprintf(stderr, "gc status: building session catalog: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		if sessions.ActiveSessions > 0 || sessions.SuspendedSessions > 0 {
			fmt.Fprintln(stdout)                                                                                            //nolint:errcheck // best-effort stdout
			fmt.Fprintf(stdout, "Sessions: %d active, %d suspended\n", sessions.ActiveSessions, sessions.SuspendedSessions) //nolint:errcheck // best-effort stdout
		}
	}

	if snapshotDegraded {
		return 1
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
	store, diagnostic, code := openCityStatusStore(cityPath, stderr)
	if code != 0 {
		return code
	}
	return doCityStatusJSONWithDiagnosticAndSnapshot(sp, cfg, cityPath, store, diagnostic, loadStatusSessionSnapshot(cityPath, cfg, cliSessionStore(store, cfg, cityPath), stderr), stdout, stderr)
}

func doCityStatusJSONWithDiagnosticAndSnapshot(
	sp runtime.Provider,
	cfg *config.City,
	cityPath string,
	store beads.Store,
	diagnostic *beads.BeadsDiagnostic,
	statusSnapshot *sessionBeadSnapshot,
	stdout, stderr io.Writer,
) int {
	snapshot := collectCityStatusSnapshotFromStoreSnapshot(sp, cfg, cityPath, store, statusSnapshot, stderr)
	snapshot.Beads = diagnostic
	// Track session-snapshot degradation so we can emit the JSON payload AND
	// signal the failure via exit code. Restores the pre-#2005 contract that
	// monitoring callers rely on (see #2147).
	snapshotDegraded := statusSnapshot.LoadError() != nil
	if store != nil {
		sessions, err := collectCitySessionCounts(cityPath, store, sp, cfg, statusSnapshot)
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
	if snapshotDegraded {
		return 1
	}
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
