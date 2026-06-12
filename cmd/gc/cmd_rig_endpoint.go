package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doltauth"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/go-sql-driver/mysql"
	"github.com/spf13/cobra"
)

type rigEndpointOptions struct {
	Inherit         bool
	External        bool
	Self            bool
	Force           bool
	Host            string
	Port            string
	User            string
	AdoptUnverified bool
	DryRun          bool
}

var verifyRigExternalEndpoint = verifyExternalDoltEndpoint

func newRigSetEndpointCmd(stdout, stderr io.Writer) *cobra.Command {
	var opts rigEndpointOptions
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "set-endpoint <rig>",
		Short: "Set the canonical endpoint ownership for a rig",
		Long: `Set the canonical endpoint ownership for a rig.

Use --inherit to make a rig derive its endpoint from the current city
topology. Use --external to pin the rig to its own external Dolt endpoint.
Use --self to mark the rig as running its own local Dolt server on
127.0.0.1 at the given --port; while the city is in managed_city mode the
command requires --force because the rig's .beads/dolt-server.port mirror
will no longer track the managed city Dolt.

This command owns the rig's canonical .beads/config.yaml topology state.`,
		Example: `  gc rig set-endpoint frontend --inherit
  gc rig set-endpoint frontend --external --host db.example.com --port 3307
  gc rig set-endpoint frontend --external --host db.example.com --port 3307 --user agent --adopt-unverified
  gc rig set-endpoint frontend --self --port 28232 --force
  gc rig set-endpoint frontend --inherit --dry-run`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if jsonOutput {
				if cmdRigSetEndpoint(args[0], opts, io.Discard, stderr) != 0 {
					return errExit
				}
				return writeManagementActionJSON(stdout, managementActionResult{
					Command:  commandName("rig", "set-endpoint"),
					Action:   "set-endpoint",
					Name:     args[0],
					Rig:      args[0],
					DryRun:   managementBoolPtr(opts.DryRun),
					Endpoint: rigEndpointJSONFromOptions(opts),
				})
			}
			if cmdRigSetEndpoint(args[0], opts, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
		ValidArgsFunction: completeRigNames,
	}
	cmd.Flags().BoolVar(&opts.Inherit, "inherit", false, "inherit the city endpoint")
	cmd.Flags().BoolVar(&opts.External, "external", false, "set an explicit external endpoint for the rig")
	cmd.Flags().BoolVar(&opts.Self, "self", false, "mark the rig as running its own local Dolt on 127.0.0.1")
	cmd.Flags().BoolVar(&opts.Force, "force", false, "acknowledge conflicting managed-city state when using --self")
	cmd.Flags().StringVar(&opts.Host, "host", "", "external Dolt host")
	cmd.Flags().StringVar(&opts.Port, "port", "", "external Dolt port (required with --external or --self)")
	cmd.Flags().StringVar(&opts.User, "user", "", "external Dolt user")
	cmd.Flags().BoolVar(&opts.AdoptUnverified, "adopt-unverified", false, "record the endpoint without live validation")
	cmd.Flags().BoolVar(&opts.DryRun, "dry-run", false, "show the canonical changes without writing files")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output in JSONL format")
	return cmd
}

func cmdRigSetEndpoint(rigName string, opts rigEndpointOptions, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc rig set-endpoint: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return doRigSetEndpoint(fsys.OSFS{}, cityPath, rigName, opts, stdout, stderr)
}

