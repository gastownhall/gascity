package sqlite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storebinding"
)

var (
	// ErrLegacyCombinedSourceNotFound reports that the historical the legacy combined layout
	// combined source is absent from its one supported location.
	ErrLegacyCombinedSourceNotFound = errors.New("legacy combined SQLite source not found")
	// ErrLegacyCombinedSourceChanged reports a source that changed while its
	// temporary read-only snapshot was being made.
	ErrLegacyCombinedSourceChanged = errors.New("legacy combined SQLite source changed during snapshot")
	// ErrLegacyCombinedSourceClosed reports reads attempted after Close.
	ErrLegacyCombinedSourceClosed = errors.New("legacy combined SQLite source is closed")

	makeLegacyCombinedSnapshotTempDir = makeRecoverableSQLiteSnapshotTempDir
	removeLegacyCombinedSnapshotRoot  = os.RemoveAll
)

const (
	legacyCombinedDatabaseFilename = "beads.sqlite"
	legacyCombinedSnapshotPrefix   = "gascity-legacy-combined-"
	legacyCombinedComponentID      = storebinding.ComponentID("legacy-combined-source")
	legacyCombinedFenceProjection  = storebinding.FenceProjection("sqlite.legacy-combined.snapshot")
)

// LegacyCombinedSourceDir returns the only recognized historical combined
// source directory: <city>/.gc/infra/.beads. It deliberately does not fall
// back to older generic .gc/beads.sqlite paths.
func LegacyCombinedSourceDir(cityRoot string) (string, error) {
	if cityRoot == "" {
		return "", fmt.Errorf("resolving legacy combined source: empty city root")
	}
	absCity, err := filepath.Abs(cityRoot)
	if err != nil {
		return "", fmt.Errorf("resolving legacy combined source: %w", err)
	}
	return filepath.Join(filepath.Clean(absCity), ".gc", "infra", ".beads"), nil
}

// LegacyCombinedRecord is one retained historical bead paired with its
// canonical semantic owner. IDs and all bead fields remain unchanged.
type LegacyCombinedRecord struct {
	Class coordclass.Class
	Bead  beads.Bead
}

// LegacyCombinedSource is a read-only snapshot adapter for the historical
// the legacy combined layout combined source. It is migration-only: it never exposes a writable
// store or becomes an active binding.
type LegacyCombinedSource struct {
	store       *beads.SQLiteStore
	snapshotDir string
	closeStore  func(*beads.SQLiteStore) error
	removeDir   func(string) error

	mu             sync.RWMutex
	closed         bool
	cleanupPending bool
}

// OpenLegacyCombinedSource opens a stable private snapshot of the exact
// historical source. The caller supplies the once-per-city guard whose
// generation is expected by this migration. No SQLite connection is opened
// against the source itself: the snapshot is made only while a concrete
// claim-owning SQLite writer fence is held.
func OpenLegacyCombinedSource(ctx context.Context, cityRoot string, guard storebinding.MigrationGuard, generation storebinding.Generation) (*LegacyCombinedSource, error) {
	return openLegacyCombinedSource(ctx, cityRoot, guard, generation, nil)
}

func openLegacyCombinedSource(ctx context.Context, cityRoot string, guard storebinding.MigrationGuard, generation storebinding.Generation, beforeCopy func()) (*LegacyCombinedSource, error) {
	request, err := newLegacyCombinedFenceRequest(ctx, cityRoot, generation)
	if err != nil {
		return nil, err
	}
	fence, err := storebinding.AcquireWriterFence(ctx, guard, legacyCombinedFenceAcquirer{beforeCopy: beforeCopy}, request)
	if err != nil {
		return nil, fmt.Errorf("acquiring legacy combined writer fence: %w", err)
	}
	source, snapshotErr := openLegacyCombinedSnapshot(ctx, fence)
	releaseErr := fence.Release(ctx)
	if snapshotErr != nil {
		if releaseErr != nil {
			return nil, errors.Join(snapshotErr, fmt.Errorf("releasing legacy combined writer fence: %w", releaseErr))
		}
		return nil, snapshotErr
	}
	if releaseErr != nil {
		return nil, errors.Join(
			fmt.Errorf("releasing legacy combined writer fence: %w", releaseErr),
			source.Close(),
		)
	}
	return source, nil
}

