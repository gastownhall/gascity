package main

// cityinit.Initializer implementation. Bridges the domain interface
// declared in internal/cityinit to the concrete scaffold + finalize
// helpers in this package. Supplied to api.NewSupervisorMux at
// construction so POST /v0/city calls Init in-process — no
// subprocess, no 30-second deadline, no stderr-scraping.
//
// The long-term plan is to move doInit/finalizeInit and their
// helpers into internal/cityinit so the domain logic physically
// lives in the object model (per engdocs/architecture/api-control-plane.md §1). This
// bridge is the minimum viable unblocker: the HTTP API no longer
// shells out, both surfaces drive the same in-process code path via
// the same typed contract, and the refactor of where the body lives
// is a follow-up.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/cityinit"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/supervisor"
)

// localInitializer implements cityinit.Initializer by dispatching to
// this package's existing doInit + finalizeInit functions.
type localInitializer struct{}

// NewInitializer returns the Initializer the supervisor uses to
// service POST /v0/city. Exported so `gc supervisor run` can wire it
// into api.NewSupervisorMux.
func NewInitializer() cityinit.Initializer {
	return localInitializer{}
}

func ensureCityEventLog(cityPath string) {
	if fr, err := events.NewFileRecorder(filepath.Join(cityPath, ".gc", "events.jsonl"), io.Discard); err == nil {
		fr.Close() //nolint:errcheck // best-effort
	}
}

func recordCityEvent(cityPath, eventType, subject string, payload any) {
	fr, err := events.NewFileRecorder(filepath.Join(cityPath, ".gc", "events.jsonl"), io.Discard)
	if err != nil {
		return
	}
	defer fr.Close() //nolint:errcheck // best-effort

	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fr.Record(events.Event{
		Type:    eventType,
		Actor:   "gc",
		Subject: subject,
		Payload: raw,
	})
}

type scaffoldRollbackEntry struct {
	mode       os.FileMode
	data       []byte
	linkTarget string
}

type scaffoldSnapshot struct {
	root    string
	entries map[string]scaffoldRollbackEntry
}

type scaffoldRollbackState struct {
	root   string
	before map[string]scaffoldRollbackEntry
	after  map[string]scaffoldRollbackEntry
}

func captureScaffoldSnapshot(root string) (*scaffoldSnapshot, error) {
	snapshot := &scaffoldSnapshot{
		root:    root,
		entries: make(map[string]scaffoldRollbackEntry),
	}
	for _, rel := range scaffoldManagedPaths() {
		if err := snapshot.capture(rel); err != nil {
			return nil, err
		}
	}
	return snapshot, nil
}

func scaffoldManagedPaths() []string {
	seen := make(map[string]struct{}, len(initConventionDirs)+5)
	paths := make([]string, 0, len(initConventionDirs)+5)
	add := func(rel string) {
		if rel == "" {
			return
		}
		if _, ok := seen[rel]; ok {
			return
		}
		seen[rel] = struct{}{}
		paths = append(paths, rel)
	}

	add(citylayout.RuntimeRoot)
	add("hooks")
	add(citylayout.CityConfigFile)
	add("pack.toml")
	add(".gitignore")
	for _, rel := range initConventionDirs {
		add(rel)
	}
	return paths
}

func newScaffoldRollbackState(root string) (*scaffoldRollbackState, error) {
	snapshot, err := captureScaffoldSnapshot(root)
	if err != nil {
		return nil, err
	}
	return &scaffoldRollbackState{
		root:   root,
		before: snapshot.entries,
	}, nil
}

func (s *scaffoldSnapshot) capture(rel string) error {
	abs := filepath.Join(s.root, rel)
	_, err := os.Lstat(abs)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("snapshot %q: %w", abs, err)
	}
	return filepath.Walk(abs, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("snapshot %q: %w", path, walkErr)
		}
		relPath, err := filepath.Rel(s.root, path)
		if err != nil {
			return fmt.Errorf("relative path for %q: %w", path, err)
		}
		entry := scaffoldRollbackEntry{mode: info.Mode()}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return fmt.Errorf("readlink %q: %w", path, err)
			}
			entry.linkTarget = target
		} else if !info.IsDir() {
			data, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("read %q: %w", path, err)
			}
			entry.data = data
		}
		s.entries[filepath.Clean(relPath)] = entry
		return nil
	})
}

func (s *scaffoldRollbackState) markScaffoldState() error {
	snapshot, err := captureScaffoldSnapshot(s.root)
	if err != nil {
		return err
	}
	s.after = snapshot.entries
	return nil
}

