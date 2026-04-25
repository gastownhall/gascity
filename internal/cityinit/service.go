package cityinit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// ServiceDeps contains the side-effecting operations Service needs from
// the binary layer while the scaffold/finalize body is still being split
// out of cmd/gc.
type ServiceDeps struct {
	FS              ScaffoldFS
	Initializer     Initializer
	Registry        Registry
	Reloader        SupervisorReloader
	LifecycleEvents LifecycleEvents
}

// RegisteredCity is the minimal registry view Service needs for
// asynchronous unregister.
type RegisteredCity struct {
	Name string
	Path string
}

// LifecycleEvents records durable city lifecycle events required by async
// clients. Implementations live at process edges so this package does not own
// stdout/stderr or event-log output sinks.
type LifecycleEvents interface {
	EnsureCityLog(cityPath string) error
	CityCreated(cityPath, name string) error
	CityUnregisterRequested(city RegisteredCity) error
}

// Service owns city scaffolding/finalization orchestration for both the
// CLI and HTTP projections.
type Service struct {
	deps ServiceDeps
}

// NewService constructs the concrete city-init service. Returns
// ErrNotWired if the universally required FS dependency is nil.
func NewService(deps ServiceDeps) (*Service, error) {
	if deps.FS == nil {
		return nil, fmt.Errorf("%w: FS is required", ErrNotWired)
	}
	return &Service{deps: deps}, nil
}

// ValidateInitRequest validates a city init request before side effects.
func (s *Service) ValidateInitRequest(req InitRequest) error {
	if req.Dir == "" {
		return fmt.Errorf("%w: dir is required", ErrInvalidDirectory)
	}
	if !filepath.IsAbs(req.Dir) {
		return fmt.Errorf("%w: dir must be absolute: %q", ErrInvalidDirectory, req.Dir)
	}
	if req.Provider == "" && req.StartCommand == "" {
		return fmt.Errorf("%w: provider or start_command required", ErrInvalidProvider)
	}
	if req.Provider != "" && req.StartCommand != "" {
		return fmt.Errorf("%w: provider and start_command are mutually exclusive", ErrInvalidProvider)
	}
	if req.Provider != "" {
		if !IsBuiltinProvider(req.Provider) {
			return fmt.Errorf("%w: unknown provider %q", ErrInvalidProvider, req.Provider)
		}
	}
	if req.BootstrapProfile != "" {
		if _, err := NormalizeBootstrapProfile(req.BootstrapProfile); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidBootstrapProfile, err)
		}
	}
	return nil
}

// Init scaffolds and finalizes a city synchronously.
func (s *Service) Init(ctx context.Context, req InitRequest) (*InitResult, error) {
	req = s.normalizeRequest(req)
	if err := s.ValidateInitRequest(req); err != nil {
		return nil, err
	}
	if err := s.validateInitDeps(); err != nil {
		return nil, err
	}
	if err := s.deps.FS.MkdirAll(req.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating directory %q: %w", req.Dir, err)
	}
	if s.hasScaffold(req.Dir) {
		return nil, ErrAlreadyInitialized
	}
	if err := s.deps.Initializer.Scaffold(ctx, req); err != nil {
		return nil, err
	}
	if err := s.deps.Initializer.Finalize(ctx, req); err != nil {
		return nil, err
	}
	return &InitResult{
		CityName:     s.resolveCityName(req.NameOverride, "", req.Dir),
		CityPath:     req.Dir,
		ProviderUsed: req.Provider,
	}, nil
}