func newLegacyCombinedFenceRequest(ctx context.Context, cityRoot string, generation storebinding.Generation) (storebinding.FenceRequest, error) {
	sourceDir, err := LegacyCombinedSourceDir(cityRoot)
	if err != nil {
		return storebinding.FenceRequest{}, err
	}
	databasePath := filepath.Join(sourceDir, legacyCombinedDatabaseFilename)
	if err := ensureNoSQLiteSourceDescriptors(databasePath); err != nil {
		return storebinding.FenceRequest{}, fmt.Errorf("opening legacy combined source: %w", err)
	}
	if _, err := os.Stat(sourceDir); errors.Is(err, os.ErrNotExist) {
		return storebinding.FenceRequest{}, ErrLegacyCombinedSourceNotFound
	} else if err != nil {
		return storebinding.FenceRequest{}, fmt.Errorf("stating legacy combined source: %w", err)
	}
	state, err := captureLegacyCombinedSourceContext(ctx, sourceDir)
	if err != nil {
		return storebinding.FenceRequest{}, err
	}
	if !state.database.Present {
		return storebinding.FenceRequest{}, ErrLegacyCombinedSourceNotFound
	}
	if !state.database.Mode.IsRegular() {
		return storebinding.FenceRequest{}, fmt.Errorf("opening legacy combined source: database is not a regular file")
	}
	target, err := newLegacyCombinedTarget(databasePath, state)
	if err != nil {
		return storebinding.FenceRequest{}, err
	}
	cityGCDir := filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(sourceDir))), ".gc")
	scope, err := storebinding.NewMigrationGuardScope(cityGCDir)
	if err != nil {
		return storebinding.FenceRequest{}, fmt.Errorf("opening legacy combined source: %w", err)
	}
	return storebinding.FenceRequest{
		Target:             target,
		GuardScope:         scope,
		ExpectedGeneration: generation,
		Components:         []storebinding.ComponentID{legacyCombinedComponentID},
		Role:               storebinding.FenceRoleSource,
	}, nil
}

type legacyCombinedFenceAcquirer struct{ beforeCopy func() }

func (a legacyCombinedFenceAcquirer) AcquireFence(ctx context.Context, claim storebinding.MigrationGuardClaim, request storebinding.FenceRequest) (storebinding.WriterFence, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, targetComponent, err := legacyCombinedTargetPath(request.Target)
	if err != nil {
		return nil, err
	}
	if len(request.Components) != 1 || request.Components[0] != legacyCombinedComponentID {
		return nil, fmt.Errorf("opening legacy combined source: request must reserve the historical source")
	}
	if err := validateSQLiteMigrationGuardClaim(claim); err != nil {
		return nil, fmt.Errorf("opening legacy combined source: %w", err)
	}
	if err := ensureNoSQLiteSourceDescriptors(path); err != nil {
		return nil, fmt.Errorf("opening legacy combined source: %w", err)
	}
	sourceDir := filepath.Dir(path)
	before, err := captureLegacyCombinedSourceContext(ctx, sourceDir)
	if err != nil {
		return nil, err
	}
	if !before.database.Present || !before.database.Mode.IsRegular() || legacyCombinedComponentIdentity(path, before) != targetComponent.PhysicalIdentity {
		return nil, &storebinding.FenceTargetMovedError{Component: legacyCombinedComponentID}
	}
	reservation, err := acquireSQLiteWriterReservation(ctx, path, claim, sqliteWriterReservationSource{
		directory:     before.directory.Identity,
		database:      before.database.Identity,
		wal:           before.files[legacyCombinedDatabaseFilename+"-wal"].Identity,
		shm:           before.files[legacyCombinedDatabaseFilename+"-shm"].Identity,
		journal:       before.files[legacyCombinedDatabaseFilename+"-journal"].Identity,
		sequenceFloor: before.files[graphSequenceFloorFilename].Identity,
	})
	if err != nil {
		if errors.Is(err, errSQLiteSourceChanged) {
			err = &storebinding.FenceTargetMovedError{Component: legacyCombinedComponentID}
		}
		if reservation != nil {
			return &legacyCombinedFence{
				target:         request.Target.Clone(),
				components:     append([]storebinding.ComponentID(nil), request.Components...),
				role:           request.Role,
				generation:     request.ExpectedGeneration,
				claim:          claim,
				reservation:    reservation,
				cleanupPending: true,
			}, fmt.Errorf("opening legacy combined source: %w", err)
		}
		return nil, fmt.Errorf("opening legacy combined source: %w", err)
	}
	locked, err := captureLegacyCombinedSourceContext(ctx, sourceDir)
	if err != nil {
		return releaseIncompleteLegacyCombinedReservation(request, claim, reservation, err)
	}
	if !before.equalForFence(locked) {
		return releaseIncompleteLegacyCombinedReservation(request, claim, reservation, ErrLegacyCombinedSourceChanged)
	}
	return &legacyCombinedFence{
		target:      request.Target.Clone(),
		components:  append([]storebinding.ComponentID(nil), request.Components...),
		role:        request.Role,
		generation:  request.ExpectedGeneration,
		claim:       claim,
		reservation: reservation,
		sourceDir:   sourceDir,
		state:       before,
		beforeCopy:  a.beforeCopy,
	}, nil
}

