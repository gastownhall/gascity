package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/spf13/cobra"
)

// migrate-proxied orchestrates bd's own `migrate from-server-to-proxied-server`
// for a city that was initialized the legacy GC-managed way (one gc-owned
// `dolt sql-server` over `<city>/.beads/dolt`, metadata dolt_mode=server,
// gc.endpoint_origin=managed_city). It is an ordering and residue command, not
// a second migration implementation: bd owns the journal, the mode flip and
// the sidecar, and every refusal here exists because bd cannot see something
// it needs to.
//
// This is the interim rc.2 path. The journaled ownership handoff (beads #6281)
// supersedes it once it lands; see engdocs/runbooks/beads-migrate-proxied.md.

const (
	migrateProxiedStatusMigrated  = "migrated"
	migrateProxiedStatusAlready   = "already-migrated"
	migrateProxiedStatusPlanned   = "would-migrate"
	migrateProxiedStatusFailed    = "failed"
	migrateProxiedIdleTimeoutFlag = "0"
)

type migrateProxiedOptions struct {
	JSON   bool
	DryRun bool
	Rigs   []string
}

// migrateProxiedScope is one unit of work: the city, or one rig.
type migrateProxiedScope struct {
	Label  string // "city" or "rig:<name>"
	Name   string
	Path   string
	Prefix string
	IsCity bool

	// SharedRootRel is the relative dolt_data_dir a rig needs so bd roots it at
	// the city's data dir (Option A in 20-migrate-spike.md §5). Empty for the
	// city and for a rig that already owns a Dolt root.
	SharedRootRel string
}

type migrateProxiedScopeResult struct {
	Scope       string `json:"scope"`
	Path        string `json:"path"`
	Status      string `json:"status"`
	DoltMode    string `json:"dolt_mode,omitempty"`
	DoltDataDir string `json:"dolt_data_dir,omitempty"`
	Detail      string `json:"detail,omitempty"`
	Error       string `json:"error,omitempty"`
}

type migrateProxiedReport struct {
	City   string                      `json:"city"`
	DryRun bool                        `json:"dry_run"`
	Scopes []migrateProxiedScopeResult `json:"scopes"`
	Failed int                         `json:"failed"`
}

// runBdScopeCommand is the seam command tests replace. Production runs the bd
// binary this scope is pinned to, in the scope directory, with a minimal env.
var runBdScopeCommand = func(cityPath, scopeRoot string, args ...string) ([]byte, error) {
	bdPath, err := resolveBdBinaryForScope(cityPath, scopeRoot)
	if err != nil {
		return nil, err
	}
	env := map[string]string{"BEADS_DIR": filepath.Join(scopeRoot, ".beads")}
	if err := pinBdGCEnvironment(env); err != nil {
		return nil, err
	}
	applyExportSuppressionEnv(env)
	// A migration must not inherit the legacy server's coordinates: bd would
	// take them as an external upstream and refuse, or worse, migrate against
	// the wrong endpoint.
	for _, key := range []string{
		"BEADS_DOLT_HOST", "BEADS_DOLT_PORT", "BEADS_DOLT_SOCKET", "BEADS_DOLT_USER",
		"BEADS_DOLT_PASSWORD", "BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_PORT",
		"BEADS_DOLT_SERVER_SOCKET", "BEADS_DOLT_DATA_DIR", "BEADS_PROXIED_SERVER_ROOT_PATH",
	} {
		env[key] = ""
	}
	return beads.ExecCommandRunnerWithEnv(env)(scopeRoot, bdPath, args...)
}

// runDoltInitDataDir is the seam for `dolt init`, which gc runs only to give
// bd's root validator the `.dolt/repo_state.json` gc's multi-database data dir
// never had. Proven non-destructive in 20-migrate-spike.md §3.
var runDoltInitDataDir = func(dataDir string) ([]byte, error) {
	cmd := exec.Command("dolt", "init")
	cmd.Dir = dataDir
	return cmd.CombinedOutput()
}

