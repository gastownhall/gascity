package main

import (
	"fmt"
	"io"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/worker"
	"github.com/spf13/cobra"
)

// ---------------------------------------------------------------------------
// gc rig status <name>
// ---------------------------------------------------------------------------

// newRigStatusCmd creates the "gc rig status <name>" subcommand.
func newRigStatusCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "status [name]",
		Short: "Show rig status and agent running state",
		Args:  cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdRigStatus(args, jsonOutput, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
		ValidArgsFunction: completeRigNames,
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output in JSON format")
	return cmd
}

// cmdRigStatus is the CLI entry point for showing rig status.
func cmdRigStatus(args []string, jsonOutput bool, stdout, stderr io.Writer) int {
	ctx, err := resolveContext()
	if err != nil {
		fmt.Fprintf(stderr, "gc rig status: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	rigName := ctx.RigName
	if len(args) > 0 {
		rigName = args[0]
	}
	if rigName == "" {
		fmt.Fprintln(stderr, "gc rig status: missing rig name") //nolint:errcheck // best-effort stderr
		return 1
	}
	cityPath := ctx.CityPath
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc rig status: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Find the rig.
	var rig config.Rig
	found := false
	for _, r := range cfg.Rigs {
		if r.Name == rigName {
			rig = r
			found = true
			break
		}
	}
	if !found {
		fmt.Fprintln(stderr, rigNotFoundMsg("gc rig status", rigName, cfg)) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Collect agents belonging to this rig.
	var rigAgents []config.Agent
	for _, a := range cfg.Agents {
		if a.Dir == rigName {
			rigAgents = append(rigAgents, a)
		}
	}

	cityName := loadedCityName(cfg, cityPath)
	var store beads.Store
	if cityPath != "" {
		if opened, err := openCityStoreAt(cityPath); err == nil {
			store = opened
		}
	}
	statusSnapshot := loadStatusSessionSnapshot(store, stderr)
	sp := newStatusSessionProviderForCityWithSnapshot(cfg, cityPath, statusSnapshot)
	dops := newDrainOps(sp)
	return doRigStatusWithStoreAndSnapshot(sp, dops, rig, rigAgents, cityPath, cityName, cfg.Workspace.SessionTemplate, cfg, store, statusSnapshot, jsonOutput, stdout, stderr)
}

// RigStatusJSON is the JSON output format for "gc rig status --json".
type RigStatusJSON struct {
	SchemaVersion string           `json:"schema_version"`
	CityPath      string           `json:"city_path"`
	CityName      string           `json:"city_name"`
	Rig           RigStatusRig     `json:"rig"`
	Agents        []RigStatusAgent `json:"agents"`
}

// RigStatusRig describes the selected rig in "gc rig status --json".
type RigStatusRig struct {
	Name          string `json:"name"`
	Path          string `json:"path"`
	Prefix        string `json:"prefix"`
	DefaultBranch string `json:"default_branch,omitempty"`
	Suspended     bool   `json:"suspended"`
	Beads         string `json:"beads"`
}

// RigStatusAgent describes an agent or concrete pool instance for a rig.
type RigStatusAgent struct {
	Name               string `json:"name"`
	QualifiedName      string `json:"qualified_name"`
	RuntimeSessionName string `json:"runtime_session_name"`
	SessionID          string `json:"session_id,omitempty"`
	Running            bool   `json:"running"`
	Suspended          bool   `json:"suspended"`
	Draining           bool   `json:"draining"`
	Status             string `json:"status"`
}

func doRigStatusWithStoreAndSnapshot(
	sp runtime.Provider,
	dops drainOps,
	rig config.Rig,
	agents []config.Agent,
	cityPath, cityName, sessionTemplate string,
	cfg *config.City,
	store beads.Store,
	statusSnapshot *sessionBeadSnapshot,
	jsonOutput bool,
	stdout, stderr io.Writer,
) int {
	registerStatusProviderACPRoutes(sp, statusSnapshot, cityName, cfg)
	if jsonOutput {
		return renderRigStatusJSON(sp, dops, rig, agents, cityPath, cityName, sessionTemplate, cfg, store, statusSnapshot, stdout, stderr)
	}

	suspStr := "no"
	if rig.Suspended {
		suspStr = "yes"
	}

	fmt.Fprintf(stdout, "%s:\n", rig.Name)              //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "  Path:       %s\n", rig.Path) //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "  Suspended:  %s\n", suspStr)  //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "  Agents:\n")                  //nolint:errcheck // best-effort stdout

	for _, a := range agents {
		sp0 := scaleParamsFor(&a)
		if !a.SupportsInstanceExpansion() {
			target := statusObservationTargetForIdentity(statusSnapshot, cityName, a.QualifiedName(), sessionTemplate)
			obs := observeSessionTargetWithWarning("gc rig status", cityPath, store, sp, cfg, target, stderr)
			status := agentStatusLine(obs.Running, dops, target.runtimeSessionName, a.Suspended || obs.Suspended)
			fmt.Fprintf(stdout, "    %-12s%s\n", a.QualifiedName(), status) //nolint:errcheck // best-effort stdout
		} else {
			for _, qualifiedInstance := range discoverPoolInstances(a.Name, a.Dir, sp0, &a, cityName, sessionTemplate, sp) {
				target := statusObservationTargetForIdentity(statusSnapshot, cityName, qualifiedInstance, sessionTemplate)
				obs := observeSessionTargetWithWarning("gc rig status", cityPath, store, sp, cfg, target, stderr)
				status := agentStatusLine(obs.Running, dops, target.runtimeSessionName, a.Suspended || obs.Suspended)
				fmt.Fprintf(stdout, "    %-12s%s\n", qualifiedInstance, status) //nolint:errcheck // best-effort stdout
			}
		}
	}
	return 0
}

func renderRigStatusJSON(
	sp runtime.Provider,
	dops drainOps,
	rig config.Rig,
	agents []config.Agent,
	cityPath, cityName, sessionTemplate string,
	cfg *config.City,
	store beads.Store,
	statusSnapshot *sessionBeadSnapshot,
	stdout, stderr io.Writer,
) int {
	result := RigStatusJSON{
		SchemaVersion: "1",
		CityPath:      cityPath,
		CityName:      cityName,
		Rig: RigStatusRig{
			Name:          rig.Name,
			Path:          rig.Path,
			Prefix:        rig.EffectivePrefix(),
			DefaultBranch: rig.EffectiveDefaultBranch(),
			Suspended:     rig.Suspended,
			Beads:         rigBeadsStatus(fsys.OSFS{}, rig.Path),
		},
	}
	for _, a := range agents {
		sp0 := scaleParamsFor(&a)
		if !a.SupportsInstanceExpansion() {
			target := statusObservationTargetForIdentity(statusSnapshot, cityName, a.QualifiedName(), sessionTemplate)
			obs := observeSessionTargetWithWarning("gc rig status", cityPath, store, sp, cfg, target, stderr)
			result.Agents = append(result.Agents, rigStatusAgentJSON(a.Name, a.QualifiedName(), target, obs, dops, a.Suspended))
			continue
		}
		for _, qualifiedInstance := range discoverPoolInstances(a.Name, a.Dir, sp0, &a, cityName, sessionTemplate, sp) {
			target := statusObservationTargetForIdentity(statusSnapshot, cityName, qualifiedInstance, sessionTemplate)
			obs := observeSessionTargetWithWarning("gc rig status", cityPath, store, sp, cfg, target, stderr)
			result.Agents = append(result.Agents, rigStatusAgentJSON(a.Name, qualifiedInstance, target, obs, dops, a.Suspended))
		}
	}
	if err := writeCLIJSONLine(stdout, result); err != nil {
		fmt.Fprintf(stderr, "gc rig status: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return 0
}

func rigStatusAgentJSON(name, qualifiedName string, target statusObservationTarget, obs worker.LiveObservation, dops drainOps, agentSuspended bool) RigStatusAgent {
	suspended := agentSuspended || obs.Suspended || target.suspended
	draining := false
	if obs.Running {
		draining, _ = dops.isDraining(target.runtimeSessionName)
	}
	status := "stopped"
	if obs.Running {
		status = "running"
		if draining {
			status = "draining"
		}
	}
	if suspended && !obs.Running {
		status = "suspended"
	}
	return RigStatusAgent{
		Name:               name,
		QualifiedName:      qualifiedName,
		RuntimeSessionName: target.runtimeSessionName,
		SessionID:          target.sessionID,
		Running:            obs.Running,
		Suspended:          suspended,
		Draining:           draining,
		Status:             status,
	}
}

// agentStatusLine returns a human-readable status string for an agent session.
// The drain probe is a runtime metadata lookup (tmux show-environment) per
// session; skip it when the session is not running because the draining flag
// is meaningless then and the probe dominates wall time on idle cities.
func agentStatusLine(running bool, dops drainOps, sn string, suspended bool) string {
	if !running {
		if suspended {
			return "stopped  (suspended)"
		}
		return "stopped"
	}
	if draining, _ := dops.isDraining(sn); draining {
		return "running  (draining)"
	}
	return "running"
}