func releaseIncompleteLegacyCombinedReservation(request storebinding.FenceRequest, claim storebinding.MigrationGuardClaim, reservation sqliteWriterReservation, cause error) (storebinding.WriterFence, error) {
	if err := reservation.Release(); err != nil {
		return &legacyCombinedFence{
			target:         request.Target.Clone(),
			components:     append([]storebinding.ComponentID(nil), request.Components...),
			role:           request.Role,
			generation:     request.ExpectedGeneration,
			claim:          claim,
			reservation:    reservation,
			cleanupPending: true,
		}, errors.Join(cause, fmt.Errorf("releasing incomplete legacy combined writer reservation: %w", err))
	}
	return nil, cause
}

func legacyCombinedClasses() (storebinding.ClassSet, error) {
	return storebinding.NewClassSet(coordclass.Classes()...)
}

func newLegacyCombinedTarget(path string, state legacyCombinedState) (storebinding.FenceTarget, error) {
	classes, err := legacyCombinedClasses()
	if err != nil {
		return storebinding.FenceTarget{}, err
	}
	locator, err := graphLocator(path)
	if err != nil {
		return storebinding.FenceTarget{}, err
	}
	return storebinding.NewFenceTarget(ProviderID, classes, []storebinding.FenceComponentTarget{{
		ID:               legacyCombinedComponentID,
		Locator:          locator,
		PhysicalIdentity: legacyCombinedComponentIdentity(path, state),
		Classes:          classes,
	}})
}

func legacyCombinedComponentIdentity(path string, state legacyCombinedState) storebinding.PhysicalIdentity {
	database := state.database.Identity
	if !state.database.Present {
		return absentIdentity(path)
	}
	directory := legacyCombinedFileIdentity(state.directory)
	floor := legacyCombinedFileIdentity(state.files[graphSequenceFloorFilename]) + "\x00" + state.sequenceFloor.identity()
	sum := sha256.Sum256([]byte("gascity.sqlite.legacy-combined.v3\x00" + string(database) + "\x00" + directory + "\x00" + floor))
	return storebinding.PhysicalIdentity("sha256:" + hex.EncodeToString(sum[:]))
}

func legacyCombinedFileIdentity(file legacyCombinedFileState) string {
	if !file.Present {
		return "absent"
	}
	return strings.Join([]string{
		"present",
		string(file.Identity),
		file.Mode.String(),
		fmt.Sprint(file.Size),
		fmt.Sprint(file.ModTime.UnixNano()),
		file.Hash,
	}, "\x00")
}

func legacyCombinedTargetPath(target storebinding.FenceTarget) (string, storebinding.FenceComponentTarget, error) {
	if err := target.Validate(); err != nil {
		return "", storebinding.FenceComponentTarget{}, err
	}
	classes, err := legacyCombinedClasses()
	if err != nil {
		return "", storebinding.FenceComponentTarget{}, err
	}
	if target.Provider != ProviderID || !target.Classes.Equal(classes) || len(target.Components) != 1 {
		return "", storebinding.FenceComponentTarget{}, fmt.Errorf("opening legacy combined source: invalid fence target")
	}
	component := target.Components[0]
	if component.ID != legacyCombinedComponentID || !component.Classes.Equal(classes) {
		return "", storebinding.FenceComponentTarget{}, fmt.Errorf("opening legacy combined source: invalid fence target")
	}
	parsed, err := url.Parse(string(component.Locator))
	if err != nil || parsed.Scheme != "file" || parsed.Host != "" || parsed.RawQuery != "" || parsed.Path == "" {
		return "", storebinding.FenceComponentTarget{}, fmt.Errorf("opening legacy combined source: invalid fence target")
	}
	path := filepath.Clean(parsed.Path)
	if !filepath.IsAbs(path) || filepath.Base(path) != legacyCombinedDatabaseFilename || filepath.Base(filepath.Dir(path)) != ".beads" || filepath.Base(filepath.Dir(filepath.Dir(path))) != "infra" || filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(path)))) != ".gc" {
		return "", storebinding.FenceComponentTarget{}, fmt.Errorf("opening legacy combined source: invalid fence target")
	}
	canonicalPath, err := canonicalPath(path)
	if err != nil || canonicalPath != path {
		return "", storebinding.FenceComponentTarget{}, fmt.Errorf("opening legacy combined source: invalid fence target")
	}
	return path, component, nil
}

