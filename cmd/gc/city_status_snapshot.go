package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/suspensionstate"
	"github.com/gastownhall/gascity/internal/worker"
)

// statusObservationConcurrency caps how many agent observations gc status
// runs in parallel. Observations are mostly tmux probes; the bound keeps the
// command from fanning out to hundreds of goroutines on very large cities
// while still cutting wall time on the common 10-30 agent case.
const statusObservationConcurrency = 8

// observeStatusTargetsParallel runs observeSessionTargetWithWarning for each
// target concurrently with a bounded worker pool. Results are returned in
// input order. stderr is shared safely across goroutines.
func observeStatusTargetsParallel(
	sp runtime.Provider,
	cfg *config.City,
	cityPath string,
	store beads.Store,
	targets []statusObservationTarget,
	stderr io.Writer,
) []worker.LiveObservation {
	out := make([]worker.LiveObservation, len(targets))
	if len(targets) == 0 {
		return out
	}
	safeStderr := lockedStderr(stderr)
	sem := make(chan struct{}, statusObservationConcurrency)
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, t statusObservationTarget) {
			defer wg.Done()
			defer func() { <-sem }()
			out[i] = observeSessionTargetWithWarning("gc status", cityPath, store, sp, cfg, t, safeStderr)
		}(i, t)
	}
	wg.Wait()
	return out
}

type cityStatusSnapshot struct {
	CityName        string
	CityPath        string
	EffectiveAPIURL string
	Controller      ControllerJSON
	Suspended       bool
	Beads           *beads.BeadsDiagnostic
	// ConditionalWrites is the daemon's latched §12.5 snapshot; nil on the
	// local fallback path (a stopped controller has no latched state to show).
	ConditionalWrites *api.StatusConditionalWrites
	Agents            []cityStatusAgentRow
	Rigs              []StatusRigJSON
	NamedSessions     []cityStatusNamedSession
	Partial           bool
	PartialErrors     []string
	Summary           StatusSummaryJSON
}

type cityStatusAgentRow struct {
	Agent       StatusAgentJSON
	SessionName string
	GroupName   string
	ScaleLabel  string
	Expanded    bool
}

type cityStatusNamedSession struct {
	Identity string
	Status   string
	Mode     string
}

type rigStatusCounts struct {
	Total     int
	Suspended int
}

func openCityStatusStore(cityPath string, stderr io.Writer) (beads.Store, *beads.BeadsDiagnostic, int) {
	if cityPath == "" {
		return nil, nil, 0
	}
	if !cityStatusStorePresent(cityPath) {
		return nil, nil, 0
	}
	opened, err := openCityStoreAtForStatus(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc status: opening bead store: %v\n", err) //nolint:errcheck // best-effort stderr
		return nil, nil, 1
	}
	return opened.Store, diagnosticPtr(opened.Diagnostic), 0
}

func cityStatusStorePresent(cityPath string) bool {
	for _, candidate := range []string{
		filepath.Join(cityPath, ".beads"),
		filepath.Join(cityPath, ".gc", "beads.json"),
	} {
		if _, err := os.Stat(candidate); err == nil {
			return true
		}
	}
	return false
}

func buildCityStoreHealth(cityPath string, store beads.Store) *StoreHealth {
	return collectStoreHealth(cityPath, store)
}

func collectCityStatusSnapshot(sp runtime.Provider, cfg *config.City, cityPath string, store beads.Store, stderr io.Writer) cityStatusSnapshot {
	return collectCityStatusSnapshotFromStoreSnapshot(sp, cfg, cityPath, store, loadStatusSessionSnapshot(cityPath, cfg, cliSessionStore(store, cfg, cityPath), stderr), stderr)
}

