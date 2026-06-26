package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/pidutil"
)

// providerLifecycleLaunchctlGetenv reads a value from `launchctl getenv` on
// macOS. Used by providerLifecycleProcessEnv as a fallback when an env var
// the bd-provider script consumes (currently only GC_DOLT_LOGLEVEL) isn't
// set in os.Environ — without this, `gc start` from a user shell silently
// drops `launchctl setenv` values because they live in launchd's domain,
// not the shell's env. Returns "" on non-Darwin or when the key is unset
// or launchctl is unavailable.
//
// var so tests can stub without invoking real launchctl. Same pattern as
// supervisorLaunchctlRun / supervisorLaunchdActive in cmd_supervisor_lifecycle.go.
var providerLifecycleLaunchctlGetenv = func(key string) string {
	if goruntime.GOOS != "darwin" {
		return ""
	}
	out, err := exec.Command("launchctl", "getenv", key).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// cityDoltConfigs stores per-city Dolt configuration keyed by cityPath.
// Registered by startBeadsLifecycle so env builders and isExternalDolt can
// read city-scoped config without relying on process-global env vars (which
// break supervisor multi-tenancy where multiple cities share one process).
var cityDoltConfigs sync.Map // cityPath → config.DoltConfig

// providerOpSemaphores limits concurrent provider operations per city.
// When dolt goes down, health checks and recovery attempts from multiple
// callers can pile up. Without backpressure, all queued operations fire
// simultaneously when dolt restarts, causing a thundering herd that
// hammers the server back down. Each semaphore allows at most 1
// concurrent provider operation per city (serialize lifecycle ops).
var providerOpSemaphores sync.Map // cityPath → chan struct{}

// lastBeadsProviderRecover records the timestamp of the most recent
// recover attempt per city so healthBeadsProvider can refuse a 2nd
// recover within providerRecoverCooldown of the prior one. Together
// with the breaker-aware skip, this breaks the low-RSS restart-loop
// where each patrol tick re-trips the bd circuit breaker and
// re-desyncs the managed-dolt PID.
var lastBeadsProviderRecover sync.Map // cityPath → time.Time

// providerRecoverCooldown is the minimum interval between consecutive
// managed-dolt recover attempts on a single city. Stubbable for tests.
// 30s is the lower bound suggested by issue #2792 — long enough to
// span the bd breaker cooldown + dolt startup, short enough that a
// genuinely-degraded server still recovers on the next tick.
var providerRecoverCooldown = func() time.Duration { return 30 * time.Second }

// providerRecoverNow is the clock for the recover-backoff window.
// Stubbable for tests.
var providerRecoverNow = time.Now

// isBreakerOpenError reports whether err looks like a bd circuit
// breaker fail-fast — emitted by the bd client when the breaker is
// open. The two substrings hedge against either half of the canonical
// message being rephrased upstream; they match the strings the
// integration suite already asserts on
// (test/integration/integration_test.go).
func isBreakerOpenError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "dolt circuit breaker is open") ||
		strings.Contains(s, "server appears down, failing fast")
}

func cityDoltConfigHasLifecycleFields(cfg config.DoltConfig) bool {
	return cfg.Host != "" ||
		cfg.Port != 0 ||
		cfg.ArchiveLevel != nil ||
		cfg.AutoGCEnabled != nil ||
		cfg.MaxConnections != 0 ||
		cfg.ReadTimeoutMillis != 0 ||
		cfg.WriteTimeoutMillis != 0 ||
		cfg.DoltLockReleaseTimeout != ""
}

func registerCityDoltConfig(cityPath string, cfg config.DoltConfig) {
	cityDoltConfigs.Store(normalizePathForCompare(cityPath), cfg)
}

func clearCityDoltConfig(cityPath string) {
	cityDoltConfigs.Delete(normalizePathForCompare(cityPath))
}

var resolveProviderLifecycleGCBinary = func() string {
	if isTestBinary() {
		return ""
	}
	if exe, err := os.Executable(); err == nil && exe != "" {
		return exe
	}
	if path, err := exec.LookPath("gc"); err == nil && path != "" {
		return path
	}
	return ""
}

var (
	providerProbeTimeout = 10 * time.Second
	// Override only in tests that do not call t.Parallel while the hook is changed.
	providerLifecycleContext = context.WithTimeout
)

var (
	initDirIfReadyEnsureBeadsProvider = ensureBeadsProvider
	initDirIfReadyInitAndHookDir      = initAndHookDir
	initDirIfReadyWaitForManagedDolt  = waitForManagedDoltInitReady
	initAndHookDirWaitForScopeReady   = waitForBeadsScopeReadyAfterRecovery
)

func isRetryableManagedDoltLifecycleError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "dolt server exited during startup") ||
		strings.Contains(msg, "did not become query-ready") ||
		strings.Contains(msg, "signal: terminated") ||
		strings.Contains(msg, "table not found: issues") ||
		strings.Contains(msg, "table not found: config")
}

// ── Consolidated lifecycle operations ────────────────────────────────────
//
// The bead store lifecycle has a strict ordering:
//
//   start → [init + hooks]* → (agents run) → health* → stop
//
// These high-level functions enforce that ordering so call sites don't
// need to know the sequence. Use these instead of calling the low-level
// functions (ensureBeadsProvider, initBeadsForDir, installBeadHooks)
// directly.
//
// Exec provider protocol operations:
//   start         — start the backing service
//   init          — initialize beads in a directory
//   health        — check provider health
//   stop          — stop the backing service

// startBeadsLifecycle runs the full bead store startup sequence:
// start → init+hooks(city) → init+hooks(each rig) → regenerate routes.
// Called by gc start and controller config reload. Rigs must have absolute
// paths before calling (resolve relative paths first).
func startBeadsLifecycle(cityPath, _ string, cfg *config.City, stderr io.Writer) error {
	if err := validateCanonicalCompatDoltDrift(cityPath, cfg); err != nil {
		return err
	}
	// Register per-city dolt config so env builders and isExternalDolt can
	// read it without process-global env vars. This is the single
	// registration point — supervisor, standalone, and reload all flow
	// through here. Always write (or clear) to handle config reload:
	// removing [dolt] after a reload must not leave stale entries.
	if cityDoltConfigHasLifecycleFields(cfg.Dolt) {
		registerCityDoltConfig(cityPath, cfg.Dolt)
	} else {
		clearCityDoltConfig(cityPath)
	}
	skipLocalDolt := false
	if cityUsesManagedDoltBeadsLifecycle(cityPath) {
		owned, err := managedDoltLifecycleOwned(cityPath)
		if err != nil {
			return err
		}
		skipLocalDolt = !owned
	} else if cityUsesDoltliteBeadsBackend(cityPath) {
		skipLocalDolt = true
	}
	if !skipLocalDolt {
		if err := ensureBeadsProvider(cityPath); err != nil {
			return fmt.Errorf("bead store: %w", err)
		}
	}
	beadsPrefix := config.EffectiveHQPrefix(cfg)
	// Leave doltDatabase empty unless the caller knows a canonical server DB
	// identity that differs from the bead prefix. New managed bd stores still
	// default to prefix-named databases, but older/imported metadata may carry
	// a different dolt_database that gc-beads-bd should preserve.
	if err := initAndHookDir(cityPath, cityPath, beadsPrefix); err != nil {
		return fmt.Errorf("init city beads: %w", err)
	}
	for i := range cfg.Rigs {
		if strings.TrimSpace(cfg.Rigs[i].Path) == "" {
			continue
		}
		prefix := cfg.Rigs[i].EffectivePrefix()
		if err := initAndHookDir(cityPath, cfg.Rigs[i].Path, prefix); err != nil {
			return fmt.Errorf("init rig %q beads: %w", cfg.Rigs[i].Name, err)
		}
	}
	if err := normalizeCanonicalBdScopeFiles(cityPath, cfg, stderr); err != nil {
		return err
	}
	// Regenerate routes for cross-rig routing.
	if len(cfg.Rigs) > 0 {
		allRigs := collectRigRoutes(cityPath, cfg)
		if err := writeAllRoutes(allRigs); err != nil {
			return fmt.Errorf("writing routes: %w", err)
		}
	}
	return nil
}

// initDirIfReady initializes beads for a single directory, ensuring the
// backing service is ready first. For the bd provider, this is a no-op
// (Dolt isn't running until gc start). Used by gc init and gc rig add.
//
// Returns (deferred bool, err). deferred=true means the bd provider
// skipped init — the caller should tell the user it's deferred to gc start.
func initDirIfReady(cityPath, dir, prefix string) (deferred bool, err error) {
	provider := beadsProvider(cityPath)
	if cityUsesManagedDoltBeadsLifecycle(cityPath) {
		if gcDoltSkip() {
			// Defer to controller/startup without forcing a new dolt_database:
			// preserve existing metadata identity when present.
			if err := seedDeferredManagedBeadsErr(cityPath, dir, prefix, ""); err != nil {
				return false, err
			}
			return true, nil
		}
		owned, err := managedDoltLifecycleOwned(cityPath)
		if err != nil {
			return false, err
		}
		if !owned {
			if err := initDirIfReadyInitAndHookDir(cityPath, dir, prefix); err != nil {
				return false, err
			}
			return false, nil
		}
		if err := initDirIfReadyManagedDolt(cityPath, dir, prefix, provider); err != nil {
			return false, err
		}
		return false, nil
	}

	if provider == "" {
		if err := seedDeferredManagedBeadsErr(cityPath, dir, prefix, ""); err != nil {
			return false, err
		}
		return true, nil
	}
	// For exec: providers, probe to check if the backing service is available.
	// If not available (exit 2 or error), defer initialization to gc start.
	if strings.HasPrefix(provider, "exec:") {
		script := strings.TrimPrefix(provider, "exec:")
		if !runProviderProbe(script, cityPath, provider) {
			if cityUsesBdStoreContract(cityPath) {
				if err := seedDeferredManagedBeadsErr(cityPath, dir, prefix, ""); err != nil {
					return false, err
				}
			}
			return true, nil // Not running — defer to gc start.
		}
	}
	if err := initDirIfReadyManagedDolt(cityPath, dir, prefix, provider); err != nil {
		return false, err
	}
	return false, nil
}

func initDirIfReadyManagedDolt(cityPath, dir, prefix, _ string) error {
	if err := initDirIfReadyEnsureBeadsProvider(cityPath); err != nil {
		return fmt.Errorf("bead store: %w", err)
	}
	if err := initDirIfReadyWaitForManagedDolt(cityPath, managedDoltInitReadyTimeout); err != nil {
		return err
	}
	return initDirIfReadyInitAndHookDir(cityPath, dir, prefix)
}