type legacyCombinedFence struct {
	target      storebinding.FenceTarget
	components  []storebinding.ComponentID
	role        storebinding.FenceRole
	generation  storebinding.Generation
	claim       storebinding.MigrationGuardClaim
	reservation sqliteWriterReservation
	sourceDir   string
	state       legacyCombinedState
	beforeCopy  func()

	mu             sync.Mutex
	released       bool
	cleanupPending bool
}

func (f *legacyCombinedFence) Target() storebinding.FenceTarget { return f.target.Clone() }

func (f *legacyCombinedFence) CoveredComponents() []storebinding.ComponentID {
	return append([]storebinding.ComponentID(nil), f.components...)
}

func (f *legacyCombinedFence) Role() storebinding.FenceRole { return f.role }

func (f *legacyCombinedFence) Generation() storebinding.Generation { return f.generation }

func (f *legacyCombinedFence) Held(context.Context) (bool, error) {
	if f == nil {
		return false, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.released && !f.cleanupPending && f.reservation != nil && f.claim.Held(), nil
}

func openLegacyCombinedSnapshot(ctx context.Context, fence storebinding.WriterFence) (*LegacyCombinedSource, error) {
	operation := &legacyCombinedSnapshotOperation{}
	if err := storebinding.InspectProviderFence(ctx, fence, operation); err != nil {
		return nil, err
	}
	return operation.source, nil
}

func (f *legacyCombinedFence) openSnapshotHeld(ctx context.Context) (source *LegacyCombinedSource, returnErr error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	before, err := captureLegacyCombinedSourceContext(ctx, f.sourceDir)
	if err != nil {
		return nil, err
	}
	if !before.equalForFence(f.state) {
		return nil, ErrLegacyCombinedSourceChanged
	}
	if f.beforeCopy != nil {
		f.beforeCopy()
	}
	preCopy, err := captureLegacyCombinedSourceContext(ctx, f.sourceDir)
	if err != nil {
		return nil, err
	}
	if !preCopy.equalForFence(f.state) {
		return nil, ErrLegacyCombinedSourceChanged
	}

	snapshotRoot, err := makeLegacyCombinedSnapshotTempDir("", legacyCombinedSnapshotPrefix)
	if err != nil {
		return nil, fmt.Errorf("creating legacy combined snapshot: %w", err)
	}
	observeSQLiteBoundary("legacy-snapshot-root-created")
	defer func() {
		if source != nil {
			return
		}
		if err := removeLegacyCombinedSnapshotRoot(snapshotRoot); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("removing legacy combined snapshot: %w", err))
		}
	}()
	snapshotDir := filepath.Join(snapshotRoot, ".beads")
	observeSQLiteBoundary("legacy-snapshot-copy-before")
	if err := copyLegacyCombinedSnapshot(ctx, snapshotDir, f.state, f.reservation.snapshotFiles()); err != nil {
		if errors.Is(err, errSQLiteSourceChanged) || errors.Is(err, os.ErrNotExist) {
			return nil, ErrLegacyCombinedSourceChanged
		}
		return nil, err
	}
	observeSQLiteBoundary("legacy-snapshot-copy-after")
	after, err := captureLegacyCombinedSourceContext(ctx, f.sourceDir)
	if err != nil {
		return nil, err
	}
	if !after.equalForFence(f.state) {
		return nil, ErrLegacyCombinedSourceChanged
	}

	// Recovery may need to rebuild a derived WAL index or roll back a hot
	// journal, so first open the private disposable copy read-write. Nothing
	// returned to callers retains that capability.
	observeSQLiteBoundary("legacy-private-recovery-before")
	opened, err := beads.OpenSQLiteStore(
		snapshotDir,
		beads.WithSQLiteStorePrivateRecovery(),
		beads.WithSQLiteStoreIDPrefix("gcg"),
	)
	if err != nil {
		return nil, fmt.Errorf("opening legacy combined snapshot: %w", err)
	}
	recoveryStore, ok := opened.(*beads.SQLiteStore)
	if !ok {
		unexpected := fmt.Errorf("opening legacy combined snapshot: unexpected store type %T", opened)
		if closer, isCloser := opened.(interface{ CloseStore() error }); isCloser {
			if err := closer.CloseStore(); err != nil {
				unexpected = errors.Join(unexpected, fmt.Errorf("closing unexpected legacy combined snapshot store: %w", err))
			}
		}
		return nil, unexpected
	}
	if err := recoveryStore.CloseStore(); err != nil {
		return nil, fmt.Errorf("recovering legacy combined snapshot: %w", err)
	}
	observeSQLiteBoundary("legacy-private-recovery-after")

	opened, err = beads.OpenSQLiteStore(snapshotDir, beads.WithSQLiteStoreReadOnly(), beads.WithSQLiteStoreIDPrefix("gcg"))
	if err != nil {
		return nil, fmt.Errorf("reopening recovered legacy combined snapshot read-only: %w", err)
	}
	store, ok := opened.(*beads.SQLiteStore)
	if !ok {
		unexpected := fmt.Errorf("reopening recovered legacy combined snapshot: unexpected store type %T", opened)
		if closer, isCloser := opened.(interface{ CloseStore() error }); isCloser {
			if err := closer.CloseStore(); err != nil {
				unexpected = errors.Join(unexpected, fmt.Errorf("closing unexpected recovered legacy combined snapshot store: %w", err))
			}
		}
		return nil, unexpected
	}
	source = &LegacyCombinedSource{store: store, snapshotDir: snapshotRoot}
	return source, nil
}