func collectCityStatusSnapshotFromStoreSnapshot(
	sp runtime.Provider,
	cfg *config.City,
	cityPath string,
	store beads.Store,
	statusSnapshot *sessionBeadSnapshot,
	stderr io.Writer,
) cityStatusSnapshot {
	citySt, _ := loadSuspensionState(fsys.OSFS{}, cityPath)
	suspended := os.Getenv("GC_SUSPENDED") == "1"
	if cfg != nil {
		suspended = citySuspendedWithState(cfg, citySt)
	}
	snapshot := cityStatusSnapshot{
		CityPath:        cityPath,
		EffectiveAPIURL: resolveEffectiveAPIURL(cityPath, cfg),
		Controller:      controllerStatusForCity(cityPath),
		Suspended:       suspended,
	}
	snapshot.CityName = loadedCityName(cfg, cityPath)
	registerStatusProviderACPRoutes(sp, statusSnapshot, snapshot.CityName, cfg)
	if snapshot.Controller.Running && cityPath != "" {
		snapshot.Summary.StoreHealth = buildCityStoreHealth(cityPath, store)
	}
	if cfg == nil {
		return snapshot
	}

	suspState, _ := loadSuspensionState(fsys.OSFS{}, cityPath)
	suspendedRigs := buildEffectiveSuspendedRigNames(cfg, suspState)

	rigCounts := make(map[string]*rigStatusCounts, len(cfg.Rigs))
	addRigCount := func(rigName string, rowSuspended bool) {
		if rigName == "" {
			return
		}
		tally := rigCounts[rigName]
		if tally == nil {
			tally = &rigStatusCounts{}
			rigCounts[rigName] = tally
		}
		tally.Total++
		if rowSuspended {
			tally.Suspended++
		}
	}

	// Phase 1: walk the agent config and materialize a row + observation
	// target per (agent or pool instance) without contacting the runtime.
	// Each plan entry remembers everything needed to stitch the observation
	// result back in once it arrives.
	type agentPlan struct {
		row       cityStatusAgentRow
		target    statusObservationTarget
		suspended bool
		rigDir    string
	}
	var plans []agentPlan

	for _, a := range cfg.Agents {
		suspended := a.Suspended || (a.Dir != "" && suspendedRigs[a.Dir])
		sp0 := scaleParamsFor(&a)
		scope := "city"
		if a.Dir != "" {
			scope = "rig"
		}

		if a.SupportsInstanceExpansion() {
			maxDisplay := fmt.Sprintf("max=%d", sp0.Max)
			if sp0.Max < 0 {
				maxDisplay = "max=unlimited"
			}
			scaleLabel := fmt.Sprintf("scaled (min=%d, %s)", sp0.Min, maxDisplay)
			headerShown := false
			for _, qualifiedInstance := range discoverPoolInstances(a.Name, a.Dir, sp0, &a, snapshot.CityName, cfg.Workspace.SessionTemplate, sp) {
				target := statusObservationTargetForIdentity(statusSnapshot, snapshot.CityName, qualifiedInstance, cfg.Workspace.SessionTemplate)
				_, instanceName := config.ParseQualifiedName(qualifiedInstance)
				row := cityStatusAgentRow{
					Agent: StatusAgentJSON{
						Name:          instanceName,
						QualifiedName: qualifiedInstance,
						Scope:         scope,
						Pool:          nil,
					},
					SessionName: target.runtimeSessionName,
					GroupName:   a.QualifiedName(),
					Expanded:    true,
				}
				if !headerShown {
					row.ScaleLabel = scaleLabel
					headerShown = true
				}
				plans = append(plans, agentPlan{row: row, target: target, suspended: suspended, rigDir: a.Dir})
			}
			continue
		}

		target := statusObservationTargetForIdentity(statusSnapshot, snapshot.CityName, a.QualifiedName(), cfg.Workspace.SessionTemplate)
		row := cityStatusAgentRow{
			Agent: StatusAgentJSON{
				Name:          a.Name,
				QualifiedName: a.QualifiedName(),
				Scope:         scope,
			},
			SessionName: target.runtimeSessionName,
			GroupName:   a.QualifiedName(),
			Expanded:    false,
		}
		plans = append(plans, agentPlan{row: row, target: target, suspended: suspended, rigDir: a.Dir})
	}

	// Phase 2: fan out runtime observations across the worker pool. This is
	// the long pole on multi-rig cities; running the probes serially used to
	// dominate gc status wall time.
	targets := make([]statusObservationTarget, len(plans))
	for i, p := range plans {
		targets[i] = p.target
	}
	observations := observeStatusTargetsParallel(sp, cfg, cityPath, store, targets, stderr)

	if statusProviderPartial(sp) {
		snapshot.Partial = true
		snapshot.PartialErrors = append(snapshot.PartialErrors, "runtime status probe incomplete; non-running agent rows are unknown")
	}

	// Phase 3: stitch observation results back into rows and tallies in the
	// original order to keep output deterministic.
	for i, p := range plans {
		obs := observations[i]
		p.row.Agent.Running = obs.Running
		p.row.Agent.Suspended = p.suspended || obs.Suspended || p.target.suspended
		snapshot.Agents = append(snapshot.Agents, p.row)
		snapshot.Summary.TotalAgents++
		if obs.Running {
			snapshot.Summary.RunningAgents++
		}
		addRigCount(p.rigDir, p.suspended || obs.Suspended || p.target.suspended)
	}

	for _, r := range cfg.Rigs {
		suspended := suspendedRigs[r.Name]
		if !suspended {
			if tally := rigCounts[r.Name]; tally != nil && tally.Total > 0 && tally.Total == tally.Suspended {
				suspended = true
			}
		}
		snapshot.Rigs = append(snapshot.Rigs, StatusRigJSON{
			Name:                r.Name,
			Path:                r.Path,
			Prefix:              r.EffectivePrefix(),
			Suspended:           suspended,
			DefaultSlingTarget:  r.DefaultSlingTarget,
			DefaultSlingTargets: r.DefaultSlingTargets,
		})
	}

	for _, ns := range cfg.NamedSessions {
		identity := ns.QualifiedName()
		mode := ns.ModeOrDefault()
		status := namedSessionStatusForCity(cityPath, cfg, store, statusSnapshot, snapshot.CityName, identity, mode, suspState, suspendedRigs)
		snapshot.NamedSessions = append(snapshot.NamedSessions, cityStatusNamedSession{
			Identity: identity,
			Status:   status,
			Mode:     mode,
		})
	}

	return snapshot
}