//nolint:unparam // FS seam is intentional for command tests
func doRigSetEndpoint(fs fsys.FS, cityPath, rigName string, opts rigEndpointOptions, stdout, stderr io.Writer) int {
	if err := validateRigEndpointOptions(opts); err != nil {
		fmt.Fprintf(stderr, "gc rig set-endpoint: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	tomlPath := filepath.Join(cityPath, "city.toml")
	cfg, err := loadCityConfigForEditFS(fs, tomlPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc rig set-endpoint: loading config: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	persistCfg := *cfg
	persistCfg.Rigs = append([]config.Rig(nil), cfg.Rigs...)
	resolveRigPaths(cityPath, cfg.Rigs)

	rig, ok := rigByName(cfg, rigName)
	if !ok {
		fmt.Fprintln(stderr, rigNotFoundMsg("gc rig set-endpoint", rigName, cfg)) //nolint:errcheck // best-effort stderr
		return 1
	}
	if strings.TrimSpace(rig.Path) == "" {
		// Unbound rig: the downstream helpers join paths against rig.Path
		// (snapshotRigEndpointFiles, ensureCanonicalScopeMetadataIfPresent,
		// syncRigManagedPortArtifact, etc.). Empty rig.Path would produce
		// relative `.beads/...` writes under the current working directory
		// instead of erroring cleanly.
		fmt.Fprintf(stderr, "gc rig set-endpoint: rig %q is declared but has no path binding — run `gc rig add <dir> --name %s` to bind it before setting its endpoint\n", rig.Name, rig.Name) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !scopeUsesManagedBdStoreContract(cityPath, rig.Path) {
		fmt.Fprintln(stderr, "gc rig set-endpoint: only supported for bd-backed beads providers") //nolint:errcheck // best-effort stderr
		return 1
	}

	cityState, err := resolveOwnerCityConfigState(cityPath, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "gc rig set-endpoint: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	currentState, err := resolveOwnerRigConfigState(cityPath, rig, cityState)
	if err != nil {
		fmt.Fprintf(stderr, "gc rig set-endpoint: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	targetState := requestedRigEndpointState(rig, currentState, cityState, opts)

	if opts.Self && cityState.EndpointOrigin == contract.EndpointOriginManagedCity && !opts.Force {
		fmt.Fprintf(stderr, "gc rig set-endpoint: --self conflicts with managed_city: the rig's .beads/dolt-server.port mirror will stop tracking the managed city Dolt and any rig-local Dolt must be started and managed independently of `gc start`. Re-run with --force to acknowledge.\n") //nolint:errcheck // best-effort stderr
		return 1
	}

	if opts.DryRun {
		printRigEndpointDryRun(stdout, rig, currentState, targetState)
		return 0
	}

	if opts.Inherit && cityState.EndpointOrigin == contract.EndpointOriginManagedCity {
		if _, err := readManagedRuntimePublishedPort(cityPath); err != nil {
			fmt.Fprintf(stderr, "gc rig set-endpoint: managed city endpoint unavailable: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
	}

	if (opts.External || opts.Self) && !opts.AdoptUnverified {
		if err := verifyRigExternalEndpoint(targetState, rig.Path, rig.Path); err != nil {
			fmt.Fprintf(stderr, "gc rig set-endpoint: validate endpoint: %v\n", err)                                               //nolint:errcheck // best-effort stderr
			fmt.Fprintf(stderr, "gc rig set-endpoint: rerun with --adopt-unverified to record this endpoint without validation\n") //nolint:errcheck // best-effort stderr
			return 1
		}
		targetState.EndpointStatus = contract.EndpointStatusVerified
	}

	if opts.Self && cityState.EndpointOrigin == contract.EndpointOriginManagedCity {
		fmt.Fprintf(stderr, "gc rig set-endpoint: WARN: rig %q now runs its own Dolt on 127.0.0.1:%s, independent of the city's managed Dolt; `gc start` will not supervise it.\n", rig.Name, targetState.DoltPort) //nolint:errcheck // best-effort stderr
	}

	snapshots, err := snapshotRigEndpointFiles(fs, cityPath, rig.Path)
	if err != nil {
		fmt.Fprintf(stderr, "gc rig set-endpoint: snapshot canonical files: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := ensureCanonicalScopeMetadataIfPresent(fs, rig.Path); err != nil {
		writeRigEndpointRollbackError(fs, stderr, snapshots, "canonicalizing metadata", err)
		return 1
	}
	if err := ensureCanonicalScopeConfig(fs, rig.Path, targetState); err != nil {
		writeRigEndpointRollbackError(fs, stderr, snapshots, "writing canonical config", err)
		return 1
	}
	if err := syncRigEndpointCompatConfig(fs, cityPath, &persistCfg, rigName, targetState); err != nil {
		writeRigEndpointRollbackError(fs, stderr, snapshots, "syncing compat city config", err)
		return 1
	}
	if err := syncRigManagedPortArtifact(cityPath, rig.Path, cityState, targetState); err != nil {
		writeRigEndpointRollbackError(fs, stderr, snapshots, "syncing managed port artifact", err)
		return 1
	}

	printRigEndpointResult(stdout, rig, targetState)
	return 0
}

func validateRigEndpointOptions(opts rigEndpointOptions) error {
	modes := 0
	if opts.Inherit {
		modes++
	}
	if opts.External {
		modes++
	}
	if opts.Self {
		modes++
	}
	if modes != 1 {
		return fmt.Errorf("choose exactly one of --inherit, --external, or --self")
	}
	if opts.Force && !opts.Self {
		return fmt.Errorf("--force is only valid with --self")
	}
	if opts.Inherit {
		if strings.TrimSpace(opts.Host) != "" || strings.TrimSpace(opts.Port) != "" || strings.TrimSpace(opts.User) != "" {
			return fmt.Errorf("--inherit does not accept --host, --port, or --user")
		}
		if opts.AdoptUnverified {
			return fmt.Errorf("--adopt-unverified is only valid with --external")
		}
		return nil
	}

	if opts.Self {
		if strings.TrimSpace(opts.Host) != "" {
			return fmt.Errorf("--self always uses 127.0.0.1; do not pass --host")
		}
		if strings.TrimSpace(opts.User) != "" {
			return fmt.Errorf("--self does not accept --user")
		}
		port := strings.TrimSpace(opts.Port)
		if port == "" {
			return fmt.Errorf("--self requires --port")
		}
		value, err := strconv.Atoi(port)
		if err != nil || value <= 0 {
			return fmt.Errorf("invalid --port %q", port)
		}
		return nil
	}

	host := strings.TrimSpace(opts.Host)
	port := strings.TrimSpace(opts.Port)
	if host == "" {
		return fmt.Errorf("--external requires --host")
	}
	if err := validateExplicitExternalHost(host); err != nil {
		return err
	}
	if port == "" {
		return fmt.Errorf("--external requires --port")
	}
	value, err := strconv.Atoi(port)
	if err != nil || value <= 0 {
		return fmt.Errorf("invalid --port %q", port)
	}
	return nil
}

func rigByName(cfg *config.City, rigName string) (config.Rig, bool) {
	for i := range cfg.Rigs {
		if strings.EqualFold(cfg.Rigs[i].Name, rigName) {
			return cfg.Rigs[i], true
		}
	}
	return config.Rig{}, false
}

func resolveOwnerCityConfigState(cityPath string, cfg *config.City) (contract.ConfigState, error) {
	state, _, err := resolveDesiredCityEndpointState(cityPath, cfg.Dolt, config.EffectiveHQPrefix(cfg))
	if err != nil {
		return contract.ConfigState{}, err
	}
	return state, nil
}

func resolveOwnerRigConfigState(cityPath string, rig config.Rig, cityState contract.ConfigState) (contract.ConfigState, error) {
	state, err := resolveDesiredRigEndpointState(cityPath, rig, cityState)
	if err != nil {
		return contract.ConfigState{}, err
	}
	return state, nil
}

func requestedRigEndpointState(rig config.Rig, currentState, cityState contract.ConfigState, opts rigEndpointOptions) contract.ConfigState {
	if opts.Inherit {
		return inheritedRigDoltConfigState(rig.Path, rig.EffectivePrefix(), cityState)
	}

	if opts.Self {
		state := contract.ConfigState{
			IssuePrefix:    rig.EffectivePrefix(),
			EndpointOrigin: contract.EndpointOriginExplicit,
			EndpointStatus: contract.EndpointStatusVerified,
			DoltHost:       "127.0.0.1",
			DoltPort:       strings.TrimSpace(opts.Port),
		}
		if opts.AdoptUnverified {
			state.EndpointStatus = contract.EndpointStatusUnverified
		}
		return state
	}

	user := strings.TrimSpace(opts.User)
	if user == "" && currentState.EndpointOrigin == contract.EndpointOriginExplicit {
		user = strings.TrimSpace(currentState.DoltUser)
	}

	state := contract.ConfigState{
		IssuePrefix:    rig.EffectivePrefix(),
		EndpointOrigin: contract.EndpointOriginExplicit,
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

func ensureCanonicalScopeConfig(fs fsys.FS, scopeRoot string, state contract.ConfigState) error {
	beadsDir := filepath.Join(scopeRoot, ".beads")
	if err := ensureBeadsDir(fs, beadsDir); err != nil {
		return err
	}
	_, err := contract.EnsureCanonicalConfig(fs, filepath.Join(beadsDir, "config.yaml"), state)
	return err
}

func requireCanonicalScopeMetadata(fs fsys.FS, scopeRoot string) error {
	path := filepath.Join(scopeRoot, ".beads", "metadata.json")
	if _, err := fs.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("missing canonical metadata %s", path)
		}
		return err
	}
	doltDatabase, ok, err := contract.ReadDoltDatabase(fs, path)
	if err != nil {
		return err
	}
	if !ok || strings.TrimSpace(doltDatabase) == "" {
		return fmt.Errorf("missing pinned dolt_database in %s", path)
	}
	return nil
}

func ensureCanonicalScopeMetadataIfPresent(fs fsys.FS, scopeRoot string) error {
	path := filepath.Join(scopeRoot, ".beads", "metadata.json")
	doltDatabase, err := func() (string, error) {
		if err := requireCanonicalScopeMetadata(fs, scopeRoot); err != nil {
			return "", err
		}
		doltDatabase, _, err := contract.ReadDoltDatabase(fs, path)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(doltDatabase), nil
	}()
	if err != nil {
		return err
	}
	_, err = contract.EnsureCanonicalMetadata(fs, path, contract.MetadataState{
		Database:     "dolt",
		Backend:      "dolt",
		DoltMode:     "server",
		DoltDatabase: doltDatabase,
	})
	return err
}

func syncRigManagedPortArtifact(cityPath, rigPath string, cityState, rigState contract.ConfigState) error {
	if cityState.EndpointOrigin == contract.EndpointOriginManagedCity && rigState.EndpointOrigin == contract.EndpointOriginInheritedCity {
		port, err := readManagedRuntimePublishedPort(cityPath)
		if err != nil {
			return err
		}
		return writeDoltPortFileStrict(fsys.OSFS{}, rigPath, port)
	}
	return removeDoltPortFileStrict(rigPath)
}

func readManagedRuntimePublishedPort(cityPath string) (string, error) {
	if cityUsesBdStoreContract(cityPath) {
		owned, err := managedDoltLifecycleOwned(cityPath)
		if err != nil {
			return "", fmt.Errorf("determine managed dolt ownership for published port: %w", err)
		}
		if !owned {
			return "", fmt.Errorf("managed dolt lifecycle is not owned by this city")
		}
	}
	data, err := os.ReadFile(managedDoltStatePath(cityPath))
	if err != nil {
		return "", err
	}
	var state doltRuntimeState
	if err := json.Unmarshal(data, &state); err != nil {
		return "", err
	}
	if !state.Running || state.Port <= 0 {
		return "", fmt.Errorf("dolt runtime state unavailable")
	}
	if state.PID > 0 || strings.TrimSpace(state.DataDir) != "" {
		if !validDoltRuntimeState(state, cityPath) {
			return "", fmt.Errorf("dolt runtime state unavailable")
		}
	}
	if state.PID < 0 {
		return "", fmt.Errorf("dolt runtime state unavailable")
	}
	return strconv.Itoa(state.Port), nil
}

func writeDoltPortFileStrict(fs fsys.FS, dir, port string) error {
	if strings.TrimSpace(dir) == "" || strings.TrimSpace(port) == "" {
		return fmt.Errorf("missing rig path or port")
	}
	portFile := filepath.Join(dir, ".beads", "dolt-server.port")
	if data, err := os.ReadFile(portFile); err == nil && strings.TrimSpace(string(data)) == strings.TrimSpace(port) {
		return nil
	}
	if err := ensureBeadsDir(fs, filepath.Dir(portFile)); err != nil {
		return err
	}
	writePath, err := resolveDoltPortFileWritePath(fs, portFile)
	if err != nil {
		return err
	}
	if err := ensureBeadsDir(fs, filepath.Dir(writePath)); err != nil {
		return err
	}
	return fsys.WriteFileAtomic(fs, writePath, []byte(strings.TrimSpace(port)+"\n"), 0o644)
}

func resolveDoltPortFileWritePath(fs fsys.FS, portFile string) (string, error) {
	writePath, err := fsys.ResolveSymlinks(fs, portFile)
	if err != nil {
		return "", fmt.Errorf("resolving managed dolt port file %q for rewrite: %w", portFile, err)
	}
	return writePath, nil
}

func removeDoltPortFileStrict(dir string) error {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	return removeResolvedDoltPortFile(fsys.OSFS{}, dir)
}

// removeResolvedDoltPortFile clears the managed dolt port mirror under dir,
// resolving an operator symlink to its target first so the link entry is
// preserved and only the resolved target is removed. This mirrors the
// symlink-preserving write path (resolveDoltPortFileWritePath); removing the
// unresolved link instead would delete the operator's symlink and make the
// next port publication recreate a regular file at the link path (the
// ga-lurp5d clobber class). Missing files, including dangling links, are not
// an error.
func removeResolvedDoltPortFile(fs fsys.FS, dir string) error {
	portFile := filepath.Join(dir, ".beads", "dolt-server.port")
	target, err := fsys.ResolveSymlinks(fs, portFile)
	if err != nil {
		return fmt.Errorf("resolving managed dolt port file %q for cleanup: %w", portFile, err)
	}
	if err := fs.Remove(target); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func printRigEndpointDryRun(stdout io.Writer, rig config.Rig, current, target contract.ConfigState) {
	fmt.Fprintln(stdout, "WOULD UPDATE: rig endpoint")                                    //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "  rig: %s\n", rig.Name)                                          //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "  from: %s\n", describeRigEndpointState(current))                //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "  to:   %s\n", describeRigEndpointState(target))                 //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "  file: %s\n", filepath.Join(rig.Path, ".beads", "config.yaml")) //nolint:errcheck // best-effort stdout
}

func printRigEndpointResult(stdout io.Writer, rig config.Rig, state contract.ConfigState) {
	fmt.Fprintln(stdout, "UPDATED: rig endpoint")                         //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "  rig: %s\n", rig.Name)                          //nolint:errcheck // best-effort stdout
	fmt.Fprintf(stdout, "  state: %s\n", describeRigEndpointState(state)) //nolint:errcheck // best-effort stdout
	next := rigEndpointFollowupCommand(rig, state)
	if next == "" {
		fmt.Fprintln(stdout, "  next: none") //nolint:errcheck // best-effort stdout
	} else {
		fmt.Fprintf(stdout, "  next: %s\n", next) //nolint:errcheck // best-effort stdout
	}
}

func rigEndpointFollowupCommand(rig config.Rig, state contract.ConfigState) string {
	if state.EndpointOrigin != contract.EndpointOriginExplicit || state.EndpointStatus != contract.EndpointStatusUnverified {
		return ""
	}
	parts := []string{"gc rig set-endpoint", rig.Name, "--external", "--host", state.DoltHost, "--port", state.DoltPort}
	if user := strings.TrimSpace(state.DoltUser); user != "" {
		parts = append(parts, "--user", user)
	}
	return strings.Join(parts, " ")
}

func describeRigEndpointState(state contract.ConfigState) string {
	parts := []string{string(state.EndpointOrigin)}
	if state.DoltHost != "" || state.DoltPort != "" {
		addr := net.JoinHostPort(defaultHost(state.DoltHost, state.DoltPort), strings.TrimSpace(state.DoltPort))
		parts = append(parts, addr)
	}
	if user := strings.TrimSpace(state.DoltUser); user != "" {
		parts = append(parts, "user="+user)
	}
	if status := strings.TrimSpace(string(state.EndpointStatus)); status != "" {
		parts = append(parts, "status="+status)
	}
	return strings.Join(parts, " ")
}

func defaultHost(host, port string) string {
	host = strings.TrimSpace(host)
	if host == "" && strings.TrimSpace(port) != "" {
		return "127.0.0.1"
	}
	return host
}

func canonicalValidationPassword(host, port, authScopeRoot string) string {
	// Persisted verified status is based on canonical store-local auth only.
	// Transient GC_DOLT_* overrides remain process-local escape hatches and
	// must not redefine what GC records as the canonical verified state.
	if pass := doltauth.ReadStoreLocalPassword(authScopeRoot); pass != "" {
		return pass
	}
	portValue, err := strconv.Atoi(strings.TrimSpace(port))
	if err != nil || portValue <= 0 {
		return ""
	}
	path := strings.TrimSpace(os.Getenv("BEADS_CREDENTIALS_FILE"))
	if path == "" {
		path = doltauth.DefaultCredentialsPath()
	}
	if path == "" {
		return ""
	}
	return doltauth.ReadCredentialsPassword(path, host, portValue)
}

func verifyExternalDoltEndpoint(state contract.ConfigState, databaseScopeRoot, authScopeRoot string) error {
	host := defaultHost(state.DoltHost, state.DoltPort)
	port := strings.TrimSpace(state.DoltPort)
	if host == "" || port == "" {
		return fmt.Errorf("missing external endpoint")
	}

	databasePath := filepath.Join(databaseScopeRoot, ".beads", "metadata.json")
	database, ok, err := contract.ReadDoltDatabase(fsys.OSFS{}, databasePath)
	if err != nil {
		return err
	}
	if !ok || strings.TrimSpace(database) == "" {
		return fmt.Errorf("missing pinned dolt_database in %s", databasePath)
	}
	localProjectID, err := readCanonicalProjectID(databasePath)
	if err != nil {
		return err
	}

	user := strings.TrimSpace(state.DoltUser)
	if user == "" {
		user = "root"
	}
	password := canonicalValidationPassword(host, port, authScopeRoot)

	cfg := mysql.NewConfig()
	cfg.User = user
	cfg.Passwd = password
	cfg.Net = "tcp"
	cfg.Addr = net.JoinHostPort(host, port)
	cfg.DBName = strings.TrimSpace(database)
	cfg.Timeout = 5 * time.Second
	cfg.ReadTimeout = 5 * time.Second
	cfg.WriteTimeout = 5 * time.Second
	cfg.AllowNativePasswords = true

	db, err := sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck // best-effort cleanup

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return err
	}
	var branch string
	if err := db.QueryRowContext(ctx, "SELECT active_branch()").Scan(&branch); err != nil {
		return fmt.Errorf("database %q is not a Dolt database", strings.TrimSpace(database))
	}

	var issuesTable string
	if err := db.QueryRowContext(ctx, "SHOW TABLES LIKE 'issues'").Scan(&issuesTable); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("beads store not usable on external endpoint: database %q is missing the issues table", strings.TrimSpace(database))
		}
		return fmt.Errorf("beads store not usable on external endpoint: %w", err)
	}

	databaseProjectID, ok, err := readDatabaseProjectID(ctx, db)
	if err != nil {
		return fmt.Errorf("beads store not usable on external endpoint: %w", err)
	}
	if localProjectID == "" {
		return fmt.Errorf("external endpoint identity unverifiable: neither %s nor .beads/metadata.json carry a project_id; rerun with --adopt-unverified or seed the canonical identity first", projectIdentityDisplayPath)
	}
	if !ok {
		return fmt.Errorf("external endpoint identity unverifiable: database %q is missing metadata _project_id; rerun with --adopt-unverified", strings.TrimSpace(database))
	}
	if localProjectID != databaseProjectID {
		return fmt.Errorf(
			"PROJECT IDENTITY MISMATCH — refusing to connect:\n"+
				"  canonical local project_id    = %q   (from "+projectIdentityDisplayPath+" or metadata.json)\n"+
				"  database metadata._project_id  = %q\n"+
				"\n"+
				"Inspect both values and resolve manually before reconnecting.",
			localProjectID, databaseProjectID,
		)
	}
	return nil
}

func readCanonicalProjectID(metadataPath string) (string, error) {
	scopeRoot, err := scopeRootFromMetadataPath(metadataPath)
	if err != nil {
		return "", err
	}
	if projectID, ok, err := contract.ReadProjectIdentity(fsys.OSFS{}, scopeRoot); err != nil {
		return "", err
	} else if ok {
		return projectID, nil
	}
	return readManagedMetadataProjectID(metadataPath)
}

func readDatabaseProjectID(ctx context.Context, db *sql.DB) (string, bool, error) {
	var projectID string
	if err := db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE `key` = '_project_id'").Scan(&projectID); err != nil {
		if err == sql.ErrNoRows || isMissingDoltMetadataTableError(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("read database _project_id: %w", err)
	}
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return "", false, nil
	}
	return projectID, true, nil
}

func isMissingDoltMetadataTableError(err error) bool {
	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) && mysqlErr.Number == 1146 {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "table not found: metadata") ||
		strings.Contains(msg, "table 'metadata' doesn't exist") ||
		strings.Contains(msg, "no such table: metadata")
}

type fileSnapshot struct {
	path   string
	data   []byte
	exists bool
}

func snapshotRigCanonicalFiles(fs fsys.FS, scopeRoot string) ([]fileSnapshot, error) {
	paths := []string{
		filepath.Join(scopeRoot, ".beads", "metadata.json"),
		filepath.Join(scopeRoot, ".beads", "config.yaml"),
	}
	snapshots := make([]fileSnapshot, 0, len(paths))
	for _, path := range paths {
		snap, err := snapshotResolvedFile(fs, path)
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, snap)
	}
	return snapshots, nil
}

func syncRigEndpointCompatConfig(fs fsys.FS, cityPath string, cfg *config.City, rigName string, state contract.ConfigState) error {
	for i := range cfg.Rigs {
		if !strings.EqualFold(cfg.Rigs[i].Name, rigName) {
			continue
		}
		cfg.Rigs[i].DoltHost = strings.TrimSpace(state.DoltHost)
		cfg.Rigs[i].DoltPort = strings.TrimSpace(state.DoltPort)
		return writeCityConfigForEditFS(fs, filepath.Join(cityPath, "city.toml"), cfg)
	}
	return fmt.Errorf("rig %q not found in city config", rigName)
}

func snapshotRigEndpointFiles(fs fsys.FS, cityPath, scopeRoot string) ([]fileSnapshot, error) {
	cityToml, err := snapshotResolvedFile(fs, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		return nil, err
	}
	paths := []string{
		config.SiteBindingPath(cityPath),
		filepath.Join(scopeRoot, ".beads", "metadata.json"),
		filepath.Join(scopeRoot, ".beads", "config.yaml"),
	}
	snapshots := make([]fileSnapshot, 0, len(paths)+1)
	snapshots = append(snapshots, cityToml)
	for _, path := range paths {
		snap, err := snapshotResolvedFile(fs, path)
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, snap)
	}
	return snapshots, nil
}

// snapshotResolvedFile snapshots path for rollback through any symlink
// chain: restoring at the link path would replace the link with a regular
// file (the ga-lurp5d failure mode), so the snapshot records the resolved
// target and the restore writes there instead. Resolve-only by design — a
// rollback writes the original bytes back, so the key-loss rewrite guard
// does not apply. A path blocked by a regular-file intermediate cannot
// exist; it snapshots as missing, matching snapshotOptionalFile.
func snapshotResolvedFile(fs fsys.FS, path string) (fileSnapshot, error) {
	resolved, err := fsys.ResolveSymlinks(fs, path)
	if err != nil {
		if errors.Is(err, syscall.ENOTDIR) {
			return fileSnapshot{path: path}, nil
		}
		return fileSnapshot{}, err
	}
	return snapshotOptionalFile(fs, resolved)
}

func snapshotOptionalFile(fs fsys.FS, path string) (fileSnapshot, error) {
	data, err := fs.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR) {
			return fileSnapshot{path: path}, nil
		}
		return fileSnapshot{}, err
	}
	cp := append([]byte(nil), data...)
	return fileSnapshot{path: path, data: cp, exists: true}, nil
}

// cityTomlRollbackPath returns the symlink-resolved city.toml path that a
// rollback snapshot must read and later restore. Resolving first means an
// atomic restore rewrites the real target file and leaves a live city.toml
// symlink intact, instead of replacing the link with a regular file (the
// failure ResolveCityRewritePath/ResolveCityAppendPath exist to prevent). When
// city.toml is a plain file (or not yet created), resolution is a no-op and the
// path is unchanged. The controller config-mutation snapshot
// (captureConfigMutationSnapshot) routes through this so it stays symlink-aware,
// matching the CLI rollback snapshots that resolve via snapshotResolvedFile.
func cityTomlRollbackPath(fs fsys.FS, cityPath string) (string, error) {
	return fsys.ResolveSymlinks(fs, filepath.Join(cityPath, "city.toml"))
}

func writeRigEndpointRollbackError(fs fsys.FS, stderr io.Writer, snapshots []fileSnapshot, action string, cause error) {
	if restoreErr := restoreSnapshots(fs, snapshots); restoreErr != nil {
		fmt.Fprintf(stderr, "gc rig set-endpoint: %s: %v (rollback failed: %v)\n", action, cause, restoreErr) //nolint:errcheck // best-effort stderr
		return
	}
	fmt.Fprintf(stderr, "gc rig set-endpoint: %s: %v\n", action, cause) //nolint:errcheck // best-effort stderr
}

func restoreSnapshots(fs fsys.FS, snapshots []fileSnapshot) error {
	var failures []string
	for _, snap := range snapshots {
		if err := restoreSnapshot(fs, snap); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", snap.path, err))
		}
	}
	if len(failures) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(failures, "; "))
}

func restoreSnapshot(fs fsys.FS, snap fileSnapshot) error {
	if !snap.exists {
		if err := fs.Remove(snap.path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return fsys.WriteFileAtomic(fs, snap.path, snap.data, 0o644)
}
