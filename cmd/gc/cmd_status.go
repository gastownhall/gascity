package main

import (
	"fmt"
	"io"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/spf13/cobra"
)

// ---------------------------------------------------------------------------
// gc rig status <name>
// ---------------------------------------------------------------------------

// newRigStatusCmd creates the "gc rig status <name>" subcommand.
func newRigStatusCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "status [name]",
		Short: "Show rig status and agent running state",
		Args:  cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdRigStatus(args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

// cmdRigStatus is the CLI entry point for showing rig status. Routes
// through the supervisor API (shared /status handler) and filters the
// response to this rig's agents client-side; falls back to the local
// snapshot otherwise.
func cmdRigStatus(args []string, stdout, stderr io.Writer) int {
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
	sp := newSessionProvider()
	dops := newDrainOps(sp)
	c, reason := rigStatusAPIClient(cityPath)
	return routeRigStatus(cityPath, cityName, rig, rigAgents, cfg.Workspace.SessionTemplate, sp, dops, c, reason, stdout, stderr)
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
	sp runtime.Provider,
	dops drainOps,
	c *api.Client,
	nilReason string,
	stdout, stderr io.Writer,
) int {
	const cmdName = "rig status"
	if c != nil {
		cr, err := c.GetStatus()
		if err == nil {
			logRoute(stderr, cmdName, "api", "")
			return renderRigStatusFromAPI(cr, rig, dops, stdout)
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
	return doRigStatus(sp, dops, rig, rigAgents, cityPath, cityName, sessionTemplate, stdout, stderr)
}

// renderRigStatusFromAPI filters the supervisor's StatusView by rig name
// and renders the same text output the fallback path produces. Pool
// expansion, scale labels, and drain-state rendering all live in
// agentStatusLine, so this function only needs to emit header lines
// ("<rig>:", "Path:", "Suspended:") and dispatch to agentStatusLine for
// each agent row.
func renderRigStatusFromAPI(cr api.CachedRead[api.StatusView], rig config.Rig, dops drainOps, stdout io.Writer) int {
	suspStr := "no"
	// Prefer the server's suspension view when it carries this rig;
	// fall back to the local config when the server omits it (e.g., rig
	// misconfigured or not yet reconciled).
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

	fmt.Fprintf(stdout, "%s:\n", rig.Name)              //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "  Path:       %s\n", rig.Path) //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "  Suspended:  %s\n", suspStr)  //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "  Agents:\n")                  //nolint:errcheck // best-effort stdout

	for _, a := range cr.Body.Agents {
		if a.Scope != "rig" {
			continue
		}
		// Qualified name starts with "<rig>/"; filter to this rig only.
		prefix := rig.Name + "/"
		if len(a.QualifiedName) <= len(prefix) || a.QualifiedName[:len(prefix)] != prefix {
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

// doRigStatus prints rig info and per-agent running state.
func doRigStatus(
	sp runtime.Provider,
	dops drainOps,
	rig config.Rig,
	agents []config.Agent,
	cityPath, cityName, sessionTemplate string,
	stdout, stderr io.Writer,
) int {
	_ = stderr // reserved for future error reporting
	var store beads.Store
	if cityPath != "" {
		if opened, err := openCityStoreAt(cityPath); err == nil {
			store = opened
		}
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
			sn := cliSessionName(cityPath, cityName, a.QualifiedName(), sessionTemplate)
			obs := observeSessionTargetWithWarning("gc rig status", cityPath, store, sp, nil, sn, stderr)
			status := agentStatusLine(obs.Running, dops, sn, a.Suspended || obs.Suspended)
			fmt.Fprintf(stdout, "    %-12s%s\n", a.QualifiedName(), status) //nolint:errcheck // best-effort stdout
		} else {
			for _, qualifiedInstance := range discoverPoolInstances(a.Name, a.Dir, sp0, &a, cityName, sessionTemplate, sp) {
				sn := cliSessionName(cityPath, cityName, qualifiedInstance, sessionTemplate)
				obs := observeSessionTargetWithWarning("gc rig status", cityPath, store, sp, nil, sn, stderr)
				status := agentStatusLine(obs.Running, dops, sn, a.Suspended || obs.Suspended)
				fmt.Fprintf(stdout, "    %-12s%s\n", qualifiedInstance, status) //nolint:errcheck // best-effort stdout
			}
		}
	}
	return 0
}

// agentStatusLine returns a human-readable status string for an agent session.
func agentStatusLine(running bool, dops drainOps, sn string, suspended bool) string {
	draining, _ := dops.isDraining(sn)

	switch {
	case running && draining:
		return "running  (draining)"
	case running:
		return "running"
	case suspended:
		return "stopped  (suspended)"
	default:
		return "stopped"
	}
}