func desiredScopeDoltConfigStateForInit(cityPath, dir, prefix string) (contract.ConfigState, bool, error) {
	if strings.TrimSpace(dir) == "" || strings.TrimSpace(prefix) == "" {
		return contract.ConfigState{}, false, nil
	}
	cityPath = normalizePathForCompare(cityPath)
	cityDolt := config.DoltConfig{}
	if cfg, err := loadCityConfig(cityPath, io.Discard); err == nil {
		resolveRigPaths(cityPath, cfg.Rigs)
		cityPrefix := config.EffectiveHQPrefix(cfg)
		cityDolt = cfg.Dolt
		cityState, _, err := resolveDesiredCityEndpointState(cityPath, cityDolt, cityPrefix)
		if err != nil {
			return contract.ConfigState{}, false, err
		}
		if samePath(cityPath, dir) {
			cityState.IssuePrefix = prefix
			return cityState, true, nil
		}
		for i := range cfg.Rigs {
			if samePath(cfg.Rigs[i].Path, dir) {
				rig := cfg.Rigs[i]
				rig.Prefix = prefix
				rigState, err := resolveDesiredRigEndpointState(cityPath, rig, cityState)
				if err != nil {
					return contract.ConfigState{}, false, err
				}
				return rigState, true, nil
			}
		}
		rigState, err := resolveDesiredRigEndpointState(cityPath, config.Rig{Name: filepath.Base(dir), Path: dir, Prefix: prefix}, cityState)
		if err != nil {
			return contract.ConfigState{}, false, err
		}
		return rigState, true, nil
	}
	if loaded, ok := cityDoltConfigs.Load(cityPath); ok {
		if cfg, ok := loaded.(config.DoltConfig); ok {
			cityDolt = cfg
		}
	}
	cityState, _, err := resolveDesiredCityEndpointState(cityPath, cityDolt, prefix)
	if err != nil {
		return contract.ConfigState{}, false, err
	}
	if samePath(cityPath, dir) {
		return cityState, true, nil
	}
	rigState, err := resolveDesiredRigEndpointState(cityPath, config.Rig{Name: filepath.Base(dir), Path: dir, Prefix: prefix}, cityState)
	if err != nil {
		return contract.ConfigState{}, false, err
	}
	return rigState, true, nil
}

//nolint:unparam // keep fs seam for future testable FS injection
func ensureCanonicalScopeConfigState(fs fsys.FS, dir string, state contract.ConfigState) error {
	beadsDir := filepath.Join(dir, ".beads")
	if err := ensureBeadsDir(fs, beadsDir); err != nil {
		return err
	}
	changed, err := contract.EnsureCanonicalConfig(fs, filepath.Join(beadsDir, "config.yaml"), state)
	if err != nil {
		return err
	}
	if changed && state.EndpointOrigin != contract.EndpointOriginExplicit {
		// PR 1965 made export.auto:false canonical, but a pre-existing
		// .beads/issues.jsonl from before this normalization still triggers
		// bd's auto-import-on-write trap (sa-41j3kp) — bd sees the file,
		// detects a "stale DB", and stalls bd create for the full 2m
		// subprocess timeout while it re-imports the JSONL. The file is a
		// stale export from when auto-export was on; with the canonical
		// config now suppressing auto-export, nothing will refresh it. Explicit
		// opt-out scopes keep JSONL as load-bearing state.
		removeStaleBdExportJSONL(fs, beadsDir)
	}
	return nil
}

// removeStaleBdExportJSONL removes .beads/issues.jsonl if present. Called after
// EnsureCanonicalConfig writes export.auto:false, since the file is a stale
// export that bd's auto-import path would otherwise re-load on every write,
// stalling bd create for the full subprocess timeout on large datasets.
// Best-effort: any error is non-fatal because the env-var BD_EXPORT_AUTO=false
// path (bdRuntimeEnv) is a second line of defense for gc-initiated calls.
func removeStaleBdExportJSONL(fs fsys.FS, beadsDir string) {
	path := filepath.Join(beadsDir, "issues.jsonl")
	if _, err := fs.Stat(path); err != nil {
		return
	}
	_ = fs.Remove(path)
}

func seedDeferredManagedBeads(cityPath, dir, prefix, doltDatabase string) {
	_ = seedDeferredManagedBeadsErr(cityPath, dir, prefix, doltDatabase)
}

func seedDeferredManagedBeadsErr(cityPath, dir, prefix, doltDatabase string) error {
	if usesPostgres, err := scopeUsesPostgresBackendForInit(cityPath, dir); err != nil {
		return err
	} else if usesPostgres {
		return nil
	}
	if state, ok, err := desiredScopeDoltConfigStateForInit(cityPath, dir, prefix); err != nil {
		return err
	} else if ok {
		if err := ensureCanonicalScopeConfigState(fsys.OSFS{}, dir, state); err != nil {
			return err
		}
	}
	if strings.TrimSpace(doltDatabase) == "" {
		doltDatabase = readDeferredManagedDoltDatabase(filepath.Join(dir, ".beads", "metadata.json"), defaultScopeDoltDatabase(cityPath, dir, prefix))
	}
	return ensureCanonicalScopeMetadataForInit(fsys.OSFS{}, dir, doltDatabase)
}

func readDeferredManagedDoltDatabase(path, fallback string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return fallback
	}

	var meta map[string]any
	if json.Unmarshal(data, &meta) != nil {
		return fallback
	}
	if db := strings.TrimSpace(fmt.Sprint(meta["dolt_database"])); db != "" && db != "<nil>" {
		return db
	}
	return fallback
}

func defaultScopeDoltDatabase(cityPath, dir, prefix string) string {
	if samePath(cityPath, dir) {
		return "hq"
	}
	return prefix
}

func isReservedManagedDoltDatabase(name string) bool {
	_, ok := managedDoltSystemDatabases[strings.ToLower(strings.TrimSpace(name))]
	return ok
}

func canonicalScopeDoltDatabase(cityPath, dir, prefix string) string {
	return readDeferredManagedDoltDatabase(filepath.Join(dir, ".beads", "metadata.json"), defaultScopeDoltDatabase(cityPath, dir, prefix))
}

func normalizeCanonicalBdScopeFilesForInit(cityPath, dir, prefix, doltDatabase string) error {
	if !cityUsesBdStoreContract(cityPath) {
		return nil
	}
	if usesPostgres, err := scopeUsesPostgresBackendForInit(cityPath, dir); err != nil {
		return err
	} else if usesPostgres {
		return nil
	}
	if state, ok, err := desiredScopeDoltConfigStateForInit(cityPath, dir, prefix); err != nil {
		return err
	} else if ok {
		if err := ensureCanonicalScopeConfigState(fsys.OSFS{}, dir, state); err != nil {
			return err
		}
	}
	if strings.TrimSpace(doltDatabase) == "" {
		doltDatabase = canonicalScopeDoltDatabase(cityPath, dir, prefix)
	}
	if isReservedManagedDoltDatabase(doltDatabase) {
		// Preserve legacy probe metadata during startup normalization so old
		// scopes can still boot and migrate deliberately. New init paths still
		// reject this reserved name when it is not already pinned in metadata.
		return ensureCanonicalScopeMetadataForInit(fsys.OSFS{}, dir, doltDatabase)
	}
	return enforceCanonicalScopeMetadataForInit(fsys.OSFS{}, dir, doltDatabase)
}

// initAndHookDir is the atomic unit of bead store initialization:
// init the directory, then remove any stale gc-managed bead event hooks.
// The ordering matters because init (bd init) may recreate .beads/ and
// wipe existing hooks. installBeadHooks only removes gc-stamped hooks and
// is always safe to run regardless of event_hooks config.
func initAndHookDir(cityPath, dir, prefix string) error {
	if usesPostgres, err := scopeUsesPostgresBackendForInit(cityPath, dir); err != nil {
		return err
	} else if usesPostgres {
		if err := installBeadHooks(dir, cityPath); err != nil {
			return fmt.Errorf("install hooks at %s: %w", dir, err)
		}
		return nil
	}
	doltDatabase := canonicalScopeDoltDatabase(cityPath, dir, prefix)
	if err := normalizeCanonicalBdScopeFilesForInit(cityPath, dir, prefix, doltDatabase); err != nil {
		return err
	}
	if err := initBeadsForDir(cityPath, dir, prefix, doltDatabase); err != nil {
		return err
	}
	if err := normalizeCanonicalBdScopeFilesForInit(cityPath, dir, prefix, doltDatabase); err != nil {
		return err
	}
	if cityUsesBdStoreContract(cityPath) && currentResolvableManagedDoltPort(cityPath) != "" {
		if err := syncManagedDoltPortMirrors(cityPath); err != nil {
			return fmt.Errorf("sync managed dolt port mirrors after init: %w", err)
		}
		if err := initAndHookDirWaitForScopeReady(dir, cityPath, time.Now().Add(10*time.Second)); err != nil {
			return fmt.Errorf("waiting for initialized bead scope readiness: %w", err)
		}
		// Strong post-init validation: confirm the canonical database
		// actually exists on the running Dolt server. The scope-ready
		// check above pings via the bead store, which has historically
		// returned ok against a server that knows the database name in
		// metadata but doesn't have it in its catalog (gascity-3
		// reproducer where bd init's CREATE DATABASE was silently
		// swallowed and the city's hq was never created). An explicit
		// SHOW DATABASES check fails fast at the actual init step
		// instead of leaking the failure to a downstream "database not
		// found" at gc session attach time.
		if err := verifyManagedDoltDatabaseExistsAfterInit(cityPath, dir, doltDatabase); err != nil {
			return fmt.Errorf("verifying canonical scope database after init: %w", err)
		}
	}
	// Non-fatal: hooks are convenience (event forwarding), not critical.
	if err := installBeadHooks(dir, cityPath); err != nil {
		return fmt.Errorf("install hooks at %s: %w", dir, err)
	}
	return nil
}

func scopeUsesPostgresBackendForInit(cityPath, dir string) (bool, error) {
	if !cityUsesBdStoreContract(cityPath) {
		return false, nil
	}
	path := scopeMetadataJSONPath(dir)
	state, ok, err := contract.LoadMetadataState(fsys.OSFS{}, path)
	if err != nil {
		if allowLegacyDoltMetadataRepair(fsys.OSFS{}, path, err) {
			return false, nil
		}
		return false, err
	}
	if ok {
		switch state.Backend {
		case "postgres":
			return true, nil
		case "dolt":
			return false, nil
		}
	}
	_, usesPostgres, err := postgresMetadataForScope(cityPath, dir)
	return usesPostgres, err
}

