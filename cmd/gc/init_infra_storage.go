package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/execenv"
	"github.com/gastownhall/gascity/internal/fsys"
)

const (
	initInfrastructureWitness   = "init-infrastructure.json"
	initInfrastructureOwnerFile = ".gc-init-owner"
)

// This is a durable initialization intent, never a process-status record. A
// resumed command uses the same database identity even if the city was moved.
type initInfrastructurePlan struct {
	Version           int    `json:"version"`
	Owner             string `json:"owner"`
	Database          string `json:"database"`
	Prefix            string `json:"prefix"`
	ConfigSHA256      string `json:"config_sha256"`
	PackSHA256        string `json:"pack_sha256"`
	ReadyConfigSHA256 string `json:"ready_config_sha256"`
	ProjectID         string `json:"project_id,omitempty"`
	Ready             bool   `json:"ready"`
}

type initInfrastructureMetadata struct {
	Backend   string `json:"backend"`
	Mode      string `json:"dolt_mode"`
	Database  string `json:"dolt_database"`
	ProjectID string `json:"project_id"`
	Host      string `json:"dolt_server_host"`
	Port      int    `json:"dolt_server_port"`
	Socket    string `json:"dolt_server_socket"`
}

type initInfrastructureOps struct {
	Run            func(context.Context, string, []string, ...string) error
	EnsureIdentity func(context.Context, string, initInfrastructurePlan) error
	Verify         func(context.Context, string, initInfrastructurePlan) error
	Commit         func(string, []byte, os.FileMode) error
}

func initInfrastructurePath(city string) string {
	return filepath.Join(city, ".gc", initInfrastructureWitness)
}

func initInfrastructureHash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// freshInitInfrastructureCandidate is sampled before scaffolding. Existing
// city/store data never becomes eligible merely because its config lacks storage.
func freshInitInfrastructureCandidate(fs fsys.FS, city string, adopting bool) bool {
	if adopting {
		return false
	}
	for _, name := range []string{"city.toml", "pack.toml", ".beads", filepath.Join(".gc", "store"), filepath.Join(".gc", "storage")} {
		if _, err := fs.Lstat(filepath.Join(city, name)); !errors.Is(err, os.ErrNotExist) {
			return false
		}
	}
	return true
}

// recordFreshInitInfrastructure runs before API registration, but starts no
// database. An explicit storage/operation policy, another bead provider, and
// preserved/adopted data keep their current behavior.
func recordFreshInitInfrastructure(fs fsys.FS, city string, fresh bool) error {
	if !fresh || gcDoltSkip() {
		return nil
	}
	provider := strings.TrimSpace(os.Getenv("GC_BEADS"))
	if provider != "" && provider != "bd" {
		return nil
	}
	raw, err := fs.ReadFile(filepath.Join(city, "city.toml"))
	if err != nil {
		return err
	}
	var cfg config.City
	md, err := toml.Decode(string(raw), &cfg)
	if err != nil {
		return err
	}
	if md.IsDefined("storage") || md.IsDefined("ops") || cfg.Dolt.Host != "" || len(cfg.Include) != 0 || len(cfg.Workspace.LegacyIncludes()) != 0 || (cfg.Beads.Backend != "" && cfg.Beads.Backend != "dolt") || cfg.Beads.NativeTransport == "off" || cfg.Beads.ConditionalWrites == "off" || beads.ForceNativeFallbackActive() || (cfg.Beads.Provider != "" && cfg.Beads.Provider != "bd") {
		return nil
	}
	packRaw, eligible, err := bundledInitInfrastructurePack(fs, city, cfg.Beads.Provider)
	if err != nil || !eligible {
		return err
	}
	if _, err := fs.Lstat(initInfrastructurePath(city)); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			return errors.New("infrastructure initialization intent already exists")
		}
		return fmt.Errorf("inspecting infrastructure initialization intent: %w", err)
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return err
	}
	owner := hex.EncodeToString(id[:])
	prefix, ok := config.ReservedClassPrefix(config.BeadClassGraph)
	if !ok || prefix == "" {
		return errors.New("graph infrastructure has no reserved prefix")
	}
	plan := initInfrastructurePlan{Version: 2, Owner: owner, Database: "gc_infra_" + owner, Prefix: prefix, ConfigSHA256: initInfrastructureHash(raw), PackSHA256: initInfrastructureHash(packRaw), ReadyConfigSHA256: initInfrastructureHash(initInfrastructureConfig(raw))}
	return saveInitInfrastructurePlan(fs, city, plan)
}