func namedSessionStatusForCity(
	cityPath string,
	cfg *config.City,
	store beads.Store,
	statusSnapshot *sessionBeadSnapshot,
	cityName string,
	identity string,
	mode string,
	suspState suspensionstate.State,
	suspendedRigs map[string]bool,
) string {
	status := "reserved-unmaterialized"
	if spec, ok := findNamedSessionSpec(cfg, cityName, identity); ok {
		if mode == "always" && namedSessionBlockedBySuspension(cfg, spec.Agent, suspState, suspendedRigs) {
			status = "degraded blocked"
		}
	}
	if store == nil {
		return status
	}
	if statusSnapshot != nil {
		if info, ok := statusSnapshot.FindInfoByNamedIdentity(identity); ok {
			if state := strings.TrimSpace(info.MetadataState); state != "" {
				return state
			}
			return "materialized"
		}
		// Bead not in snapshot. If the snapshot itself is degraded
		// (load timeout or list error), surface that as a lookup error
		// so operators see the same signal the pre-snapshot resolver
		// path produced. See gastownhall/gascity#2148.
		if err := statusSnapshot.LoadError(); err != nil {
			return "lookup error: " + err.Error()
		}
		return status
	}

	// Route the session-ID resolve and the bead fetch through the session
	// coordination-class store so a [beads.classes.sessions] relocation reaches
	// this named-session status lookup. Identity to store at the default backend.
	sessStore := cliSessionStore(store, cfg, cityPath)
	id, err := resolveSessionIDWithConfig(cityPath, cfg, sessStore, identity)
	if err != nil {
		if errors.Is(err, session.ErrSessionNotFound) {
			return status
		}
		return "lookup error: " + err.Error()
	}

	info, err := sessionFrontDoor(sessStore).Get(id)
	if err != nil {
		return "lookup error: " + err.Error()
	}
	// Read the raw state (verbatim MetadataState) through the typed session
	// front door rather than cracking bead.Metadata inline (class-store leak closure).
	if state := strings.TrimSpace(info.MetadataState); state != "" {
		return state
	}
	return "materialized"
}