type legacyCombinedSnapshotOperation struct {
	source *LegacyCombinedSource
}

func (*legacyCombinedSnapshotOperation) FenceProjection() storebinding.FenceProjection {
	return legacyCombinedFenceProjection
}

// ExecuteProviderFenceOperation runs one nonescaping legacy snapshot while
// this fence's reservation and migration claim are held.
func (f *legacyCombinedFence) ExecuteProviderFenceOperation(ctx context.Context, projection storebinding.FenceProjection, operation storebinding.ProviderFenceOperation) error {
	if f == nil || projection != legacyCombinedFenceProjection {
		return storebinding.ErrInvalidFence
	}
	snapshot, ok := operation.(*legacyCombinedSnapshotOperation)
	if !ok || snapshot == nil {
		return storebinding.ErrInvalidFence
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.released || f.cleanupPending || f.reservation == nil || !f.claim.Held() {
		return storebinding.ErrFenceNotHeld
	}
	source, err := f.openSnapshotHeld(ctx)
	if err != nil {
		return err
	}
	snapshot.source = source
	return nil
}

func (f *legacyCombinedFence) Release(context.Context) error {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.released {
		return nil
	}
	if f.reservation != nil {
		if err := f.reservation.Release(); err != nil {
			f.cleanupPending = true
			return fmt.Errorf("releasing legacy combined writer fence: %w", err)
		}
		f.reservation = nil
	}
	observeSQLiteBoundary("legacy-claim-release-before")
	if err := f.claim.Release(); err != nil {
		f.cleanupPending = true
		return fmt.Errorf("releasing legacy combined migration guard claim: %w", err)
	}
	observeSQLiteBoundary("legacy-claim-release-after")
	f.cleanupPending = false
	f.released = true
	return nil
}

// ReadAll returns every retained source bead together with its owner selected
// by coordclass.Classify. Dependencies are loaded from the historic child table
// so imports preserve edges even when bead_json omitted them.
func (s *LegacyCombinedSource) ReadAll() ([]LegacyCombinedRecord, error) {
	if s == nil {
		return nil, ErrLegacyCombinedSourceClosed
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed || s.cleanupPending || s.store == nil {
		return nil, ErrLegacyCombinedSourceClosed
	}
	beadsInSource, err := s.store.List(beads.ListQuery{
		AllowScan:     true,
		IncludeClosed: true,
		TierMode:      beads.TierBoth,
		Sort:          beads.SortCreatedAsc,
	})
	if err != nil {
		return nil, fmt.Errorf("listing legacy combined source: %w", err)
	}
	records := make([]LegacyCombinedRecord, 0, len(beadsInSource))
	for _, bead := range beadsInSource {
		dependencies, err := s.store.DepList(bead.ID, "down")
		if err != nil {
			return nil, fmt.Errorf("reading legacy combined dependencies for %q: %w", bead.ID, err)
		}
		sort.Slice(dependencies, func(i, j int) bool {
			if dependencies[i].DependsOnID != dependencies[j].DependsOnID {
				return dependencies[i].DependsOnID < dependencies[j].DependsOnID
			}
			return dependencies[i].Type < dependencies[j].Type
		})
		bead.Dependencies = dependencies
		bead.Needs = nil
		records = append(records, LegacyCombinedRecord{Class: coordclass.Classify(bead), Bead: bead})
	}
	return records, nil
}

// ReadClass returns the retained source beads belonging to one semantic class.
func (s *LegacyCombinedSource) ReadClass(class coordclass.Class) ([]beads.Bead, error) {
	if !knownCoordClass(class) {
		return nil, fmt.Errorf("reading legacy combined source: unknown class %q", class)
	}
	records, err := s.ReadAll()
	if err != nil {
		return nil, err
	}
	result := make([]beads.Bead, 0, len(records))
	for _, record := range records {
		if record.Class == class {
			result = append(result, record.Bead)
		}
	}
	return result, nil
}

// Close releases the temporary snapshot. A failed close remains cleanup-pending
// and is retried on the next call; only a successful complete cleanup is
// cached as closed.
func (s *LegacyCombinedSource) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.cleanupPending = true
	if s.store != nil {
		closeStore := s.closeStore
		if closeStore == nil {
			closeStore = func(store *beads.SQLiteStore) error { return store.CloseStore() }
		}
		if err := closeStore(s.store); err != nil {
			return fmt.Errorf("closing legacy combined snapshot store: %w", err)
		}
		s.store = nil
	}
	if s.snapshotDir != "" {
		removeDir := s.removeDir
		if removeDir == nil {
			removeDir = os.RemoveAll
		}
		if err := removeDir(s.snapshotDir); err != nil {
			return fmt.Errorf("removing legacy combined snapshot: %w", err)
		}
		s.snapshotDir = ""
	}
	s.cleanupPending = false
	s.closed = true
	return nil
}

func knownCoordClass(want coordclass.Class) bool {
	for _, class := range coordclass.Classes() {
		if class == want {
			return true
		}
	}
	return false
}

type legacyCombinedState struct {
	directory     legacyCombinedFileState
	entries       []string
	files         map[string]legacyCombinedFileState
	database      legacyCombinedFileState
	sequenceFloor sqliteSequenceFloorState
}

type legacyCombinedFileState struct {
	Present  bool
	Mode     os.FileMode
	Size     int64
	ModTime  time.Time
	Hash     string
	Identity storebinding.PhysicalIdentity
}

// equal compares every captured source fact, including the mutable WAL-index
// read marks that equalForFence deliberately tolerates. It is the strict
// mutation-free census comparator: a source with no concurrent activity must
// satisfy it, and it is what proves the relaxed fenced comparator is actually
// relaxing something the census can see.
func (s legacyCombinedState) equal(other legacyCombinedState) bool {
	if s.directory != other.directory || s.sequenceFloor != other.sequenceFloor || len(s.files) != len(other.files) || len(s.entries) != len(other.entries) {
		return false
	}
	for index := range s.entries {
		if s.entries[index] != other.entries[index] {
			return false
		}
	}
	for name, file := range s.files {
		if other.files[name] != file {
			return false
		}
	}
	return true
}

// equalForFence ignores mutable read marks in the WAL-index payload while
// retaining the index entry, mode, and inode as fenced namespace facts.
func (s legacyCombinedState) equalForFence(other legacyCombinedState) bool {
	if s.directory != other.directory || s.sequenceFloor != other.sequenceFloor || len(s.files) != len(other.files) || len(s.entries) != len(other.entries) {
		return false
	}
	for index := range s.entries {
		if s.entries[index] != other.entries[index] {
			return false
		}
	}
	for name, file := range s.files {
		otherFile, ok := other.files[name]
		if !ok {
			return false
		}
		if name == legacyCombinedDatabaseFilename+"-shm" {
			if file.Present != otherFile.Present ||
				file.Mode != otherFile.Mode ||
				file.Size != otherFile.Size ||
				file.Hash != otherFile.Hash ||
				file.Identity != otherFile.Identity {
				return false
			}
			continue
		}
		if otherFile != file {
			return false
		}
	}
	return true
}

func captureLegacyCombinedSource(directory string) (legacyCombinedState, error) {
	return captureLegacyCombinedSourceContext(context.Background(), directory)
}

func captureLegacyCombinedSourceContext(ctx context.Context, directory string) (legacyCombinedState, error) {
	if err := ctx.Err(); err != nil {
		return legacyCombinedState{}, err
	}
	info, err := os.Stat(directory)
	if err != nil {
		return legacyCombinedState{}, fmt.Errorf("stating legacy combined source directory: %w", err)
	}
	if !info.IsDir() {
		return legacyCombinedState{}, fmt.Errorf("inspecting legacy combined source: source path is not a directory")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return legacyCombinedState{}, fmt.Errorf("reading legacy combined source directory: %w", err)
	}
	state := legacyCombinedState{
		directory: legacyCombinedFileState{
			Present:  true,
			Mode:     info.Mode(),
			Size:     info.Size(),
			ModTime:  info.ModTime(),
			Identity: physicalIdentity(directory, info),
		},
		files: make(map[string]legacyCombinedFileState, len(entries)),
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return legacyCombinedState{}, err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return legacyCombinedState{}, fmt.Errorf("inspecting legacy combined source: symbolic-link entry %q is unsupported", entry.Name())
		}
		fileInfo, err := entry.Info()
		if err != nil {
			return legacyCombinedState{}, fmt.Errorf("stating legacy combined source entry %q: %w", entry.Name(), err)
		}
		if (legacyCombinedSQLiteComponent(entry.Name()) || entry.Name() == graphSequenceFloorFilename) && fileInfo.Mode().IsRegular() && platformFileHasMultipleLinks(fileInfo) {
			return legacyCombinedState{}, fmt.Errorf("inspecting legacy combined source: hard-linked SQLite component %q is unsupported", entry.Name())
		}
		path := filepath.Join(directory, entry.Name())
		var file legacyCombinedFileState
		switch entry.Name() {
		case graphSequenceFloorFilename:
			file, state.sequenceFloor, err = captureLegacyCombinedSequenceFloorFileContext(ctx, path, fileInfo)
		case legacyCombinedDatabaseFilename + "-shm":
			file, err = captureLegacyCombinedSHMFileContext(ctx, path, fileInfo)
		default:
			file, err = captureLegacyCombinedFileContext(ctx, path, fileInfo)
		}
		if err != nil {
			return legacyCombinedState{}, err
		}
		state.entries = append(state.entries, entry.Name())
		state.files[entry.Name()] = file
	}
	sort.Strings(state.entries)
	state.database = state.files[legacyCombinedDatabaseFilename]
	return state, nil
}

