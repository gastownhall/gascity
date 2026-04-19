package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	Host            string
	Port            string
	User            string
	AdoptUnverified bool
	DryRun          bool
}

var verifyRigExternalEndpoint = verifyExternalDoltEndpoint

func newRigSetEndpointCmd(stdout, stderr io.Writer) *cobra.Command {
	var opts rigEndpointOptions
	cmd := &cobra.Command{
		Use:   "set-endpoint <rig>",
		Short: "Set the canonical endpoint ownership for a rig",
		Long: `Set the canonical endpoint ownership for a rig.

Use --inherit to make a rig derive its endpoint from the current city
topology. Use --external to pin the rig to its own external Dolt endpoint.

This command owns the rig's canonical .beads/config.yaml topology state.`,
		Example: `  gc rig set-endpoint frontend --inherit
  gc rig set-endpoint frontend --external --host db.example.com --port 3307
  gc rig set-endpoint frontend --external --host db.example.com --port 3307 --user agent --adopt-unverified
  gc rig set-endpoint frontend --inherit --dry-run`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if cmdRigSetEndpoint(args[0], opts, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&opts.Inherit, "inherit", false, "inherit the city endpoint")
	cmd.Flags().BoolVar(&opts.External, "external", false, "set an explicit external endpoint for the rig")
	cmd.Flags().StringVar(&opts.Host, "host", "", "external Dolt host")
	cmd.Flags().StringVar(&opts.Port, "port", "", "external Dolt port")
	cmd.Flags().StringVar(&opts.User, "user", "", "external Dolt user")
	cmd.Flags().BoolVar(&opts.AdoptUnverified, "adopt-unverified", false, "record the endpoint without live validation")
	cmd.Flags().BoolVar(&opts.DryRun, "dry-run", false, "show the canonical changes without writing files")
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

	if opts.External && !opts.AdoptUnverified {
		if err := verifyRigExternalEndpoint(targetState, rig.Path, rig.Path); err != nil {
			fmt.Fprintf(stderr, "gc rig set-endpoint: validate external endpoint: %v\n", err)                                      //nolint:errcheck // best-effort stderr
			fmt.Fprintf(stderr, "gc rig set-endpoint: rerun with --adopt-unverified to record this endpoint without validation\n") //nolint:errcheck // best-effort stderr
			return 1
		}
		targetState.EndpointStatus = contract.EndpointStatusVerified
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
	if opts.Inherit == opts.External {
		return fmt.Errorf("choose exactly one of --inherit or --external")
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
		DoltDatabase: strings.TrimSpace(doltDatabase),
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
	data, err := os.ReadFile(managedDoltStatePath(cityPath))
	if err != nil {
		return "", err
	}
	var state doltRuntimeState
	if err := json.Unmarshal(data, &state); err != nil {
		return "", err
	}
	if !state.Running || state.Port == 0 {
		return "", fmt.Errorf("dolt runtime state unavailable")
	}
	if state.PID > 0 && !pidAlive(state.PID) {
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
	return fsys.WriteFileAtomic(fs, portFile, []byte(strings.TrimSpace(port)+"\n"), 0o644)
}

func removeDoltPortFileStrict(dir string) error {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	if err := os.Remove(filepath.Join(dir, ".beads", "dolt-server.port")); err != nil && !os.IsNotExist(err) {
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
		return fmt.Errorf("external endpoint identity unverifiable: local metadata.json is missing project_id; rerun with --adopt-unverified or repair the canonical metadata first")
	}
	if !ok {
		return fmt.Errorf("external endpoint identity unverifiable: database %q is missing metadata _project_id; rerun with --adopt-unverified", strings.TrimSpace(database))
	}
	if localProjectID != databaseProjectID {
		return fmt.Errorf("PROJECT IDENTITY MISMATCH — refusing to connect: local metadata.json project_id %q does not match database _project_id %q", localProjectID, databaseProjectID)
	}
	return nil
}

func readCanonicalProjectID(metadataPath string) (string, error) {
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		return "", err
	}
	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil {
		return "", nil
	}
	raw, ok := meta["project_id"]
	if !ok || raw == nil {
		return "", nil
	}
	switch value := raw.(type) {
	case string:
		return strings.TrimSpace(value), nil
	default:
		projectID := strings.TrimSpace(fmt.Sprint(value))
		if projectID == "" || projectID == "<nil>" || strings.EqualFold(projectID, "null") {
			return "", nil
		}
		return projectID, nil
	}
}

func readDatabaseProjectID(ctx context.Context, db *sql.DB) (string, bool, error) {
	var projectID string
	if err := db.QueryRowContext(ctx, "SELECT value FROM metadata WHERE `key` = '_project_id'").Scan(&projectID); err != nil {
		if err == sql.ErrNoRows {
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
		snap, err := snapshotOptionalFile(fs, path)
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
	paths := []string{
		filepath.Join(cityPath, "city.toml"),
		config.SiteBindingPath(cityPath),
		filepath.Join(scopeRoot, ".beads", "metadata.json"),
		filepath.Join(scopeRoot, ".beads", "config.yaml"),
	}
	snapshots := make([]fileSnapshot, 0, len(paths))
	for _, path := range paths {
		snap, err := snapshotOptionalFile(fs, path)
		if err != nil {
			return nil, err
		}
		snapshots = append(snapshots, snap)
	}
	return snapshots, nil
}

func snapshotOptionalFile(fs fsys.FS, path string) (fileSnapshot, error) {
	data, err := fs.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fileSnapshot{path: path}, nil
		}
		return fileSnapshot{}, err
	}
	cp := append([]byte(nil), data...)
	return fileSnapshot{path: path, data: cp, exists: true}, nil
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