func collectCitySessionCounts(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City, snapshot *sessionBeadSnapshot) (StatusSummaryJSON, error) {
	summary := StatusSummaryJSON{}
	if snapshot != nil {
		return countCitySessionsFromSnapshot(snapshot), nil
	}
	if store == nil {
		return summary, nil
	}
	if cityPath != "" {
		if _, err := os.Stat(cityPath); err != nil {
			return summary, nil
		}
	}
	if store == nil {
		return summary, nil
	}
	// Route the session catalog through the session coordination-class store so a
	// [beads.classes.sessions] relocation reaches the active/suspended counts.
	// Identity to store at the default backend.
	catalog, err := workerSessionCatalogWithConfig(cityPath, cliSessionStore(store, cfg, cityPath), sp, cfg)
	if err != nil {
		return summary, err
	}
	sessions, err := catalog.List("", "")
	if err != nil {
		return summary, err
	}
	for _, s := range sessions {
		switch s.State {
		case session.StateActive:
			summary.ActiveSessions++
		case session.StateSuspended:
			summary.SuspendedSessions++
		}
	}
	return summary, nil
}

func countCitySessionsFromSnapshot(snapshot *sessionBeadSnapshot) StatusSummaryJSON {
	summary := StatusSummaryJSON{}
	if snapshot == nil {
		return summary
	}
	for _, info := range snapshot.OpenInfos() {
		if info.Closed || !session.IsSessionBeadOrRepairableInfo(info) {
			continue
		}
		switch sessionMetadataStateInfo(info) {
		case string(session.StateActive):
			summary.ActiveSessions++
		case string(session.StateSuspended):
			summary.SuspendedSessions++
		}
	}
	return summary
}

func cityStatusJSONFromSnapshot(snapshot cityStatusSnapshot, summary StatusSummaryJSON) StatusJSON {
	agents := make([]StatusAgentJSON, 0, len(snapshot.Agents))
	for _, row := range snapshot.Agents {
		agents = append(agents, row.Agent)
	}
	rigs := snapshot.Rigs
	if rigs == nil {
		rigs = []StatusRigJSON{}
	}
	var signals []string
	if snapshot.Suspended {
		signals = append(signals, "city_suspended")
	}
	if !snapshot.Controller.Running {
		signals = append(signals, "controller_not_running")
	}
	// A running count of zero is a fact about the agents only when the probe
	// answered for all of them. During partial status the count is zero
	// because nothing was observed, so no_agents_running reports a live city
	// as dead — the same false reading the text renderer stopped emitting
	// when it replaced "stopped" rows with "unknown". The JSON is the surface
	// dashboards and health checks read, and the natural response to "no
	// agents running" with queued work is to restart the agents and re-sling
	// the work, which duplicates builds that are already in flight.
	// Read the counts off the summary parameter rather than snapshot.Summary:
	// the parameter is what this payload publishes as running_agents and
	// total_agents, so a signal or an unknown count derived from the other one
	// could contradict the numbers printed beside it. Both production callers
	// pass snapshot.Summary, so this changes nothing today; it keeps all three
	// values on one source if that ever stops being true.
	unknownAgents := unknownAgentCount(summary.RunningAgents, summary.TotalAgents, snapshot.Partial)
	switch {
	case unknownAgents > 0:
		signals = append(signals, "agent_state_unknown")
	case summary.TotalAgents > 0 && summary.RunningAgents == 0:
		signals = append(signals, "no_agents_running")
	}
	summary.UnknownAgents = unknownAgents
	degraded := len(signals) > 0
	running := snapshot.Controller.Running
	return StatusJSON{
		SchemaVersion:     "1",
		OK:                true,
		CityName:          snapshot.CityName,
		Workspace:         WorkspaceJSON{Name: snapshot.CityName, Path: snapshot.CityPath},
		CityPath:          snapshot.CityPath,
		Controller:        snapshot.Controller,
		Running:           running,
		Suspended:         snapshot.Suspended,
		Partial:           snapshot.Partial,
		PartialErrors:     append([]string(nil), snapshot.PartialErrors...),
		Health:            HealthJSON{Usable: running && !snapshot.Suspended, Degraded: degraded, Signals: signals},
		Beads:             snapshot.Beads,
		ConditionalWrites: snapshot.ConditionalWrites,
		Agents:            agents,
		Rigs:              rigs,
		Summary:           summary,
	}
}