func newBeadsCityMigrateProxiedCmd(stdout, stderr io.Writer) *cobra.Command {
	var opts migrateProxiedOptions
	cmd := &cobra.Command{
		Use:   "migrate-proxied",
		Short: "Migrate a legacy GC-managed city to bd's proxied-server topology",
		Long: `Migrate a legacy GC-managed city, and the rigs that share its Dolt data
directory, onto bd's proxied-server topology.

The city's gc-managed ` + "`dolt sql-server`" + ` must already be stopped: run gc stop
first. bd cannot see a server gc started (it looks only for its own pid file),
so migrating against a live one commits the mode flip and leaves the scope
unusable until the server dies.

Each scope is migrated with bd's own ` + "`bd migrate from-server-to-proxied-server`" + `,
city first. The command is idempotent — an already-proxied scope reports
"already migrated" — so a partially failed run can simply be rerun.

This is the interim rc.2 path. The journaled ownership handoff supersedes it.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if cmdBeadsCityMigrateProxied(opts, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&opts.JSON, "json", false, "emit the per-scope report as JSON")
	cmd.Flags().BoolVar(&opts.DryRun, "dry-run", false, "report the plan without migrating anything")
	cmd.Flags().StringArrayVar(&opts.Rigs, "rig", nil, "migrate only this rig (repeatable; default is every rig in city.toml)")
	return cmd
}

func cmdBeadsCityMigrateProxied(opts migrateProxiedOptions, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc beads city migrate-proxied: %v\n", err) //nolint:errcheck
		return 1
	}
	return doBeadsCityMigrateProxied(cityPath, opts, stdout, stderr)
}

func doBeadsCityMigrateProxied(cityPath string, opts migrateProxiedOptions, stdout, stderr io.Writer) int {
	const name = "gc beads city migrate-proxied"
	if !cityUsesBdStoreContract(cityPath) {
		fmt.Fprintf(stderr, "%s: only supported for bd-backed beads providers\n", name) //nolint:errcheck
		return 1
	}
	scopes, err := planMigrateProxiedScopes(cityPath, opts.Rigs)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", name, err) //nolint:errcheck
		return 1
	}
	// Entry fence. Re-checked immediately before every bd migrate below: a
	// reconcile tick or a stray `gc bd` can restart the managed server between
	// the two, and bd would not notice.
	if err := requireNoManagedDoltServer(cityPath); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", name, err) //nolint:errcheck
		return 1
	}

	report := migrateProxiedReport{City: cityPath, DryRun: opts.DryRun}
	for i := range scopes {
		report.Scopes = append(report.Scopes, migrateProxiedScopeOutcome(cityPath, scopes[i], opts))
	}
	for _, r := range report.Scopes {
		if r.Status == migrateProxiedStatusFailed {
			report.Failed++
		}
	}

	if opts.JSON {
		// ok:true says the report itself is complete, exactly as `gc doctor
		// --json` does with failing checks. Per-scope outcomes live in
		// scopes[].status and the count in failed; the process exit code
		// carries the overall verdict.
		if code := writeCLIJSONLineOrExit(stdout, stderr, name, report); code != 0 {
			return code
		}
	} else {
		printMigrateProxiedReport(stdout, report)
	}
	if report.Failed > 0 {
		fmt.Fprintf(stderr, "%s: %d scope(s) failed; completed scopes stay migrated, rerun to finish the rest\n", name, report.Failed) //nolint:errcheck
		return 1
	}
	return 0
}

func migrateProxiedScopeOutcome(cityPath string, scope migrateProxiedScope, opts migrateProxiedOptions) migrateProxiedScopeResult {
	result := migrateProxiedScopeResult{Scope: scope.Label, Path: scope.Path, DoltDataDir: scope.SharedRootRel}
	classification, err := classifyMigrateProxiedScope(cityPath, scope)
	if err != nil {
		result.Status = migrateProxiedStatusFailed
		result.Error = err.Error()
		return result
	}
	if classification.AlreadyProxied {
		result.Status = migrateProxiedStatusAlready
		result.DoltMode = "proxied-server"
		result.Detail = "metadata.json already records proxied-server"
		// An already-migrated scope can still carry gc's stale config keys —
		// normalising them is idempotent and is the whole point of step (f).
		if !opts.DryRun {
			if err := normalizeMigratedScopeConfig(cityPath, scope); err != nil {
				result.Status = migrateProxiedStatusFailed
				result.Error = err.Error()
			}
		}
		return result
	}
	if opts.DryRun {
		result.Status = migrateProxiedStatusPlanned
		result.DoltMode = "server"
		result.Detail = migrateProxiedPlanDetail(scope, classification)
		return result
	}
	if err := migrateProxiedScopeNow(cityPath, scope, classification); err != nil {
		result.Status = migrateProxiedStatusFailed
		result.Error = err.Error()
		return result
	}
	result.Status = migrateProxiedStatusMigrated
	result.DoltMode = "proxied-server"
	if err := pingMigratedScope(cityPath, scope); err != nil {
		result.Status = migrateProxiedStatusFailed
		result.Error = fmt.Sprintf("migrated but not ready: %v", err)
		return result
	}
	result.Detail = "bd ping ok"
	return result
}

