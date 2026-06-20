package main

import (
	"bytes"
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
	var jsonOut bool
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
			if doRegisterWithOptionsJSON(args, nameFlag, jsonOut, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&nameFlag, "name", "", "machine-local alias for this city registration")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSONL summary")
	cmd.Flags().BoolVar(&assumeYesForSupervisorCycle, "yes", false, "bypass the cross-city supervisor cycle confirmation prompt (warning is still printed for the audit trail)")
	return cmd
}

func doRegister(args []string, stdout, stderr io.Writer) int {
	return doRegisterWithOptions(args, "", stdout, stderr)
}

func doRegisterWithOptions(args []string, nameOverride string, stdout, stderr io.Writer) int {
	return doRegisterWithOptionsJSON(args, nameOverride, false, stdout, stderr)
}

func doRegisterWithOptionsJSON(args []string, nameOverride string, jsonOut bool, stdout, stderr io.Writer) int {
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
	registerStdout := stdout
	var registerProgress bytes.Buffer
	if jsonOut {
		registerStdout = &registerProgress
	}
	code := registerCityWithSupervisorNamed(cityPath, registerName, registerStdout, stderr, "gc register", true)
	if code != 0 {
		replayJSONModeProgress(stderr, &registerProgress)
		return code
	}
	if !jsonOut {
		return code
	}
	return writeLifecycleActionJSONOrExit(stdout, stderr, "gc register", lifecycleActionJSON{
		Command:  "register",
		Action:   "register",
		Message:  "City registered.",
		CityName: registerName,
		CityPath: cityPath,
	})
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
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "unregister [path]",
		Short: "Remove a city from the machine-wide supervisor",
		Long: `Remove a city from the machine-wide supervisor registry.

If no path is given, unregisters the current city (discovered from cwd).
If the supervisor is running, it immediately stops managing the city.
Takes a city directory path, not the name shown by 'gc cities'. Unlike
'gc register' (which is idempotent), this errors when the resolved path is not
a registered city, so it is not a silent no-op on an unknown target.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if doUnregisterJSON(args, jsonOut, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSONL summary")
	return cmd
}

func doUnregister(args []string, stdout, stderr io.Writer) int {
	return doUnregisterJSON(args, false, stdout, stderr)
}

func doUnregisterJSON(args []string, jsonOut bool, stdout, stderr io.Writer) int {
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
	entry, registered, lookupErr := registeredCityEntry(cityPath)
	if lookupErr != nil {
		fmt.Fprintf(stderr, "gc unregister: %v\n", lookupErr) //nolint:errcheck // best-effort stderr
		return 1
	}
	rawArg := ""
	if len(args) > 0 {
		rawArg = args[0]
	}
	if !registered {
		// gc unregister takes a city directory path, not a name. The common
		// footgun is passing the NAME shown by `gc cities`, or a wrong path:
		// the target resolves to a path that was never registered. Fail loudly
		// rather than exit 0 silently (non-JSON) or fabricate a success record
		// (JSON), which would leave the city registered and mislead the caller.
		writeUnregisterNotRegistered(stderr, rawArg, cityPath)
		return 1
	}
	unregisterStdout := stdout
	var unregisterProgress bytes.Buffer
	if jsonOut {
		unregisterStdout = &unregisterProgress
	}
	handled, code := unregisterCityFromSupervisor(cityPath, unregisterStdout, stderr)
	if code != 0 {
		replayJSONModeProgress(stderr, &unregisterProgress)
		return code
	}
	if !handled {
		// The registration disappeared between the pre-check above and the
		// helper's own registry read — a concurrent `gc stop` / `gc unregister`
		// from another process. The helper reports (handled=false, code=0) for
		// that not-registered state and writes nothing to stdout, so treat it as
		// the loud not-registered failure instead of emitting a fabricated
		// success record. The helper's handled bool, not the earlier pre-check,
		// is the authority on whether anything was actually unregistered.
		writeUnregisterNotRegistered(stderr, rawArg, cityPath)
		return 1
	}
	if !jsonOut {
		return code
	}
	return writeLifecycleActionJSONOrExit(stdout, stderr, "gc unregister", lifecycleActionJSON{
		Command:  "unregister",
		Action:   "unregister",
		Message:  "City unregistered.",
		CityName: entry.EffectiveName(),
		CityPath: cityPath,
	})
}

// writeUnregisterNotRegistered emits an actionable diagnostic when the
// unregister target does not resolve to a registered city. rawArg is the
// original CLI argument (empty when the city was discovered from cwd) and
// cityPath is the resolved absolute path that failed to match.
func writeUnregisterNotRegistered(stderr io.Writer, rawArg, cityPath string) {
	fmt.Fprintf(stderr, "gc unregister: no registered city at %s\n", cityPath) //nolint:errcheck // best-effort stderr
	// Only the bare-NAME footgun warrants the name-vs-path hint. A path-shaped
	// argument (relative like ./city, a trailing slash, or a symlink) is a
	// genuine path that simply was not registered; comparing the raw token to
	// the fully normalized cityPath would misfire and contradict the diagnostic,
	// so gate on "no path separator" instead of raw-string inequality.
	if trimmed := strings.TrimSpace(rawArg); trimmed != "" && !strings.ContainsRune(trimmed, filepath.Separator) {
		fmt.Fprintf(stderr, "gc unregister: %q looks like a name — gc unregister takes a city directory path, not a name\n", trimmed) //nolint:errcheck // best-effort stderr
	}
	fmt.Fprintf(stderr, "gc unregister: run 'gc cities' to see registered cities, then 'gc unregister <path>'\n") //nolint:errcheck // best-effort stderr
}

func replayJSONModeProgress(stderr io.Writer, progress *bytes.Buffer) {
	if progress == nil || progress.Len() == 0 {
		return
	}
	_, _ = io.Copy(stderr, progress)
}

func newCitiesCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	runList := func(_ *cobra.Command, _ []string) error {
		if doCities(jsonOutput, stdout, stderr) != 0 {
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
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output one JSONL result record")
	listCmd := &cobra.Command{
		Use:     "list",
		Short:   "List registered cities",
		Aliases: []string{"ls"},
		Args:    cobra.NoArgs,
		RunE:    runList,
	}
	listCmd.Flags().BoolVar(&jsonOutput, "json", false, "Output one JSONL result record")
	cmd.AddCommand(listCmd)
	return cmd
}

type citiesListJSON struct {
	SchemaVersion string             `json:"schema_version"`
	RegistryPath  string             `json:"registry_path"`
	Cities        []cityRegistryJSON `json:"cities"`
}

type cityRegistryJSON struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

func doCities(jsonOutput bool, stdout, stderr io.Writer) int {
	registryPath := supervisor.RegistryPath()
	reg := supervisor.NewRegistry(registryPath)
	entries, err := reg.List()
	if err != nil {
		fmt.Fprintf(stderr, "gc cities: %v\n", err) //nolint:errcheck
		return 1
	}

	if jsonOutput {
		cities := make([]cityRegistryJSON, 0, len(entries))
		for _, e := range entries {
			cities = append(cities, cityRegistryJSON{
				Name: e.EffectiveName(),
				Path: e.Path,
			})
		}
		if err := writeCLIJSONLine(stdout, citiesListJSON{
			SchemaVersion: "1",
			RegistryPath:  registryPath,
			Cities:        cities,
		}); err != nil {
			fmt.Fprintf(stderr, "gc cities: writing JSON: %v\n", err) //nolint:errcheck
			return 1
		}
		return 0
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