func allowLegacyDoltMetadataRepair(fs fsys.FS, path string, err error) bool {
	var parseErr *contract.MetadataParseError
	if !errors.As(err, &parseErr) {
		return false
	}
	data, readErr := fs.ReadFile(path)
	if readErr != nil {
		return false
	}
	var raw struct {
		Backend          string `json:"backend"`
		PostgresHost     string `json:"postgres_host"`
		PostgresPort     string `json:"postgres_port"`
		PostgresUser     string `json:"postgres_user"`
		PostgresDatabase string `json:"postgres_database"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(raw.Backend), "legacy") {
		return false
	}
	return strings.TrimSpace(raw.PostgresHost) == "" &&
		strings.TrimSpace(raw.PostgresPort) == "" &&
		strings.TrimSpace(raw.PostgresUser) == "" &&
		strings.TrimSpace(raw.PostgresDatabase) == ""
}

// verifyManagedDoltDatabaseExistsAfterInit confirms the named database is
// present in the running managed Dolt server's catalog. Used as a post-init
// guardrail to catch the silent-init failure mode where bd init reports
// success but the database was never actually created. Returns nil when
// the database is found, or an actionable error otherwise.
//
// The function is a no-op (returns nil) when the city does not use the bd
// store contract or when no managed Dolt port is resolvable — the caller
// already gates on those conditions, but we double-check defensively so
// the helper is safe to call from new sites without re-checking.
var verifyManagedDoltDatabaseExistsAfterInit = func(cityPath, dir, dbName string) error {
	if !cityUsesBdStoreContract(cityPath) {
		return nil
	}
	port := currentResolvableManagedDoltPort(cityPath)
	if port == "" {
		return nil
	}
	dbName = strings.TrimSpace(dbName)
	if dbName == "" {
		return nil
	}
	if isLegacyManagedDoltProbeDatabase(dbName) {
		// Startup normalization preserves this one legacy reserved database
		// when existing metadata already uses it as the real bead store.
		return nil
	}

	dbs, err := managedDoltListUserDatabasesAfterInit(port)
	if err != nil {
		return err
	}
	for _, d := range dbs {
		if strings.EqualFold(d, dbName) {
			return nil
		}
	}
	return fmt.Errorf("database %q not found in managed Dolt server catalog after init for scope %s (server-visible: %v); bd init reported success but the database was never created — usually means CREATE DATABASE was swallowed (see gc-beads-bd.sh)", dbName, dir, dbs)
}

var managedDoltListUserDatabasesAfterInit = func(port string) ([]string, error) {
	host, user := managedDoltConnectHost(""), "root"
	db, err := managedDoltOpenDB(host, port, user)
	if err != nil {
		return nil, fmt.Errorf("connect to managed Dolt at %s:%s: %w", host, port, err)
	}
	defer db.Close() //nolint:errcheck

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire connection to managed Dolt: %w", err)
	}
	defer conn.Close() //nolint:errcheck

	dbs, err := managedDoltSelectUserDatabasesFromConn(ctx, conn)
	if err != nil {
		return nil, fmt.Errorf("list databases on managed Dolt: %w", err)
	}
	return dbs, nil
}

func shouldRetryExecBdInit(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "bd schema not visible")
}

func isBdAlreadyInitializedError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already initialized") || strings.Contains(msg, "already exists")
}

// resolveRigPaths resolves relative rig paths to absolute (relative to
// cityPath). Mutates rigs in place. Must be called after loading city config
// and before any access to rigs[i].Path for filesystem operations. Required
// call sites include: doRigList, doRigAdd, doRigRemove, doRigDefault,
// cmd_start, cmd_hook, cmd_sling, dispatch_runtime, city_runtime,
// cmd_supervisor, cmd_convoy_dispatch.
func resolveRigPaths(cityPath string, rigs []config.Rig) {
	for i := range rigs {
		if strings.TrimSpace(rigs[i].Path) == "" {
			continue
		}
		if !filepath.IsAbs(rigs[i].Path) {
			rigs[i].Path = filepath.Join(cityPath, rigs[i].Path)
		}
	}
}

// ── Low-level provider operations ────────────────────────────────────────
//
// These are the building blocks. Prefer the consolidated functions above
// for new call sites. These remain exported for tests that need to verify
// individual operations.

// ensureBeadsProvider starts the bead store's backing service if needed.
// For exec providers, fires "start". For file providers, always available.
// Acquires a per-city semaphore to prevent concurrent start operations
// from causing spawn storms.
func ensureBeadsProvider(cityPath string) error {
	if cityUsesBdStoreContract(cityPath) && gcDoltSkip() {
		return nil
	}
	if cityUsesDoltliteBeadsBackend(cityPath) {
		return nil
	}
	provider := beadsProvider(cityPath)
	if strings.HasPrefix(provider, "exec:") {
		release, err := acquireProviderSemaphoreForOp(cityPath, "start")
		if err != nil {
			return err
		}
		defer release()

		script := strings.TrimPrefix(provider, "exec:")
		managedBDProvider := samePath(script, gcBeadsBdScriptPath(cityPath))
		if managedBDProvider {
			if err := standaloneBdDoltConflictIfPresent(cityPath); err != nil {
				return err
			}
		}
		providerEnv, envErr := providerLifecycleProcessEnvWithError(cityPath, provider)
		if envErr != nil {
			return envErr
		}
		if err := runProviderOpWithEnv(script, providerEnv, "start"); err != nil {
			// Managed bd startup occasionally reports a start error even though
			// the Dolt server is already live. If the follow-up health probe
			// succeeds, prefer the actual server state over the start error.
			if managedBDProvider {
				if healthErr := runProviderOpWithEnv(script, providerEnv, "health"); healthErr == nil {
					if err := publishManagedDoltRuntimeStateIfOwned(cityPath); err != nil {
						return err
					}
					return nil
				}
			}
			return err
		}
		if err := publishManagedDoltRuntimeStateIfOwned(cityPath); err != nil {
			return err
		}
	}
	return nil
}

// shutdownBeadsProvider stops the bead store's backing service.
// Called by gc stop after agents have been terminated.
// For exec providers, fires "stop". For file providers, always available.
func shutdownBeadsProvider(cityPath string) error {
	if cityUsesBdStoreContract(cityPath) && gcDoltSkip() {
		return clearManagedDoltRuntimeStateUnlessPostgres(cityPath)
	}
	if cityUsesDoltliteBeadsBackend(cityPath) {
		return clearManagedDoltRuntimeStateUnlessPostgres(cityPath)
	}
	provider := beadsProvider(cityPath)
	if strings.HasPrefix(provider, "exec:") {
		if providerUsesBdStoreContract(provider) {
			owned, err := managedDoltLifecycleOwned(cityPath)
			if err != nil {
				return err
			}
			if !owned {
				return clearManagedDoltRuntimeStateUnlessPostgres(cityPath)
			}
		}
		script := strings.TrimPrefix(provider, "exec:")
		providerEnv, err := providerLifecycleProcessEnvWithError(cityPath, provider)
		if err != nil {
			return err
		}
		if err := runProviderOpWithEnv(script, providerEnv, "stop"); err != nil {
			return err
		}
		if err := clearManagedDoltRuntimeStateIfOwned(cityPath); err != nil {
			return err
		}
	}
	return nil
}

// initBeadsForDir initializes bead store infrastructure in a directory.
// Idempotent — skips if already initialized. Callers should use
// initAndHookDir instead to ensure hooks are installed afterward.
//
// Every load-bearing exec path that invokes bd init locally ensures
// BEADS_DIR=<dir>/.beads. bd init creates a .git/ as a side effect when
// BEADS_DIR is unset (upstream gastownhall/beads cmd/bd/init.go), so generic
// exec providers get the scope's bead directory in the subprocess env and
// providers that run bd init elsewhere (for example gc-beads-k8s inside the
// pod) must set it in their own wrapper before invoking bd init.
func initBeadsForDir(cityPath, dir, prefix, doltDatabase string) error {
	if cityUsesBdStoreContract(cityPath) && gcDoltSkip() {
		if err := seedDeferredManagedBeadsErr(cityPath, dir, prefix, doltDatabase); err != nil {
			return err
		}
		return nil
	}
	provider := beadsProvider(cityPath)
	if provider == "file" {
		return initFileStoreForDir(cityPath, dir)
	}
	if strings.HasPrefix(provider, "exec:") {
		args := []string{"init", dir, prefix}
		if strings.TrimSpace(doltDatabase) != "" {
			args = append(args, doltDatabase)
		}
		script := strings.TrimPrefix(provider, "exec:")
		if execProviderUsesCanonicalBdScopeFiles(provider) && cityUsesDoltliteBeadsBackend(cityPath) {
			env, err := providerLifecycleProcessEnvWithError(cityPath, provider)
			if err != nil {
				return err
			}
			if err := runProviderOpWithEnv(script, env, args...); err != nil {
				if isBdAlreadyInitializedError(err) {
					return nil
				}
				return err
			}
			return nil
		}
		if execProviderUsesCanonicalBdScopeFiles(provider) && !execProviderNeedsScopedDoltInit(provider) {
			baseEnv, err := providerLifecycleProcessEnvForScopeInitWithError(cityPath, dir, provider)
			if err != nil {
				return err
			}
			overrides := map[string]string{
				"BEADS_DIR": filepath.Join(dir, ".beads"),
			}
			canonicalDoltDatabase := strings.TrimSpace(doltDatabase)
			if canonicalDoltDatabase == "" {
				canonicalDoltDatabase = canonicalScopeDoltDatabase(cityPath, dir, prefix)
			}
			if strings.TrimSpace(canonicalDoltDatabase) != "" {
				args = []string{"init", dir, prefix, canonicalDoltDatabase}
			}
			if strings.TrimSpace(cityPath) != "" {
				overrides["GC_PACK_STATE_DIR"] = citylayout.PackStateDir(cityPath, "dolt")
				if err := applyCanonicalScopeInitDoltEnv(overrides, cityPath, dir); err != nil {
					return err
				}
			}
			env := overlayEnvEntries(baseEnv, overrides)
			if err := runProviderOpWithEnv(script, env, args...); err != nil {
				if isBdAlreadyInitializedError(err) {
					return finalizeCanonicalBdScopeInit(cityPath, dir, prefix, canonicalDoltDatabase)
				}
				if shouldRetryExecBdInit(err) {
					for attempt := 0; attempt < 3; attempt++ {
						time.Sleep(time.Second)
						retryErr := runProviderOpWithEnv(script, env, args...)
						if retryErr == nil {
							return finalizeCanonicalBdScopeInit(cityPath, dir, prefix, canonicalDoltDatabase)
						}
						if !shouldRetryExecBdInit(retryErr) {
							return retryErr
						}
						err = retryErr
					}
				}
				return err
			}
			return finalizeCanonicalBdScopeInit(cityPath, dir, prefix, canonicalDoltDatabase)
		}
		if !execProviderNeedsScopedDoltInit(provider) {
			baseEnv, err := cityRuntimeProcessEnvWithError(cityPath)
			if err != nil {
				return err
			}
			if strings.TrimSpace(cityPath) == "" {
				baseEnv = os.Environ()
			}
			env := overlayEnvEntries(baseEnv, map[string]string{
				"BEADS_DIR": filepath.Join(dir, ".beads"),
			})
			if err := runProviderOpWithEnv(script, env, args...); err != nil {
				if shouldRetryExecBdInit(err) {
					for attempt := 0; attempt < 3; attempt++ {
						time.Sleep(time.Second)
						retryErr := runProviderOpWithEnv(script, env, args...)
						if retryErr == nil {
							return nil
						}
						if !shouldRetryExecBdInit(retryErr) {
							return retryErr
						}
						err = retryErr
					}
				}
				return err
			}
			return nil
		}
		target, err := resolveConfiguredExecStoreTarget(cityPath, dir)
		if err != nil {
			return err
		}
		providerEnv, err := gcExecLifecycleInitProcessEnv(cityPath, target, provider)
		if err != nil {
			return err
		}
		return runProviderOpWithEnv(script, providerEnv, args...)
	}
	if shouldInitDefaultRigBdStore(cityPath, dir, provider) {
		return initDefaultRigBdStore(cityPath, dir, prefix, doltDatabase)
	}
	return nil
}

func shouldInitDefaultRigBdStore(cityPath, dir, provider string) bool {
	if strings.TrimSpace(cityPath) == "" || strings.TrimSpace(dir) == "" {
		return false
	}
	if samePath(resolveStoreScopeRoot(cityPath, dir), resolveStoreScopeRoot(cityPath, cityPath)) {
		return false
	}
	provider = strings.TrimSpace(provider)
	return provider != "" && provider != "file" && !strings.HasPrefix(provider, "exec:") && !providerUsesBdStoreContract(provider)
}

func initDefaultRigBdStore(cityPath, dir, prefix, doltDatabase string) error {
	canonicalDoltDatabase := strings.TrimSpace(doltDatabase)
	if canonicalDoltDatabase == "" {
		canonicalDoltDatabase = canonicalScopeDoltDatabase(cityPath, dir, prefix)
	}
	env := map[string]string{
		"BEADS_DIR": filepath.Join(dir, ".beads"),
	}
	applyExportSuppressionEnv(env)
	args := []string{"init", "--server", "-p", prefix, "--skip-hooks"}
	if canonicalDoltDatabase != "" {
		args = append(args, "--database", canonicalDoltDatabase)
	}
	if _, err := beads.ExecCommandRunnerWithEnv(env)(dir, "bd", args...); err != nil {
		if isBdAlreadyInitializedError(err) {
			return finalizeCanonicalBdScopeInit(cityPath, dir, prefix, canonicalDoltDatabase)
		}
		return fmt.Errorf("bd init: %w", err)
	}
	return finalizeCanonicalBdScopeInit(cityPath, dir, prefix, canonicalDoltDatabase)
}

func finalizeCanonicalBdScopeInit(cityPath, dir, prefix, doltDatabase string) error {
	if state, ok, err := forcedScopeDoltConfigStateForInit(cityPath, dir, prefix); err != nil {
		return err
	} else if ok {
		if err := ensureCanonicalScopeConfigState(fsys.OSFS{}, dir, state); err != nil {
			return err
		}
	}
	if strings.TrimSpace(doltDatabase) == "" {
		doltDatabase = defaultScopeDoltDatabase(cityPath, dir, prefix)
	}
	if isReservedManagedDoltDatabase(doltDatabase) {
		if err := ensureCanonicalScopeMetadataForInit(fsys.OSFS{}, dir, doltDatabase); err != nil {
			return err
		}
	} else if err := enforceCanonicalScopeMetadataForInit(fsys.OSFS{}, dir, doltDatabase); err != nil {
		return err
	}
	store, err := openStoreAtForCity(dir, cityPath)
	if err != nil {
		return err
	}
	return verifyCanonicalBdScopeStoreReady(store)
}

func verifyCanonicalBdScopeStoreReady(store beads.Store) error {
	var lastErr error
	for attempt := 0; attempt < 20; attempt++ {
		_, err := store.List(beads.ListQuery{AllowScan: true, Limit: 1})
		if err == nil {
			return nil
		}
		lastErr = err
		time.Sleep(500 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("store verification failed")
	}
	return lastErr
}

//nolint:unparam // error slot preserves the resolver-shaped contract
func forcedScopeDoltConfigStateForInit(cityPath, dir, prefix string) (contract.ConfigState, bool, error) {
	if strings.TrimSpace(dir) == "" || strings.TrimSpace(prefix) == "" {
		return contract.ConfigState{}, false, nil
	}
	cityPath = normalizePathForCompare(cityPath)
	cityDolt := config.DoltConfig{}
	if cfg, err := loadCityConfig(cityPath, io.Discard); err == nil {
		resolveRigPaths(cityPath, cfg.Rigs)
		cityState := desiredCityDoltConfigState(cityPath, cfg.Dolt, config.EffectiveHQPrefix(cfg))
		if samePath(cityPath, dir) {
			cityState.IssuePrefix = prefix
			return cityState, true, nil
		}
		for i := range cfg.Rigs {
			if samePath(cfg.Rigs[i].Path, dir) {
				rig := cfg.Rigs[i]
				rig.Prefix = prefix
				return desiredRigDoltConfigState(cityPath, rig, cityState), true, nil
			}
		}
		return desiredRigDoltConfigState(cityPath, config.Rig{Name: filepath.Base(dir), Path: dir, Prefix: prefix}, cityState), true, nil
	}
	if loaded, ok := cityDoltConfigs.Load(cityPath); ok {
		if cfg, ok := loaded.(config.DoltConfig); ok {
			cityDolt = cfg
		}
	}
	cityState := desiredCityDoltConfigState(cityPath, cityDolt, prefix)
	if samePath(cityPath, dir) {
		return cityState, true, nil
	}
	return desiredRigDoltConfigState(cityPath, config.Rig{Name: filepath.Base(dir), Path: dir, Prefix: prefix}, cityState), true, nil
}

func initFileStoreForDir(cityPath, dir string) error {
	if !fileStoreUsesScopedRoots(cityPath) {
		return nil
	}
	return ensurePersistedScopeLocalFileStore(dir)
}

// healthBeadsProvider checks the bead store's backing service health.
// For exec providers, fires the "health" operation. For bd (dolt), runs
// a three-layer health check and attempts recovery on failure. For file
// provider, always healthy (no-op).
//
// Acquires a per-city semaphore to prevent concurrent health/recovery
// operations from causing a thundering herd when dolt bounces.
func healthBeadsProvider(cityPath string) error {
	if cityUsesBdStoreContract(cityPath) && gcDoltSkip() {
		return nil
	}
	if cityUsesDoltliteBeadsBackend(cityPath) {
		return nil
	}
	provider := beadsProvider(cityPath)
	if strings.HasPrefix(provider, "exec:") {
		release, err := acquireProviderSemaphoreForOp(cityPath, "health")
		if err != nil {
			return err
		}
		defer release()

		script := strings.TrimPrefix(provider, "exec:")
		providerEnv, err := providerLifecycleProcessEnvWithError(cityPath, provider)
		if err != nil {
			return err
		}
		if err := runProviderOpWithEnv(script, providerEnv, "health"); err != nil {
			if providerUsesBdStoreContract(provider) {
				owned, ownershipErr := managedDoltLifecycleOwned(cityPath)
				if ownershipErr != nil {
					return fmt.Errorf("determine managed dolt ownership: %w", ownershipErr)
				}
				if !owned {
					return err
				}
				// Breaker-aware preflight: if the bd circuit breaker is
				// open, a recovery is already in flight (#2533 clears the
				// breaker on kill). Skip recover here so the next restart
				// doesn't re-trip the breaker and re-desync the PID.
				if isBreakerOpenError(err) {
					return err
				}
				// Recover backoff: refuse a 2nd recover within
				// providerRecoverCooldown of the prior one, keyed per
				// city. This alone breaks the low-RSS restart-loop where
				// each tick (~60-110s apart) starts a fresh recover.
				cityKey := normalizePathForCompare(cityPath)
				now := providerRecoverNow()
				if v, loaded := lastBeadsProviderRecover.Load(cityKey); loaded {
					if last, ok := v.(time.Time); ok && now.Sub(last) < providerRecoverCooldown() {
						return err
					}
				}
				lastBeadsProviderRecover.Store(cityKey, now)
			}
			if recErr := runProviderOpWithEnv(script, providerEnv, "recover"); recErr != nil {
				return fmt.Errorf("unhealthy (%w) and recovery failed: %w", err, recErr)
			}
			if pubErr := publishManagedDoltRuntimeStateIfOwned(cityPath); pubErr != nil {
				return fmt.Errorf("recovered but failed to publish managed dolt runtime state: %w", pubErr)
			}
			if waitErr := waitForAllBeadsScopesReadyAfterRecovery(cityPath, 10*time.Second); waitErr != nil {
				return fmt.Errorf("recovered but store not ready: %w", waitErr)
			}
		} else if providerUsesBdStoreContract(provider) && currentManagedDoltPort(cityPath) == "" {
			owned, ownershipErr := managedDoltLifecycleOwned(cityPath)
			if ownershipErr != nil {
				return fmt.Errorf("determine managed dolt ownership: %w", ownershipErr)
			}
			if !owned {
				return nil
			}
			if pubErr := publishManagedDoltRuntimeStateIfOwned(cityPath); pubErr != nil {
				return fmt.Errorf("healthy but failed to publish managed dolt runtime state: %w", pubErr)
			}
			if waitErr := waitForAllBeadsScopesReadyAfterRecovery(cityPath, 10*time.Second); waitErr != nil {
				return fmt.Errorf("healthy but store not ready after publishing managed dolt runtime state: %w", waitErr)
			}
		}
		return nil
	}
	return nil // file: always healthy
}

func waitForAllBeadsScopesReadyAfterRecovery(cityPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	if err := waitForBeadsScopeReadyAfterRecovery(cityPath, cityPath, deadline); err != nil {
		return err
	}
	// Use the full config load (site-binding overlay applied) so
	// migrated rigs (rig.path only in .gc/site.toml) are still waited
	// for. A raw config.Load here would silently skip every migrated
	// rig — the site binding wouldn't populate rig.Path.
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		return nil
	}
	resolveRigPaths(cityPath, cfg.Rigs)
	for _, rig := range cfg.Rigs {
		if strings.TrimSpace(rig.Path) == "" {
			continue
		}
		if err := waitForBeadsScopeReadyAfterRecovery(resolveStoreScopeRoot(cityPath, rig.Path), cityPath, deadline); err != nil {
			return fmt.Errorf("rig %q store not ready: %w", rig.Name, err)
		}
	}
	return nil
}

func waitForBeadsScopeReadyAfterRecovery(scopeRoot, cityPath string, deadline time.Time) error {
	var lastErr error
	for {
		store, err := openStoreAtForCity(scopeRoot, cityPath)
		if err == nil {
			pingErr := store.Ping()
			if pingErr == nil {
				return nil
			}
			lastErr = pingErr
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			if lastErr == nil {
				lastErr = fmt.Errorf("timed out waiting for beads store readiness")
			}
			return lastErr
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// isExternalDolt returns true when the city uses an explicitly configured
// (user-managed) Dolt server rather than the managed local one.
//
// Checks canonical city .beads config first, then falls back to deprecated
// city.toml-derived registration only when the canonical file does not exist.
// Env vars remain explicit per-process overrides for non-controller paths.
// With canonical or compat config, any explicit host or port means
// "user-managed" regardless of whether the host resolves to localhost.
// Without config, the env-var fallback excludes localhost addresses for
// backwards compatibility.
func isExternalDolt(cityPath string) bool {
	target, ok, err := resolvedRuntimeCityDoltTarget(cityPath, false)
	return err == nil && ok && target.External
}

// doltHostForCity returns the effective Dolt host for a city.
// Canonical or compat-configured targets win over ambient env so child
// processes stay aligned with the resolved city endpoint. Env-only host
// overrides remain a last-resort fallback when no configured target exists.
func doltHostForCity(cityPath string) string {
	target, ok, err := resolvedRuntimeCityDoltTarget(cityPath, false)
	if err != nil || !ok || !target.External {
		return ""
	}
	return target.Host
}

// doltPortForCity returns the effective Dolt port for a city.
// Canonical or compat-configured targets win over ambient env so child
// processes stay aligned with the resolved city endpoint. Env-only port
// overrides remain a last-resort fallback when no configured target exists.
func doltPortForCity(cityPath string) string {
	target, ok, err := resolvedRuntimeCityDoltTarget(cityPath, false)
	if err != nil || !ok || !target.External {
		return ""
	}
	return target.Port
}

func configuredCityDoltTarget(cityPath string) (string, string, bool) {
	host, port, ok, _ := resolveConfiguredCityDoltTarget(cityPath)
	return host, port, ok
}

func resolveConfiguredCityDoltTarget(cityPath string) (string, string, bool, bool) {
	cityPath = normalizePathForCompare(cityPath)
	resolved, err := contract.ResolveScopeConfigState(fsys.OSFS{}, cityPath, cityPath, "")
	if err != nil {
		var invalid *contract.InvalidCanonicalConfigError
		if errors.As(err, &invalid) {
			return "", "", false, true
		}
		return "", "", false, false
	}
	if resolved.Kind == contract.ScopeConfigAuthoritative {
		if resolved.State.EndpointOrigin == contract.EndpointOriginCityCanonical {
			return canonicalExternalHost(resolved.State.DoltHost, resolved.State.DoltPort), strings.TrimSpace(resolved.State.DoltPort), true, false
		}
		return "", "", false, false
	}
	if resolved.Kind == contract.ScopeConfigMissing || resolved.Kind == contract.ScopeConfigLegacyMinimal {
		if v, ok := cityDoltConfigs.Load(cityPath); ok {
			dc := v.(config.DoltConfig)
			port := ""
			if dc.Port != 0 {
				port = strconv.Itoa(dc.Port)
			}
			host := canonicalExternalHost(dc.Host, port)
			if host != "" || port != "" {
				return host, port, true, false
			}
		}
	}
	return "", "", false, false
}

type doltRuntimeState struct {
	Running   bool   `json:"running"`
	PID       int    `json:"pid"`
	Port      int    `json:"port"`
	DataDir   string `json:"data_dir"`
	StartedAt string `json:"started_at"`
}

// currentDoltPort returns the controller-managed Dolt port for the city.
// Published runtime state is preferred; valid provider state is accepted while
// publication catches up so the raw-bd compatibility mirror does not get
// removed during a live managed-Dolt window.
// .beads/dolt-server.port is a compatibility mirror for raw bd, not a GC
// control-plane input.
func currentDoltPort(cityPath string) string {
	if port := currentResolvableManagedDoltPort(cityPath); port != "" {
		writeDoltPortFile(cityPath, port, "", io.Discard)
		return port
	}
	removeDoltPortFile(cityPath)
	return ""
}

func managedDoltStatePath(cityPath string) string {
	return filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt", "dolt-state.json")
}

func currentManagedDoltPort(cityPath string) string {
	owned, err := managedDoltLifecycleOwned(cityPath)
	if err != nil {
		log.Printf("gc: managed dolt ownership probe failed for %s: %v", cityPath, err)
		return ""
	}
	if !owned {
		return ""
	}
	data, err := os.ReadFile(managedDoltStatePath(cityPath))
	if err != nil {
		return ""
	}
	var state doltRuntimeState
	if json.Unmarshal(data, &state) != nil {
		return ""
	}
	if !validDoltRuntimeState(state, cityPath) {
		return ""
	}
	return strconv.Itoa(state.Port)
}

func validDoltRuntimeState(state doltRuntimeState, cityPath string) bool {
	if !state.Running || state.Port <= 0 || state.PID <= 0 {
		return false
	}
	expectedDataDir := filepath.Join(cityPath, ".beads", "dolt")
	if !samePath(strings.TrimSpace(state.DataDir), expectedDataDir) {
		return false
	}
	if !pidAlive(state.PID) {
		return false
	}
	if !doltPortReachable(strconv.Itoa(state.Port)) {
		return false
	}
	holderPID := findPortHolderPID(strconv.Itoa(state.Port))
	if holderPID > 0 && holderPID != state.PID {
		return false
	}
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		return false
	}
	owned, deleted := inspectManagedDoltOwnership(state.PID, layout)
	if deleted {
		return false
	}
	if holderPID == state.PID {
		return true
	}
	return owned
}

func pidAlive(pid int) bool {
	return pidutil.Alive(pid)
}

func doltPortReachable(port string) bool {
	if strings.TrimSpace(port) == "" {
		return false
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", port), 250*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// writeDoltPortFile writes the managed Dolt port into dir/.beads/dolt-server.port.
// When the existing file contains a non-empty port different from the one being
// written, a WARN line naming scopeLabel and both ports is emitted on warn so
// operators can see that their on-disk port file is being reconciled to the
// canonical managed port. scopeLabel may be empty for silent callers; warn may
// be nil or io.Discard to suppress warnings entirely.
func writeDoltPortFile(dir, port, scopeLabel string, warn io.Writer) {
	if dir == "" || port == "" {
		return
	}
	trimmedPort := strings.TrimSpace(port)
	if trimmedPort == "" {
		return
	}
	portFile := filepath.Join(dir, ".beads", "dolt-server.port")
	existing := ""
	if data, err := os.ReadFile(portFile); err == nil {
		existing = strings.TrimSpace(string(data))
		if existing == trimmedPort {
			return
		}
	}
	if warn != nil && existing != "" && existing != trimmedPort {
		label := strings.TrimSpace(scopeLabel)
		if label == "" {
			label = dir
		}
		fmt.Fprintf(warn, "WARN: %s .beads/dolt-server.port rewrite %s → %s (managed city port)\n", label, existing, trimmedPort) //nolint:errcheck // best-effort stderr
	}
	writePath, err := resolveDoltPortFileWritePath(fsys.OSFS{}, portFile)
	if err != nil {
		return
	}
	if err := ensureBeadsDir(fsys.OSFS{}, filepath.Dir(writePath)); err != nil {
		return
	}
	_ = fsys.WriteFileAtomic(fsys.OSFS{}, writePath, []byte(trimmedPort+"\n"), 0o644)
}

func removeDoltPortFile(dir string) {
	if dir == "" {
		return
	}
	// Resolve through any operator symlink so cleanup clears the target and
	// preserves the link, mirroring writeDoltPortFile's symlink-preserving
	// write path (ga-lurp5d). Best-effort: ignore the resolve/remove error.
	_ = removeResolvedDoltPortFile(fsys.OSFS{}, dir)
}

func removeScopeLocalDoltServerArtifacts(dir string) error {
	if dir == "" {
		return nil
	}
	for _, name := range []string{
		"dolt-server.pid",
		"dolt-server.lock",
		"dolt-server.log",
		"dolt-server.port",
	} {
		if err := os.Remove(filepath.Join(dir, ".beads", name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func validateManagedDoltDatabaseName(path, doltDatabase string) (string, error) {
	doltDatabase = strings.TrimSpace(doltDatabase)
	if doltDatabase == "" {
		return "", fmt.Errorf("missing pinned dolt_database for %s", path)
	}
	if isReservedManagedDoltDatabase(doltDatabase) {
		return "", fmt.Errorf("reserved pinned dolt_database %q for %s: used internally by managed Dolt health probes; choose a different dolt_database in metadata.json and rename or move the bead database before retrying", doltDatabase, path)
	}
	return doltDatabase, nil
}

func isLegacyManagedDoltProbeDatabase(name string) bool {
	return strings.EqualFold(strings.TrimSpace(name), managedDoltProbeDatabase)
}

func ensureCanonicalScopeMetadata(fs fsys.FS, scopeRoot, doltDatabase string, preserveExisting bool) error {
	path := filepath.Join(scopeRoot, ".beads", "metadata.json")
	preserveReservedExisting := false
	if preserveExisting {
		if existing, ok, err := contract.LoadMetadataState(fs, path); err != nil {
			if !allowLegacyDoltMetadataRepair(fs, path, err) {
				return err
			}
		} else if ok && existing.Backend == "postgres" {
			return nil
		}
		if existing, ok, err := contract.ReadDoltDatabase(fs, path); err != nil {
			return err
		} else if ok && strings.TrimSpace(existing) != "" {
			doltDatabase = strings.TrimSpace(existing)
			if isReservedManagedDoltDatabase(doltDatabase) {
				// New init paths reject this reserved name, but existing metadata
				// may use the legacy probe database as its real bead store.
				// Preserve only that one migration case; Dolt system databases
				// are unsafe bead-store targets even when already pinned.
				preserveReservedExisting = isLegacyManagedDoltProbeDatabase(doltDatabase)
			}
		}
	}
	var err error
	if !preserveReservedExisting {
		if doltDatabase, err = validateManagedDoltDatabaseName(path, doltDatabase); err != nil {
			return err
		}
	}
	if err := ensureBeadsDir(fs, filepath.Dir(path)); err != nil {
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

func ensureCanonicalDoltliteScopeMetadata(fs fsys.FS, scopeRoot, doltDatabase string, preserveExisting bool) error {
	path := filepath.Join(scopeRoot, ".beads", "metadata.json")
	if preserveExisting {
		if existing, ok, err := contract.LoadMetadataState(fs, path); err != nil {
			if !allowLegacyDoltMetadataRepair(fs, path, err) {
				return err
			}
		} else if ok && existing.Backend == "postgres" {
			return nil
		}
		if existing, ok, err := contract.ReadDoltDatabase(fs, path); err != nil {
			return err
		} else if ok && strings.TrimSpace(existing) != "" {
			doltDatabase = strings.TrimSpace(existing)
		}
	}
	if err := ensureBeadsDir(fs, filepath.Dir(path)); err != nil {
		return err
	}
	_, err := contract.EnsureCanonicalMetadata(fs, path, contract.MetadataState{
		Database:     "doltlite",
		Backend:      "doltlite",
		DoltDatabase: doltDatabase,
	})
	return err
}

//nolint:unparam // keep fs seam for future testable FS injection
func ensureCanonicalScopeMetadataForInit(fs fsys.FS, scopeRoot, doltDatabase string) error {
	return ensureCanonicalScopeMetadata(fs, scopeRoot, doltDatabase, true)
}

//nolint:unparam // keep fs seam for future testable FS injection
func ensureCanonicalDoltliteScopeMetadataForInit(fs fsys.FS, scopeRoot, doltDatabase string) error {
	return ensureCanonicalDoltliteScopeMetadata(fs, scopeRoot, doltDatabase, true)
}

//nolint:unparam // keep fs seam for future testable FS injection
func enforceCanonicalScopeMetadataForInit(fs fsys.FS, scopeRoot, doltDatabase string) error {
	return ensureCanonicalScopeMetadata(fs, scopeRoot, doltDatabase, false)
}

// normalizeCanonicalBdScopeFiles reconciles canonical bd metadata/config/port
// mirrors under the city and each rig. warn receives operator-visible WARN
// lines when port-file rewrites change on-disk contents (pass io.Discard to
// suppress, or a stderr writer from the caller to show them). When omitted,
// warning output is suppressed.
func normalizeCanonicalBdScopeFiles(cityPath string, cfg *config.City, warns ...io.Writer) error {
	if cfg == nil {
		return nil
	}
	var warn io.Writer
	if len(warns) > 0 {
		warn = warns[0]
	}
	if warn == nil {
		warn = io.Discard
	}
	resolveRigPaths(cityPath, cfg.Rigs)
	if scopeUsesManagedBdStoreContract(cityPath, cityPath) {
		if usesPostgres, err := scopeUsesPostgresBackendForInit(cityPath, cityPath); err != nil {
			return fmt.Errorf("classifying city backend: %w", err)
		} else if !usesPostgres {
			doltDatabase := defaultScopeDoltDatabase(cityPath, cityPath, config.EffectiveHQPrefix(cfg))
			if cityUsesDoltliteBeadsBackend(cityPath) {
				if err := ensureCanonicalDoltliteScopeMetadataForInit(fsys.OSFS{}, cityPath, doltDatabase); err != nil {
					return fmt.Errorf("canonicalizing city doltlite metadata: %w", err)
				}
			} else if err := ensureCanonicalScopeMetadataForInit(fsys.OSFS{}, cityPath, doltDatabase); err != nil {
				return fmt.Errorf("canonicalizing city metadata: %w", err)
			}
		}
	}
	for i := range cfg.Rigs {
		if !rigUsesManagedBdStoreContract(cityPath, cfg.Rigs[i]) {
			continue
		}
		if usesPostgres, err := scopeUsesPostgresBackendForInit(cityPath, cfg.Rigs[i].Path); err != nil {
			return fmt.Errorf("classifying rig %q backend: %w", cfg.Rigs[i].Name, err)
		} else if !usesPostgres {
			doltDatabase := defaultScopeDoltDatabase(cityPath, cfg.Rigs[i].Path, cfg.Rigs[i].EffectivePrefix())
			if cityUsesDoltliteBeadsBackend(cityPath) {
				if err := ensureCanonicalDoltliteScopeMetadataForInit(fsys.OSFS{}, cfg.Rigs[i].Path, doltDatabase); err != nil {
					return fmt.Errorf("canonicalizing rig %q doltlite metadata: %w", cfg.Rigs[i].Name, err)
				}
			} else if err := ensureCanonicalScopeMetadataForInit(fsys.OSFS{}, cfg.Rigs[i].Path, doltDatabase); err != nil {
				return fmt.Errorf("canonicalizing rig %q metadata: %w", cfg.Rigs[i].Name, err)
			}
		}
	}
	if err := syncConfiguredDoltPortFiles(cityPath, cfg.Dolt, config.EffectiveHQPrefix(cfg), cfg.Rigs, warn); err != nil {
		return fmt.Errorf("syncing canonical dolt config: %w", err)
	}
	return nil
}

// syncConfiguredDoltPortFiles reconciles each scope's .beads/dolt-server.port
// compatibility mirror with the canonical managed-city Dolt port. When warn is
// non-nil, a WARN line is emitted for every port file whose prior non-empty
// contents disagreed with the canonical port (operator-visible signal that gc
// is overriding a rig-local or stale port). Pass io.Discard to suppress.
func syncConfiguredDoltPortFiles(cityPath string, cityDolt config.DoltConfig, cityPrefix string, rigs []config.Rig, warn io.Writer) error {
	if warn == nil {
		warn = io.Discard
	}
	resolveRigPaths(cityPath, rigs)
	cityUsesBd := scopeUsesManagedBdStoreContract(cityPath, cityPath)
	cityUsesPostgres := false
	if cityUsesBd {
		usesPostgres, err := scopeUsesPostgresBackendForInit(cityPath, cityPath)
		if err != nil {
			return fmt.Errorf("classifying city backend: %w", err)
		}
		cityUsesPostgres = usesPostgres
	}
	anyRigUsesBd := false
	for _, rig := range rigs {
		if rigUsesManagedBdStoreContract(cityPath, rig) {
			anyRigUsesBd = true
			break
		}
	}
	if !cityUsesBd && !anyRigUsesBd {
		return nil
	}
	// .beads/config.yaml is a bd compatibility mirror, not the canonical
	// source of routing identity. GC owns reconciliation of the mirrored
	// prefix and endpoint shape from city.toml plus runtime publication.
	// .beads/dolt-server.port remains a managed-local compatibility artifact
	// only. External scopes must resolve from canonical config, not a loopback
	// port file that older callers may misinterpret as local ownership.
	cityState, err := syncDesiredCityDoltConfigState(cityPath, cityDolt, cityPrefix)
	if err != nil {
		return err
	}
	managedPort := ""
	if cityState.EndpointOrigin == contract.EndpointOriginManagedCity && !cityUsesPostgres {
		managedPort = currentDoltPort(cityPath)
	}
	if cityUsesBd {
		if err := normalizeScopeDoltConfig(cityPath, cityState); err != nil {
			return err
		}
		if !cityUsesPostgres {
			if managedPort != "" {
				writeDoltPortFile(cityPath, managedPort, "city", warn)
			} else {
				removeDoltPortFile(cityPath)
			}
		}
	} else {
		removeDoltPortFile(cityPath)
	}

	for i := range rigs {
		rig := normalizedRigConfig(cityPath, rigs[i])
		if strings.TrimSpace(rig.Path) == "" {
			continue
		}
		if !rigUsesManagedBdStoreContract(cityPath, rig) {
			removeDoltPortFile(rig.Path)
			continue
		}
		rigState, err := syncDesiredRigDoltConfigState(cityPath, rig, cityState)
		if err != nil {
			return err
		}
		rigManagedPort := ""
		if cityState.EndpointOrigin == contract.EndpointOriginManagedCity && rigState.EndpointOrigin == contract.EndpointOriginInheritedCity {
			rigManagedPort = managedPort
		}
		if err := normalizeScopeDoltConfig(rig.Path, rigState); err != nil {
			return err
		}
		if rigManagedPort != "" {
			writeDoltPortFile(rig.Path, rigManagedPort, "rig "+rig.Name, warn)
		} else {
			removeDoltPortFile(rig.Path)
		}
	}
	return nil
}

func syncDesiredCityDoltConfigState(cityPath string, cityDolt config.DoltConfig, cityPrefix string) (contract.ConfigState, error) {
	state, _, err := resolveDesiredCityEndpointState(cityPath, cityDolt, cityPrefix)
	if err != nil {
		return contract.ConfigState{}, err
	}
	return state, nil
}

func syncDesiredRigDoltConfigState(cityPath string, rig config.Rig, cityState contract.ConfigState) (contract.ConfigState, error) {
	state, err := resolveDesiredRigEndpointState(cityPath, rig, cityState)
	if err != nil {
		return contract.ConfigState{}, err
	}
	return state, nil
}

func normalizedRigConfig(cityPath string, rig config.Rig) config.Rig {
	if !filepath.IsAbs(rig.Path) {
		rig.Path = filepath.Join(cityPath, rig.Path)
	}
	return rig
}

func desiredCityDoltConfigState(cityPath string, cityDolt config.DoltConfig, cityPrefix string) contract.ConfigState {
	cityHost, cityPort := configuredExternalDoltTargetForCity(cityDolt)
	if cityHost != "" || cityPort != "" {
		state := contract.ConfigState{
			IssuePrefix:    cityPrefix,
			EndpointOrigin: contract.EndpointOriginCityCanonical,
			DoltHost:       cityHost,
			DoltPort:       cityPort,
		}
		state.DoltUser = preservedDoltUser(cityPath, state)
		state.EndpointStatus = preservedEndpointStatus(cityPath, state, contract.EndpointStatusUnverified)
		return state
	}

	return contract.ConfigState{
		IssuePrefix:    cityPrefix,
		EndpointOrigin: contract.EndpointOriginManagedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	}
}

func desiredRigDoltConfigState(cityPath string, rig config.Rig, cityState contract.ConfigState) contract.ConfigState {
	rig = normalizedRigConfig(cityPath, rig)
	if rig.DoltHost != "" || rig.DoltPort != "" {
		state := contract.ConfigState{
			IssuePrefix:    rig.EffectivePrefix(),
			EndpointOrigin: contract.EndpointOriginExplicit,
		}
		state.DoltHost, state.DoltPort = configuredExternalDoltTargetForRig(rig)
		state.DoltUser = preservedDoltUser(rig.Path, state)
		state.EndpointStatus = preservedEndpointStatus(rig.Path, state, contract.EndpointStatusUnverified)
		return state
	}

	return inheritedRigDoltConfigState(rig.Path, rig.EffectivePrefix(), cityState)
}

func inheritedRigDoltConfigState(rigPath, prefix string, cityState contract.ConfigState) contract.ConfigState {
	state := contract.ConfigState{
		IssuePrefix:    prefix,
		EndpointOrigin: contract.EndpointOriginInheritedCity,
	}
	if cityState.EndpointOrigin == contract.EndpointOriginCityCanonical {
		state.DoltHost = cityState.DoltHost
		state.DoltPort = cityState.DoltPort
		state.DoltUser = strings.TrimSpace(cityState.DoltUser)
		state.EndpointStatus = inheritedEndpointStatus(rigPath, state, cityState.EndpointStatus)
		return state
	}
	state.EndpointStatus = contract.EndpointStatusVerified
	return state
}

func wrapInvalidEndpointStateError(scope string, err error) error {
	var invalid *contract.InvalidCanonicalConfigError
	if !errors.As(err, &invalid) {
		return err
	}
	switch scope {
	case "city":
		return fmt.Errorf("invalid canonical city endpoint state in %s: %w", invalid.Path, invalid.Err)
	case "rig":
		return fmt.Errorf("invalid canonical rig endpoint state in %s: %w", invalid.Path, invalid.Err)
	default:
		return err
	}
}

func validateCanonicalCompatDoltDrift(cityPath string, cfg *config.City) error {
	if cfg == nil || !workspaceUsesManagedBdStoreContract(cityPath, cfg.Rigs) {
		return nil
	}
	cityResolved, err := contract.ResolveScopeConfigState(fsys.OSFS{}, cityPath, cityPath, config.EffectiveHQPrefix(cfg))
	if err != nil {
		return wrapInvalidEndpointStateError("city", err)
	}
	cityState := cityResolved.State
	cityCanonical := cityResolved.Kind == contract.ScopeConfigAuthoritative
	compatCityHost, compatCityPort := configuredExternalDoltTargetForCity(cfg.Dolt)
	if cityCanonical {
		switch cityState.EndpointOrigin {
		case contract.EndpointOriginManagedCity:
			if compatCityHost != "" || compatCityPort != "" {
				return fmt.Errorf("deprecated city.toml [dolt] endpoint conflicts with canonical managed city config")
			}
		case contract.EndpointOriginCityCanonical:
			if (compatCityHost != "" || compatCityPort != "") && !sameConfiguredExternalTarget(cityState.DoltHost, cityState.DoltPort, compatCityHost, compatCityPort) {
				return fmt.Errorf("deprecated city.toml [dolt] endpoint drifts from canonical city endpoint")
			}
		}
	}
	for i := range cfg.Rigs {
		rig := normalizedRigConfig(cityPath, cfg.Rigs[i])
		rigResolved, err := contract.ResolveScopeConfigState(fsys.OSFS{}, cityPath, rig.Path, rig.EffectivePrefix())
		if err != nil {
			return wrapInvalidEndpointStateError("rig", err)
		}
		rigState := rigResolved.State
		rigCanonical := rigResolved.Kind == contract.ScopeConfigAuthoritative
		if !rigCanonical {
			continue
		}
		compatRigHost, compatRigPort := configuredExternalDoltTargetForRig(cfg.Rigs[i])
		switch rigState.EndpointOrigin {
		case contract.EndpointOriginInheritedCity:
			if cityState.EndpointOrigin == contract.EndpointOriginManagedCity {
				if compatRigHost != "" || compatRigPort != "" {
					return fmt.Errorf("deprecated rig dolt_host/dolt_port conflict with inherited canonical endpoint for rig %q", cfg.Rigs[i].Name)
				}
				break
			}
			if (compatRigHost != "" || compatRigPort != "") && !sameConfiguredExternalTarget(rigState.DoltHost, rigState.DoltPort, compatRigHost, compatRigPort) {
				return fmt.Errorf("deprecated rig dolt_host/dolt_port drift from inherited canonical endpoint for rig %q", cfg.Rigs[i].Name)
			}
		case contract.EndpointOriginExplicit:
			if (compatRigHost != "" || compatRigPort != "") && !sameConfiguredExternalTarget(rigState.DoltHost, rigState.DoltPort, compatRigHost, compatRigPort) {
				return fmt.Errorf("deprecated rig dolt_host/dolt_port drift from canonical endpoint for rig %q", cfg.Rigs[i].Name)
			}
		}
	}
	return nil
}

func sameConfiguredExternalTarget(aHost, aPort, bHost, bPort string) bool {
	return canonicalExternalHost(aHost, aPort) == canonicalExternalHost(bHost, bPort) && strings.TrimSpace(aPort) == strings.TrimSpace(bPort)
}

func configuredExternalDoltTargetForCity(dc config.DoltConfig) (string, string) {
	// Canonical tracked endpoint defaults come only from persisted city config.
	// Env-only GC_DOLT_* overrides remain process-local escape hatches and must
	// not be mirrored into tracked .beads/config.yaml files.
	port := ""
	if dc.Port != 0 {
		port = strconv.Itoa(dc.Port)
	}
	return canonicalExternalHost(dc.Host, port), port
}

func configuredExternalDoltTargetForRig(rig config.Rig) (string, string) {
	port := strings.TrimSpace(rig.DoltPort)
	return canonicalExternalHost(rig.DoltHost, port), port
}

func canonicalExternalHost(host, port string) string {
	host = strings.TrimSpace(host)
	if host == "" && strings.TrimSpace(port) != "" {
		return "127.0.0.1"
	}
	return host
}

func preservedDoltUser(dir string, want contract.ConfigState) string {
	existing, ok, err := contract.ReadConfigState(fsys.OSFS{}, filepath.Join(dir, ".beads", "config.yaml"))
	if err != nil || !ok {
		return ""
	}
	if existing.EndpointOrigin == want.EndpointOrigin {
		return strings.TrimSpace(existing.DoltUser)
	}
	// During migration, preserve legacy external dolt.user when the existing
	// file still lacks gc.endpoint_origin but already points at the same
	// external endpoint we are canonicalizing.
	if existing.EndpointOrigin == "" && (strings.TrimSpace(want.DoltHost) != "" || strings.TrimSpace(want.DoltPort) != "") {
		if strings.TrimSpace(existing.DoltPort) == strings.TrimSpace(want.DoltPort) && canonicalExternalHost(existing.DoltHost, existing.DoltPort) == canonicalExternalHost(want.DoltHost, want.DoltPort) {
			return strings.TrimSpace(existing.DoltUser)
		}
	}
	return ""
}

func preservedEndpointStatus(dir string, want contract.ConfigState, fallback contract.EndpointStatus) contract.EndpointStatus {
	existing, ok, err := contract.ReadConfigState(fsys.OSFS{}, filepath.Join(dir, ".beads", "config.yaml"))
	if err != nil || !ok {
		return fallback
	}
	if existing.EndpointOrigin != want.EndpointOrigin {
		return fallback
	}
	if strings.TrimSpace(existing.DoltHost) != strings.TrimSpace(want.DoltHost) {
		return fallback
	}
	if strings.TrimSpace(existing.DoltPort) != strings.TrimSpace(want.DoltPort) {
		return fallback
	}
	if strings.TrimSpace(existing.DoltUser) != strings.TrimSpace(want.DoltUser) {
		return fallback
	}
	if existing.EndpointStatus == contract.EndpointStatusVerified {
		return contract.EndpointStatusVerified
	}
	return fallback
}

func inheritedEndpointStatus(_ string, _ contract.ConfigState, inherited contract.EndpointStatus) contract.EndpointStatus {
	// Inherited rigs do not own independent endpoint verification state.
	// Their canonical endpoint status is the city endpoint status, even when
	// the local mirrored host/port/user fields need to be normalized.
	return inherited
}

func normalizeScopeDoltConfig(dir string, state contract.ConfigState) error {
	return ensureCanonicalScopeConfigState(fsys.OSFS{}, dir, state)
}

// runProviderProbe runs a "probe" operation against an exec beads script.
// Returns true if the backing service is available (exit 0), false if not
// available (exit 2) or on any error. Unlike runProviderOp, exit 2 means
// "not running" rather than "not needed."
func runProviderProbe(script, cityPath, provider string) bool {
	ctx, cancel := providerLifecycleContext(context.Background(), providerProbeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, script, "probe")
	cmd.WaitDelay = 2 * time.Second
	prepareProviderOpCommand(cmd)
	if cityPath != "" {
		env, err := providerLifecycleProcessEnvWithError(cityPath, provider)
		if err != nil {
			return false
		}
		cmd.Env = env
	}
	return cmd.Run() == nil
}

func providerLifecycleDoltPathEnv(cityPath string) []string {
	cityPath = normalizePathForCompare(cityPath)
	packStateDir := citylayout.PackStateDir(cityPath, "dolt")
	dataDir := filepath.Join(cityPath, ".beads", "dolt")
	return []string{
		"GC_PACK_STATE_DIR=" + packStateDir,
		"GC_DOLT_DATA_DIR=" + dataDir,
		"GC_DOLT_LOG_FILE=" + filepath.Join(packStateDir, "dolt.log"),
		"GC_DOLT_STATE_FILE=" + filepath.Join(packStateDir, "dolt-provider-state.json"),
		"GC_DOLT_PID_FILE=" + filepath.Join(packStateDir, "dolt.pid"),
		"GC_DOLT_LOCK_FILE=" + filepath.Join(packStateDir, "dolt.lock"),
		"GC_DOLT_CONFIG_FILE=" + filepath.Join(packStateDir, "dolt-config.yaml"),
	}
}

func providerLifecycleProcessEnvWithError(cityPath, provider string) ([]string, error) {
	if strings.TrimSpace(cityPath) == "" {
		return nil, nil
	}
	cityPath = normalizePathForCompare(cityPath)
	env, err := cityRuntimeProcessEnvWithError(cityPath)
	if err != nil {
		return nil, err
	}
	return providerLifecycleProcessEnvFromBase(cityPath, provider, env), nil
}

func providerLifecycleProcessEnvForScopeInitWithError(cityPath, scopeRoot, provider string) ([]string, error) {
	env, err := providerLifecycleProcessEnvWithError(cityPath, provider)
	if err == nil {
		if providerUsesBdStoreContract(provider) && scopeRuntimeEnvIndependentOfCityProjection(cityPath, scopeRoot) {
			env = providerLifecycleIndependentScopeInitEnv(cityPath, scopeRoot, env)
		}
		return env, nil
	}
	if !providerUsesBdStoreContract(provider) || !cityPostgresProjectionErrorCanBeBypassed(cityPath, err) || !scopeRuntimeEnvIndependentOfCityProjection(cityPath, scopeRoot) {
		return nil, err
	}
	cityPath = normalizePathForCompare(cityPath)
	overrides := cityRuntimeEnvMapForCity(cityPath)
	setExecProjectedBackendEnvEmpty(overrides)
	overrides["BEADS_DOLT_AUTO_START"] = "0"
	applyLegacyRigScopeInitDoltEnv(overrides, cityPath, scopeRoot)
	baseEnv := mergeRuntimeEnv(os.Environ(), overrides)
	return providerLifecycleProcessEnvFromBase(cityPath, provider, baseEnv), nil
}

func providerLifecycleIndependentScopeInitEnv(cityPath, scopeRoot string, env []string) []string {
	cityPath = normalizePathForCompare(cityPath)
	overrides := map[string]string{}
	applyLegacyRigScopeInitDoltEnv(overrides, cityPath, scopeRoot)
	ensureProjectedPostgresEnvExplicit(overrides)
	return overlayEnvEntries(env, overrides)
}

func scopeRuntimeEnvIndependentOfCityProjection(cityPath, scopeRoot string) bool {
	if strings.TrimSpace(cityPath) == "" || samePath(cityPath, scopeRoot) {
		return false
	}
	var explicitRig *config.Rig
	if cfg, err := loadCityConfig(cityPath, io.Discard); err == nil && cfg != nil {
		explicitRig = rigConfigForScopeRoot(cityPath, scopeRoot, cfg.Rigs)
	}
	return rigRuntimeEnvIndependentOfCityProjection(cityPath, scopeRoot, explicitRig)
}

func applyLegacyRigScopeInitDoltEnv(env map[string]string, cityPath, scopeRoot string) {
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil || cfg == nil {
		return
	}
	explicitRig := rigConfigForScopeRoot(cityPath, scopeRoot, cfg.Rigs)
	if explicitRig == nil || (explicitRig.DoltHost == "" && explicitRig.DoltPort == "") {
		return
	}
	clearProjectedPostgresEnv(env)
	applyLegacyRigExternalTarget(env, *explicitRig)
	clearProjectedDoltPasswordEnv(env)
	applyResolvedDoltAuthEnv(env, scopeRoot, "")
	mirrorBeadsDoltEnv(env)
}

func providerLifecycleProcessEnvFromBase(cityPath, provider string, env []string) []string {
	if !providerUsesBdStoreContract(provider) {
		return env
	}
	if cityUsesDoltliteBeadsBackend(cityPath) {
		env = removeEnvKey(env, "GC_BEADS_BACKEND")
		env = removeEnvKey(env, "BEADS_BACKEND")
		env = append(env, "GC_BEADS_BACKEND=doltlite", "BEADS_BACKEND=doltlite")
		envMap := runtimeEnvEntriesToMap(env)
		clearProjectedDoltEnv(envMap)
		clearProjectedPostgresEnv(envMap)
		return mergeRuntimeEnv(nil, envMap)
	}
	for _, key := range []string{
		"GC_PACK_STATE_DIR",
		"GC_DOLT_DATA_DIR",
		"GC_DOLT_LOG_FILE",
		"GC_DOLT_STATE_FILE",
		"GC_DOLT_PID_FILE",
		"GC_DOLT_LOCK_FILE",
		"GC_DOLT_CONFIG_FILE",
		"GC_DOLT_ARCHIVE_LEVEL",
		"GC_DOLT_AUTO_GC_ENABLED",
		"GC_DOLT_MAX_CONNECTIONS",
		"GC_DOLT_READ_TIMEOUT_MILLIS",
		"GC_DOLT_WRITE_TIMEOUT_MILLIS",
		"GC_DOLT_LOCK_RELEASE_TIMEOUT_MS",
	} {
		env = removeEnvKey(env, key)
	}
	env = append(env, providerLifecycleDoltPathEnv(cityPath)...)
	if gcBin := resolveProviderLifecycleGCBinary(); gcBin != "" {
		env = removeEnvKey(env, "GC_BIN")
		env = append(env, "GC_BIN="+gcBin)
	}
	// Strip any inherited test-mode env unconditionally so a stray
	// GC_MANAGED_DOLT_TEST_MODE=1 in a production parent shell can never
	// reach child managed-dolt processes. Only Go test binaries
	// (managedDoltTestMode()) get the variable re-injected. Gating on
	// managedDoltTestModeEnabled() — which also honors the env var itself —
	// would have re-injected the stray value, defeating the guard
	// (gastownhall/gascity#2313 follow-up M1).
	env = removeEnvKey(env, managedDoltTestModeEnv)
	env = removeEnvKey(env, managedDoltTestParentPIDEnv)
	if managedDoltTestMode() {
		env = append(env,
			managedDoltTestModeEnv+"=1",
			managedDoltTestParentPIDEnv+"="+managedDoltTestParentPIDString(),
		)
	}
	// Propagate archive_level from city config so the managed dolt
	// server inherits it without shell-script changes.
	if v, ok := cityDoltConfigs.Load(cityPath); ok {
		dc, _ := v.(config.DoltConfig)
		if dc.ArchiveLevel != nil {
			env = append(env, fmt.Sprintf("GC_DOLT_ARCHIVE_LEVEL=%d", *dc.ArchiveLevel))
		}
		if dc.AutoGCEnabled != nil {
			env = append(env, fmt.Sprintf("GC_DOLT_AUTO_GC_ENABLED=%t", *dc.AutoGCEnabled))
		}
		if dc.MaxConnections > 0 {
			env = append(env, fmt.Sprintf("GC_DOLT_MAX_CONNECTIONS=%d", dc.MaxConnections))
		}
		if dc.ReadTimeoutMillis > 0 {
			env = append(env, fmt.Sprintf("GC_DOLT_READ_TIMEOUT_MILLIS=%d", dc.ReadTimeoutMillis))
		}
		if dc.WriteTimeoutMillis > 0 {
			env = append(env, fmt.Sprintf("GC_DOLT_WRITE_TIMEOUT_MILLIS=%d", dc.WriteTimeoutMillis))
		}
		// An explicit "0s" is meaningful (probe once, no wait), so gate on
		// field presence rather than a non-zero duration.
		if dc.DoltLockReleaseTimeout != "" {
			env = append(env, fmt.Sprintf("GC_DOLT_LOCK_RELEASE_TIMEOUT_MS=%d", dc.DoltLockReleaseTimeoutDuration().Milliseconds()))
		}
	}
	// `gc start` runs in the user's shell, which doesn't see vars set
	// only via `launchctl setenv` — those live in launchd's domain.
	// Fall back to launchctl-getenv so the managed dolt server's log
	// level honors `launchctl setenv GC_DOLT_LOGLEVEL` even when the
	// shell hasn't `export`ed it. The supervisor's reconcile path
	// runs the same lookup; either source delivers the value.
	const loglevelPrefix = "GC_DOLT_LOGLEVEL="
	loglevelInEnv := false
	for _, entry := range env {
		if strings.HasPrefix(entry, loglevelPrefix) {
			loglevelInEnv = true
			break
		}
	}
	if !loglevelInEnv {
		if val := providerLifecycleLaunchctlGetenv("GC_DOLT_LOGLEVEL"); val != "" {
			env = append(env, loglevelPrefix+val)
		}
	}
	return env
}

func runtimeEnvEntriesToMap(environ []string) map[string]string {
	out := make(map[string]string, len(environ))
	for _, entry := range environ {
		key, value, ok := strings.Cut(entry, "=")
		if ok && key != "" {
			out[key] = value
		}
	}
	return out
}

// acquireProviderSemaphore returns a per-city semaphore channel and waits
// until a slot is available or ctx is canceled. Call the returned function to
// release. Semaphore entries intentionally live for the process lifetime:
// deleting an entry while a lifecycle operation is still running would allow a
// second channel for the same city and break serialization. The map is bounded
// by city roots seen by this controller process.
// This serializes lifecycle operations per city to prevent thundering herd
// when dolt bounces: without this, concurrent health checks all trigger
// recovery simultaneously, spawning a storm of processes that overwhelm
// dolt on restart.
func acquireProviderSemaphore(ctx context.Context, cityPath string) (func(), error) {
	cityPath = normalizePathForCompare(cityPath)
	v, _ := providerOpSemaphores.LoadOrStore(cityPath, make(chan struct{}, 1))
	sem := v.(chan struct{})
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for provider lifecycle slot for %q: %w", cityPath, ctx.Err())
	}
}

func acquireProviderSemaphoreForOp(cityPath, op string) (func(), error) {
	ctx, cancel := providerLifecycleContext(context.Background(), providerOpTimeout(op))
	release, err := acquireProviderSemaphore(ctx, cityPath)
	if err != nil {
		cancel()
		return nil, err
	}
	return func() {
		release()
		cancel()
	}, nil
}

// providerOpTimeout returns the context timeout for a given lifecycle
// operation. The "start", "recover", and "init" operations get a longer
// timeout: dolt server startup can take 30+ seconds for large data dirs, and
// initializing a rig's bead store can likewise exceed 30s when it creates or
// migrates a database on a busy shared dolt server. Under the old 30s budget,
// init of an existing-but-unmigrated rig DB during a config reload was
// SIGKILLed, leaving the supervisor "keeping old config" so newly configured
// rigs never came online. All other operations use 30s.
var providerOpTimeout = func(op string) time.Duration {
	switch op {
	case "start", "recover", "init":
		return 120 * time.Second
	default:
		return 30 * time.Second
	}
}

// runProviderOp runs a lifecycle operation against an exec beads script.
// Exit 2 = not needed (treated as success, no-op). Used for start,
// init, health, recover, and stop operations.
// cityPath is exported via the canonical city runtime env so scripts can
// locate the city root and runtime directories.
func runProviderOp(script, cityPath string, args ...string) error {
	if cityPath == "" {
		return runProviderOpWithEnv(script, nil, args...)
	}
	env, err := cityRuntimeProcessEnvWithError(cityPath)
	if err != nil {
		return err
	}
	return runProviderOpWithEnv(script, env, args...)
}

func runProviderOpWithEnv(script string, environ []string, args ...string) error {
	op := ""
	if len(args) > 0 {
		op = args[0]
	}
	ctx, cancel := providerLifecycleContext(context.Background(), providerOpTimeout(op))
	defer cancel()

	cmd := exec.CommandContext(ctx, script, args...)
	cmd.WaitDelay = 2 * time.Second
	prepareProviderOpCommand(cmd)
	if len(environ) > 0 {
		cmd.Env = environ
	}

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 2 {
			return nil // Not needed
		}
		// Detect missing script or missing dolt binary.
		if errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("exec beads %s: provider script not found (%s); run \"gc doctor\" for diagnostics", args[0], script)
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("exec beads %s: %s", args[0], msg)
	}
	return nil
}
