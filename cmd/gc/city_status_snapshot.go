package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

type cityStatusSnapshot struct {
	CityName      string
	CityPath      string
	Controller    ControllerJSON
	Suspended     bool
	Agents        []cityStatusAgentRow
	Rigs          []StatusRigJSON
	NamedSessions []cityStatusNamedSession
	Summary       StatusSummaryJSON
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

func openCityStatusStore(cityPath string, stderr io.Writer) (beads.Store, int) {
	if cityPath == "" {
		return nil, 0
	}
	opened, err := openCityStoreAtForStatus(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc status: opening bead store: %v\n", err) //nolint:errcheck // best-effort stderr
		return nil, 1
	}
	return opened, 0
}

func collectCityStatusSnapshot(sp runtime.Provider, cfg *config.City, cityPath string, store beads.Store, stderr io.Writer) cityStatusSnapshot {
	return collectCityStatusSnapshotFromStoreSnapshot(sp, cfg, cityPath, store, loadStatusSessionSnapshot(store), stderr)
}

func collectCityStatusSnapshotFromStoreSnapshot(
	sp runtime.Provider,
	cfg *config.City,
	cityPath string,
	store beads.Store,
	statusSnapshot *sessionBeadSnapshot,
	stderr io.Writer,
) cityStatusSnapshot {
	suspended := os.Getenv("GC_SUSPENDED") == "1"
	if cfg != nil {
		suspended = citySuspended(cfg)
	}
	snapshot := cityStatusSnapshot{
		CityPath:   cityPath,
		Controller: controllerStatusForCity(cityPath),
		Suspended:  suspended,
	}
	snapshot.CityName = loadedCityName(cfg, cityPath)
	registerStatusProviderACPRoutes(sp, statusSnapshot, snapshot.CityName, cfg)
	if cfg == nil {
		return snapshot
	}

	suspendedRigs := make(map[string]bool, len(cfg.Rigs))
	for _, r := range cfg.Rigs {
		if r.Suspended {
			suspendedRigs[r.Name] = true
		}
	}

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
				obs := observeSessionTargetWithWarning("gc status", cityPath, store, sp, cfg, target, stderr)
				_, instanceName := config.ParseQualifiedName(qualifiedInstance)
				row := cityStatusAgentRow{
					Agent: StatusAgentJSON{
						Name:          instanceName,
						QualifiedName: qualifiedInstance,
						Scope:         scope,
						Running:       obs.Running,
						Suspended:     suspended || obs.Suspended,
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
				snapshot.Agents = append(snapshot.Agents, row)
				snapshot.Summary.TotalAgents++
				if obs.Running {
					snapshot.Summary.RunningAgents++
				}
				addRigCount(a.Dir, suspended || obs.Suspended)
			}
			continue
		}

		target := statusObservationTargetForIdentity(statusSnapshot, snapshot.CityName, a.QualifiedName(), cfg.Workspace.SessionTemplate)
		obs := observeSessionTargetWithWarning("gc status", cityPath, store, sp, cfg, target, stderr)
		snapshot.Agents = append(snapshot.Agents, cityStatusAgentRow{
			Agent: StatusAgentJSON{
				Name:          a.Name,
				QualifiedName: a.QualifiedName(),
				Scope:         scope,
				Running:       obs.Running,
				Suspended:     suspended || obs.Suspended,
			},
			SessionName: target.runtimeSessionName,
			GroupName:   a.QualifiedName(),
			Expanded:    false,
		})
		snapshot.Summary.TotalAgents++
		if obs.Running {
			snapshot.Summary.RunningAgents++
		}
		addRigCount(a.Dir, suspended || obs.Suspended)
	}

	for _, r := range cfg.Rigs {
		suspended := r.Suspended
		if !suspended {
			if tally := rigCounts[r.Name]; tally != nil && tally.Total > 0 && tally.Total == tally.Suspended {
				suspended = true
			}
		}
		snapshot.Rigs = append(snapshot.Rigs, StatusRigJSON{
			Name:      r.Name,
			Path:      r.Path,
			Suspended: suspended,
		})
	}

	for _, ns := range cfg.NamedSessions {
		identity := ns.QualifiedName()
		mode := ns.ModeOrDefault()
		status := namedSessionStatusForCity(cityPath, cfg, store, snapshot.CityName, identity, mode, suspendedRigs)
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
	cityName string,
	identity string,
	mode string,
	suspendedRigs map[string]bool,
) string {
	status := "reserved-unmaterialized"
	if spec, ok := findNamedSessionSpec(cfg, cityName, identity); ok {
		if mode == "always" && namedSessionBlockedBySuspension(cfg, spec.Agent, suspendedRigs) {
			status = "degraded blocked"
		}
	}
	if store == nil {
		return status
	}

	id, err := resolveSessionIDWithConfig(cityPath, cfg, store, identity)
	if err != nil {
		if errors.Is(err, session.ErrSessionNotFound) {
			return status
		}
		return "lookup error: " + err.Error()
	}

	bead, err := store.Get(id)
	if err != nil {
		return "lookup error: " + err.Error()
	}
	if state := strings.TrimSpace(bead.Metadata["state"]); state != "" {
		return state
	}
	return "materialized"
}

func collectCitySessionCounts(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City) (StatusSummaryJSON, error) {
	summary := StatusSummaryJSON{}
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
	catalog, err := workerSessionCatalogWithConfig(cityPath, store, sp, cfg)
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

func cityStatusJSONFromSnapshot(snapshot cityStatusSnapshot, summary StatusSummaryJSON) StatusJSON {
	var agents []StatusAgentJSON
	for _, row := range snapshot.Agents {
		agents = append(agents, row.Agent)
	}
	return StatusJSON{
		CityName:   snapshot.CityName,
		CityPath:   snapshot.CityPath,
		Controller: snapshot.Controller,
		Suspended:  snapshot.Suspended,
		Agents:     agents,
		Rigs:       snapshot.Rigs,
		Summary:    summary,
	}
}

func renderCityStatusText(snapshot cityStatusSnapshot, dops drainOps, stdout io.Writer) {
	fmt.Fprintf(stdout, "%s  %s\n", snapshot.CityName, snapshot.CityPath)                //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "  Controller: %s\n", controllerStatusLine(snapshot.Controller)) //nolint:errcheck // best-effort stdout
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
				fmt.Fprintf(stdout, "  %-24s%s\n", row.GroupName, row.ScaleLabel) //nolint:errcheck // best-effort stdout
			}
			status := agentStatusLine(row.Agent.Running, dops, row.SessionName, row.Agent.Suspended)
			if row.Expanded {
				fmt.Fprintf(stdout, "    %-22s%s\n", row.Agent.QualifiedName, status) //nolint:errcheck // best-effort stdout
			} else {
				fmt.Fprintf(stdout, "  %-24s%s\n", row.Agent.QualifiedName, status) //nolint:errcheck // best-effort stdout
			}
		}
		fmt.Fprintln(stdout)                                                                                        //nolint:errcheck // best-effort stdout
		fmt.Fprintf(stdout, "%d/%d agents running\n", snapshot.Summary.RunningAgents, snapshot.Summary.TotalAgents) //nolint:errcheck // best-effort stdout
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
}