// bundledInitInfrastructurePack accepts only the exact generated default
// imports. An authored include, dependency, import or storage policy must never
// become permission to append an implicit split. Its bytes are pinned in the
// pending witness so edits made before or during recovery are also preserved.
func bundledInitInfrastructurePack(fs fsys.FS, city, provider string) ([]byte, bool, error) {
	raw, err := fs.ReadFile(filepath.Join(city, "pack.toml"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var pack initPackConfig
	md, err := toml.Decode(string(raw), &pack)
	if err != nil {
		return nil, false, err
	}
	if pack.Pack.Schema != initPackSchemaVersion || len(pack.Pack.Includes) != 0 || len(pack.Pack.Requires) != 0 ||
		md.IsDefined("include") || md.IsDefined("storage") || md.IsDefined("ops") || md.IsDefined("beads") {
		return nil, false, nil
	}
	want := newInitPackConfig(pack.Pack.Name)
	want.Imports = config.GascityCityWithProviders(pack.Pack.Name, "", nil).Imports
	addBuiltinImportsToInitPack(&want, provider)
	return raw, reflect.DeepEqual(pack.Imports, want.Imports), nil
}

func initInfrastructureConfig(raw []byte) []byte {
	return append(append([]byte(nil), raw...), []byte("\n[storage.classes]\nwork = \"work\"\ngraph = \"infra\"\nsessions = \"infra\"\nmessaging = \"infra\"\norders = \"infra\"\nnudges = \"infra\"\n\n[storage.bindings.infra]\nprovider = \"beads-workspace\"\nconfig_ref = \"infra\"\n\n[ops]\nauthoritative = [\"message\", \"interrupt\", \"respond\"]\n")...)
}

func saveInitInfrastructurePlan(fs fsys.FS, city string, plan initInfrastructurePlan) error {
	raw, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return err
	}
	return fsys.WriteFileAtomic(fs, initInfrastructurePath(city), append(raw, '\n'), 0o600)
}

func readInitInfrastructurePlan(fs fsys.FS, city string) (*initInfrastructurePlan, error) {
	path := initInfrastructurePath(city)
	info, err := fs.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("infrastructure intent %s is not a regular file", path)
	}
	raw, err := fs.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var plan initInfrastructurePlan
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&plan); err != nil {
		return nil, fmt.Errorf("reading infrastructure intent: %w", err)
	}
	if plan.Version != 2 || !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(plan.Owner) || plan.Database != "gc_infra_"+plan.Owner || plan.Prefix == "" || len(plan.ConfigSHA256) != 64 || len(plan.PackSHA256) != 64 || len(plan.ReadyConfigSHA256) != 64 {
		return nil, errors.New("invalid infrastructure initialization intent; preserve it for recovery")
	}
	prefix, ok := config.ReservedClassPrefix(config.BeadClassGraph)
	if !ok || plan.Prefix != prefix {
		return nil, errors.New("infrastructure intent has another reserved namespace")
	}
	return &plan, nil
}

// resumeInitInfrastructure is the shared CLI/API-bootstrap continuation. It
// changes only the workspace its pending intent owns; a city without a witness
// is untouched. Failed provisioning leaves the intent and original config for
// an identical retry, and a changed config is refused before any next effect.
var initInfrastructureDefaultOps = initInfrastructureOps{
	Run: runInitInfrastructureCommand,
	EnsureIdentity: func(ctx context.Context, workspace string, plan initInfrastructurePlan) error {
		return beads.EnsureNativeWorkspaceIdentityWithoutAmbientEnv(ctx, workspace, plan.Prefix, plan.ProjectID)
	},
	Verify: verifyInitInfrastructure,
}

func resumeInitInfrastructure(ctx context.Context, city string) error {
	return resumeInitInfrastructureWithOps(ctx, city, initInfrastructureDefaultOps)
}

