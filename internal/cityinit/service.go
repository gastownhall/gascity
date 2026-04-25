package cityinit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/gastownhall/gascity/internal/fsys"
)

// ServiceDeps contains the side-effecting operations Service needs from
// the binary layer while the scaffold/finalize body is still being split
// out of cmd/gc.
type ServiceDeps struct {
	DoInit                          func(ctx context.Context, req InitRequest) error
	Finalize                        func(ctx context.Context, req InitRequest) error
	RegisterCity                    func(ctx context.Context, dir, nameOverride string) error
	ReloadSupervisor                func()
	ReloadSupervisorAfterUnregister func()
	FindCity                        func(ctx context.Context, name string) (RegisteredCity, error)
	UnregisterCity                  func(ctx context.Context, city RegisteredCity) error
	LifecycleEvents                 LifecycleEvents
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

// NewService constructs the concrete city-init service.
func NewService(deps ServiceDeps) *Service {
	return &Service{deps: deps}
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
	if err := os.MkdirAll(req.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating directory %q: %w", req.Dir, err)
	}
	if s.hasScaffold(req.Dir) {
		return nil, ErrAlreadyInitialized
	}
	if err := s.deps.DoInit(ctx, req); err != nil {
		return nil, err
	}
	if err := s.deps.Finalize(ctx, req); err != nil {
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
	if _, err := os.Stat(req.Dir); err == nil {
		dirExisted = true
		var snapshotErr error
		rollbackState, snapshotErr = newScaffoldRollbackState(req.Dir, s.managedPaths())
		if snapshotErr != nil {
			return nil, fmt.Errorf("snapshot rollback state for %q: %w", req.Dir, snapshotErr)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("stat directory %q: %w", req.Dir, err)
	}
	if err := os.MkdirAll(req.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating directory %q: %w", req.Dir, err)
	}
	if s.hasScaffold(req.Dir) {
		return nil, ErrAlreadyInitialized
	}
	if err := s.deps.DoInit(ctx, req); err != nil {
		return nil, rollbackScaffoldFailure(req.Dir, dirExisted, rollbackState, err)
	}

	cityName := s.resolveCityName(req.NameOverride, "", req.Dir)
	if err := s.lifecycleEvents().EnsureCityLog(req.Dir); err != nil {
		return nil, rollbackScaffoldFailure(req.Dir, dirExisted, rollbackState, fmt.Errorf("creating city event log: %w", err))
	}
	if dirExisted && rollbackState != nil {
		if err := rollbackState.markScaffoldState(); err != nil {
			return nil, fmt.Errorf("snapshot scaffold state for %q: %w", req.Dir, err)
		}
	}

	if err := s.deps.RegisterCity(ctx, req.Dir, req.NameOverride); err != nil {
		if dirExisted {
			if rollbackState != nil {
				if cleanupErr := rollbackState.restore(); cleanupErr != nil {
					return nil, errors.Join(fmt.Errorf("register with supervisor: %w", err), fmt.Errorf("restoring existing directory after failed registration: %w", cleanupErr))
				}
			}
		} else if cleanupErr := os.RemoveAll(req.Dir); cleanupErr != nil {
			return nil, errors.Join(fmt.Errorf("register with supervisor: %w", err), fmt.Errorf("cleaning scaffold after failed registration: %w", cleanupErr))
		}
		return nil, fmt.Errorf("register with supervisor: %w", err)
	}
	if err := s.lifecycleEvents().CityCreated(req.Dir, cityName); err != nil {
		return nil, fmt.Errorf("record city created event: %w", err)
	}
	if s.deps.ReloadSupervisor != nil {
		s.deps.ReloadSupervisor()
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
	if s.deps.FindCity == nil || s.deps.UnregisterCity == nil {
		return nil, ErrNotWired
	}
	city, err := s.deps.FindCity(ctx, name)
	if err != nil {
		if errors.Is(err, ErrNotRegistered) {
			return nil, err
		}
		return nil, fmt.Errorf("reading supervisor registry: %w", err)
	}
	if err := s.deps.UnregisterCity(ctx, city); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %q: %w", ErrNotRegistered, name, err)
		}
		return nil, fmt.Errorf("removing %q from supervisor registry: %w", name, err)
	}
	if err := s.lifecycleEvents().CityUnregisterRequested(city); err != nil {
		return nil, fmt.Errorf("record city unregister requested event: %w", err)
	}
	if s.deps.ReloadSupervisorAfterUnregister != nil {
		s.deps.ReloadSupervisorAfterUnregister()
	} else if s.deps.ReloadSupervisor != nil {
		s.deps.ReloadSupervisor()
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
	return CityHasScaffoldFS(fsys.OSFS{}, dir)
}

func (s *Service) validateInitDeps() error {
	if s.deps.DoInit == nil ||
		s.deps.Finalize == nil {
		return ErrNotWired
	}
	return nil
}

func (s *Service) validateScaffoldDeps() error {
	if s.deps.DoInit == nil ||
		s.deps.RegisterCity == nil {
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
	if s.deps.LifecycleEvents != nil {
		return s.deps.LifecycleEvents
	}
	return noopLifecycleEvents{}
}

func rollbackScaffoldFailure(dir string, dirExisted bool, rollbackState *scaffoldRollbackState, err error) error {
	if dirExisted && rollbackState != nil {
		if markErr := rollbackState.markScaffoldState(); markErr != nil {
			return errors.Join(err, fmt.Errorf("snapshot scaffold state for rollback: %w", markErr))
		}
		if cleanupErr := rollbackState.restore(); cleanupErr != nil {
			return errors.Join(err, fmt.Errorf("restoring existing directory after scaffold failure: %w", cleanupErr))
		}
		return err
	}
	if !dirExisted {
		if cleanupErr := os.RemoveAll(dir); cleanupErr != nil {
			return errors.Join(err, fmt.Errorf("cleaning scaffold after failure: %w", cleanupErr))
		}
	}
	return err
}

type scaffoldRollbackEntry struct {
	mode       os.FileMode
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

func newScaffoldRollbackState(root string, paths []string) (*scaffoldRollbackState, error) {
	snapshot, err := captureScaffoldSnapshot(root, paths)
	if err != nil {
		return nil, err
	}
	return &scaffoldRollbackState{
		root:   root,
		paths:  append([]string(nil), paths...),
		before: snapshot.entries,
	}, nil
}

func captureScaffoldSnapshot(root string, paths []string) (*scaffoldSnapshot, error) {
	snapshot := &scaffoldSnapshot{
		root:    root,
		paths:   append([]string(nil), paths...),
		entries: make(map[string]scaffoldRollbackEntry),
	}
	for _, rel := range paths {
		if err := snapshot.capture(rel); err != nil {
			return nil, err
		}
	}
	return snapshot, nil
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
	snapshot, err := captureScaffoldSnapshot(s.root, s.paths)
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
	current, err := captureScaffoldSnapshot(s.root, s.paths)
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

type noopLifecycleEvents struct{}

func (noopLifecycleEvents) EnsureCityLog(string) error { return nil }

func (noopLifecycleEvents) CityCreated(string, string) error { return nil }

func (noopLifecycleEvents) CityUnregisterRequested(RegisteredCity) error { return nil }
