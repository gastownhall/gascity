package main

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
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

	cfg, err := loadCityConfig(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc status: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	sp := newSessionProvider()
	dops := newDrainOps(sp)
	if jsonOutput {
		return doCityStatusJSON(sp, cfg, cityPath, stdout, stderr)
	}
	return doCityStatus(sp, dops, cfg, cityPath, stdout, stderr)
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
	_ = stderr // reserved for future error reporting

	cityName := cfg.Workspace.Name
	if cityName == "" {
		cityName = filepath.Base(cityPath)
	}

	// Header: city name and path.
	fmt.Fprintf(stdout, "%s  %s\n", cityName, cityPath) //nolint:errcheck // best-effort stdout

	ctrl := controllerStatusForCity(cityPath)
	fmt.Fprintf(stdout, "  Controller: %s\n", controllerStatusLine(ctrl)) //nolint:errcheck // best-effort stdout

	// Suspended status.
	if citySuspended(cfg) {
		fmt.Fprintf(stdout, "  Suspended:  yes\n") //nolint:errcheck // best-effort stdout
	} else {
		fmt.Fprintf(stdout, "  Suspended:  no\n") //nolint:errcheck // best-effort stdout
	}

	// Build set of suspended rig names.
	suspendedRigs := make(map[string]bool)
	for _, r := range cfg.Rigs {
		if r.Suspended {
			suspendedRigs[r.Name] = true
		}
	}

	// Agents section.
	if len(cfg.Agents) > 0 {
		fmt.Fprintln(stdout) //nolint:errcheck // best-effort stdout
		fmt.Fprintln(stdout, "Agents:")

		var totalAgents, runningAgents int

		for _, a := range cfg.Agents {
			// Effective suspended: agent-level or inherited from rig.
			suspended := a.Suspended || (a.Dir != "" && suspendedRigs[a.Dir])
			sp0 := scaleParamsFor(&a)

			if a.SupportsInstanceExpansion() {
				// Instance-expanding template — show header then instances.
				maxDisplay := fmt.Sprintf("max=%d", sp0.Max)
				if sp0.Max < 0 {
					maxDisplay = "max=unlimited"
				}
				fmt.Fprintf(stdout, "  %-24sscaled (min=%d, %s)\n", a.QualifiedName(), sp0.Min, maxDisplay) //nolint:errcheck // best-effort stdout
				for _, qualifiedInstance := range discoverPoolInstances(a.Name, a.Dir, sp0, &a, cityName, cfg.Workspace.SessionTemplate, sp) {
					sn := cliSessionName(cityPath, cityName, qualifiedInstance, cfg.Workspace.SessionTemplate)
					status := agentStatusLine(sp, dops, sn, suspended)
					fmt.Fprintf(stdout, "    %-22s%s\n", qualifiedInstance, status) //nolint:errcheck // best-effort stdout
					totalAgents++
					if sp.IsRunning(sn) {
						runningAgents++
					}
				}
			} else {
				// Non-expanding template.
				sn := cliSessionName(cityPath, cityName, a.QualifiedName(), cfg.Workspace.SessionTemplate)
				status := agentStatusLine(sp, dops, sn, suspended)
				fmt.Fprintf(stdout, "  %-24s%s\n", a.QualifiedName(), status) //nolint:errcheck // best-effort stdout
				totalAgents++
				if sp.IsRunning(sn) {
					runningAgents++
				}
			}
		}

		printNamedSessionStatus(stdout, cfg, cityName, cityPath, suspendedRigs)

		// Summary line.
		fmt.Fprintln(stdout)                                                      //nolint:errcheck // best-effort stdout
		fmt.Fprintf(stdout, "%d/%d agents running\n", runningAgents, totalAgents) //nolint:errcheck // best-effort stdout
	}

	// Rigs section.
	if len(cfg.Rigs) > 0 {
		fmt.Fprintln(stdout) //nolint:errcheck // best-effort stdout
		fmt.Fprintln(stdout, "Rigs:")
		for _, r := range cfg.Rigs {
			annotation := ""
			if r.Suspended {
				annotation = "  (suspended)"
			}
			fmt.Fprintf(stdout, "  %-24s%s%s\n", r.Name, r.Path, annotation) //nolint:errcheck // best-effort stdout
		}
	}

	// Chat sessions count (best-effort — skip if store unavailable).
	if store, err := openCityStoreAt(cityPath); err == nil {
		mgr := newSessionManagerWithConfig(cityPath, store, sp, cfg)
		if sessions, err := mgr.List("", ""); err == nil && len(sessions) > 0 {
			var active, suspended int
			for _, s := range sessions {
				switch s.State {
				case session.StateActive:
					active++
				case session.StateSuspended:
					suspended++
				}
			}
			fmt.Fprintln(stdout)                                                          //nolint:errcheck // best-effort stdout
			fmt.Fprintf(stdout, "Sessions: %d active, %d suspended\n", active, suspended) //nolint:errcheck // best-effort stdout
		}
	}

	return 0
}