func resumeInitInfrastructureWithOps(ctx context.Context, city string, ops initInfrastructureOps) error {
	fs := fsys.OSFS{}
	plan, err := readInitInfrastructurePlan(fs, city)
	if err != nil || plan == nil || plan.Ready {
		return err
	}
	if gcDoltSkip() {
		return errors.New("native infrastructure initialization is pending; GC_DOLT=skip prevents completing it")
	}
	if beads.ForceNativeFallbackActive() {
		return errors.New("native infrastructure initialization is pending; GC_BEADS_FORCE_FALLBACK prevents completing it; clear the override to resume")
	}
	if provider := strings.TrimSpace(os.Getenv("GC_BEADS")); provider != "" && provider != "bd" {
		return errors.New("native infrastructure initialization is pending; restore the original bd provider before retrying")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// All competing CLI/supervisor resumes serialize on this city's owned lock.
	lock, err := os.OpenFile(filepath.Join(city, ".gc", "init-infrastructure.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close() //nolint:errcheck // releasing an initialization lock
	for {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) //nolint:errcheck // the descriptor is closed above
	plan, err = readInitInfrastructurePlan(fs, city)
	if err != nil || plan == nil || plan.Ready {
		return err
	}
	configPath := filepath.Join(city, "city.toml")
	raw, err := fs.ReadFile(configPath)
	if err != nil {
		return err
	}
	sha := initInfrastructureHash(raw)
	if sha != plan.ConfigSHA256 && sha != plan.ReadyConfigSHA256 {
		return errors.New("city.toml changed during infrastructure initialization; refusing to replace it; preserve the city and reconcile the pending intent")
	}
	packPath := filepath.Join(city, "pack.toml")
	packRaw, err := fs.ReadFile(packPath)
	if err != nil {
		return err
	}
	if initInfrastructureHash(packRaw) != plan.PackSHA256 {
		return errors.New("pack.toml changed during infrastructure initialization; refusing to replace its storage policy")
	}
	workspace := filepath.Join(city, ".gc", "storage", "infra")
	if err := claimInitInfrastructureWorkspace(city, workspace, plan.Owner); err != nil {
		return err
	}
	// This journal already owns provider-scoped retirement, including detached
	// path entries; GC stop can retire bd's server without a second SQL owner.
	entry, owned, err := providerScopeOwnership(city, workspace)
	if err != nil {
		return err
	}
	if owned && entry.State == providerScopeReady {
		// The provider journal commits before the final witness write. A crash
		// there is recoverable only under this initializer's exact durable
		// identity; readiness is re-proved below, never assumed from the journal.
		if entry.LifecycleOwner != providerScopeLifecycleOwner || !samePath(entry.ScopePath, workspace) || plan.ProjectID == "" {
			return errors.New("ready infrastructure ownership does not match this pending initializer")
		}
		if _, err := readInitInfrastructureMetadata(workspace, *plan); err != nil {
			return err
		}
	} else if err := persistProviderScopeOwnership(city, workspace, providerScopeIntent{Transport: "direct", Target: "local"}); err != nil {
		return err
	}
	env := initInfrastructureToolEnv(city, workspace)
	if ops.Run == nil || ops.EnsureIdentity == nil || ops.Verify == nil {
		return errors.New("infrastructure initializer is not wired")
	}
	if _, err := os.Stat(filepath.Join(workspace, ".beads", "metadata.json")); errors.Is(err, os.ErrNotExist) {
		if err := ops.Run(ctx, workspace, env, "git", "init", "--quiet"); err != nil {
			return err
		}
		if err := ops.Run(ctx, workspace, env, "bd", "init", "--server", "--shared-server=false", "--database", plan.Database, "-p", plan.Prefix, "--skip-hooks", "--skip-agents", "--non-interactive"); err != nil {
			return fmt.Errorf("initializing native infrastructure: %w", err)
		}
	} else if err != nil {
		return err
	}
	metadata, err := readInitInfrastructureMetadata(workspace, *plan)
	if err != nil {
		return err
	}
	if plan.ProjectID == "" {
		plan.ProjectID = metadata.ProjectID
		if err := saveInitInfrastructurePlan(fs, city, *plan); err != nil {
			return err
		}
	}
	// bd writes metadata before it atomically bootstraps database identity. A
	// death in that interval must finish the same persisted identity, without
	// forcing a reinitialization or adopting any existing/partial identity.
	if err := ops.EnsureIdentity(ctx, workspace, *plan); err != nil {
		return fmt.Errorf("confirming initialized infrastructure identity: %w", err)
	}
	types := append(append([]string(nil), beads.RequiredCustomTypes...), "wisp", "wait", "order", "nudge")
	if err := ops.Run(ctx, workspace, env, "bd", "config", "set", "types.custom", strings.Join(types, ",")); err != nil {
		return err
	}
	if err := ops.Verify(ctx, workspace, *plan); err != nil {
		return fmt.Errorf("native infrastructure is not ready: %w", err)
	}
	// Detect edits made while bd/probes ran; never serialize a stale config over
	// a corrected file. The complete intended bytes are stable across retries.
	current, err := fs.ReadFile(configPath)
	if err != nil {
		return err
	}
	if initInfrastructureHash(current) != sha {
		return errors.New("city.toml changed while infrastructure was provisioned; refusing to overwrite it")
	}
	currentPack, err := fs.ReadFile(packPath)
	if err != nil {
		return err
	}
	if initInfrastructureHash(currentPack) != plan.PackSHA256 {
		return errors.New("pack.toml changed while infrastructure was provisioned; refusing to activate its storage")
	}
	if sha == plan.ConfigSHA256 {
		desired := initInfrastructureConfig(raw)
		if initInfrastructureHash(desired) != plan.ReadyConfigSHA256 {
			return errors.New("infrastructure config fingerprint disagrees with its initialization intent")
		}
		info, err := fs.Lstat(configPath)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("city.toml is not a regular file; refusing initialization")
		}
		commit := ops.Commit
		if commit == nil {
			commit = func(path string, data []byte, mode os.FileMode) error {
				return fsys.WriteFileAtomic(fs, path, data, mode)
			}
		}
		if err := commit(configPath, desired, info.Mode().Perm()); err != nil {
			return err
		}
	}
	if err := markProviderScopeOwnershipReady(city, workspace); err != nil {
		return err
	}
	plan.Ready = true
	return saveInitInfrastructurePlan(fs, city, *plan)
}

func claimInitInfrastructureWorkspace(city, workspace, owner string) error {
	fs := fsys.OSFS{}
	for _, path := range []string{filepath.Join(city, ".gc"), filepath.Join(city, ".gc", "storage"), workspace} {
		info, err := fs.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			if err := fs.MkdirAll(path, 0o700); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("infrastructure path %s is not an owned directory", path)
		}
	}
	marker := filepath.Join(workspace, initInfrastructureOwnerFile)
	raw, err := fs.ReadFile(marker)
	if err == nil {
		if string(raw) != owner+"\n" {
			return errors.New("infrastructure workspace belongs to another initializer")
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	entries, err := fs.ReadDir(workspace)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return errors.New("refusing to adopt an unowned nonempty infrastructure workspace")
	}
	return fsys.WriteFileAtomic(fs, marker, []byte(owner+"\n"), 0o600)
}

func initInfrastructureToolEnv(city, workspace string) []string {
	var env []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "BEADS_") || strings.HasPrefix(key, "BD_") || strings.HasPrefix(key, "GC_") || strings.HasPrefix(key, "DOLT_") || strings.HasPrefix(key, "GIT_") || execenv.IsSensitiveKey(key) {
			continue
		}
		env = append(env, entry)
	}
	return execenv.WithUsageMetricsDisabled(append(env, "BEADS_DIR="+filepath.Join(workspace, ".beads"), "GIT_CEILING_DIRECTORIES="+city, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_AUTHOR_NAME=Gas City infrastructure", "GIT_AUTHOR_EMAIL=gascity@localhost", "GIT_COMMITTER_NAME=Gas City infrastructure", "GIT_COMMITTER_EMAIL=gascity@localhost"))
}

func runInitInfrastructureCommand(parent context.Context, dir string, env []string, args ...string) error {
	ctx, cancel := context.WithTimeout(parent, providerOwnedOpTimeout("init", false))
	defer cancel()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	raw, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", args[0], err, strings.TrimSpace(string(raw)))
	}
	if len(raw) > 0 {
		fmt.Fprint(os.Stderr, string(raw))
	} //nolint:errcheck // prerequisite progress and warnings
	return nil
}

func readInitInfrastructureMetadata(workspace string, plan initInfrastructurePlan) (initInfrastructureMetadata, error) {
	var md initInfrastructureMetadata
	raw, err := os.ReadFile(filepath.Join(workspace, ".beads", "metadata.json"))
	if err != nil {
		return md, err
	}
	if err := json.Unmarshal(raw, &md); err != nil {
		return md, err
	}
	if md.Backend != "dolt" || md.Mode != "server" || md.Database != plan.Database || md.ProjectID == "" || md.Port != 0 || md.Socket != "" || (md.Host != "" && md.Host != "127.0.0.1" && md.Host != "localhost") {
		return md, errors.New("infrastructure metadata does not identify the initializer's bd-owned direct-local workspace")
	}
	if plan.ProjectID != "" && plan.ProjectID != md.ProjectID {
		return md, errors.New("infrastructure project identity changed; refusing another database")
	}
	return md, nil
}

func verifyInitInfrastructure(ctx context.Context, workspace string, plan initInfrastructurePlan) (verifyErr error) {
	store, err := beads.OpenNativeDoltStoreAtWithoutAmbientEnv(ctx, workspace)
	if err != nil {
		return err
	}
	defer func() { verifyErr = errors.Join(verifyErr, store.CloseStore()) }()
	if store.IDPrefix() != plan.Prefix {
		return errors.New("infrastructure namespace differs from its initialization intent")
	}
	if capable, reason := beads.ProbeConditionalWrites(store); !capable {
		return fmt.Errorf("native infrastructure lacks conditional writes: %s", reason)
	}
	batcher, ok := beads.GuardedBatcherFor(store)
	if !ok {
		return beads.ErrGuardedBatchUnsupported
	}
	id := plan.Prefix + "-init-" + plan.Owner
	probe := beads.Bead{ID: id, Title: "Gas City infrastructure readiness", Type: "event", Ephemeral: true, Metadata: map[string]string{"gc.init_owner": plan.Owner}}
	existing, err := store.Get(id)
	if errors.Is(err, beads.ErrNotFound) {
		if _, err = batcher.ApplyGuarded(ctx, beads.GuardedBatch{Reason: "native infrastructure readiness", Items: []beads.GuardedItem{{Create: &probe}}}); err != nil {
			writeErr := err
			existing, err = store.Get(id)
			if err != nil {
				return errors.Join(writeErr, err)
			}
		} else {
			existing, err = store.Get(id)
		}
	}
	if err != nil {
		return err
	}
	if existing.Metadata["gc.init_owner"] != plan.Owner {
		return errors.New("readiness probe identity collision")
	}
	defer func() {
		for _, probeID := range []string{id, id + "-atomic"} {
			row, err := store.Get(probeID)
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			if err != nil {
				verifyErr = errors.Join(verifyErr, err)
				continue
			}
			if row.Metadata["gc.init_owner"] != plan.Owner {
				verifyErr = errors.Join(verifyErr, errors.New("refusing to remove a foreign readiness row"))
				continue
			}
			verifyErr = errors.Join(verifyErr, store.Delete(probeID))
		}
	}()
	title := "stale guard must not commit"
	wrong := existing.Revision + 1
	err = store.UpdateIfMatch(id, wrong, beads.UpdateOpts{Title: &title})
	if !beads.IsPreconditionFailed(err) {
		return errors.Join(errors.New("native revision mismatch was not refused"), err)
	}
	second := probe
	second.ID = id + "-atomic"
	_, err = batcher.ApplyGuarded(ctx, beads.GuardedBatch{Items: []beads.GuardedItem{{Create: &second}, {Update: &beads.GuardedUpdate{ID: id, ExpectedRevision: &wrong, Opts: beads.UpdateOpts{Title: &title}}}}})
	if !beads.IsPreconditionFailed(err) {
		return errors.Join(errors.New("native batch revision mismatch was not refused"), err)
	}
	if _, err := store.Get(second.ID); !errors.Is(err, beads.ErrNotFound) {
		return errors.Join(errors.New("refused batch left a readiness row"), err)
	}
	return nil
}
