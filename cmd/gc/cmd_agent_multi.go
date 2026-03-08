package main

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/spf13/cobra"
)

// newAgentStartCmd creates the "gc agent start <template> [--name <name>]" command.
func newAgentStartCmd(stdout, stderr io.Writer) *cobra.Command {
	var nameFlag string
	cmd := &cobra.Command{
		Use:   "start <template>",
		Short: "Start a multi-instance agent",
		Long: `Start a named instance of a multi-instance agent template.

If --name is not provided, a sequential name is auto-generated.
Starting a stopped instance resumes it (no new bead is created).`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdAgentStart(args[0], nameFlag, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&nameFlag, "name", "", "instance name (auto-generated if omitted)")
	return cmd
}

func cmdAgentStart(templateInput, nameFlag string, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc agent start: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cfg, err := loadCityConfig(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc agent start: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	found, ok := resolveAgentIdentity(cfg, templateInput, currentRigContext(cfg))
	if !ok {
		fmt.Fprintln(stderr, agentNotFoundMsg("gc agent start", templateInput, cfg)) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !found.IsMulti() {
		fmt.Fprintf(stderr, "gc agent start: agent %q is not a multi-instance template (set multi = true)\n", found.QualifiedName()) //nolint:errcheck // best-effort stderr
		return 1
	}

	store, code := openCityStore(stderr, "gc agent start")
	if code != 0 {
		return code
	}
	reg := newMultiRegistry(store)

	templateQN := found.QualifiedName()
	name := nameFlag
	if name == "" {
		name, err = reg.nextName(templateQN)
		if err != nil {
			fmt.Fprintf(stderr, "gc agent start: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}

	mi, resumed, err := reg.start(templateQN, name)
	if err != nil {
		fmt.Fprintf(stderr, "gc agent start: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	_ = mi // bead created; controller picks up on next tick
	if resumed {
		fmt.Fprintf(stdout, "Resumed instance '%s/%s'\n", templateQN, name) //nolint:errcheck // best-effort stdout
	} else {
		fmt.Fprintf(stdout, "Started instance '%s/%s'\n", templateQN, name) //nolint:errcheck // best-effort stdout
	}
	return 0
}

// newAgentStopCmd creates the "gc agent stop <instance>" command.
func newAgentStopCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "stop <template/instance>",
		Short: "Stop a multi-instance agent",
		Long: `Stop a running multi-instance agent.

The session is killed and the instance bead is marked stopped.
Use "gc agent start" to resume it later, or "gc agent destroy" to
permanently remove it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdAgentStop(args[0], stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

func cmdAgentStop(input string, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc agent stop: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cfg, err := loadCityConfig(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc agent stop: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	store, code := openCityStore(stderr, "gc agent stop")
	if code != 0 {
		return code
	}
	reg := newMultiRegistry(store)

	templateQN, instanceName, err := resolveMultiInstance(cfg, reg, input)
	if err != nil {
		fmt.Fprintf(stderr, "gc agent stop: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	cityName := cfg.Workspace.Name
	if cityName == "" {
		cityName = filepath.Base(cityPath)
	}
	instanceQN := templateQN + "/" + instanceName
	sp := newSessionProvider()

	sn := agent.SessionNameFor(cityName, instanceQN, cfg.Workspace.SessionTemplate)
	if sp.IsRunning(sn) {
		if err := sp.Stop(sn); err != nil {
			fmt.Fprintf(stderr, "gc agent stop: killing session: %v\n", err) //nolint:errcheck // best-effort stderr
			// Don't update registry on stop failure — session may still be alive.
			return 1
		}
		// Run registry cleanup after successful session stop.
		_ = reg.stop(templateQN, instanceName)
		rec := openCityRecorder(stderr)
		rec.Record(events.Event{
			Type:    events.AgentStopped,
			Actor:   eventActor(),
			Subject: instanceQN,
		})
	} else {
		// Session not running — still update registry.
		if err := reg.stop(templateQN, instanceName); err != nil {
			fmt.Fprintf(stderr, "gc agent stop: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}
	fmt.Fprintf(stdout, "Stopped instance '%s/%s'\n", templateQN, instanceName) //nolint:errcheck // best-effort stdout
	return 0
}

// newAgentDestroyCmd creates the "gc agent destroy <instance>" command.
func newAgentDestroyCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "destroy <template/instance>",
		Short: "Destroy a stopped multi-instance agent",
		Long: `Permanently remove a stopped multi-instance agent by closing its bead.

The instance must be stopped first. Use "gc agent stop" before destroying.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdAgentDestroy(args[0], stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

func cmdAgentDestroy(input string, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc agent destroy: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cfg, err := loadCityConfig(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc agent destroy: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	store, code := openCityStore(stderr, "gc agent destroy")
	if code != 0 {
		return code
	}
	reg := newMultiRegistry(store)

	templateQN, instanceName, err := resolveMultiInstance(cfg, reg, input)
	if err != nil {
		fmt.Fprintf(stderr, "gc agent destroy: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if err := reg.destroy(templateQN, instanceName); err != nil {
		fmt.Fprintf(stderr, "gc agent destroy: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintf(stdout, "Destroyed instance '%s/%s'\n", templateQN, instanceName) //nolint:errcheck // best-effort stdout
	return 0
}

// resolveMultiInstance resolves a user input to a (template, instance) pair.
// Accepts:
//   - "template/instance" — explicit
//   - "instance" — unambiguous across all multi templates
func resolveMultiInstance(cfg *config.City, reg *multiRegistry, input string) (string, string, error) {
	// Try explicit template/instance format.
	if i := strings.Index(input, "/"); i >= 0 {
		templateQN := input[:i]
		instanceName := input[i+1:]
		// Verify the template exists and is multi.
		found, ok := resolveAgentIdentity(cfg, templateQN, currentRigContext(cfg))
		if !ok || !found.IsMulti() {
			return "", "", fmt.Errorf("agent %q is not a multi-instance template", templateQN)
		}
		mi, err := reg.findInstance(found.QualifiedName(), instanceName)
		if err != nil {
			return "", "", err
		}
		if mi == nil {
			return "", "", fmt.Errorf("instance %q not found for template %q", instanceName, found.QualifiedName())
		}
		return found.QualifiedName(), instanceName, nil
	}

	// Bare name — search all multi templates for a matching instance.
	var matches []struct {
		template string
		name     string
	}
	for _, a := range cfg.Agents {
		if !a.IsMulti() {
			continue
		}
		mi, err := reg.findInstance(a.QualifiedName(), input)
		if err != nil {
			continue
		}
		if mi != nil {
			matches = append(matches, struct {
				template string
				name     string
			}{a.QualifiedName(), input})
		}
	}
	switch len(matches) {
	case 0:
		return "", "", fmt.Errorf("multi instance %q not found", input)
	case 1:
		return matches[0].template, matches[0].name, nil
	default:
		var templates []string
		for _, m := range matches {
			templates = append(templates, m.template)
		}
		return "", "", fmt.Errorf("ambiguous instance %q found in templates: %s (use template/instance format)",
			input, strings.Join(templates, ", "))
	}
}