func legacyCombinedSQLiteComponent(name string) bool {
	switch name {
	case legacyCombinedDatabaseFilename,
		legacyCombinedDatabaseFilename + "-wal",
		legacyCombinedDatabaseFilename + "-shm",
		legacyCombinedDatabaseFilename + "-journal":
		return true
	default:
		return false
	}
}

func captureLegacyCombinedSHMFileContext(ctx context.Context, path string, info os.FileInfo) (legacyCombinedFileState, error) {
	file := legacyCombinedFileState{
		Present: true,
		Mode:    info.Mode(),
		Size:    info.Size(),
		ModTime: info.ModTime(),
	}
	if !info.Mode().IsRegular() {
		return file, nil
	}
	hash, err := hashPinnedSQLiteSHMStableBytes(ctx, path, info)
	if err != nil {
		return legacyCombinedFileState{}, fmt.Errorf("reading legacy combined source entry: %w", err)
	}
	file.Hash = hash
	file.Identity = physicalIdentity(path, info)
	return file, nil
}

func captureLegacyCombinedFileContext(ctx context.Context, path string, info os.FileInfo) (file legacyCombinedFileState, returnErr error) {
	if err := ctx.Err(); err != nil {
		return legacyCombinedFileState{}, err
	}
	file = legacyCombinedFileState{
		Present: true,
		Mode:    info.Mode(),
		Size:    info.Size(),
		ModTime: info.ModTime(),
	}
	if !info.Mode().IsRegular() {
		return file, nil
	}
	opened, err := os.Open(path)
	if err != nil {
		return legacyCombinedFileState{}, fmt.Errorf("reading legacy combined source entry: %w", err)
	}
	defer func() {
		if err := closeSQLiteCensusFile(opened); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("closing legacy combined source census descriptor: %w", err))
		}
	}()
	openedInfo, err := opened.Stat()
	if err != nil {
		return legacyCombinedFileState{}, fmt.Errorf("stating legacy combined source entry: %w", err)
	}
	if !sameLegacyCombinedCensusFile(info, openedInfo) {
		return legacyCombinedFileState{}, fmt.Errorf("reading legacy combined source entry: %w", errSQLiteSourceChanged)
	}
	file.Hash, err = hashSQLiteSourceFile(ctx, opened)
	if err != nil {
		return legacyCombinedFileState{}, fmt.Errorf("reading legacy combined source entry: %w", err)
	}
	finalInfo, err := opened.Stat()
	if err != nil {
		return legacyCombinedFileState{}, fmt.Errorf("restating legacy combined source entry: %w", err)
	}
	if !sameLegacyCombinedCensusFile(info, finalInfo) {
		return legacyCombinedFileState{}, fmt.Errorf("reading legacy combined source entry: %w", errSQLiteSourceChanged)
	}
	file.Identity = physicalIdentity(path, info)
	return file, nil
}

