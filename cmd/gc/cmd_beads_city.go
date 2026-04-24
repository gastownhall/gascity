package main

import (
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/spf13/cobra"
)

type cityEndpointOptions struct {
	External        bool
	Host            string
	Port            string
	User            string
	AdoptUnverified bool
	DryRun          bool
}

type cityRigEndpointPlan struct {
	Rig     config.Rig
	Current contract.ConfigState
	Target  contract.ConfigState
	Update  bool
}

var verifyCityExternalEndpoint = verifyExternalDoltEndpoint

func newBeadsCityCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "city",
		Short: "Manage canonical city endpoint topology",
		Long: `Manage the canonical city endpoint topology for bd-backed beads stores.

Use use-managed to make the city GC-managed again. Use use-external to pin the
city to an external Dolt endpoint and rewrite inherited rig mirrors.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				fmt.Fprintln(stderr, "gc beads city: missing subcommand (use-managed, use-external)") //nolint:errcheck
			} else {
				fmt.Fprintf(stderr, "gc beads city: unknown subcommand %q\n", args[0]) //nolint:errcheck
			}
			return errExit
		},
	}
	cmd.AddCommand(
		newBeadsCityUseManagedCmd(stdout, stderr),
		newBeadsCityUseExternalCmd(stdout, stderr),
	)
	return cmd
}

func newBeadsCityUseManagedCmd(stdout, stderr io.Writer) *cobra.Command {
	var opts cityEndpointOptions
	cmd := &cobra.Command{
		Use:   "use-managed",
		Short: "Set the city endpoint to GC-managed",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdBeadsCityUseManaged(opts, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&opts.DryRun, "dry-run", false, "show the canonical changes without writing files")
	return cmd
}

func newBeadsCityUseExternalCmd(stdout, stderr io.Writer) *cobra.Command {
	var opts cityEndpointOptions
	opts.External = true
	cmd := &cobra.Command{
		Use:   "use-external",
		Short: "Set the city endpoint to an external Dolt server",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdBeadsCityUseExternal(opts, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.Host, "host", "", "external Dolt host")
	cmd.Flags().StringVar(&opts.Port, "port", "", "external Dolt port")
	cmd.Flags().StringVar(&opts.User, "user", "", "external Dolt user")
	cmd.Flags().BoolVar(&opts.AdoptUnverified, "adopt-unverified", false, "record the endpoint without live validation")
	cmd.Flags().BoolVar(&opts.DryRun, "dry-run", false, "show the canonical changes without writing files")
	return cmd
}

func cmdBeadsCityUseManaged(opts cityEndpointOptions, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc beads city use-managed: %v\n", err) //nolint:errcheck
		return 1
	}
	return doBeadsCityEndpoint(fsys.OSFS{}, cityPath, opts, stdout, stderr)
}

func cmdBeadsCityUseExternal(opts cityEndpointOptions, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc beads city use-external: %v\n", err) //nolint:errcheck
		return 1
	}
	return doBeadsCityEndpoint(fsys.OSFS{}, cityPath, opts, stdout, stderr)
}

//nolint:unparam // FS seam is intentional for command tests
func doBeadsCityEndpoint(fs fsys.FS, cityPath string, opts cityEndpointOptions, stdout, stderr io.Writer) int {
	name := cityEndpointCommandName(opts)
	if err := validateCityEndpointOptions(opts); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", name, err) //nolint:errcheck
		return 1
	}
	if !cityUsesBdStoreContract(cityPath) {
		fmt.Fprintf(stderr, "%s: only supported for bd-backed beads providers\n", name) //nolint:errcheck
		return 1
	}

	rawCfg, err := loadCityConfigForEditFS(fs, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		fmt.Fprintf(stderr, "%s: loading config: %v\n", name, err) //nolint:errcheck
		return 1
	}
	cfg, err := loadCityConfigFS(fs, filepath.Join(cityPath, "city.toml"), stderr)
	if err != nil {
		fmt.Fprintf(stderr, "%s: loading expanded config: %v\n", name, err) //nolint:errcheck
		return 1
	}
	tomlCfg := *rawCfg
	tomlCfg.Rigs = append([]config.Rig(nil), rawCfg.Rigs...)
	resolveRigPaths(cityPath, cfg.Rigs)

	currentState, err := resolveOwnerCityConfigState(cityPath, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", name, err) //nolint:errcheck
		return 1
	}
	targetState := requestedCityEndpointState(cfg, currentState, opts)
	plans, err := planCityRigEndpointUpdates(cityPath, cfg.Rigs, currentState, targetState)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", name, err) //nolint:errcheck
		return 1
	}

	if opts.DryRun {
		printCityEndpointDryRun(stdout, currentState, targetState, plans)
		return 0
	}

	if opts.External && !opts.AdoptUnverified {
		if err := validateCityExternalEndpointChange(cityPath, targetState, plans); err != nil {
			fmt.Fprintf(stderr, "%s: validate external endpoint: %v\n", name, err)                                      //nolint:errcheck
			fmt.Fprintf(stderr, "%s: rerun with --adopt-unverified to record this endpoint without validation\n", name) //nolint:errcheck
			return 1
		}
		targetState.EndpointStatus = contract.EndpointStatusVerified
		for i := range plans {
			if plans[i].Update {
				plans[i].Target.EndpointStatus = contract.EndpointStatusVerified
			}
		}
	}

	managedStopScript := ""
	var managedStopEnv []string
	if currentState.EndpointOrigin == contract.EndpointOriginManagedCity && targetState.EndpointOrigin == contract.EndpointOriginCityCanonical {

		provider := beadsProvider(cityPath)
		if strings.HasPrefix(provider, "exec:") && providerUsesBdStoreContract(provider) {
			managedStopScript = strings.TrimPrefix(provider, "exec:")
			configuredProvider := configuredBeadsProviderValue(cityPath)
			if (configuredProvider == "" || configuredProvider == "bd") && execProviderBase(provider) == "gc-beads-bd" {
				if err := MaterializeBuiltinPacks(cityPath); err != nil {
					fmt.Fprintf(stderr, "%s: materialize managed provider: %v\n", name, err) //nolint:errcheck
					return 1
				}
				managedStopScript = gcBeadsBdScriptPath(cityPath)
			}
			managedStopEnv = append([]string(nil), providerLifecycleProcessEnv(cityPath, provider)...)
		}
	}

	snapshots, err := snapshotCityTopologyFiles(fs, cityPath, plans)
	if err != nil {
		fmt.Fprintf(stderr, "%s: snapshot canonical files: %v\n", name, err) //nolint:errcheck
		return 1
	}
	if err := ensureCanonicalScopeMetadataIfPresent(fs, cityPath); err != nil {
		writeCityEndpointRollbackError(fs, stderr, snapshots, name, "canonicalizing metadata", err)
		return 1
	}
	if err := ensureCanonicalScopeConfig(fs, cityPath, targetState); err != nil {
		writeCityEndpointRollbackError(fs, stderr, snapshots, name, "writing canonical config", err)
		return 1
	}
	for _, plan := range plans {
		if !plan.Update {
			continue
		}
		if err := ensureCanonicalScopeMetadataIfPresent(fs, plan.Rig.Path); err != nil {
			writeCityEndpointRollbackError(fs, stderr, snapshots, name, "canonicalizing inherited rig metadata", err)
			return 1
		}
		if err := ensureCanonicalScopeConfig(fs, plan.Rig.Path, plan.Target); err != nil {
			writeCityEndpointRollbackError(fs, stderr, snapshots, name, "writing inherited rig config", err)
			return 1
		}
	}
	if err := syncCityEndpointCompatConfig(fs, cityPath, filepath.Join(cityPath, "city.toml"), &tomlCfg, targetState, plans); err != nil {
		writeCityEndpointRollbackError(fs, stderr, snapshots, name, "writing legacy city.toml endpoint config", err)
		return 1
	}
	if err := syncCityManagedPortArtifacts(fs, cityPath, targetState, plans); err != nil {
		writeCityEndpointRollbackError(fs, stderr, snapshots, name, "syncing managed port artifacts", err)
		return 1
	}

	if managedStopScript != "" {
		if err := runProviderOpWithEnv(managedStopScript, managedStopEnv, "stop"); err != nil {
			writeCityEndpointRollbackError(fs, stderr, snapshots, name, "stopping managed local provider", err)
			return 1
		}
		if err := clearManagedDoltRuntimeStateIfOwned(cityPath); err != nil {
			writeCityEndpointRollbackError(fs, stderr, snapshots, name, "clearing managed runtime state", err)
			return 1
		}
	}

	printCityEndpointResult(stdout, targetState, plans)
	return 0
}

func cityEndpointCommandName(opts cityEndpointOptions) string {
	if opts.External {
		return "gc beads city use-external"
	}
	return "gc beads city use-managed"
}

func validateExplicitExternalHost(host string) error {
	host = strings.TrimSpace(host)
	switch strings.Trim(host, "[]") {
	case "0.0.0.0", "::":
		return fmt.Errorf("invalid --host %q: use a concrete host, not a bind address", host)
	default:
		return nil
	}
}

func validateCityEndpointOptions(opts cityEndpointOptions) error {
	if !opts.External {
		if strings.TrimSpace(opts.Host) != "" || strings.TrimSpace(opts.Port) != "" || strings.TrimSpace(opts.User) != "" {
			return fmt.Errorf("use-managed does not accept --host, --port, or --user")
		}
		if opts.AdoptUnverified {
			return fmt.Errorf("--adopt-unverified is only valid with use-external")
		}
		return nil
	}
	host := strings.TrimSpace(opts.Host)
	if host == "" {
		return fmt.Errorf("use-external requires --host")
	}
	if err := validateExplicitExternalHost(host); err != nil {
		return err
	}
	port := strings.TrimSpace(opts.Port)
	if port == "" {
		return fmt.Errorf("use-external requires --port")
	}
	value, err := strconv.Atoi(port)
	if err != nil || value <= 0 {
		return fmt.Errorf("invalid --port %q", port)
	}
	return nil
}

func requestedCityEndpointState(cfg *config.City, currentState contract.ConfigState, opts cityEndpointOptions) contract.ConfigState {
	prefix := config.EffectiveHQPrefix(cfg)
	if !opts.External {
		return contract.ConfigState{
			IssuePrefix:    prefix,
			EndpointOrigin: contract.EndpointOriginManagedCity,
			EndpointStatus: contract.EndpointStatusVerified,
		}
	}
	user := strings.TrimSpace(opts.User)
	if user == "" && currentState.EndpointOrigin == contract.EndpointOriginCityCanonical {
		user = strings.TrimSpace(currentState.DoltUser)
	}
	state := contract.ConfigState{
		IssuePrefix:    prefix,
		EndpointOrigin: contract.EndpointOriginCityCanonical,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       strings.TrimSpace(opts.Host),
		DoltPort:       strings.TrimSpace(opts.Port),
		DoltUser:       user,
	}
	if opts.AdoptUnverified {
		state.EndpointStatus = contract.EndpointStatusUnverified
	}
	return state
}

func planCityRigEndpointUpdates(cityPath string, rigs []config.Rig, currentCityState, targetCityState contract.ConfigState) ([]cityRigEndpointPlan, error) {
	plans := make([]cityRigEndpointPlan, 0, len(rigs))
	for i := range rigs {
		current, err := resolveOwnerRigConfigState(cityPath, rigs[i], currentCityState)
		if err != nil {
			return nil, err
		}
		plan := cityRigEndpointPlan{Rig: rigs[i], Current: current, Target: current}
		if current.EndpointOrigin == contract.EndpointOriginExplicit {
			plans = append(plans, plan)
			continue
		}

		plan.Current = inheritedRigDoltConfigState(rigs[i].Path, rigs[i].EffectivePrefix(), currentCityState)
		plan.Target = inheritedRigDoltConfigState(rigs[i].Path, rigs[i].EffectivePrefix(), targetCityState)
		plan.Update = true
		plans = append(plans, plan)
	}
	return plans, nil
}

func validateCityExternalEndpointChange(cityPath string, targetState contract.ConfigState, plans []cityRigEndpointPlan) error {
	if err := verifyCityExternalEndpoint(targetState, cityPath, cityPath); err != nil {
		return fmt.Errorf("city scope: %w", err)
	}
	for _, plan := range plans {
		if !plan.Update {
			continue
		}
		if err := verifyCityExternalEndpoint(plan.Target, plan.Rig.Path, cityPath); err != nil {
			return fmt.Errorf("rig %s: %w", plan.Rig.Name, err)
		}
	}
	return nil
}

func snapshotCityTopologyFiles(fs fsys.FS, cityPath string, plans []cityRigEndpointPlan) ([]fileSnapshot, error) {
	snapshots := make([]fileSnapshot, 0, len(plans)+3)
	cityToml, err := snapshotOptionalFile(fs, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		return nil, err
	}
	snapshots = append(snapshots, cityToml)
	siteToml, err := snapshotOptionalFile(fs, config.SiteBindingPath(cityPath))
	if err != nil {
		return nil, err
	}
	snapshots = append(snapshots, siteToml)
	citySnapshots, err := snapshotRigCanonicalFiles(fs, cityPath)
	if err != nil {
		return nil, err
	}
	snapshots = append(snapshots, citySnapshots...)
	for _, plan := range plans {
		if !plan.Update {
			continue
		}
		rigSnapshots, err := snapshotRigCanonicalFiles(fs, plan.Rig.Path)
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, rigSnapshots...)
	}
	portSnapshots, err := snapshotCityManagedPortFiles(fs, cityPath, plans)
	if err != nil {
		return nil, err
	}
	snapshots = append(snapshots, portSnapshots...)
	return snapshots, nil
}

func snapshotCityManagedPortFiles(fs fsys.FS, cityPath string, plans []cityRigEndpointPlan) ([]fileSnapshot, error) {
	seen := map[string]struct{}{}
	paths := []string{filepath.Join(cityPath, ".beads", "dolt-server.port")}
	for _, plan := range plans {
		paths = append(paths, filepath.Join(plan.Rig.Path, ".beads", "dolt-server.port"))
	}
	snapshots := make([]fileSnapshot, 0, len(paths))
	for _, path := range paths {
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		snap, err := snapshotOptionalFile(fs, path)
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, snap)
	}
	return snapshots, nil
}

func syncCityEndpointCompatConfig(fs fsys.FS, cityPath, tomlPath string, cfg *config.City, targetState contract.ConfigState, plans []cityRigEndpointPlan) error {
	changed := false
	if targetState.EndpointOrigin == contract.EndpointOriginCityCanonical {
		host := strings.TrimSpace(targetState.DoltHost)
		port, err := strconv.Atoi(strings.TrimSpace(targetState.DoltPort))
		if err != nil {
			return fmt.Errorf("invalid canonical city endpoint port %q: %w", targetState.DoltPort, err)
		}
		if cfg.Dolt.Host != host {
			cfg.Dolt.Host = host
			changed = true
		}
		if cfg.Dolt.Port != port {
			cfg.Dolt.Port = port
			changed = true
		}
	} else if cfg.Dolt.Host != "" || cfg.Dolt.Port != 0 {
		cfg.Dolt = config.DoltConfig{}
		changed = true
	}

	for i := range cfg.Rigs {
		rigPath := cfg.Rigs[i].Path
		if !filepath.IsAbs(rigPath) {
			rigPath = filepath.Join(cityPath, rigPath)
		}
		for _, plan := range plans {
			if !plan.Update || !samePath(rigPath, plan.Rig.Path) {
				continue
			}
			host := strings.TrimSpace(plan.Target.DoltHost)
			port := strings.TrimSpace(plan.Target.DoltPort)
			if cfg.Rigs[i].DoltHost != host {
				cfg.Rigs[i].DoltHost = host
				changed = true
			}
			if cfg.Rigs[i].DoltPort != port {
				cfg.Rigs[i].DoltPort = port
				changed = true
			}
			break
		}
	}
	if !changed {
		return nil
	}
	return writeCityConfigForEditFS(fs, tomlPath, cfg)
}

func syncCityManagedPortArtifacts(fs fsys.FS, cityPath string, cityState contract.ConfigState, plans []cityRigEndpointPlan) error {
	managedPort := ""
	if cityState.EndpointOrigin == contract.EndpointOriginManagedCity {
		port, err := readManagedRuntimePublishedPort(cityPath)
		if err == nil {
			managedPort = port
		}
	}
	if managedPort != "" {
		if err := writeDoltPortFileStrict(fs, cityPath, managedPort); err != nil {
			return err
		}
	} else if err := removeDoltPortFileStrict(cityPath); err != nil {
		return err
	}
	for _, plan := range plans {
		if managedPort != "" && plan.Update && plan.Target.EndpointOrigin == contract.EndpointOriginInheritedCity {
			if err := writeDoltPortFileStrict(fs, plan.Rig.Path, managedPort); err != nil {
				return err
			}
			continue
		}
		if err := removeDoltPortFileStrict(plan.Rig.Path); err != nil {
			return err
		}
	}
	return nil
}

func printCityEndpointDryRun(stdout io.Writer, current, target contract.ConfigState, plans []cityRigEndpointPlan) {
	fmt.Fprintln(stdout, "WOULD UPDATE: city endpoint")                                                            //nolint:errcheck
	fmt.Fprintf(stdout, "  city: %s -> %s\n", describeRigEndpointState(current), describeRigEndpointState(target)) //nolint:errcheck
	fmt.Fprintf(stdout, "  file: %s\n", filepath.Join(".beads", "config.yaml"))                                    //nolint:errcheck
	for _, plan := range plans {
		if !plan.Update {
			continue
		}
		fmt.Fprintf(stdout, "  rig %s: %s -> %s\n", plan.Rig.Name, describeRigEndpointState(plan.Current), describeRigEndpointState(plan.Target)) //nolint:errcheck
	}
}

func printCityEndpointResult(stdout io.Writer, state contract.ConfigState, plans []cityRigEndpointPlan) {
	fmt.Fprintln(stdout, "UPDATED: city endpoint")                        //nolint:errcheck
	fmt.Fprintf(stdout, "  state: %s\n", describeRigEndpointState(state)) //nolint:errcheck
	updated := 0
	for _, plan := range plans {
		if plan.Update {
			updated++
		}
	}
	fmt.Fprintf(stdout, "  inherited rigs updated: %d\n", updated) //nolint:errcheck
	next := cityEndpointFollowupCommand(state)
	if next == "" {
		fmt.Fprintln(stdout, "  next: none") //nolint:errcheck
	} else {
		fmt.Fprintf(stdout, "  next: %s\n", next) //nolint:errcheck
	}
}

func cityEndpointFollowupCommand(state contract.ConfigState) string {
	if state.EndpointOrigin != contract.EndpointOriginCityCanonical || state.EndpointStatus != contract.EndpointStatusUnverified {
		return ""
	}
	parts := []string{"gc beads city use-external", "--host", state.DoltHost, "--port", state.DoltPort}
	if user := strings.TrimSpace(state.DoltUser); user != "" {
		parts = append(parts, "--user", user)
	}
	return strings.Join(parts, " ")
}

func writeCityEndpointRollbackError(fs fsys.FS, stderr io.Writer, snapshots []fileSnapshot, name, action string, cause error) {
	if restoreErr := restoreSnapshots(fs, snapshots); restoreErr != nil {
		fmt.Fprintf(stderr, "%s: %s: %v (rollback failed: %v)\n", name, action, cause, restoreErr) //nolint:errcheck
		return
	}
	fmt.Fprintf(stderr, "%s: %s: %v\n", name, action, cause) //nolint:errcheck
}
