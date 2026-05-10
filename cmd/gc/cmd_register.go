package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/supervisor"
	"github.com/spf13/cobra"
)

func newRegisterCmd(stdout, stderr io.Writer) *cobra.Command {
	var nameFlag string
	cmd := &cobra.Command{
		Use:   "register [path]",
		Short: "Register a city with the machine-wide supervisor",
		Long: `Register a city directory with the machine-wide supervisor.

If no path is given, registers the current city (discovered from cwd).
Use --name to set the machine-local registration alias. The alias is stored
in the machine-local supervisor registry and never written back to city.toml.
When --name is omitted, the current effective city identity is used
(site-bound workspace name if present, otherwise legacy workspace.name,
otherwise the directory basename) — in every case city.toml is not modified.
Registration is idempotent — registering the same city twice is a no-op.
The supervisor is started if needed and immediately reconciles the city.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if doRegisterWithOptions(args, nameFlag, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&nameFlag, "name", "", "machine-local alias for this city registration")
	return cmd
}

func doRegister(args []string, stdout, stderr io.Writer) int {
	return doRegisterWithOptions(args, "", stdout, stderr)
}

func doRegisterWithOptions(args []string, nameOverride string, stdout, stderr io.Writer) int {
	var cityPath string
	var err error
	if len(args) > 0 {
		cityPath, err = validateCityPath(args[0])
	} else {
		cityPath, err = resolveCommandCity(nil)
	}
	if err != nil {
		fmt.Fprintf(stderr, "gc register: %v\n", err) //nolint:errcheck
		return 1
	}

	// Verify it's a city directory (city.toml is the defining marker).
	if _, sErr := os.Stat(filepath.Join(cityPath, "city.toml")); sErr != nil {
		fmt.Fprintf(stderr, "gc register: %s is not a city directory (no city.toml found)\n", cityPath) //nolint:errcheck
		return 1
	}
	registerName, err := resolveRegistrationName(cityPath, nameOverride)
	if err != nil {
		fmt.Fprintf(stderr, "gc register: %v\n", err) //nolint:errcheck
		return 1
	}
	return registerCityWithSupervisorNamed(cityPath, registerName, stdout, stderr, "gc register", true)
}

// resolveRegistrationName returns the machine-local alias to store in the
// supervisor registry. The alias is never written back to city.toml — the
// registry is the sole source of truth for registration identity
// (gastownhall/gascity#602).
func resolveRegistrationName(cityPath, nameOverride string) (string, error) {
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		return "", fmt.Errorf("loading city.toml: %w", err)
	}
	if alias := strings.TrimSpace(nameOverride); alias != "" {
		return alias, nil
	}
	return config.EffectiveCityName(cfg, filepath.Base(filepath.Clean(cityPath))), nil
}

func newUnregisterCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "unregister [path]",
		Short: "Remove a city from the machine-wide supervisor",
		Long: `Remove a city from the machine-wide supervisor registry.

If no path is given, unregisters the current city (discovered from cwd).
If the supervisor is running, it immediately stops managing the city.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if doUnregister(args, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	return cmd
}

func doUnregister(args []string, stdout, stderr io.Writer) int {
	var cityPath string
	var err error
	if len(args) > 0 {
		cityPath, err = filepath.Abs(args[0])
		if err == nil {
			cityPath = normalizePathForCompare(cityPath)
		}
	} else {
		cityPath, err = resolveCommandCity(nil)
	}
	if err != nil {
		fmt.Fprintf(stderr, "gc unregister: %v\n", err) //nolint:errcheck
		return 1
	}
	_, code := unregisterCityFromSupervisor(cityPath, stdout, stderr)
	return code
}

func newCitiesCmd(stdout, stderr io.Writer) *cobra.Command {
	runList := func(_ *cobra.Command, _ []string) error {
		if doCities(stdout, stderr) != 0 {
			return errExit
		}
		return nil
	}
	cmd := &cobra.Command{
		Use:   "cities",
		Short: "List registered cities",
		Long:  `List all cities registered with the machine-wide supervisor.`,
		Args:  cobra.NoArgs,
		RunE:  runList,
	}
	cmd.AddCommand(&cobra.Command{
		Use:     "list",
		Short:   "List registered cities",
		Aliases: []string{"ls"},
		Args:    cobra.NoArgs,
		RunE:    runList,
	})
	return cmd
}

func doCities(stdout, stderr io.Writer) int {
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	entries, err := reg.List()
	if err != nil {
		fmt.Fprintf(stderr, "gc cities: %v\n", err) //nolint:errcheck
		return 1
	}

	if len(entries) == 0 {
		fmt.Fprintln(stdout, "No cities registered. Use 'gc register' to add a city.") //nolint:errcheck
		return 0
	}

	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tPATH") //nolint:errcheck
	for _, e := range entries {
		fmt.Fprintf(tw, "%s\t%s\n", e.EffectiveName(), e.Path) //nolint:errcheck
	}
	tw.Flush() //nolint:errcheck
	return 0
}
