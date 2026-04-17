package main

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/spf13/cobra"
)

// newRestartCmd creates the top-level "gc restart" command.
func newRestartCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "restart [path]",
		Short: "Restart all agent sessions in the city",
		Long: `Restart the city by stopping it then starting it again.

Equivalent to running "gc stop" followed by "gc start". Under supervisor
mode this unregisters the city, then re-registers it and triggers an
immediate reconcile.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdRestart(args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

// cmdRestart stops the city, then re-starts it under the supervisor.
func cmdRestart(args []string, stdout, stderr io.Writer) int {
	if code := cmdStop(args, stdout, stderr); code != 0 {
		return code
	}
	return doStart(args, false /*controllerMode*/, stdout, stderr)
}

// newRigRestartCmd creates the "gc rig restart <name>" subcommand.
func newRigRestartCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "restart [name]",
		Short: "Restart all agents in a rig",
		Long: `Kill all agent sessions belonging to a rig.

The reconciler will restart the agents on its next tick. This is a
quick way to force-refresh all agents working on a particular project.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdRigRestart(args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

// cmdRigRestart kills all agent sessions in a rig. The reconciler restarts
// them on its next tick.
func cmdRigRestart(args []string, stdout, stderr io.Writer) int {
	ctx, err := resolveContext()
	if err != nil {
		fmt.Fprintf(stderr, "gc rig restart: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	rigName := ctx.RigName
	if len(args) > 0 {
		rigName = args[0]
	}
	if rigName == "" {
		fmt.Fprintln(stderr, "gc rig restart: missing rig name") //nolint:errcheck // best-effort stderr
		return 1
	}
	cityPath := ctx.CityPath
	cfg, err := loadCityConfig(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc rig restart: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Verify rig exists.
	found := false
	for _, r := range cfg.Rigs {
		if r.Name == rigName {
			found = true
			break
		}
	}
	if !found {
		fmt.Fprintln(stderr, rigNotFoundMsg("gc rig restart", rigName, cfg)) //nolint:errcheck // best-effort stderr
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
	rec := openCityRecorder(stderr)
	store, _ := openCityStoreAt(cityPath)
	return doRigRestart(sp, rec, store, cfg, rigAgents, rigName, cityName, cfg.Workspace.SessionTemplate, stdout, stderr)
}

// doRigRestart kills sessions for all agents in a rig. The reconciler will
// restart them. Returns 0 even if no agents were running.
func doRigRestart(
	sp runtime.Provider,
	rec events.Recorder,
	store beads.Store,
	cfg *config.City,
	agents []config.Agent,
	rigName, cityName, sessionTemplate string,
	stdout, stderr io.Writer,
) int {
	var targets []stopTarget
	for _, a := range agents {
		sp0 := scaleParamsFor(&a)
		if !a.SupportsInstanceExpansion() {
			// Non-expanding template.
			sn := lookupSessionNameOrLegacy(store, cityName, a.QualifiedName(), sessionTemplate)
			if sp.IsRunning(sn) {
				targets = append(targets, stopTarget{
					name:     sn,
					template: a.QualifiedName(),
					subject:  a.QualifiedName(),
					order:    len(targets),
					resolved: true,
				})
			}
		} else {
			// Pool agent: resolve live instances from beads first, then legacy discovery.
			for _, ref := range resolvePoolSessionRefs(store, a.Name, a.Dir, sp0, &a, cityName, sessionTemplate, sp, stderr) {
				if !sp.IsRunning(ref.sessionName) {
					continue
				}
				targets = append(targets, stopTarget{
					name:     ref.sessionName,
					template: a.QualifiedName(),
					subject:  ref.qualifiedInstance,
					order:    len(targets),
					resolved: true,
				})
			}
		}
	}
	dependencyCfg := cfg
	if dependencyCfg == nil {
		dependencyCfg = &config.City{Agents: agents}
	}
	killed := stopTargetsBounded(targets, dependencyCfg, sp, rec, eventActor(), io.Discard, stderr)

	fmt.Fprintf(stdout, "Restarted %d agent(s) in rig '%s' (killed sessions; reconciler will restart)\n", killed, rigName) //nolint:errcheck // best-effort stdout
	return 0
}