func migrateProxiedPlanDetail(scope migrateProxiedScope, classification migrateProxiedClassification) string {
	parts := make([]string, 0, 3)
	if classification.NeedsDoltInit {
		parts = append(parts, "dolt init "+filepath.Join(scope.Path, ".beads", "dolt"))
	}
	if scope.SharedRootRel != "" {
		parts = append(parts, "set metadata dolt_data_dir="+scope.SharedRootRel)
	}
	parts = append(parts, "bd migrate from-server-to-proxied-server")
	return strings.Join(parts, "; ")
}

// migrateProxiedScopeNow performs the ordered, non-dry-run work for one scope.
func migrateProxiedScopeNow(cityPath string, scope migrateProxiedScope, classification migrateProxiedClassification) error {
	if classification.NeedsDoltInit {
		dataDir := filepath.Join(scope.Path, ".beads", "dolt")
		if out, err := runDoltInitDataDir(dataDir); err != nil {
			return fmt.Errorf("dolt init %s: %w: %s", dataDir, err, strings.TrimSpace(string(out)))
		}
	}
	if scope.SharedRootRel != "" {
		if err := contract.SetMetadataDoltDataDir(fsys.OSFS{}, scopeMetadataJSONPath(scope.Path), scope.SharedRootRel); err != nil {
			return err
		}
	}
	// Re-fence immediately before handing control to bd. bd's own running-server
	// check consults only its own .beads/dolt-server.pid, which gc never writes.
	if err := requireNoManagedDoltServer(cityPath); err != nil {
		return err
	}
	args := []string{"migrate", "from-server-to-proxied-server", "--idle-timeout", migrateProxiedIdleTimeoutFlag}
	if migrateProxiedSupportsJSON(cityPath, scope.Path) {
		args = append(args, "--json")
	}
	out, err := runBdScopeCommand(cityPath, scope.Path, args...)
	if err != nil {
		return fmt.Errorf("bd migrate from-server-to-proxied-server: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if err := requireScopeMigrationCommitted(scope.Path); err != nil {
		return err
	}
	return normalizeMigratedScopeConfig(cityPath, scope)
}

// requireScopeMigrationCommitted verifies bd's own outcome rather than its exit
// code: a committed migration leaves dolt_mode=proxied-server in metadata.json
// and removes the in-flight journal.
func requireScopeMigrationCommitted(scopeRoot string) error {
	mode, ok, err := contract.ReadDoltMode(fsys.OSFS{}, scopeMetadataJSONPath(scopeRoot))
	if err != nil {
		return err
	}
	if !ok || !strings.EqualFold(strings.TrimSpace(mode), "proxied-server") {
		return fmt.Errorf("bd reported success but %s still records dolt_mode %q", scopeMetadataJSONPath(scopeRoot), strings.TrimSpace(mode))
	}
	journal := filepath.Join(scopeRoot, ".beads", "migrate-dolt-mode.json")
	if _, err := os.Stat(journal); err == nil {
		return fmt.Errorf("bd left its migration journal behind at %s; the migration did not commit", journal)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// normalizeMigratedScopeConfig rewrites .beads/config.yaml through the canonical
// writer. For a proxied scope the resolved state carries no dolt.mode and no
// host/port/user, so the writer drops gc's pre-migration keys. gc journals
// nothing in .gc: R1 classifies a proxied metadata binding as provider-owned on
// its own.
func normalizeMigratedScopeConfig(cityPath string, scope migrateProxiedScope) error {
	state, ok, err := desiredScopeDoltConfigStateForInit(cityPath, scope.Path, scope.Prefix)
	if err != nil {
		return fmt.Errorf("resolve canonical config for %s: %w", scope.Label, err)
	}
	if !ok {
		return fmt.Errorf("resolve canonical config for %s: no canonical endpoint state", scope.Label)
	}
	if !strings.EqualFold(strings.TrimSpace(state.DoltMode), "proxied-server") {
		return fmt.Errorf("refusing to rewrite %s: canonical state resolved dolt mode %q, want proxied-server", scope.Label, state.DoltMode)
	}
	return normalizeScopeDoltConfig(scope.Path, state)
}

func pingMigratedScope(cityPath string, scope migrateProxiedScope) error {
	out, err := runBdScopeCommand(cityPath, scope.Path, "ping", "--json")
	if err != nil {
		return fmt.Errorf("bd ping: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// migrateProxiedSupportsJSON reports whether this bd's migrate verb accepts
// --json. rc.2 does; a caller-supplied older bd may not, and gc reads the
// outcome from metadata.json either way.
func migrateProxiedSupportsJSON(cityPath, scopeRoot string) bool {
	out, err := runBdScopeCommand(cityPath, scopeRoot, "migrate", "from-server-to-proxied-server", "--help")
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "--json")
}

type migrateProxiedClassification struct {
	AlreadyProxied bool
	NeedsDoltInit  bool
}

// classifyMigrateProxiedScope decides whether a scope is a legacy GC-managed
// direct scope this command may migrate. Every refusal is typed and names the
// reason, because the alternative — guessing — is how a rig ends up pointed at
// an empty database.
func classifyMigrateProxiedScope(cityPath string, scope migrateProxiedScope) (migrateProxiedClassification, error) {
	metadataPath := scopeMetadataJSONPath(scope.Path)
	metadata, ok, err := contract.LoadMetadataState(fsys.OSFS{}, metadataPath)
	if err != nil {
		return migrateProxiedClassification{}, fmt.Errorf("read %s: %w", metadataPath, err)
	}
	if !ok {
		return migrateProxiedClassification{}, fmt.Errorf("%s has no %s; there is no initialized beads store to migrate", scope.Label, metadataPath)
	}
	backend := strings.TrimSpace(metadata.Backend)
	if !contract.IsDoltBackend(backend) {
		return migrateProxiedClassification{}, fmt.Errorf("%s uses beads backend %q; only a dolt backend has a server topology to migrate", scope.Label, backend)
	}
	mode := strings.ToLower(strings.TrimSpace(metadata.DoltMode))
	switch mode {
	case "proxied-server":
		return migrateProxiedClassification{AlreadyProxied: true}, nil
	case "embedded":
		return migrateProxiedClassification{}, fmt.Errorf("%s is an embedded Dolt scope; bd has no server-to-proxied migration for it", scope.Label)
	case "server", "":
		// The migratable shape. An empty mode is the pre-dolt_mode legacy
		// direct server (see freshScopeCanonicalDoltMode).
	default:
		return migrateProxiedClassification{}, fmt.Errorf("%s records unsupported dolt_mode %q", scope.Label, metadata.DoltMode)
	}

	if transferred, err := committedBeadsHandoffOwnsScope(scope.Path); err != nil {
		return migrateProxiedClassification{}, err
	} else if transferred {
		return migrateProxiedClassification{}, fmt.Errorf("%s was already handed to the beads provider by the ownership handoff; it is not gc's to migrate", scope.Label)
	}
	if _, journaled, err := providerScopeOwnership(cityPath, scope.Path); err != nil {
		return migrateProxiedClassification{}, err
	} else if journaled {
		return migrateProxiedClassification{}, fmt.Errorf("%s is recorded in .gc/scope-ownership.json as provider-owned; it was not initialized the legacy GC-managed way", scope.Label)
	}

	cfg, configured, err := contract.ReadConfigState(fsys.OSFS{}, filepath.Join(scope.Path, ".beads", "config.yaml"))
	if err != nil {
		return migrateProxiedClassification{}, err
	}
	if configured {
		switch cfg.EndpointOrigin {
		case contract.EndpointOriginManagedCity, contract.EndpointOriginInheritedCity, "":
		default:
			return migrateProxiedClassification{}, fmt.Errorf("%s tracks an external Dolt endpoint (gc.endpoint_origin %q); migrate it with gc beads city use-managed first, or leave it external", scope.Label, cfg.EndpointOrigin)
		}
		if host := strings.TrimSpace(cfg.DoltHost); host != "" {
			return migrateProxiedClassification{}, fmt.Errorf("%s pins dolt.host %q; an external endpoint is not gc's to migrate", scope.Label, host)
		}
	}

	classification := migrateProxiedClassification{}
	if scope.SharedRootRel == "" {
		// This scope migrates against its own data dir, so bd's root validator
		// has to find a real Dolt repo there. gc's multi-database data dir
		// never was one.
		dataDir := filepath.Join(scope.Path, ".beads", "dolt")
		hasRepo, err := doltRootIsInitialized(dataDir)
		if err != nil {
			return migrateProxiedClassification{}, err
		}
		if !hasRepo {
			empty, err := dirIsEmptyOrAbsent(dataDir)
			if err != nil {
				return migrateProxiedClassification{}, err
			}
			if empty && !scope.IsCity {
				// A rig with an empty data dir and no shared-root plan would
				// come up on a brand-new empty database while its real data
				// stayed in the city's dir, and bd would not say a word.
				return migrateProxiedClassification{}, fmt.Errorf("rig %q has an empty %s and no database in the city's data directory; refusing to migrate it onto an empty store", scope.Name, dataDir)
			}
			classification.NeedsDoltInit = true
		}
	}
	return classification, nil
}

// planMigrateProxiedScopes orders the work: the city first, because a rig that
// shares the city's data dir cannot be proxied while the city still is not.
func planMigrateProxiedScopes(cityPath string, selected []string) ([]migrateProxiedScope, error) {
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}
	resolveRigPaths(cityPath, cfg.Rigs)

	want := map[string]bool{}
	for _, raw := range selected {
		if trimmed := strings.TrimSpace(raw); trimmed != "" {
			want[trimmed] = true
		}
	}

	scopes := []migrateProxiedScope{{
		Label:  "city",
		Name:   "city",
		Path:   normalizePathForCompare(cityPath),
		Prefix: config.EffectiveHQPrefix(cfg),
		IsCity: true,
	}}
	matched := map[string]bool{}
	for i := range cfg.Rigs {
		rig := cfg.Rigs[i]
		if strings.TrimSpace(rig.Path) == "" {
			continue
		}
		if len(want) > 0 && !want[rig.Name] {
			continue
		}
		matched[rig.Name] = true
		scope := migrateProxiedScope{
			Label:  "rig:" + rig.Name,
			Name:   rig.Name,
			Path:   normalizePathForCompare(rig.Path),
			Prefix: rig.EffectivePrefix(),
		}
		rel, err := sharedCityRootForRig(cityPath, scope.Path)
		if err != nil {
			return nil, err
		}
		scope.SharedRootRel = rel
		scopes = append(scopes, scope)
	}
	missing := make([]string, 0, len(want))
	for rigName := range want {
		if !matched[rigName] {
			missing = append(missing, rigName)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return nil, fmt.Errorf("no such rig in city.toml: %s", strings.Join(missing, ", "))
	}
	return scopes, nil
}

// sharedCityRootForRig reports the relative dolt_data_dir a rig needs to share
// the city's proxy root, or "" when the rig owns its Dolt root already.
//
// The legacy topology put every rig's database inside the city's data dir and
// served all of them from one gc-owned sql-server. bd resolves a rig's root
// from `<rig>/.beads/dolt` unless metadata names another, so without this key
// the rig migrates against its own empty directory. The value MUST be relative:
// beads' configfile.Config.Save silently strips an absolute dolt_data_dir, and
// bd saves the config mid-migration.
func sharedCityRootForRig(cityPath, rigPath string) (string, error) {
	cityDataDir := filepath.Join(normalizePathForCompare(cityPath), ".beads", "dolt")
	database, ok, err := contract.ReadDoltDatabase(fsys.OSFS{}, scopeMetadataJSONPath(rigPath))
	if err != nil {
		return "", err
	}
	if !ok || strings.TrimSpace(database) == "" {
		return "", nil
	}
	if existing, ok, err := contract.ReadMetadataDoltDataDir(fsys.OSFS{}, scopeMetadataJSONPath(rigPath)); err != nil {
		return "", err
	} else if ok && strings.TrimSpace(existing) != "" {
		// Already pointed somewhere deliberate. Leave it exactly as it is.
		return "", nil
	}
	if _, err := os.Stat(filepath.Join(cityDataDir, strings.TrimSpace(database), ".dolt")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	// The rig's own data dir must be empty, or there are two candidate stores
	// and picking one silently would orphan the other.
	rigDataDir := filepath.Join(normalizePathForCompare(rigPath), ".beads", "dolt")
	empty, err := dirIsEmptyOrAbsent(rigDataDir)
	if err != nil {
		return "", err
	}
	if !empty {
		return "", fmt.Errorf("rig %s has its own store at %s and a database %q inside the city's data directory %s; resolve the duplicate before migrating", rigPath, rigDataDir, strings.TrimSpace(database), cityDataDir)
	}
	rel, err := filepath.Rel(filepath.Join(normalizePathForCompare(rigPath), ".beads"), cityDataDir)
	if err != nil {
		return "", fmt.Errorf("relative dolt data dir for %s: %w", rigPath, err)
	}
	return filepath.ToSlash(rel), nil
}

// requireNoManagedDoltServer fails closed unless gc's managed Dolt server for
// this city is demonstrably down.
//
// bd's migrate consults only its own .beads/dolt-server.pid to decide whether a
// server is running, and gc never writes that file. Migrating against a live
// gc-owned server therefore commits the mode flip and then cannot start the
// proxy, because Dolt still holds the exclusive data-dir lock — the workspace
// is unusable until the old server dies (20-migrate-spike.md §4). gc has to be
// the one that refuses.
//
// The question is who owns the Dolt process, not whether one exists. Once the
// city itself is migrated, bd's proxy holds the data-dir lock and serves every
// database in it — including the rigs still waiting their turn — so a bare
// "something holds the lock" test would fence the command out of its own
// second step. A live `bd db-proxy-child` for this root is therefore an
// answer, not an obstacle; a published gc runtime state never is.
func requireNoManagedDoltServer(cityPath string) error {
	statePath := managedDoltStatePath(cityPath)
	if _, err := os.Stat(statePath); err == nil {
		return fmt.Errorf("gc still publishes managed Dolt runtime state at %s; run gc stop first (bd cannot see a server gc started and would migrate onto it)", statePath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("probe managed dolt runtime state %s: %w", statePath, err)
	}
	dataDir := filepath.Join(normalizePathForCompare(cityPath), ".beads", "dolt")
	if _, bdOwned := bdOwnedProxyDoltConfig(filepath.Join(dataDir, bdProxyConfigFileName)); bdOwned {
		return nil
	}
	if holder := managedDoltDataDirLockHolder(dataDir); holder != "" {
		return fmt.Errorf("a live process holds the Dolt store lock %s; run gc stop first", holder)
	}
	if port, ok := readLegacyDoltPortMirror(cityPath); ok && loopbackPortAccepts(port) {
		return fmt.Errorf("a Dolt server is still listening on 127.0.0.1:%s (%s); run gc stop first", port, filepath.Join(cityPath, ".beads", "dolt-server.port"))
	}
	return nil
}

func readLegacyDoltPortMirror(cityPath string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(normalizePathForCompare(cityPath), ".beads", "dolt-server.port"))
	if err != nil {
		return "", false
	}
	port := strings.TrimSpace(string(data))
	if port == "" {
		return "", false
	}
	return port, true
}

func loopbackPortAccepts(port string) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", port), 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func doltRootIsInitialized(dataDir string) (bool, error) {
	info, err := os.Stat(filepath.Join(dataDir, ".dolt", "repo_state.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return !info.IsDir(), nil
}

func dirIsEmptyOrAbsent(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return true, nil
		}
		return false, err
	}
	return len(entries) == 0, nil
}

func printMigrateProxiedReport(stdout io.Writer, report migrateProxiedReport) {
	if report.DryRun {
		fmt.Fprintf(stdout, "DRY RUN: no files written (%s)\n", report.City) //nolint:errcheck
	}
	width := len("SCOPE")
	for _, r := range report.Scopes {
		if len(r.Scope) > width {
			width = len(r.Scope)
		}
	}
	fmt.Fprintf(stdout, "%-*s  %-17s  %s\n", width, "SCOPE", "STATUS", "DETAIL") //nolint:errcheck
	for _, r := range report.Scopes {
		detail := r.Detail
		if r.Error != "" {
			detail = r.Error
		}
		if r.DoltDataDir != "" {
			detail = strings.TrimSpace("dolt_data_dir=" + r.DoltDataDir + " " + detail)
		}
		fmt.Fprintf(stdout, "%-*s  %-17s  %s\n", width, r.Scope, r.Status, detail) //nolint:errcheck
	}
}
