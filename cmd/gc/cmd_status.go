package main

import (
	"fmt"
	"io"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/suspensionstate"
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

	// Collect agents belonging to this rig for the fallback path.
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
	statusSnapshot := loadStatusSessionSnapshot(cityPath, cfg, store, stderr)
	sp := newStatusSessionProviderForCityWithSnapshot(cfg, cityPath, statusSnapshot)
	dops := newDrainOps(sp)
	c, reason := rigStatusAPIClient(cityPath)
	return routeRigStatus(cityPath, cityName, rig, rigAgents, cfg.Workspace.SessionTemplate, cfg, store, statusSnapshot, sp, dops, c, reason, jsonOutput, stdout, stderr)
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

// rigStatusAPIClient returns (client, "") when the API path is available,
// or (nil, reason) when the caller should fall back. Indirected through a
// var so tests inject a client pointed at httptest.Server or force a
// specific fallback reason without spinning up a real controller.
var rigStatusAPIClient = func(cityPath string) (*api.Client, string) {
	if c := apiClient(cityPath); c != nil {
		return c, ""
	}
	return nil, apiClientFallbackReason(cityPath)
}

// routeRigStatus dispatches `gc rig status <name>` to the supervisor API
// when a controller is up; otherwise falls back to the local observation
// path. Emits exactly one route=... log line per exit path (GC_DEBUG).
func routeRigStatus(
	cityPath, cityName string,
	rig config.Rig,
	rigAgents []config.Agent,
	sessionTemplate string,
	cfg *config.City,
	store beads.Store,
	statusSnapshot *sessionBeadSnapshot,
	sp runtime.Provider,
	dops drainOps,
	c *api.Client,
	nilReason string,
	jsonOutput bool,
	stdout, stderr io.Writer,
) int {
	const cmdName = "rig status"
	if c != nil {
		cr, err := c.GetStatus()
		if err == nil {
			logRoute(stderr, cmdName, "api", "")
			return renderRigStatusFromAPI(cr, rig, dops, jsonOutput, stdout, stderr)
		}
		if !api.ShouldFallbackForRead(err) {
			logRoute(stderr, cmdName, "api", "error")
			fmt.Fprintf(stderr, "gc rig status: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		logRoute(stderr, cmdName, "fallback", api.FallbackReason(err))
	} else {
		logRoute(stderr, cmdName, "fallback", nilReason)
	}
	return doRigStatusWithStoreAndSnapshot(sp, dops, rig, rigAgents, cityPath, cityName, sessionTemplate, cfg, store, statusSnapshot, jsonOutput, stdout, stderr)
}

// renderRigStatusFromAPI filters the supervisor's StatusView by rig name
// and renders the same text output the fallback path produces. Pool
// expansion, scale labels, and drain-state rendering all live in
// agentStatusLine, so this function only needs to emit header lines
// ("<rig>:", "Path:", "Suspended:") and dispatch to agentStatusLine for
// each agent row.
func renderRigStatusFromAPI(cr api.CachedRead[api.StatusView], rig config.Rig, dops drainOps, jsonOutput bool, stdout, stderr io.Writer) int {
	suspStr := "no"
	serverSuspended := rig.Suspended
	for _, r := range cr.Body.Rigs {
		if r.Name == rig.Name {
			serverSuspended = r.Suspended
			break
		}
	}
	if serverSuspended {
		suspStr = "yes"
	}

	if jsonOutput {
		result := RigStatusJSON{
			SchemaVersion: "1",
			CityPath:      cr.Body.CityPath,
			CityName:      cr.Body.CityName,
			Rig: RigStatusRig{
				Name:          rig.Name,
				Path:          rig.Path,
				Prefix:        rig.EffectivePrefix(),
				DefaultBranch: rig.EffectiveDefaultBranch(),
				Suspended:     serverSuspended,
				Beads:         rigBeadsStatus(fsys.OSFS{}, rig.Path),
			},
		}
		for _, a := range cr.Body.Agents {
			if !rigStatusAgentBelongsToRig(a, rig.Name) {
				continue
			}
			result.Agents = append(result.Agents, rigStatusAgentJSONFromAPI(a, dops))
		}
		if err := writeCLIJSONLine(stdout, result); err != nil {
			fmt.Fprintf(stderr, "gc rig status: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return 0
	}

	fmt.Fprintf(stdout, "%s:\n", rig.Name)              //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "  Path:       %s\n", rig.Path) //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "  Suspended:  %s\n", suspStr)  //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "  Agents:\n")                  //nolint:errcheck // best-effort stdout

	for _, a := range cr.Body.Agents {
		if !rigStatusAgentBelongsToRig(a, rig.Name) {
			continue
		}
		status := agentStatusLine(a.Running, dops, a.SessionName, a.Suspended)
		fmt.Fprintf(stdout, "    %-12s%s\n", a.QualifiedName, status) //nolint:errcheck // best-effort stdout
	}
	if cr.AgeSeconds > cacheAgeBannerThresholdSeconds {
		fmt.Fprintf(stdout, "(cache age: %.0fs — reconciler may be lagging)\n", cr.AgeSeconds) //nolint:errcheck // best-effort stdout
	}
	return 0
}

func rigStatusAgentBelongsToRig(a api.StatusAgentView, rigName string) bool {
	if a.Scope != "rig" {
		return false
	}
	prefix := rigName + "/"
	return len(a.QualifiedName) > len(prefix) && a.QualifiedName[:len(prefix)] == prefix
}

func rigStatusAgentJSONFromAPI(a api.StatusAgentView, dops drainOps) RigStatusAgent {
	draining := false
	if a.Running {
		draining, _ = dops.isDraining(a.SessionName)
	}
	status := "stopped"
	if a.Running {
		status = "running"
		if draining {
			status = "draining"
		}
	}
	if a.Suspended && !a.Running {
		status = "suspended"
	}
	return RigStatusAgent{
		Name:               a.Name,
		QualifiedName:      a.QualifiedName,
		RuntimeSessionName: a.SessionName,
		Running:            a.Running,
		Suspended:          a.Suspended,
		Draining:           draining,
		Status:             status,
	}
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

	suspState, _ := loadSuspensionState(fsys.OSFS{}, cityPath)
	suspStr := "no"
	if suspensionstate.EffectiveRigSuspended(suspState, rig.Name, rig.EffectiveSuspendedOnStart()) {
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