func captureLegacyCombinedSequenceFloorFileContext(ctx context.Context, path string, info os.FileInfo) (file legacyCombinedFileState, floor sqliteSequenceFloorState, returnErr error) {
	if err := ctx.Err(); err != nil {
		return legacyCombinedFileState{}, sqliteSequenceFloorState{}, err
	}
	if !info.Mode().IsRegular() {
		return legacyCombinedFileState{}, sqliteSequenceFloorState{}, fmt.Errorf("inspecting SQLite sequence floor: not a regular file")
	}
	file = legacyCombinedFileState{
		Present: true,
		Mode:    info.Mode(),
		Size:    info.Size(),
		ModTime: info.ModTime(),
	}
	opened, err := os.Open(path)
	if err != nil {
		return legacyCombinedFileState{}, sqliteSequenceFloorState{}, fmt.Errorf("reading legacy combined source entry: %w", err)
	}
	defer func() {
		if err := closeSQLiteCensusFile(opened); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("closing legacy combined sequence-floor census descriptor: %w", err))
		}
	}()
	openedInfo, err := opened.Stat()
	if err != nil {
		return legacyCombinedFileState{}, sqliteSequenceFloorState{}, fmt.Errorf("stating legacy combined source entry: %w", err)
	}
	if !sameLegacyCombinedCensusFile(info, openedInfo) {
		return legacyCombinedFileState{}, sqliteSequenceFloorState{}, fmt.Errorf("reading legacy combined source entry: %w", errSQLiteSourceChanged)
	}
	floor, file.Hash, err = captureSQLiteSequenceFloorCensusFromFile(ctx, opened)
	if err != nil {
		return legacyCombinedFileState{}, sqliteSequenceFloorState{}, fmt.Errorf("reading legacy combined source entry: %w", err)
	}
	finalInfo, err := opened.Stat()
	if err != nil {
		return legacyCombinedFileState{}, sqliteSequenceFloorState{}, fmt.Errorf("restating legacy combined source entry: %w", err)
	}
	if !sameLegacyCombinedCensusFile(info, finalInfo) {
		return legacyCombinedFileState{}, sqliteSequenceFloorState{}, fmt.Errorf("reading legacy combined source entry: %w", errSQLiteSourceChanged)
	}
	file.Identity = physicalIdentity(path, info)
	return file, floor, nil
}