func rollbackEntryEqual(a, b scaffoldRollbackEntry) bool {
	return a.mode == b.mode && a.linkTarget == b.linkTarget && bytes.Equal(a.data, b.data)
}

func restoreRollbackEntry(abs string, entry scaffoldRollbackEntry) error {
	switch {
	case entry.mode.IsDir():
		return os.MkdirAll(abs, entry.mode.Perm())
	case entry.mode&os.ModeSymlink != 0:
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return err
		}
		if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
			return err
		}
		return os.Symlink(entry.linkTarget, abs)
	default:
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return err
		}
		return os.WriteFile(abs, entry.data, entry.mode.Perm())
	}
}

func (s *scaffoldRollbackState) restore() error {
	current, err := captureScaffoldSnapshot(s.root)
	if err != nil {
		return err
	}

	var errs []error
	var createdDirs []string
	for rel, after := range s.after {
		before, hadBefore := s.before[rel]
		currentEntry, existsNow := current.entries[rel]
		switch {
		case !hadBefore:
			if after.mode.IsDir() {
				createdDirs = append(createdDirs, rel)
				continue
			}
			if existsNow && rollbackEntryEqual(currentEntry, after) {
				if err := os.Remove(filepath.Join(s.root, rel)); err != nil && !os.IsNotExist(err) {
					errs = append(errs, fmt.Errorf("remove %q: %w", filepath.Join(s.root, rel), err))
				}
			}
		case rollbackEntryEqual(before, after):
			continue
		default:
			if after.mode.IsDir() {
				continue
			}
			if existsNow && rollbackEntryEqual(currentEntry, after) {
				if err := restoreRollbackEntry(filepath.Join(s.root, rel), before); err != nil {
					errs = append(errs, fmt.Errorf("restore %q: %w", filepath.Join(s.root, rel), err))
				}
			}
		}
	}

	for rel, before := range s.before {
		if _, hadAfter := s.after[rel]; hadAfter {
			continue
		}
		if before.mode.IsDir() {
			continue
		}
		if _, existsNow := current.entries[rel]; existsNow {
			continue
		}
		if err := restoreRollbackEntry(filepath.Join(s.root, rel), before); err != nil {
			errs = append(errs, fmt.Errorf("restore %q: %w", filepath.Join(s.root, rel), err))
		}
	}

	sort.Slice(createdDirs, func(i, j int) bool {
		return len(createdDirs[i]) > len(createdDirs[j])
	})
	for _, rel := range createdDirs {
		if err := os.Remove(filepath.Join(s.root, rel)); err != nil && !os.IsNotExist(err) {
			if errors.Is(err, syscall.ENOTEMPTY) {
				continue
			}
			errs = append(errs, fmt.Errorf("remove %q: %w", filepath.Join(s.root, rel), err))
		}
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// Scaffold runs the fast portion of city creation so the HTTP API
// handler can return 202 Accepted without blocking on the slow
// finalize work. Writes the on-disk shape (via doInit), then
// registers the city with the supervisor so the reconciler picks
// it up on its next tick. The reconciler owns finalize from there;
// readiness is signaled via city.ready / city.init_failed events on
// the supervisor event bus (see internal/api/event_payloads.go).
func (localInitializer) Scaffold(_ context.Context, req cityinit.InitRequest) (*cityinit.InitResult, error) {
	if err := validateInitRequest(&req); err != nil {
		return nil, err
	}
	dir := req.Dir
	dirExisted := false
	var rollbackState *scaffoldRollbackState
	if _, err := os.Stat(dir); err == nil {
		dirExisted = true
		rollbackState, err = newScaffoldRollbackState(dir)
		if err != nil {
			return nil, fmt.Errorf("snapshot rollback state for %q: %w", dir, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("stat directory %q: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating directory %q: %w", dir, err)
	}

	wiz := wizardConfig{
		configName:       req.ConfigName,
		provider:         req.Provider,
		startCommand:     req.StartCommand,
		bootstrapProfile: req.BootstrapProfile,
	}
	if wiz.configName == "" {
		wiz.configName = "tutorial"
	}

	if cityHasScaffoldFS(fsys.OSFS{}, dir) {
		return nil, cityinit.ErrAlreadyInitialized
	}
	if code := doInit(fsys.OSFS{}, dir, wiz, req.NameOverride, io.Discard, io.Discard); code != 0 {
		if dirExisted && rollbackState != nil {
			if markErr := rollbackState.markScaffoldState(); markErr != nil {
				return nil, errors.Join(fmt.Errorf("scaffold failed (exit %d)", code), fmt.Errorf("snapshot scaffold state for rollback: %w", markErr))
			}
			if cleanupErr := rollbackState.restore(); cleanupErr != nil {
				return nil, errors.Join(fmt.Errorf("scaffold failed (exit %d)", code), fmt.Errorf("restoring existing directory after scaffold failure: %w", cleanupErr))
			}
		}
		if code == initExitAlreadyInitialized {
			return nil, cityinit.ErrAlreadyInitialized
		}
		return nil, fmt.Errorf("scaffold failed (exit %d)", code)
	}

	cityName := resolveCityName(req.NameOverride, "", dir)

	// Create .gc/events.jsonl immediately before registration. Two reasons:
	//
	// 1. The supervisor event multiplexer (see
	//    internal/api/supervisor.go:buildMultiplexer) includes
	//    transient-city event providers via
	//    TransientCityEventSource. With the file in place, a
	//    subscriber to /v0/events/stream that connects right after
	//    POST returns 202 sees a non-empty multiplexer and can
	//    replay events via after_cursor=0.
	//
	// 2. The supervisor event stream's no-providers precheck rejects
	//    subscriptions with 503 when the multiplexer is empty. By
	//    populating at least one event log before registration,
	//    POST /v0/city → subscribe works even when no other cities
	//    exist yet (the fresh-supervisor scenario).
	//
	// The file creation is best-effort. city.created itself is emitted
	// only after registration succeeds so synchronous failures do not
	// leak a "created" event for a city the supervisor never adopted.
	ensureCityEventLog(dir)
	if dirExisted && rollbackState != nil {
		if err := rollbackState.markScaffoldState(); err != nil {
			return nil, fmt.Errorf("snapshot scaffold state for %q: %w", dir, err)
		}
	}

	// Register the city with the supervisor without blocking on the
	// reconciler's tick. The standard registerCityWithSupervisor
	// waits for prepareCityForSupervisor to complete, which is the
	// very blocking behavior the async POST /v0/city contract
	// exists to avoid.
	if err := registerCityForAPI(dir, req.NameOverride); err != nil {
		if dirExisted {
			if rollbackState != nil {
				if cleanupErr := rollbackState.restore(); cleanupErr != nil {
					return nil, errors.Join(fmt.Errorf("register with supervisor: %w", err), fmt.Errorf("restoring existing directory after failed registration: %w", cleanupErr))
				}
			}
		} else if cleanupErr := os.RemoveAll(dir); cleanupErr != nil {
			return nil, errors.Join(fmt.Errorf("register with supervisor: %w", err), fmt.Errorf("cleaning scaffold after failed registration: %w", cleanupErr))
		}
		return nil, fmt.Errorf("register with supervisor: %w", err)
	}
	recordCityEvent(dir, events.CityCreated, cityName, api.CityCreatedPayload{Name: cityName, Path: dir})
	reloadSupervisorNoWaitHook()

	return &cityinit.InitResult{
		CityName:     cityName,
		CityPath:     dir,
		ProviderUsed: req.Provider,
	}, nil
}

// Init scaffolds + finalizes a new city. Errors are mapped to the
// typed sentinels in package cityinit so callers (HTTP API, future
// in-process consumers) can pattern-match via errors.Is.
func (localInitializer) Init(_ context.Context, req cityinit.InitRequest) (*cityinit.InitResult, error) {
	if err := validateInitRequest(&req); err != nil {
		return nil, err
	}
	dir := req.Dir
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating directory %q: %w", dir, err)
	}

	wiz := wizardConfig{
		configName:       req.ConfigName,
		provider:         req.Provider,
		startCommand:     req.StartCommand,
		bootstrapProfile: req.BootstrapProfile,
	}
	if wiz.configName == "" {
		wiz.configName = "tutorial"
	}

	// Check for an already-initialized directory BEFORE calling
	// doInit so we can return ErrAlreadyInitialized without also
	// writing "gc init: already initialized" to stderr (the CLI
	// path wants that; the API does not).
	if cityHasScaffoldFS(fsys.OSFS{}, dir) {
		return nil, cityinit.ErrAlreadyInitialized
	}

	// doInit writes directly to io.Writer arguments (CLI progress
	// narration). The API path discards those; error return is
	// carried as an exit code, which we translate into typed
	// sentinels below.
	if code := doInit(fsys.OSFS{}, dir, wiz, req.NameOverride, io.Discard, io.Discard); code != 0 {
		if code == initExitAlreadyInitialized {
			return nil, cityinit.ErrAlreadyInitialized
		}
		return nil, fmt.Errorf("scaffold failed (exit %d)", code)
	}

	// finalizeInit likewise writes to io.Writer and returns 0/1.
	// Discard its narration; the HTTP response conveys structured
	// errors via the sentinel types.
	if code := finalizeInit(dir, io.Discard, io.Discard, initFinalizeOptions{
		skipProviderReadiness: req.SkipProviderReadiness,
		showProgress:          false,
		commandName:           "gc init",
	}); code != 0 {
		// finalizeInit's current contract is "blocked, check
		// stderr". Without a structured return type we can't map
		// to specific sentinels; future work splits this out.
		return nil, fmt.Errorf("finalize failed (exit %d)", code)
	}

	cityName := resolveCityName(req.NameOverride, "", dir)
	return &cityinit.InitResult{
		CityName:     cityName,
		CityPath:     dir,
		ProviderUsed: req.Provider,
	}, nil
}

// Unregister removes the city's registry entry and signals the
// supervisor to reconcile. Fire-and-forget: returns as soon as the
// registry file is updated and the reload signal is sent. The
// supervisor reconciler discovers the missing entry on its next
// tick, stops the city's controller, and emits city.unregistered
// (or city.unregister_failed on stop failure). See cmd_supervisor.go
// for the reconciler side.
//
// Looks the city up by name. The registry is keyed by path on disk,
// so we scan entries to find the one whose effective name matches.
// Name collisions would violate the registry's uniqueness invariant
// and are rejected at Register time; we take the first match.
//
// Emits city.unregister_requested to the city's event log before
// unregistering so /v0/events/stream subscribers see the start of
// the teardown. Once the registry entry is gone, the transient
// event-provider lookup (cityRegistry.TransientCityEventProviders)
// will still surface this city to new subscribers via its snap.all
// entry until the reconciler fully drops it.
func (localInitializer) Unregister(_ context.Context, req cityinit.UnregisterRequest) (*cityinit.UnregisterResult, error) {
	name := strings.TrimSpace(req.CityName)
	if name == "" {
		return nil, fmt.Errorf("%w: city_name is required", cityinit.ErrNotRegistered)
	}

	reg := newSupervisorRegistry()
	entries, err := reg.List()
	if err != nil {
		return nil, fmt.Errorf("reading supervisor registry: %w", err)
	}
	var match supervisor.CityEntry
	var found bool
	for _, e := range entries {
		if e.EffectiveName() == name {
			match = e
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("%w: %q", cityinit.ErrNotRegistered, name)
	}

	if err := reg.Unregister(match.Path); err != nil {
		// Should not happen — we just read this entry — but wrap to
		// satisfy the ErrNotRegistered contract if it does.
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %q: %w", cityinit.ErrNotRegistered, name, err)
		}
		return nil, fmt.Errorf("removing %q from supervisor registry: %w", name, err)
	}
	recordCityEvent(
		match.Path,
		events.CityUnregisterRequested,
		match.EffectiveName(),
		api.CityUnregisterRequestedPayload{Name: match.EffectiveName(), Path: match.Path},
	)

	// Wake the reconciler; same fire-and-forget signal the Scaffold
	// path uses. If the supervisor isn't reachable the periodic
	// ticker picks up the change on its next interval.
	reloadSupervisorNoWait()

	return &cityinit.UnregisterResult{
		CityName: match.EffectiveName(),
		CityPath: match.Path,
	}, nil
}

// validateInitRequest performs the membership / mutual-exclusion
// checks that the HTTP layer previously did inline. Keeping them in
// the bridge means the CLI also benefits from the same validation
// when its call site moves (follow-up).
func validateInitRequest(req *cityinit.InitRequest) error {
	if req.Dir == "" {
		return fmt.Errorf("%w: dir is required", cityinit.ErrInvalidProvider)
	}
	if !filepath.IsAbs(req.Dir) {
		return fmt.Errorf("dir must be absolute: %q", req.Dir)
	}
	if req.Provider == "" && req.StartCommand == "" {
		return fmt.Errorf("%w: provider or start_command required", cityinit.ErrInvalidProvider)
	}
	if req.Provider != "" && req.StartCommand != "" {
		return fmt.Errorf("%w: provider and start_command are mutually exclusive", cityinit.ErrInvalidProvider)
	}
	if req.Provider != "" {
		if _, ok := config.BuiltinProviders()[req.Provider]; !ok {
			return fmt.Errorf("%w: unknown provider %q", cityinit.ErrInvalidProvider, req.Provider)
		}
	}
	if req.BootstrapProfile != "" {
		// normalizeBootstrapProfile accepts every spelling the CLI
		// and HTTP API currently support; reuse it here so the two
		// projections can't disagree.
		if _, err := normalizeBootstrapProfile(req.BootstrapProfile); err != nil {
			return fmt.Errorf("%w: %w", cityinit.ErrInvalidBootstrapProfile, err)
		}
	}
	return nil
}
