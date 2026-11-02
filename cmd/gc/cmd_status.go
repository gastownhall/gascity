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
	statusSnapshot := loadStatusSessionSnapshot(cityPath, cfg, cliSessionStore(store, cfg, cityPath), stderr)
	sp, err := newStatusSessionProviderForCityWithSnapshot(cfg, cityPath, statusSnapshot)
	if err != nil {
		fmt.Fprintf(stderr, "gc rig status: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
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
// path. Both routes print the same contract: the CLI's own city identity, the
// rig's own suspension, configured agent names, and observed liveness and
// session ids (from the server on the API route). Emits exactly one
// route=... log line per exit path (GC_DEBUG).
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
			return renderRigStatusFromAPI(cr, cityPath, cityName, rig, rigAgents, dops, jsonOutput, stdout, stderr)
		}
		if !api.ShouldFallbackForRead(c, err) {
			logRoute(stderr, cmdName, "api", "error")
			fmt.Fprintf(stderr, "gc rig status: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		logRoute(stderr, cmdName, "fallback", api.FallbackReason(c, err))
	} else {
		logRoute(stderr, cmdName, "fallback", nilReason)
	}
	return doRigStatusWithStoreAndSnapshot(sp, dops, rig, rigAgents, cityPath, cityName, sessionTemplate, cfg, store, statusSnapshot, jsonOutput, stdout, stderr)
}

// renderRigStatusFromAPI renders the supervisor's StatusView rows for one
// rig in the local route's contract. The envelope (city identity, rig
// suspension) comes from the CLI's own config and suspension state; the
// rows carry server-observed liveness and session ids, named by their
// configured agent. Text output emits the header lines ("<rig>:", "Path:",
// "Suspended:") and dispatches each row to agentStatusLineWithPartial.
func renderRigStatusFromAPI(cr api.CachedRead[api.StatusView], cityPath, cityName string, rig config.Rig, rigAgents []config.Agent, dops drainOps, jsonOutput bool, stdout, stderr io.Writer) int {
	rigSuspended, err := loadRigStatusSuspension(cityPath, rig)
	if err != nil {
		fmt.Fprintf(stderr, "gc rig status: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if jsonOutput {
		nameByQualifiedName := make(map[string]string, len(rigAgents))
		for _, a := range rigAgents {
			nameByQualifiedName[a.QualifiedName()] = a.Name
		}
		result := newRigStatusJSON(cityPath, cityName, rig, rigSuspended)
		for _, a := range cr.Body.Agents {
			if !rigStatusAgentBelongsToRig(a, rig.Name) {
				continue
			}
			name, ok := nameByQualifiedName[a.GroupName]
			if !ok {
				name = a.Name
			}
			result.Agents = append(result.Agents, newRigStatusAgent(name, a.QualifiedName, a.SessionName, a.SessionID, a.Running, a.Suspended || rigSuspended, dops))
		}
		if err := writeCLIJSONLine(stdout, result); err != nil {
			fmt.Fprintf(stderr, "gc rig status: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return 0
	}

	writeRigStatusHeader(stdout, rig, rigSuspended)
	for _, a := range cr.Body.Agents {
		if !rigStatusAgentBelongsToRig(a, rig.Name) {
			continue
		}
		status := agentStatusLineWithPartial(a.Running, dops, a.SessionName, a.Suspended || rigSuspended, cr.Body.Partial)
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

// loadRigStatusSuspension reports the rig's own effective suspension: the
// suspension state file, else the rig's suspended_on_start.
func loadRigStatusSuspension(cityPath string, rig config.Rig) (bool, error) {
	st, err := loadSuspensionState(fsys.OSFS{}, cityPath)
	if err != nil {
		return false, fmt.Errorf("loading suspension state: %w", err)
	}
	return suspensionstate.EffectiveRigSuspended(st, rig.Name, rig.EffectiveSuspendedOnStart()), nil
}

// newRigStatusJSON builds the "gc rig status --json" envelope shared by both
// routes. Agents is never nil, so a rig with no agents prints [].
func newRigStatusJSON(cityPath, cityName string, rig config.Rig, rigSuspended bool) RigStatusJSON {
	return RigStatusJSON{
		SchemaVersion: "1",
		CityPath:      cityPath,
		CityName:      cityName,
		Rig: RigStatusRig{
			Name:          rig.Name,
			Path:          rig.Path,
			Prefix:        rig.EffectivePrefix(),
			DefaultBranch: rig.EffectiveDefaultBranch(),
			Suspended:     rigSuspended,
			Beads:         rigBeadsStatus(fsys.OSFS{}, rig.Path),
		},
		Agents: []RigStatusAgent{},
	}
}

// newRigStatusAgent derives one agent row, including its draining flag and
// status word, identically on both routes.
func newRigStatusAgent(name, qualifiedName, runtimeSessionName, sessionID string, running, suspended bool, dops drainOps) RigStatusAgent {
	draining := false
	if running {
		draining, _ = dops.isDraining(runtimeSessionName)
	}
	status := "stopped"
	if running {
		status = "running"
		if draining {
			status = "draining"
		}
	}
	if suspended && !running {
		status = "suspended"
	}
	return RigStatusAgent{
		Name:               name,
		QualifiedName:      qualifiedName,
		RuntimeSessionName: runtimeSessionName,
		SessionID:          sessionID,
		Running:            running,
		Suspended:          suspended,
		Draining:           draining,
		Status:             status,
	}
}

// rigStatusAgentSuspended is the local route's suspension for one row: the
// agent's own config, its rig's effective suspension, or an observed or
// session-recorded suspension.
func rigStatusAgentSuspended(agentSuspended, rigSuspended bool, obs worker.LiveObservation, target statusObservationTarget) bool {
	return agentSuspended || rigSuspended || obs.Suspended || target.suspended
}

func writeRigStatusHeader(stdout io.Writer, rig config.Rig, rigSuspended bool) {
	suspStr := "no"
	if rigSuspended {
		suspStr = "yes"
	}
	fmt.Fprintf(stdout, "%s:\n", rig.Name)              //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "  Path:       %s\n", rig.Path) //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "  Suspended:  %s\n", suspStr)  //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "  Agents:\n")                  //nolint:errcheck // best-effort stdout
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
	rigSuspended, err := loadRigStatusSuspension(cityPath, rig)
	if err != nil {
		fmt.Fprintf(stderr, "gc rig status: %v\n", err) //nolint:errcheck
		return 1
	}
	if jsonOutput {
		return renderRigStatusJSON(sp, dops, rig, agents, cityPath, cityName, sessionTemplate, cfg, store, statusSnapshot, rigSuspended, stdout, stderr)
	}

	writeRigStatusHeader(stdout, rig, rigSuspended)
	for _, a := range agents {
		sp0 := scaleParamsFor(&a)
		identities := []string{a.QualifiedName()}
		if a.SupportsInstanceExpansion() {
			identities = discoverPoolInstances(a.Name, a.Dir, sp0, &a, cityName, sessionTemplate, sp)
		}
		for _, identity := range identities {
			target := statusObservationTargetForIdentity(statusSnapshot, cityName, identity, sessionTemplate)
			obs := observeSessionTargetWithWarning("gc rig status", cityPath, store, sp, cfg, target, stderr)
			suspended := rigStatusAgentSuspended(a.Suspended, rigSuspended, obs, target)
			status := agentStatusLineWithPartial(obs.Running, dops, target.runtimeSessionName, suspended, statusProviderPartial(sp))
			fmt.Fprintf(stdout, "    %-12s%s\n", identity, status) //nolint:errcheck // best-effort stdout
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
	rigSuspended bool,
	stdout, stderr io.Writer,
) int {
	result := newRigStatusJSON(cityPath, cityName, rig, rigSuspended)
	for _, a := range agents {
		sp0 := scaleParamsFor(&a)
		identities := []string{a.QualifiedName()}
		if a.SupportsInstanceExpansion() {
			identities = discoverPoolInstances(a.Name, a.Dir, sp0, &a, cityName, sessionTemplate, sp)
		}
		for _, identity := range identities {
			target := statusObservationTargetForIdentity(statusSnapshot, cityName, identity, sessionTemplate)
			obs := observeSessionTargetWithWarning("gc rig status", cityPath, store, sp, cfg, target, stderr)
			suspended := rigStatusAgentSuspended(a.Suspended, rigSuspended, obs, target)
			result.Agents = append(result.Agents, newRigStatusAgent(a.Name, identity, target.runtimeSessionName, target.sessionID, obs.Running, suspended, dops))
		}
	}
	if err := writeCLIJSONLine(stdout, result); err != nil {
		fmt.Fprintf(stderr, "gc rig status: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return 0
}

// agentStatusLineWithPartial returns a human-readable status string for an
// agent session. The drain probe is a runtime metadata lookup (tmux
// show-environment) per session; skip it when the session is not running
// because the draining flag is meaningless then and the probe dominates wall
// time on idle cities.
func agentStatusLineWithPartial(running bool, dops drainOps, sn string, suspended bool, partial bool) string {
	if !running {
		if partial {
			if suspended {
				return "unknown  (partial status, suspended)"
			}
			return "unknown  (partial status)"
		}
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
