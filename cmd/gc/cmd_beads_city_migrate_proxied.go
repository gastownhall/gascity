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
	"syscall"
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
// On rc.2 this is the only supported way to migrate a legacy GC-managed city:
// the journaled ownership handoff needs bd verbs no beads release has yet, and
// gc ships no driver for it. See engdocs/runbooks/beads-migrate-proxied.md.

const (
	migrateProxiedStatusMigrated  = "migrated"
	migrateProxiedStatusAlready   = "already-migrated"
	migrateProxiedStatusPlanned   = "would-migrate"
	migrateProxiedStatusFailed    = "failed"
	migrateProxiedStatusSkipped   = "skipped"
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
	City    string                      `json:"city"`
	DryRun  bool                        `json:"dry_run"`
	Scopes  []migrateProxiedScopeResult `json:"scopes"`
	Failed  int                         `json:"failed"`
	Skipped int                         `json:"skipped"`
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
"already migrated" — so a partially failed run can simply be rerun. It also
retires gc's own runtime publication for the city it just handed over.

On rc.2 this is the only supported migration for a legacy GC-managed city.
Procedure, refusals and recovery: engdocs/runbooks/beads-migrate-proxied.md.`,
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
	// planMigrateProxiedScopes puts the city first because a rig that shares the
	// city's data dir cannot be proxied while the city still is not. That
	// ordering only holds if a failed city turn stops the scopes standing on its
	// root: classifyMigrateProxiedScope takes SharedRootRel != "" as proof the
	// city's turn already made a repository there, so continuing hands bd the
	// city's still-direct data dir and it commits the rig's flip against it,
	// leaving a bd-proxied rig over a root gc still legacy-manages.
	sharedRootFailed := false
	for i := range scopes {
		if sharedRootFailed && scopes[i].SharedRootRel != "" {
			report.Scopes = append(report.Scopes, migrateProxiedSkippedOutcome(scopes[i]))
			continue
		}
		outcome := migrateProxiedScopeOutcome(cityPath, scopes[i], opts)
		if outcome.Status == migrateProxiedStatusFailed && scopes[i].IsCity {
			sharedRootFailed = true
		}
		report.Scopes = append(report.Scopes, outcome)
	}
	for _, r := range report.Scopes {
		switch r.Status {
		case migrateProxiedStatusFailed:
			report.Failed++
		case migrateProxiedStatusSkipped:
			report.Skipped++
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
		fmt.Fprintf(stderr, "%s: %d scope(s) failed, %d skipped; completed scopes stay migrated, rerun to finish the rest\n", name, report.Failed, report.Skipped) //nolint:errcheck
		return 1
	}
	return 0
}

// migrateProxiedSkippedOutcome records a scope this run did not touch because
// the city whose Dolt root it shares failed to migrate. Untouched is what makes
// the rerun the runbook promises work: the scope is still direct, so the next
// run classifies it from scratch once the city is proxied.
func migrateProxiedSkippedOutcome(scope migrateProxiedScope) migrateProxiedScopeResult {
	return migrateProxiedScopeResult{
		Scope:       scope.Label,
		Path:        scope.Path,
		Status:      migrateProxiedStatusSkipped,
		DoltDataDir: scope.SharedRootRel,
		Detail:      "not attempted: this scope shares the city's Dolt data directory and the city scope failed; migrate the city first, then rerun",
	}
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
		// proxied-server in metadata.json is not the same as a finished
		// migration. bd writes the mode at phase `prepared` and removes its
		// journal only at `committed`, so a scope whose bd died in between reads
		// as already-proxied while its sidecar may be missing and its old
		// dolt-server controls un-retired. bd can resume from its own journal;
		// reporting already-migrated here is how that repair never happens.
		inFlight, err := scopeMigrationInFlight(scope.Path)
		if err != nil {
			result.Status = migrateProxiedStatusFailed
			result.Error = err.Error()
			return result
		}
		if opts.DryRun {
			if inFlight {
				result.Status = migrateProxiedStatusPlanned
				result.Detail = "resume interrupted bd migration (" + contract.MigrateDoltModeJournalFile + " present); bd migrate from-server-to-proxied-server; rewrite .beads/config.yaml; bd ping"
			}
			return result
		}
		if inFlight {
			if err := resumeInterruptedScopeMigration(cityPath, scope); err != nil {
				result.Status = migrateProxiedStatusFailed
				result.Error = err.Error()
				return result
			}
			result.Status = migrateProxiedStatusMigrated
			result.Detail = "resumed bd's interrupted migration"
		}
		// An already-migrated scope can still carry gc's stale config keys —
		// normalising them is idempotent and is the whole point of step (f).
		if err := normalizeMigratedScopeConfig(cityPath, scope); err != nil {
			result.Status = migrateProxiedStatusFailed
			result.Error = err.Error()
			return result
		}
		if inFlight {
			if err := pingMigratedScope(cityPath, scope); err != nil {
				result.Status = migrateProxiedStatusFailed
				result.Error = fmt.Sprintf("migrated but not ready: %v", err)
				return result
			}
			result.Detail = "resumed bd's interrupted migration; bd ping ok"
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
	parts := make([]string, 0, 5)
	if classification.NeedsDoltInit {
		parts = append(parts, "dolt init "+classification.DoltDataDir)
	}
	if scope.SharedRootRel != "" {
		parts = append(parts, "set metadata dolt_data_dir="+scope.SharedRootRel)
	}
	parts = append(parts, "bd migrate from-server-to-proxied-server", "rewrite .beads/config.yaml")
	if scope.IsCity {
		parts = append(parts, "retire .gc/runtime/packs/dolt")
	}
	parts = append(parts, "bd ping")
	return strings.Join(parts, "; ")
}

// migrateProxiedScopeNow performs the ordered, non-dry-run work for one scope.
func migrateProxiedScopeNow(cityPath string, scope migrateProxiedScope, classification migrateProxiedClassification) error {
	// Fence before the first write of this scope's turn, not just before bd's.
	// `dolt init` goes into the same data directory a live gc-managed server
	// holds locked, and the entry check ran an unbounded number of scopes ago.
	if err := requireNoManagedDoltServer(cityPath); err != nil {
		return err
	}
	if classification.NeedsDoltInit {
		dataDir := classification.DoltDataDir
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
	if err := runScopeMigrateToProxied(cityPath, scope); err != nil {
		return err
	}
	if err := requireScopeMigrationCommitted(scope.Path); err != nil {
		return err
	}
	return normalizeMigratedScopeConfig(cityPath, scope)
}

// runScopeMigrateToProxied hands the scope to bd's own migration verb.
func runScopeMigrateToProxied(cityPath string, scope migrateProxiedScope) error {
	args := []string{"migrate", "from-server-to-proxied-server", "--idle-timeout", migrateProxiedIdleTimeoutFlag}
	if migrateProxiedSupportsJSON(cityPath, scope.Path) {
		args = append(args, "--json")
	}
	out, err := runBdScopeCommand(cityPath, scope.Path, args...)
	if err != nil {
		return fmt.Errorf("bd migrate from-server-to-proxied-server: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// resumeInterruptedScopeMigration replays the phases bd did not reach.
//
// bd exempts its four mode-migration verbs from the proxied store-init path and
// loads its journal on entry, so rerunning the same command is how a half-
// finished flip is finished. Re-fence first: the entry fence ran an unbounded
// number of scopes ago, and bd's own running-server check consults only its own
// .beads/dolt-server.pid, which gc never writes.
func resumeInterruptedScopeMigration(cityPath string, scope migrateProxiedScope) error {
	if err := requireNoManagedDoltServer(cityPath); err != nil {
		return err
	}
	if err := runScopeMigrateToProxied(cityPath, scope); err != nil {
		return fmt.Errorf("resume interrupted migration for %s: %w", scope.Label, err)
	}
	return requireScopeMigrationCommitted(scope.Path)
}

// scopeMigrationJournalPath is where bd keeps the in-flight record of a
// mode migration for this scope.
func scopeMigrationJournalPath(scopeRoot string) string {
	return filepath.Join(scopeRoot, ".beads", contract.MigrateDoltModeJournalFile)
}

// scopeMigrationInFlight reports whether bd left a migration journal behind,
// which it does for every phase from `prepared` until `committed`.
func scopeMigrationInFlight(scopeRoot string) (bool, error) {
	if _, err := os.Stat(scopeMigrationJournalPath(scopeRoot)); err == nil {
		return true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("inspect bd migration journal for %s: %w", scopeRoot, err)
	}
	return false, nil
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
	inFlight, err := scopeMigrationInFlight(scopeRoot)
	if err != nil {
		return err
	}
	if inFlight {
		return fmt.Errorf("bd left its migration journal behind at %s; the migration did not commit", scopeMigrationJournalPath(scopeRoot))
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
	if err := normalizeScopeDoltConfig(scope.Path, state); err != nil {
		return err
	}
	if err := retireManagedDoltResidue(cityPath, scope); err != nil {
		return err
	}
	// This process just changed the city's topology. Everything downstream —
	// the bd ping below, and the next scope's migration, which reads the city's
	// projection — must see the migrated binding, not the managed-direct answer
	// cached before the flip. The stamp catches the file rewrites on its own;
	// dropping the city outright is one cheap map sweep and does not depend on
	// every classification input being stamped.
	forgetProxiedScopeRuntimeEnv(cityPath)
	return nil
}

// retireManagedDoltResidue removes the state gc published about a server it
// will never run for this scope again.
//
// AC-X's rule is that gc-side residue is handled by a documented gc command or
// by the lifecycle, never by hand — and for this residue the lifecycle does not
// come back for it. clearManagedDoltRuntimeStateUnlessBound returns early for a
// scope carrying a complete bd storage binding, which is exactly what the
// migration just created, so the publication describing the dead managed
// server would sit under .gc forever. The command that created the condition
// retires it.
//
// It is idempotent and best-effort about nothing: a residue file that cannot be
// removed is reported, because a stale dolt-state.json is also this command's
// own entry fence and a half-cleared publication would refuse the next run.
func retireManagedDoltResidue(cityPath string, scope migrateProxiedScope) error {
	// The port mirror is per scope: gc writes one into every scope it serves
	// from the managed city server, and a proxied scope has no port to mirror.
	removeDoltPortFile(scope.Path)
	if !scope.IsCity {
		return nil
	}
	// Strict, not the env-honoring resolver: this step selects files to delete,
	// and the city it deletes them for is the one --city named. The ambient
	// GC_PACK_STATE_DIR / GC_CITY_RUNTIME_DIR / GC_DOLT_* a gc-spawned agent
	// session carries describe whatever city spawned it, and the entry fence
	// above already reads the hardcoded managedDoltStatePath — so honoring env
	// here would let `gc --city A beads city migrate-proxied`, run from a city-B
	// session, pass A's fence and remove B's pid, lock, config, log and
	// provider-state files.
	layout, err := resolveManagedDoltRuntimeLayoutStrict(cityPath)
	if err != nil {
		return fmt.Errorf("resolve managed dolt runtime layout: %w", err)
	}
	for _, path := range []string{
		managedDoltStatePath(cityPath),
		layout.StateFile, layout.PIDFile, layout.LockFile, layout.ConfigFile, layout.LogFile,
	} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("retire managed dolt runtime state %s: %w", path, err)
		}
	}
	// Only when nothing else lives there. The pack state dir is gc's, but it is
	// not exclusively this pack's publication.
	if err := os.Remove(layout.PackStateDir); err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) {
		return fmt.Errorf("retire managed dolt pack state dir %s: %w", layout.PackStateDir, err)
	}
	return nil
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
	// DoltDataDir is the directory NeedsDoltInit was decided against, resolved
	// once from the scope's recorded dolt_data_dir. The executor and the
	// dry-run plan both use it, because deciding from the recorded dir and
	// initializing in the default one puts a stray Dolt root beside a store bd
	// then refuses to migrate, on every rerun. Empty when no init is needed.
	DoltDataDir string
}

// rigMirrorsCityCanonicalEndpoint reports whether a rig's dolt.host/dolt.port
// are the city's own canonical endpoint rather than an endpoint of its own.
//
// The "pins dolt.host" refusal above exists for a scope that tracks a server
// somewhere else, which is not gc's to migrate. An inherited rig under a
// canonical city is the opposite case: gc writes that host and port itself,
// because a rig scope opens through the city's endpoint and bd resolves it from
// the rig's own config (inheritedRigDoltConfigState). A city handed to bd is
// canonical, so every one of its rigs acquires the mirror on the next
// canonicalisation — and the refusal then fired on gc's own bookkeeping,
// leaving the rig with no supported hop at all.
//
// It stays narrow: only an inherited rig, only when both halves match the
// city's canonical endpoint exactly. A rig that claims anything else is still
// an external pin and is still refused.
func rigMirrorsCityCanonicalEndpoint(cityPath string, scope migrateProxiedScope, cfg contract.ConfigState) bool {
	if scope.IsCity || cfg.EndpointOrigin != contract.EndpointOriginInheritedCity {
		return false
	}
	cityCfg, configured, err := contract.ReadConfigState(fsys.OSFS{}, filepath.Join(cityPath, ".beads", "config.yaml"))
	if err != nil || !configured || cityCfg.EndpointOrigin != contract.EndpointOriginCityCanonical {
		return false
	}
	host, port := strings.TrimSpace(cfg.DoltHost), strings.TrimSpace(cfg.DoltPort)
	return host != "" && port != "" &&
		host == strings.TrimSpace(cityCfg.DoltHost) && port == strings.TrimSpace(cityCfg.DoltPort)
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
	// doltlite is the embedded engine: no server, and so nothing to migrate to
	// a proxy. gc recognizes one by either metadata field, because a doltlite
	// scope can leave `backend` at the dolt default and name the engine in
	// `database` instead (cmd/gc/cmd_bd_store_bridge.go). Asking only about
	// `backend` let that shape reach the migratable arm, where an empty
	// dolt_mode reads as the pre-dolt_mode legacy direct server.
	//
	// Anything that is neither is refused one frame earlier, by the metadata
	// loader above: it admits dolt and doltlite and names the backend it
	// rejected, so there is nothing left here for a third arm to catch.
	if strings.EqualFold(backend, "doltlite") || strings.EqualFold(strings.TrimSpace(metadata.Database), "doltlite") {
		return migrateProxiedClassification{}, fmt.Errorf("%s is a doltlite scope; the embedded engine has no server topology to migrate", scope.Label)
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
		if host := strings.TrimSpace(cfg.DoltHost); host != "" && !rigMirrorsCityCanonicalEndpoint(cityPath, scope, cfg) {
			return migrateProxiedClassification{}, fmt.Errorf("%s pins dolt.host %q; an external endpoint is not gc's to migrate", scope.Label, host)
		}
	}

	classification := migrateProxiedClassification{}
	if scope.SharedRootRel != "" {
		// The rig migrates against the city's root, which the city's own turn
		// has already made a repository.
		return classification, nil
	}
	// This scope migrates against the data dir its metadata names, so bd's root
	// validator has to find a real Dolt repo there. gc's multi-database data dir
	// never was one.
	//
	// Reading the recorded dolt_data_dir rather than assuming <scope>/.beads/dolt
	// is what makes a partly migrated rig resumable: the key is written before
	// bd runs, so a rig whose `bd migrate` failed already points at the city's
	// root and its own .beads/dolt is still the empty directory it always was.
	dataDir := scopeDoltDataDir(scope.Path)
	hasRepo, err := doltRootIsInitialized(dataDir)
	if err != nil {
		return migrateProxiedClassification{}, err
	}
	if hasRepo {
		return classification, nil
	}
	empty, err := dirIsEmptyOrAbsent(dataDir)
	if err != nil {
		return migrateProxiedClassification{}, err
	}
	if !scope.IsCity {
		// A rig's databases live in the city's data dir. If gc could not place
		// this one there, `dolt init` into the rig would migrate it onto a
		// brand-new empty repository while its real data stayed wherever it is,
		// and bd would not say a word. The one dolt init this command performs
		// is the city's.
		if empty {
			return migrateProxiedClassification{}, fmt.Errorf("rig %q has an empty %s and no database in the city's data directory; refusing to migrate it onto an empty store", scope.Name, dataDir)
		}
		return migrateProxiedClassification{}, fmt.Errorf("rig %q has a store of its own at %s that is not a Dolt repository, and no database in the city's data directory; gc will not initialize a repository over it", scope.Name, dataDir)
	}
	if !empty {
		if err := requireScopeDatabaseInDataDir(scope, dataDir); err != nil {
			return migrateProxiedClassification{}, err
		}
	}
	classification.NeedsDoltInit = true
	classification.DoltDataDir = dataDir
	return classification, nil
}

// requireScopeDatabaseInDataDir refuses a `dolt init` into a data directory
// that holds databases but not this scope's.
//
// The init is what makes gc's multi-database directory a Dolt root bd will
// open, and it is safe precisely because the databases beside it are the ones
// being migrated. A directory holding somebody else's databases and not this
// scope's is a layout gc cannot explain: the init would succeed, bd would come
// up on a fresh empty store, and every database in there would be orphaned
// without a word from either side.
func requireScopeDatabaseInDataDir(scope migrateProxiedScope, dataDir string) error {
	database, ok, err := contract.ReadDoltDatabase(fsys.OSFS{}, scopeMetadataJSONPath(scope.Path))
	if err != nil {
		return err
	}
	database = strings.TrimSpace(database)
	if !ok || database == "" {
		return nil
	}
	if _, err := os.Stat(filepath.Join(dataDir, database, ".dolt")); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return fmt.Errorf("%s records database %q, but %s holds other databases and not that one; refusing to initialize a Dolt root that would orphan them", scope.Label, database, dataDir)
}

// scopeDoltDataDir is the directory bd will resolve this scope's store from:
// the metadata's dolt_data_dir when it names one, relative to .beads as beads
// resolves it, and <scope>/.beads/dolt otherwise.
func scopeDoltDataDir(scopeRoot string) string {
	beadsDir := filepath.Join(normalizePathForCompare(scopeRoot), ".beads")
	recorded, ok, err := contract.ReadMetadataDoltDataDir(fsys.OSFS{}, scopeMetadataJSONPath(scopeRoot))
	if err != nil || !ok {
		return filepath.Join(beadsDir, "dolt")
	}
	if recorded = strings.TrimSpace(recorded); recorded == "" {
		return filepath.Join(beadsDir, "dolt")
	}
	if filepath.IsAbs(recorded) {
		return filepath.Clean(recorded)
	}
	return filepath.Clean(filepath.Join(beadsDir, recorded))
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
