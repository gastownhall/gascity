package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
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
				sn := statusSessionName(store, snapshot.CityName, qualifiedInstance, cfg.Workspace.SessionTemplate)
				obs := observeSessionTargetWithWarning("gc status", cityPath, nil, sp, cfg, sn, stderr)
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
					SessionName: sn,
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

		sn := statusSessionName(store, snapshot.CityName, a.QualifiedName(), cfg.Workspace.SessionTemplate)
		obs := observeSessionTargetWithWarning("gc status", cityPath, nil, sp, cfg, sn, stderr)
		snapshot.Agents = append(snapshot.Agents, cityStatusAgentRow{
			Agent: StatusAgentJSON{
				Name:          a.Name,
				QualifiedName: a.QualifiedName(),
				Scope:         scope,
				Running:       obs.Running,
				Suspended:     suspended || obs.Suspended,
			},
			SessionName: sn,
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
	timeout := statusNamedSessionLookupTimeout
	if timeout <= 0 {
		return namedSessionStatusForCityDirect(cityPath, cfg, store, cityName, identity, mode, suspendedRigs)
	}
	statusCh := make(chan string, 1)
	go func() {
		statusCh <- namedSessionStatusForCityDirect(cityPath, cfg, store, cityName, identity, mode, suspendedRigs)
	}()
	select {
	case status := <-statusCh:
		return status
	case <-time.After(timeout):
		return "lookup timeout"
	}
}

func statusSessionName(store beads.Store, cityName, qualifiedName, sessionTemplate string) string {
	fallback := agent.SessionNameFor(cityName, qualifiedName, sessionTemplate)
	if store == nil {
		return fallback
	}
	timeout := statusSessionNameLookupTimeout
	if timeout <= 0 {
		if name := lookupSessionNameForStatus(store, qualifiedName); name != "" {
			return name
		}
		return fallback
	}

	nameCh := make(chan string, 1)
	go func() {
		nameCh <- lookupSessionNameForStatus(store, qualifiedName)
	}()
	select {
	case name := <-nameCh:
		if name != "" {
			return name
		}
		return fallback
	case <-time.After(timeout):
		return fallback
	}
}

func lookupSessionNameForStatus(store beads.Store, qualifiedName string) string {
	queries := []beads.ListQuery{
		{
			Label: session.LabelSession,
			Metadata: map[string]string{
				"agent_name": qualifiedName,
			},
			Limit: statusNamedSessionLookupLimit,
			Sort:  beads.SortCreatedDesc,
		},
		{
			Label: session.LabelSession,
			Metadata: map[string]string{
				"template": qualifiedName,
			},
			Limit: statusNamedSessionLookupLimit,
			Sort:  beads.SortCreatedDesc,
		},
		{
			Label: session.LabelSession,
			Metadata: map[string]string{
				"common_name": qualifiedName,
			},
			Limit: statusNamedSessionLookupLimit,
			Sort:  beads.SortCreatedDesc,
		},
	}
	for _, query := range queries {
		candidates, err := store.List(query)
		if err != nil {
			return ""
		}
		if sn := newSessionBeadSnapshot(candidates).FindSessionNameByTemplate(qualifiedName); sn != "" {
			return sn
		}
	}
	return ""
}

func namedSessionStatusForCityDirect(
	_ string,
	cfg *config.City,
	store beads.Store,
	cityName string,
	identity string,
	mode string,
	suspendedRigs map[string]bool,
) string {
	status := "reserved-unmaterialized"
	spec, hasSpec := findNamedSessionSpec(cfg, cityName, identity)
	if hasSpec {
		if mode == "always" && namedSessionBlockedBySuspension(cfg, spec.Agent, suspendedRigs) {
			status = "degraded blocked"
		}
	}
	if store == nil || !hasSpec {
		return status
	}

	bead, ok, err := namedSessionBeadForStatus(store, spec)
	if err != nil {
		return "lookup error: " + err.Error()
	}
	if !ok {
		return status
	}
	if state := strings.TrimSpace(bead.Metadata["state"]); state != "" {
		return state
	}
	return "materialized"
}

const statusNamedSessionLookupLimit = 5

func namedSessionBeadForStatus(store beads.Store, spec namedSessionSpec) (beads.Bead, bool, error) {
	queries := []beads.ListQuery{
		{
			Label: session.LabelSession,
			Metadata: map[string]string{
				namedSessionIdentityMetadata: spec.Identity,
			},
			Limit: statusNamedSessionLookupLimit,
			Sort:  beads.SortCreatedDesc,
		},
		{
			Label: session.LabelSession,
			Metadata: map[string]string{
				"session_name": spec.SessionName,
			},
			Limit: statusNamedSessionLookupLimit,
			Sort:  beads.SortCreatedDesc,
		},
	}
	for _, query := range queries {
		candidates, err := store.List(query)
		if err != nil {
			return beads.Bead{}, false, err
		}
		if bead, ok := session.FindCanonicalNamedSessionBead(candidates, spec); ok {
			return bead, true, nil
		}
	}
	return beads.Bead{}, false, nil
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