func sameLegacyCombinedCensusFile(left, right os.FileInfo) bool {
	return left.Mode() == right.Mode() && left.Size() == right.Size() && left.ModTime().Equal(right.ModTime()) && physicalIdentity("", left) == physicalIdentity("", right)
}

func copyLegacyCombinedSnapshot(ctx context.Context, destinationDir string, state legacyCombinedState, pinned sqliteSnapshotFiles) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(destinationDir, 0o700); err != nil {
		return fmt.Errorf("creating legacy combined snapshot directory: %w", err)
	}
	for _, name := range []string{
		legacyCombinedDatabaseFilename,
		legacyCombinedDatabaseFilename + "-wal",
		legacyCombinedDatabaseFilename + "-journal",
		graphSequenceFloorFilename,
	} {
		file, present := state.files[name]
		if !present {
			continue
		}
		if !file.Mode.IsRegular() {
			return fmt.Errorf("copying legacy combined snapshot: non-regular SQLite component %q", name)
		}
		source, ok := pinned.component(name, legacyCombinedDatabaseFilename)
		if !ok {
			return fmt.Errorf("copying legacy combined snapshot: SQLite component %q was not pinned", name)
		}
		if err := copyPinnedSQLiteSnapshotFile(ctx, source, filepath.Join(destinationDir, name), legacyCombinedSnapshotExpectation(file)); err != nil {
			return err
		}
	}
	return nil
}

func legacyCombinedSnapshotExpectation(file legacyCombinedFileState) sqliteSnapshotExpectation {
	return sqliteSnapshotExpectation{
		mode:     file.Mode,
		size:     file.Size,
		modTime:  file.ModTime,
		hash:     file.Hash,
		identity: file.Identity,
	}
}