// Scaffold writes the fast city scaffold, registers it with the
// supervisor, emits city.created, and returns without finalization.
func (s *Service) Scaffold(ctx context.Context, req InitRequest) (*InitResult, error) {
	req = s.normalizeRequest(req)
	if err := s.ValidateInitRequest(req); err != nil {
		return nil, err
	}
	if err := s.validateScaffoldDeps(); err != nil {
		return nil, err
	}
	dirExisted := false
	var rollbackState *scaffoldRollbackState
	if _, err := s.deps.FS.Stat(req.Dir); err == nil {
		dirExisted = true
		var snapshotErr error
		rollbackState, snapshotErr = newScaffoldRollbackState(s.deps.FS, req.Dir, s.managedPaths())
		if snapshotErr != nil {
			return nil, fmt.Errorf("snapshot rollback state for %q: %w", req.Dir, snapshotErr)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("stat directory %q: %w", req.Dir, err)
	}
	if err := s.deps.FS.MkdirAll(req.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating directory %q: %w", req.Dir, err)
	}
	if s.hasScaffold(req.Dir) {
		return nil, ErrAlreadyInitialized
	}
	if err := s.deps.Initializer.Scaffold(ctx, req); err != nil {
		return nil, rollbackScaffoldFailure(s.deps.FS, req.Dir, dirExisted, rollbackState, err)
	}

	cityName := s.resolveCityName(req.NameOverride, "", req.Dir)
	if err := s.lifecycleEvents().EnsureCityLog(req.Dir); err != nil {
		return nil, rollbackScaffoldFailure(s.deps.FS, req.Dir, dirExisted, rollbackState, fmt.Errorf("creating city event log: %w", err))
	}
	if dirExisted && rollbackState != nil {
		if err := rollbackState.markScaffoldState(s.deps.FS); err != nil {
			return nil, fmt.Errorf("snapshot scaffold state for %q: %w", req.Dir, err)
		}
	}

	if err := s.deps.Registry.Register(ctx, req.Dir, req.NameOverride); err != nil {
		if dirExisted {
			if rollbackState != nil {
				if cleanupErr := rollbackState.restore(s.deps.FS); cleanupErr != nil {
					return nil, errors.Join(fmt.Errorf("register with supervisor: %w", err), fmt.Errorf("restoring existing directory after failed registration: %w", cleanupErr))
				}
			}
		} else if cleanupErr := s.deps.FS.RemoveAll(req.Dir); cleanupErr != nil {
			return nil, errors.Join(fmt.Errorf("register with supervisor: %w", err), fmt.Errorf("cleaning scaffold after failed registration: %w", cleanupErr))
		}
		return nil, fmt.Errorf("register with supervisor: %w", err)
	}
	if err := s.lifecycleEvents().CityCreated(req.Dir, cityName); err != nil {
		return nil, fmt.Errorf("record city created event: %w", err)
	}
	if s.deps.Reloader != nil {
		_ = s.deps.Reloader.Reload()
	}

	return &InitResult{
		CityName:     cityName,
		CityPath:     req.Dir,
		ProviderUsed: req.Provider,
	}, nil
}

// Unregister removes a city from the supervisor registry and emits the
// start event used by async clients.
func (s *Service) Unregister(ctx context.Context, req UnregisterRequest) (*UnregisterResult, error) {
	name := strings.TrimSpace(req.CityName)
	if name == "" {
		return nil, fmt.Errorf("%w: city_name is required", ErrNotRegistered)
	}
	if s.deps.Registry == nil || s.deps.LifecycleEvents == nil {
		return nil, ErrNotWired
	}
	city, err := s.deps.Registry.Find(ctx, name)
	if err != nil {
		if errors.Is(err, ErrNotRegistered) {
			return nil, err
		}
		return nil, fmt.Errorf("reading supervisor registry: %w", err)
	}
	if err := s.deps.Registry.Unregister(ctx, city); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %q: %w", ErrNotRegistered, name, err)
		}
		return nil, fmt.Errorf("removing %q from supervisor registry: %w", name, err)
	}
	if err := s.lifecycleEvents().CityUnregisterRequested(city); err != nil {
		return nil, fmt.Errorf("record city unregister requested event: %w", err)
	}
	if s.deps.Reloader != nil {
		_ = s.deps.Reloader.ReloadAfterUnregister()
	}
	return &UnregisterResult{
		CityName: city.Name,
		CityPath: city.Path,
	}, nil
}

func (s *Service) normalizeRequest(req InitRequest) InitRequest {
	if req.ConfigName == "" {
		req.ConfigName = "tutorial"
	}
	return req
}

func (s *Service) hasScaffold(dir string) bool {
	return CityHasScaffoldFS(s.deps.FS, dir)
}

func (s *Service) validateInitDeps() error {
	if s.deps.Initializer == nil {
		return ErrNotWired
	}
	return nil
}

func (s *Service) validateScaffoldDeps() error {
	if s.deps.Initializer == nil ||
		s.deps.Registry == nil ||
		s.deps.LifecycleEvents == nil {
		return ErrNotWired
	}
	return nil
}

func (s *Service) resolveCityName(nameOverride, sourceName, dir string) string {
	return ResolveCityName(nameOverride, sourceName, dir)
}

func (s *Service) managedPaths() []string {
	return ManagedScaffoldPaths()
}

func (s *Service) lifecycleEvents() LifecycleEvents {
	return s.deps.LifecycleEvents
}

func rollbackScaffoldFailure(sfs ScaffoldFS, dir string, dirExisted bool, rollbackState *scaffoldRollbackState, err error) error {
	if dirExisted && rollbackState != nil {
		if markErr := rollbackState.markScaffoldState(sfs); markErr != nil {
			return errors.Join(err, fmt.Errorf("snapshot scaffold state for rollback: %w", markErr))
		}
		if cleanupErr := rollbackState.restore(sfs); cleanupErr != nil {
			return errors.Join(err, fmt.Errorf("restoring existing directory after scaffold failure: %w", cleanupErr))
		}
		return err
	}
	if !dirExisted {
		if cleanupErr := sfs.RemoveAll(dir); cleanupErr != nil {
			return errors.Join(err, fmt.Errorf("cleaning scaffold after failure: %w", cleanupErr))
		}
	}
	return err
}