func diagnosticPtr(diagnostic beads.BeadsDiagnostic) *beads.BeadsDiagnostic {
	if diagnostic.Store == "" && !diagnostic.NativeStoreEligible && diagnostic.PreflightGate == "" && diagnostic.PreflightReason == "" {
		return nil
	}
	return &diagnostic
}

// statusNameColumnWidth is the historical fixed pad for the agent-name column
// in gc status text output. statusNameColumnGutter is the minimum number of
// spaces that must separate a name from the token that follows it.
const (
	statusNameColumnWidth  = 24
	statusNameColumnGutter = 2
)

// padStatusName left-aligns name in a width-wide column but always leaves at
// least statusNameColumnGutter spaces before the next token. A plain "%-24s"
// has no enforced minimum gutter, so a rig-qualified name at or past the pad
// width runs straight into the status word
// ("tar-valon/core.control-dispatcherunknown  (partial status)").
// Names short enough to keep the gutter pad exactly as "%-*s" did, measured in
// runes to match fmt's width semantics.
func padStatusName(name string, width int) string {
	n := utf8.RuneCountInString(name)
	if n+statusNameColumnGutter > width {
		return name + strings.Repeat(" ", statusNameColumnGutter)
	}
	return name + strings.Repeat(" ", width-n)
}

// unknownAgentCount is how many agent rows the runtime probe never answered
// for. Every surface reporting agent counts — the text summary, the JSON
// summary, the health signals — derives "unknown" from this one rule, so no
// two of them can disagree about the same snapshot. Outside partial status
// nothing is unknown: a row that is not running was observed not running.
func unknownAgentCount(running, total int, partial bool) int {
	if !partial {
		return 0
	}
	if unknown := total - running; unknown > 0 {
		return unknown
	}
	return 0
}

// agentSummaryLine renders the agent-count summary that closes the Agents
// block. During partial status the runtime probe did not answer, so every
// non-running row rendered "unknown  (partial status)"; folding those into a
// running/total ratio reports a live fleet as down and contradicts the rows
// thirty lines above it. Report unknown separately instead. When the status is
// not partial (or nothing is unknown) the line is byte-identical to before.
func agentSummaryLine(running, total int, partial bool) string {
	if unknown := unknownAgentCount(running, total, partial); unknown > 0 {
		return fmt.Sprintf("%d running, %d unknown of %d agents", running, unknown, total)
	}
	return fmt.Sprintf("%d/%d agents running", running, total)
}

