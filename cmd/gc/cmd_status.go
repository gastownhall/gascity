package main

import (
	"fmt"
	"io"
	"path/filepath"

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

// cmdRigStatus is the CLI entry point for showing rig status.
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

	// Collect agents belonging to this rig.
	var rigAgents []config.Agent
	for _, a := range cfg.Agents {
		if a.Dir == rigName {
			rigAgents = append(rigAgents, a)
		}
	}

	cityName := cfg.Workspace.Name
	if cityName == "" {
		cityName = filepath.Base(cityPath)
	}
	sp := newSessionProvider()
	dops := newDrainOps(sp)
	return doRigStatus(sp, dops, rig, rigAgents, cityPath, cityName, cfg.Workspace.SessionTemplate, stdout, stderr)
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
			obs, _ := workerObserveSessionTargetWithConfig(cityPath, store, sp, nil, sn)
			status := agentStatusLine(obs.Running, dops, sn, a.Suspended || obs.Suspended)
			fmt.Fprintf(stdout, "    %-12s%s\n", a.QualifiedName(), status) //nolint:errcheck // best-effort stdout
		} else {
			for _, qualifiedInstance := range discoverPoolInstances(a.Name, a.Dir, sp0, &a, cityName, sessionTemplate, sp) {
				sn := cliSessionName(cityPath, cityName, qualifiedInstance, sessionTemplate)
				obs, _ := workerObserveSessionTargetWithConfig(cityPath, store, sp, nil, sn)
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