type scaffoldRollbackEntry struct {
	mode       fs.FileMode
	data       []byte
	linkTarget string
}

type scaffoldSnapshot struct {
	root    string
	paths   []string
	entries map[string]scaffoldRollbackEntry
}

type scaffoldRollbackState struct {
	root   string
	paths  []string
	before map[string]scaffoldRollbackEntry
	after  map[string]scaffoldRollbackEntry
}

func newScaffoldRollbackState(sfs ScaffoldFS, root string, paths []string) (*scaffoldRollbackState, error) {
	snapshot, err := captureScaffoldSnapshot(sfs, root, paths)
	if err != nil {
		return nil, err
	}
	return &scaffoldRollbackState{
		root:   root,
		paths:  append([]string(nil), paths...),
		before: snapshot.entries,
	}, nil
}

func captureScaffoldSnapshot(sfs ScaffoldFS, root string, paths []string) (*scaffoldSnapshot, error) {
	snapshot := &scaffoldSnapshot{
		root:    root,
		paths:   append([]string(nil), paths...),
		entries: make(map[string]scaffoldRollbackEntry),
	}
	for _, rel := range paths {
		if err := snapshot.capture(sfs, rel); err != nil {
			return nil, err
		}
	}
	return snapshot, nil
}

func (s *scaffoldSnapshot) capture(sfs ScaffoldFS, rel string) error {
	abs := filepath.Join(s.root, rel)
	_, err := sfs.Lstat(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("snapshot %q: %w", abs, err)
	}
	return sfs.Walk(abs, func(path string, info fs.FileInfo, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("snapshot %q: %w", path, walkErr)
		}
		relPath, err := filepath.Rel(s.root, path)
		if err != nil {
			return fmt.Errorf("relative path for %q: %w", path, err)
		}
		entry := scaffoldRollbackEntry{mode: info.Mode()}
		if info.Mode()&fs.ModeSymlink != 0 {
			target, err := sfs.Readlink(path)
			if err != nil {
				return fmt.Errorf("readlink %q: %w", path, err)
			}
			entry.linkTarget = target
		} else if !info.IsDir() {
			data, err := sfs.ReadFile(path)
			if err != nil {
				return fmt.Errorf("read %q: %w", path, err)
			}
			entry.data = data
		}
		s.entries[filepath.Clean(relPath)] = entry
		return nil
	})
}

func (s *scaffoldRollbackState) markScaffoldState(sfs ScaffoldFS) error {
	snapshot, err := captureScaffoldSnapshot(sfs, s.root, s.paths)
	if err != nil {
		return err
	}
	s.after = snapshot.entries
	return nil
}

func rollbackEntryEqual(a, b scaffoldRollbackEntry) bool {
	return a.mode == b.mode && a.linkTarget == b.linkTarget && bytes.Equal(a.data, b.data)
}

func restoreRollbackEntry(sfs ScaffoldFS, abs string, entry scaffoldRollbackEntry) error {
	switch {
	case entry.mode.IsDir():
		return sfs.MkdirAll(abs, entry.mode.Perm())
	case entry.mode&fs.ModeSymlink != 0:
		if err := sfs.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return err
		}
		if err := sfs.Remove(abs); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return sfs.Symlink(entry.linkTarget, abs)
	default:
		if err := sfs.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return err
		}
		return sfs.WriteFile(abs, entry.data, entry.mode.Perm())
	}
}

func (s *scaffoldRollbackState) restore(sfs ScaffoldFS) error {
	current, err := captureScaffoldSnapshot(sfs, s.root, s.paths)
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
				if err := sfs.Remove(filepath.Join(s.root, rel)); err != nil && !errors.Is(err, fs.ErrNotExist) {
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
				if err := restoreRollbackEntry(sfs, filepath.Join(s.root, rel), before); err != nil {
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
		if err := restoreRollbackEntry(sfs, filepath.Join(s.root, rel), before); err != nil {
			errs = append(errs, fmt.Errorf("restore %q: %w", filepath.Join(s.root, rel), err))
		}
	}

	sort.Slice(createdDirs, func(i, j int) bool {
		return len(createdDirs[i]) > len(createdDirs[j])
	})
	for _, rel := range createdDirs {
		if err := sfs.Remove(filepath.Join(s.root, rel)); err != nil && !errors.Is(err, fs.ErrNotExist) {
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