func renderCityStatusText(snapshot cityStatusSnapshot, dops drainOps, stdout io.Writer) {
	fmt.Fprintf(stdout, "%s  %s\n", snapshot.CityName, snapshot.CityPath)                //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "  Controller: %s\n", controllerStatusLine(snapshot.Controller)) //nolint:errcheck // best-effort stdout
	if snapshot.EffectiveAPIURL != "" {
		fmt.Fprintf(stdout, "  API:        %s\n", snapshot.EffectiveAPIURL) //nolint:errcheck // best-effort stdout
	}
	for _, line := range controllerStatusGuidance(snapshot.Controller, snapshot.CityPath) {
		fmt.Fprintf(stdout, "  %s\n", line) //nolint:errcheck // best-effort stdout
	}

	if snapshot.Suspended {
		fmt.Fprintf(stdout, "  Suspended:  yes\n") //nolint:errcheck // best-effort stdout
	} else {
		fmt.Fprintf(stdout, "  Suspended:  no\n") //nolint:errcheck // best-effort stdout
	}

	if len(snapshot.Agents) > 0 {
		fmt.Fprintln(stdout) //nolint:errcheck // best-effort stdout
		fmt.Fprintln(stdout, "Agents:")
		for _, row := range snapshot.Agents {
			if row.ScaleLabel != "" {
				fmt.Fprintf(stdout, "  %s%s\n", padStatusName(row.GroupName, statusNameColumnWidth), row.ScaleLabel) //nolint:errcheck // best-effort stdout
			}
			status := agentStatusLineWithPartial(row.Agent.Running, dops, row.SessionName, row.Agent.Suspended, snapshot.Partial)
			if row.Expanded {
				fmt.Fprintf(stdout, "    %s%s\n", padStatusName(row.Agent.QualifiedName, statusNameColumnWidth-2), status) //nolint:errcheck // best-effort stdout
			} else {
				fmt.Fprintf(stdout, "  %s%s\n", padStatusName(row.Agent.QualifiedName, statusNameColumnWidth), status) //nolint:errcheck // best-effort stdout
			}
		}
		fmt.Fprintln(stdout)                                                                                                   //nolint:errcheck // best-effort stdout
		fmt.Fprintln(stdout, agentSummaryLine(snapshot.Summary.RunningAgents, snapshot.Summary.TotalAgents, snapshot.Partial)) //nolint:errcheck // best-effort stdout
		// Why the count is not a measurement belongs next to the count.
		// PartialErrors reached the JSON payload but never stdout, so a
		// reader piping stdout got the number while its caveat sat on
		// stderr, scrolled above a table that looks complete.
		if snapshot.Partial {
			reasons := snapshot.PartialErrors
			if len(reasons) == 0 {
				reasons = []string{"status is partial; some agent state was not observed"}
			}
			for _, reason := range reasons {
				fmt.Fprintf(stdout, "  (%s)\n", reason) //nolint:errcheck // best-effort stdout
			}
		}
	}

	if len(snapshot.NamedSessions) > 0 {
		fmt.Fprintln(stdout) //nolint:errcheck // best-effort stdout
		fmt.Fprintln(stdout, "Named sessions:")
		for _, named := range snapshot.NamedSessions {
			fmt.Fprintf(stdout, "  %-24s%s (%s)\n", named.Identity, named.Status, named.Mode) //nolint:errcheck // best-effort stdout
		}
	}

	if len(snapshot.Rigs) > 0 {
		fmt.Fprintln(stdout) //nolint:errcheck // best-effort stdout
		fmt.Fprintln(stdout, "Rigs:")
		for _, r := range snapshot.Rigs {
			annotation := ""
			if r.Suspended {
				annotation = "  (suspended)"
			}
			fmt.Fprintf(stdout, "  %-24s%s%s\n", r.Name, r.Path, annotation) //nolint:errcheck // best-effort stdout
		}
	}

	renderStoreHealthBlock(stdout, snapshot.Summary.StoreHealth)
	renderConditionalWritesBlock(stdout, snapshot.ConditionalWrites)
}

// renderConditionalWritesBlock prints the daemon's latched conditional-writes
// snapshot. Off with no notices is silent — the block earns lines only when
// the gate is on or something needs an operator's eye.
func renderConditionalWritesBlock(stdout io.Writer, cw *api.StatusConditionalWrites) {
	if cw == nil || (cw.Effective == "off" && len(cw.Notices) == 0) {
		return
	}
	fmt.Fprintln(stdout)                                                                                        //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "Conditional writes: %s (origin=%s, effective=%s)\n", cw.Mode, cw.Origin, cw.Effective) //nolint:errcheck // best-effort stdout
	for _, v := range cw.Stores {
		if v.Capable {
			fmt.Fprintf(stdout, "  %-24s%-8scapable\n", v.StoreID, v.Kind) //nolint:errcheck // best-effort stdout
			continue
		}
		fmt.Fprintf(stdout, "  %-24s%-8sINCAPABLE (probe=%s latch=%s): %s\n", v.StoreID, v.Kind, v.Probe, v.Latch, v.Reason) //nolint:errcheck // best-effort stdout
	}
	for _, n := range cw.Notices {
		fmt.Fprintf(stdout, "  ! %s\n", n.Message) //nolint:errcheck // best-effort stdout
	}
}