func printNamedSessionStatus(stdout io.Writer, cfg *config.City, cityName, cityPath string, suspendedRigs map[string]bool) {
	if cfg == nil || len(cfg.NamedSessions) == 0 {
		return
	}
	fmt.Fprintln(stdout) //nolint:errcheck // best-effort stdout
	fmt.Fprintln(stdout, "Named sessions:")
	for i := range cfg.NamedSessions {
		ns := &cfg.NamedSessions[i]
		identity := ns.QualifiedName()
		mode := ns.ModeOrDefault()
		status := "reserved-unmaterialized"
		if spec, ok := findNamedSessionSpec(cfg, cityName, identity); ok {
			if mode == "always" && namedSessionBlockedBySuspension(cfg, spec.Agent, suspendedRigs) {
				status = "degraded blocked"
			}
		}
		if store, err := openCityStoreAt(cityPath); err == nil {
			if id, err := resolveSessionIDWithConfig(cityPath, cfg, store, identity); err == nil {
				if bead, getErr := store.Get(id); getErr == nil {
					if state := strings.TrimSpace(bead.Metadata["state"]); state != "" {
						status = state
					} else {
						status = "materialized"
					}
				}
			}
		}
		fmt.Fprintf(stdout, "  %-24s%s (%s)\n", identity, status, mode) //nolint:errcheck // best-effort stdout
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

// doCityStatusJSON outputs city status as JSON. Accepts injected providers
// for testability.
func doCityStatusJSON(
	sp runtime.Provider,
	cfg *config.City,
	cityPath string,
	stdout, stderr io.Writer,
) int {
	_ = stderr // reserved for future error reporting

	cityName := cfg.Workspace.Name
	if cityName == "" {
		cityName = filepath.Base(cityPath)
	}

	// Build suspended rig lookup.
	suspendedRigs := make(map[string]bool)
	for _, r := range cfg.Rigs {
		if r.Suspended {
			suspendedRigs[r.Name] = true
		}
	}

	// Controller.
	ctrl := controllerStatusForCity(cityPath)

	// Agents.
	var agents []StatusAgentJSON
	var totalAgents, runningAgents int
	for _, a := range cfg.Agents {
		suspended := a.Suspended || (a.Dir != "" && suspendedRigs[a.Dir])
		sp0 := scaleParamsFor(&a)
		scope := "city"
		if a.Dir != "" {
			scope = "rig"
		}

		if a.SupportsInstanceExpansion() {
			// Instance-expanding template — emit each instance.
			for _, qualifiedInstance := range discoverPoolInstances(a.Name, a.Dir, sp0, &a, cityName, cfg.Workspace.SessionTemplate, sp) {
				_, instanceName := config.ParseQualifiedName(qualifiedInstance)
				sn := cliSessionName(cityPath, cityName, qualifiedInstance, cfg.Workspace.SessionTemplate)
				running := sp.IsRunning(sn)
				agents = append(agents, StatusAgentJSON{
					Name:          instanceName,
					QualifiedName: qualifiedInstance,
					Scope:         scope,
					Running:       running,
					Suspended:     suspended,
					Pool:          nil,
				})
				totalAgents++
				if running {
					runningAgents++
				}
			}
		} else {
			// Non-expanding template.
			sn := cliSessionName(cityPath, cityName, a.QualifiedName(), cfg.Workspace.SessionTemplate)
			running := sp.IsRunning(sn)
			agents = append(agents, StatusAgentJSON{
				Name:          a.Name,
				QualifiedName: a.QualifiedName(),
				Scope:         scope,
				Running:       running,
				Suspended:     suspended,
				Pool:          nil,
			})
			totalAgents++
			if running {
				runningAgents++
			}
		}
	}

	// Rigs.
	var rigs []StatusRigJSON
	for _, r := range cfg.Rigs {
		rigs = append(rigs, StatusRigJSON{
			Name:      r.Name,
			Path:      r.Path,
			Suspended: r.Suspended,
		})
	}

	summary := StatusSummaryJSON{TotalAgents: totalAgents, RunningAgents: runningAgents}

	// Chat sessions count (best-effort).
	if store, err := openCityStoreAt(cityPath); err == nil {
		mgr := newSessionManagerWithConfig(cityPath, store, sp, cfg)
		if sessions, err := mgr.List("", ""); err == nil {
			for _, s := range sessions {
				switch s.State {
				case session.StateActive:
					summary.ActiveSessions++
				case session.StateSuspended:
					summary.SuspendedSessions++
				}
			}
		}
	}

	status := StatusJSON{
		CityName:   cityName,
		CityPath:   cityPath,
		Controller: ctrl,
		Suspended:  citySuspended(cfg),
		Agents:     agents,
		Rigs:       rigs,
		Summary:    summary,
	}

	data, err := json.MarshalIndent(status, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "gc status: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintln(stdout, string(data)) //nolint:errcheck // best-effort stdout
	return 0
}

func controllerStatusForCity(cityPath string) ControllerJSON {
	if pid := controllerAlive(cityPath); pid != 0 {
		return ControllerJSON{Running: true, PID: pid, Mode: "standalone"}
	}
	_, registered, err := registeredCityEntry(cityPath)
	if err == nil && registered {
		ctrl := ControllerJSON{Mode: "supervisor"}
		if pid := supervisorAliveHook(); pid != 0 {
			ctrl.PID = pid
			if running, status, known := supervisorCityRunningHook(cityPath); known {
				ctrl.Running = running
				ctrl.Status = status
			}
		}
		return ctrl
	}
	return ControllerJSON{}
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
			return fmt.Sprintf("supervisor (PID %d)", ctrl.PID)
		}
		if ctrl.PID != 0 {
			return fmt.Sprintf("supervisor (PID %d, %s)", ctrl.PID, controllerSupervisorStatusText(ctrl.Status))
		}
		return "supervisor-managed (supervisor not running)"
	case "standalone":
		if ctrl.Running {
			return fmt.Sprintf("standalone (PID %d)", ctrl.PID)
		}
	}
	return "stopped"
}
