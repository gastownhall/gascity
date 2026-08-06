// Package sqlite contains the SQLite-specific storage-binding mechanics.
//
// It deliberately sits above the generic storebinding contract and below the
// composite provider composition: it knows how to observe and reserve the
// deployed Graph SQLite component without making internal/beads depend on
// storage-binding lifecycle types.
package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
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

	_ "modernc.org/sqlite" // Graph inspection and fences use the CGO-free deployed driver.
)

const (
	// ProviderID is the built-in provider identifier for the deployed SQLite topology.
	ProviderID = storebinding.ProviderID("sqlite")
	// GraphComponentID names the deployed Graph SQLite component.
	GraphComponentID = storebinding.ComponentID("graph")

	graphFilename        = "beads.sqlite"
	graphDirectoryName   = "graph"
	graphMarkerFilename  = "graph.migrated"
	graphFormat          = storebinding.FormatID("sqlite-beads-v1")
	graphContract        = storebinding.ContractVersion("gascity-graph-v1")
	graphFenceProjection = storebinding.FenceProjection("sqlite.graph.inspection")
)

var (
	// ErrInvalidGraphTarget reports a fence target that is not this provider's
	// exact Graph component.
	ErrInvalidGraphTarget = errors.New("invalid SQLite Graph fence target")

	makeGraphInspectionTempDir    = makeRecoverableSQLiteSnapshotTempDir
	removeGraphInspectionTempRoot = os.RemoveAll
	openGraphInspectionDatabase   = sql.Open
	closeGraphInspectionDatabase  = func(database *sql.DB) error { return database.Close() }
)

// GraphPath returns the exact deployed Graph database path below a SQLite
// binding root. An empty root means the default .gc/store root.
func GraphPath(root string) (string, error) {
	if root == "" {
		root = filepath.Join(".gc", "store")
	}
	canonicalRoot, err := canonicalPath(root)
	if err != nil {
		return "", fmt.Errorf("resolving SQLite binding root: %w", err)
	}
	return filepath.Join(canonicalRoot, graphDirectoryName, graphFilename), nil
}

// canonicalPath resolves every existing path segment while retaining missing
// trailing segments. A binding locator must never retain a symlink spelling:
// aliases need to identify the same component before fences or overlap checks
// run, while new destination paths still need a stable prospective locator.
func canonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	probe := filepath.Clean(abs)
	var missing []string
	for {
		resolved, evalErr := filepath.EvalSymlinks(probe)
		if evalErr == nil {
			for index := len(missing) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, missing[index])
			}
			return filepath.Clean(resolved), nil
		}
		if !errors.Is(evalErr, os.ErrNotExist) {
			return "", evalErr
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return "", evalErr
		}
		missing = append(missing, filepath.Base(probe))
		probe = parent
	}
}

// GraphInspector binds SQLite Graph observation to one validated binding
// specification. A composite provider composes this component-level
// inspector without losing the binding's opaque configuration reference.
type GraphInspector struct {
	spec storebinding.BindingSpec

	// Test-only deterministic seams for proving that a source changes while a
	// descriptor is being established. Production constructors leave both nil.
	beforeStaticCensus func()
	beforeSnapshotCopy func()
}

// NewGraphInspector creates a Graph component inspector for one SQLite binding.
func NewGraphInspector(spec storebinding.BindingSpec) (*GraphInspector, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if spec.Provider != ProviderID {
		return nil, fmt.Errorf("%w: provider %q", ErrInvalidGraphTarget, spec.Provider)
	}
	path, err := GraphPath(spec.Path)
	if err != nil {
		return nil, err
	}
	spec.Path = filepath.Dir(filepath.Dir(path))
	return &GraphInspector{spec: spec}, nil
}

// Inspect observes the deployed Graph component without writing to the source.
// A live WAL or any rollback-journal residue is intentionally left incomplete:
// opening either state can recover or otherwise alter the source, so a fenced
// temporary snapshot is required instead.
func (i *GraphInspector) Inspect(ctx context.Context) (storebinding.Inspection, error) {
	if i == nil {
		return storebinding.Inspection{}, errors.New("inspecting Graph with nil SQLite inspector")
	}
	spec := i.spec
	path, err := GraphPath(spec.Path)
	if err != nil {
		return storebinding.Inspection{}, err
	}
	if err := ensureNoSQLiteSourceDescriptors(path); err != nil {
		return storebinding.Inspection{}, fmt.Errorf("inspecting Graph source: %w", err)
	}
	before, err := captureGraphSourceContext(ctx, path)
	if err != nil {
		return storebinding.Inspection{}, err
	}
	target, err := newGraphTarget(path, before)
	if err != nil {
		return storebinding.Inspection{}, err
	}
	if !before.database.Present || before.hasLiveWAL() || before.hasRollbackJournal() {
		return storebinding.NewInspection(target, nil)
	}
	if i.beforeStaticCensus != nil {
		i.beforeStaticCensus()
	}

	schema, err := inspectGraphSchema(ctx, path, true)
	if err != nil {
		return storebinding.Inspection{}, err
	}
	preProbe, err := captureGraphSourceContext(ctx, path)
	if err != nil {
		return storebinding.Inspection{}, err
	}
	preProbeTarget, err := newGraphTarget(path, preProbe)
	if err != nil {
		return storebinding.Inspection{}, err
	}
	if !before.equal(preProbe) || preProbe.hasLiveWAL() || preProbe.hasRollbackJournal() {
		return storebinding.NewInspection(preProbeTarget, nil)
	}
	writerFencing, qualificationErr := qualifySQLiteStaticWriterFencing(ctx, path, preProbe.directory.Identity, preProbe.database.Identity)
	postProbe, err := captureGraphSourceContext(ctx, path)
	if err != nil {
		return storebinding.Inspection{}, err
	}
	postProbeTarget, err := newGraphTarget(path, postProbe)
	if err != nil {
		return storebinding.Inspection{}, err
	}
	if qualificationErr != nil {
		if errors.Is(qualificationErr, errSQLiteSourceChanged) {
			return storebinding.NewInspection(postProbeTarget, nil)
		}
		return storebinding.Inspection{}, fmt.Errorf("qualifying Graph source writer fence: %w", qualificationErr)
	}
	if !preProbe.equal(postProbe) || postProbe.hasLiveWAL() || postProbe.hasRollbackJournal() {
		return storebinding.NewInspection(postProbeTarget, nil)
	}
	descriptor, err := graphDescriptor(spec, target, schema, postProbe, writerFencing)
	if err != nil {
		return storebinding.Inspection{}, err
	}
	return storebinding.NewInspection(target, &descriptor)
}

// AcquireFence reserves the exact inspected Graph database with a concrete
// operating-system writer exclusion under a claimed city migration guard.
func (i *GraphInspector) AcquireFence(ctx context.Context, claim storebinding.MigrationGuardClaim, request storebinding.FenceRequest) (storebinding.WriterFence, error) {
	if i == nil {
		return nil, errors.New("acquiring Graph fence with nil SQLite inspector")
	}
	return acquireGraphFence(ctx, claim, request, i.beforeSnapshotCopy)
}

// InspectFenced produces the final Graph descriptor under a held fence while
// retaining this binding's configuration reference in the descriptor digest.
func (i *GraphInspector) InspectFenced(ctx context.Context, request storebinding.FencedInspectionRequest) (storebinding.Descriptor, error) {
	if i == nil {
		return storebinding.Descriptor{}, errors.New("inspecting fenced Graph with nil SQLite inspector")
	}
	return inspectGraphFenced(ctx, request, i.spec)
}

// InspectGraph is the stateless convenience form of GraphInspector.Inspect.
func InspectGraph(ctx context.Context, spec storebinding.BindingSpec) (storebinding.Inspection, error) {
	inspector, err := NewGraphInspector(spec)
	if err != nil {
		return storebinding.Inspection{}, err
	}
	return inspector.Inspect(ctx)
}

// acquireGraphFence reserves the exact inspected Graph database with concrete
// operating-system locks. It is private so all successful acquisition flows
// arrive through storebinding.AcquireWriterFence and retain a live city claim.
func acquireGraphFence(ctx context.Context, claim storebinding.MigrationGuardClaim, request storebinding.FenceRequest, beforeSnapshotCopy func()) (storebinding.WriterFence, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, targetComponent, err := graphTargetPath(request.Target)
	if err != nil {
		return nil, err
	}
	if err := ensureNoSQLiteSourceDescriptors(path); err != nil {
		return nil, fmt.Errorf("acquiring Graph fence: %w", err)
	}
	if len(request.Components) != 1 || request.Components[0] != GraphComponentID {
		return nil, fmt.Errorf("%w: request must reserve graph", ErrInvalidGraphTarget)
	}
	if err := validateSQLiteMigrationGuardClaim(claim); err != nil {
		return nil, fmt.Errorf("acquiring Graph fence: %w", err)
	}
	state, err := captureGraphSourceContext(ctx, path)
	if err != nil {
		return nil, graphFenceSourceCaptureError(err)
	}
	if !state.database.Present {
		return nil, &storebinding.FenceTargetMovedError{Component: GraphComponentID}
	}
	if graphComponentIdentity(path, state) != targetComponent.PhysicalIdentity {
		return nil, &storebinding.FenceTargetMovedError{Component: GraphComponentID}
	}
	reservation, err := acquireSQLiteWriterReservation(ctx, path, claim, sqliteWriterReservationSource{
		directory:     state.directory.Identity,
		database:      state.database.Identity,
		wal:           state.files[graphFilename+"-wal"].Identity,
		shm:           state.files[graphFilename+"-shm"].Identity,
		journal:       state.files[graphFilename+"-journal"].Identity,
		sequenceFloor: state.files[graphSequenceFloorFilename].Identity,
	})
	if err != nil {
		if errors.Is(err, errSQLiteSourceChanged) {
			err = &storebinding.FenceTargetMovedError{Component: GraphComponentID}
		}
		if reservation != nil {
			return &graphFence{
				target:         request.Target.Clone(),
				components:     append([]storebinding.ComponentID(nil), request.Components...),
				role:           request.Role,
				generation:     request.ExpectedGeneration,
				claim:          claim,
				reservation:    reservation,
				cleanupPending: true,
			}, fmt.Errorf("acquiring Graph writer fence: %w", err)
		}
		return nil, fmt.Errorf("acquiring Graph writer fence: %w", err)
	}
	stateAfterFence, err := captureGraphSourceContext(ctx, path)
	if err != nil {
		return releaseIncompleteGraphFence(request, claim, reservation, graphFenceSourceCaptureError(err))
	}
	if !stateAfterFence.database.Present || graphComponentIdentity(path, stateAfterFence) != targetComponent.PhysicalIdentity || !state.equalForFence(stateAfterFence) {
		return releaseIncompleteGraphFence(request, claim, reservation, &storebinding.FenceTargetMovedError{Component: GraphComponentID})
	}
	return &graphFence{
		target:             request.Target.Clone(),
		components:         append([]storebinding.ComponentID(nil), request.Components...),
		role:               request.Role,
		generation:         request.ExpectedGeneration,
		claim:              claim,
		reservation:        reservation,
		beforeSnapshotCopy: beforeSnapshotCopy,
	}, nil
}

func releaseIncompleteGraphFence(request storebinding.FenceRequest, claim storebinding.MigrationGuardClaim, reservation sqliteWriterReservation, cause error) (storebinding.WriterFence, error) {
	if err := reservation.Release(); err != nil {
		return &graphFence{
			target:         request.Target.Clone(),
			components:     append([]storebinding.ComponentID(nil), request.Components...),
			role:           request.Role,
			generation:     request.ExpectedGeneration,
			claim:          claim,
			reservation:    reservation,
			cleanupPending: true,
		}, errors.Join(cause, fmt.Errorf("releasing incomplete Graph writer fence: %w", err))
	}
	return nil, cause
}

// InspectGraphFenced creates a temporary byte-for-byte component snapshot and
// performs the final Graph schema census against that copy, never the source.
// Callers that need a nonempty ConfigRef should use GraphInspector instead.
func InspectGraphFenced(ctx context.Context, request storebinding.FencedInspectionRequest) (storebinding.Descriptor, error) {
	if err := request.Validate(ctx); err != nil {
		return storebinding.Descriptor{}, err
	}
	path, _, err := graphTargetPath(request.Target)
	if err != nil {
		return storebinding.Descriptor{}, err
	}
	return inspectGraphFenced(ctx, request, storebinding.BindingSpec{Provider: ProviderID, Path: filepath.Dir(filepath.Dir(path))})
}

func inspectGraphFenced(ctx context.Context, request storebinding.FencedInspectionRequest, spec storebinding.BindingSpec) (descriptor storebinding.Descriptor, returnErr error) {
	if err := request.Validate(ctx); err != nil {
		return storebinding.Descriptor{}, err
	}
	operation := &graphFenceInspectionOperation{
		target:             request.Target.Clone(),
		expectedGeneration: request.ExpectedGeneration,
		spec:               spec,
	}
	if err := storebinding.InspectProviderFence(ctx, request.Fence, operation); err != nil {
		return storebinding.Descriptor{}, err
	}
	return operation.descriptor.Clone(), nil
}

func inspectGraphFencedHeld(ctx context.Context, target storebinding.FenceTarget, spec storebinding.BindingSpec, fence *graphFence) (descriptor storebinding.Descriptor, returnErr error) {
	path, targetComponent, err := graphTargetPath(target)
	if err != nil {
		return storebinding.Descriptor{}, err
	}
	state, err := captureGraphSourceContext(ctx, path)
	if err != nil {
		return storebinding.Descriptor{}, graphFenceSourceCaptureError(err)
	}
	if !state.database.Present || graphComponentIdentity(path, state) != targetComponent.PhysicalIdentity {
		return storebinding.Descriptor{}, &storebinding.FenceTargetMovedError{Component: GraphComponentID}
	}
	beforeCopy, err := captureGraphSourceContext(ctx, path)
	if err != nil {
		return storebinding.Descriptor{}, graphFenceSourceCaptureError(err)
	}
	if !beforeCopy.database.Present || graphComponentIdentity(path, beforeCopy) != targetComponent.PhysicalIdentity || !state.equalForFence(beforeCopy) {
		return storebinding.Descriptor{}, &storebinding.FenceTargetMovedError{Component: GraphComponentID}
	}

	temporaryRoot, err := makeGraphInspectionTempDir("", "gascity-graph-inspection-")
	if err != nil {
		return storebinding.Descriptor{}, fmt.Errorf("creating Graph inspection snapshot: %w", err)
	}
	observeSQLiteBoundary("graph-snapshot-root-created")
	defer func() {
		if err := removeGraphInspectionTempRoot(temporaryRoot); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("removing Graph inspection snapshot: %w", err))
		}
	}()
	snapshotDir := filepath.Join(temporaryRoot, graphDirectoryName)
	if fence.beforeSnapshotCopy != nil {
		fence.beforeSnapshotCopy()
	}
	observeSQLiteBoundary("graph-snapshot-copy-before")
	if err := copyGraphSnapshot(ctx, filepath.Dir(path), snapshotDir, state, fence.reservation.snapshotFiles()); err != nil {
		if errors.Is(err, errSQLiteSourceChanged) || errors.Is(err, os.ErrNotExist) {
			return storebinding.Descriptor{}, &storebinding.FenceTargetMovedError{Component: GraphComponentID}
		}
		return storebinding.Descriptor{}, err
	}
	observeSQLiteBoundary("graph-snapshot-copy-after")

	afterCopy, err := captureGraphSourceContext(ctx, path)
	if err != nil {
		return storebinding.Descriptor{}, graphFenceSourceCaptureError(err)
	}
	if !afterCopy.database.Present || graphComponentIdentity(path, afterCopy) != targetComponent.PhysicalIdentity || !state.equalForFence(afterCopy) {
		return storebinding.Descriptor{}, &storebinding.FenceTargetMovedError{Component: GraphComponentID}
	}
	observeSQLiteBoundary("graph-private-recovery-before")
	snapshotPath := filepath.Join(snapshotDir, graphFilename)
	if err := recoverGraphPrivateSnapshot(ctx, snapshotPath); err != nil {
		return storebinding.Descriptor{}, err
	}
	schema, err := inspectGraphSchema(ctx, snapshotPath, false)
	if err != nil {
		return storebinding.Descriptor{}, err
	}
	observeSQLiteBoundary("graph-private-recovery-after")
	return graphDescriptor(spec, target, schema, state, true)
}

func graphFenceSourceCaptureError(err error) error {
	if errors.Is(err, errSQLiteSourceChanged) || errors.Is(err, os.ErrNotExist) {
		return &storebinding.FenceTargetMovedError{Component: GraphComponentID}
	}
	return err
}

type graphFence struct {
	target      storebinding.FenceTarget
	components  []storebinding.ComponentID
	role        storebinding.FenceRole
	generation  storebinding.Generation
	claim       storebinding.MigrationGuardClaim
	reservation sqliteWriterReservation

	mu                 sync.Mutex
	released           bool
	cleanupPending     bool
	beforeSnapshotCopy func()
}

type graphFenceInspectionOperation struct {
	target             storebinding.FenceTarget
	expectedGeneration storebinding.Generation
	spec               storebinding.BindingSpec
	descriptor         storebinding.Descriptor
}

func (*graphFenceInspectionOperation) FenceProjection() storebinding.FenceProjection {
	return graphFenceProjection
}

func (f *graphFence) Target() storebinding.FenceTarget { return f.target.Clone() }

// CoveredComponents returns an immutable snapshot of the components reserved by this fence.
func (f *graphFence) CoveredComponents() []storebinding.ComponentID {
	return append([]storebinding.ComponentID(nil), f.components...)
}

func (f *graphFence) Role() storebinding.FenceRole { return f.role }

func (f *graphFence) Generation() storebinding.Generation { return f.generation }

func (f *graphFence) Held(context.Context) (bool, error) {
	if f == nil {
		return false, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.released && !f.cleanupPending && f.reservation != nil && f.claim.Held(), nil
}

// ExecuteProviderFenceOperation runs one nonescaping Graph inspection while
// this fence's reservation and migration claim are held.
func (f *graphFence) ExecuteProviderFenceOperation(ctx context.Context, projection storebinding.FenceProjection, operation storebinding.ProviderFenceOperation) error {
	if f == nil || projection != graphFenceProjection {
		return storebinding.ErrInvalidFence
	}
	inspection, ok := operation.(*graphFenceInspectionOperation)
	if !ok || inspection == nil {
		return storebinding.ErrInvalidFence
	}
	if !inspection.target.Equal(f.target) || inspection.expectedGeneration != f.generation {
		return storebinding.ErrInvalidFence
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.released || f.cleanupPending || f.reservation == nil || !f.claim.Held() {
		return storebinding.ErrFenceNotHeld
	}
	descriptor, err := inspectGraphFencedHeld(ctx, inspection.target, inspection.spec, f)
	if err != nil {
		return err
	}
	inspection.descriptor = descriptor.Clone()
	return nil
}

func (f *graphFence) Release(context.Context) error {
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
			return fmt.Errorf("releasing Graph writer fence: %w", err)
		}
		f.reservation = nil
	}
	observeSQLiteBoundary("graph-claim-release-before")
	if err := f.claim.Release(); err != nil {
		f.cleanupPending = true
		return fmt.Errorf("releasing Graph migration guard claim: %w", err)
	}
	observeSQLiteBoundary("graph-claim-release-after")
	f.cleanupPending = false
	f.released = true
	return nil
}

type graphSourceState struct {
	directory     graphSourceFile
	entries       []string
	files         map[string]graphSourceFile
	database      graphSourceFile
	marker        graphSourceFile
	sequenceFloor sqliteSequenceFloorState
}

type graphSourceFile struct {
	Present  bool
	Mode     os.FileMode
	Size     int64
	ModTime  time.Time
	Hash     string
	Identity storebinding.PhysicalIdentity
}

func (s graphSourceState) hasLiveWAL() bool {
	for _, name := range []string{graphFilename + "-wal", graphFilename + "-shm"} {
		if file, ok := s.files[name]; ok && file.Present {
			return true
		}
	}
	return false
}

func (s graphSourceState) hasRollbackJournal() bool {
	return s.sidecarPresent(graphFilename + "-journal")
}

func (s graphSourceState) sidecarPresent(name string) bool {
	file, ok := s.files[name]
	return ok && file.Present
}

func (s graphSourceState) equal(other graphSourceState) bool {
	if s.directory != other.directory || s.marker != other.marker || s.sequenceFloor != other.sequenceFloor || !sameStrings(s.entries, other.entries) || len(s.files) != len(other.files) {
		return false
	}
	for name, file := range s.files {
		if other.files[name] != file {
			return false
		}
	}
	return true
}

// equalForFence compares every authoritative source fact while treating the
// WAL-index payload as derived state. A normal WAL reader may update read marks
// in -shm without changing DB or WAL authority. The index must still retain its
// namespace entry, mode, and inode so a replacement or deletion is never
// mistaken for harmless reader activity.
func (s graphSourceState) equalForFence(other graphSourceState) bool {
	if s.directory != other.directory || s.marker != other.marker || s.sequenceFloor != other.sequenceFloor || !sameStrings(s.entries, other.entries) || len(s.files) != len(other.files) {
		return false
	}
	for name, file := range s.files {
		otherFile, ok := other.files[name]
		if !ok {
			return false
		}
		if name == graphFilename+"-shm" {
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

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func captureGraphSource(databasePath string) (graphSourceState, error) {
	return captureGraphSourceContext(context.Background(), databasePath)
}

func captureGraphSourceContext(ctx context.Context, databasePath string) (graphSourceState, error) {
	if err := ctx.Err(); err != nil {
		return graphSourceState{}, err
	}
	directory := filepath.Dir(databasePath)
	marker, err := captureOptionalMarkerContext(ctx, filepath.Join(filepath.Dir(directory), graphMarkerFilename))
	if err != nil {
		return graphSourceState{}, err
	}
	info, err := os.Stat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return graphSourceState{files: map[string]graphSourceFile{}, marker: marker}, nil
	}
	if err != nil {
		return graphSourceState{}, fmt.Errorf("stating Graph source directory: %w", err)
	}
	if !info.IsDir() {
		return graphSourceState{}, fmt.Errorf("inspecting Graph source: %s is not a directory", graphDirectoryName)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return graphSourceState{}, fmt.Errorf("reading Graph source directory: %w", err)
	}
	state := graphSourceState{
		directory: graphSourceFile{
			Present:  true,
			Mode:     info.Mode(),
			Size:     info.Size(),
			ModTime:  info.ModTime(),
			Identity: physicalIdentity(directory, info),
		},
		files:  make(map[string]graphSourceFile, len(entries)),
		marker: marker,
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return graphSourceState{}, err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return graphSourceState{}, fmt.Errorf("inspecting Graph source: symbolic-link entry %q is unsupported", entry.Name())
		}
		fileInfo, err := entry.Info()
		if err != nil {
			return graphSourceState{}, fmt.Errorf("stating Graph source entry %q: %w", entry.Name(), err)
		}
		if (graphSQLiteComponent(entry.Name()) || entry.Name() == graphSequenceFloorFilename) && fileInfo.Mode().IsRegular() && platformFileHasMultipleLinks(fileInfo) {
			return graphSourceState{}, fmt.Errorf("inspecting Graph source: hard-linked SQLite component %q is unsupported", entry.Name())
		}
		path := filepath.Join(directory, entry.Name())
		var file graphSourceFile
		switch entry.Name() {
		case graphSequenceFloorFilename:
			file, state.sequenceFloor, err = captureGraphSequenceFloorFileContext(ctx, path, fileInfo)
		case graphFilename + "-shm":
			file, err = captureGraphSHMFileContext(ctx, path, fileInfo)
		default:
			file, err = captureGraphFileContext(ctx, path, fileInfo)
		}
		if err != nil {
			return graphSourceState{}, err
		}
		state.entries = append(state.entries, entry.Name())
		state.files[entry.Name()] = file
	}
	sort.Strings(state.entries)
	state.database = state.files[graphFilename]
	return state, nil
}

func graphSQLiteComponent(name string) bool {
	switch name {
	case graphFilename, graphFilename + "-wal", graphFilename + "-shm", graphFilename + "-journal":
		return true
	default:
		return false
	}
}

func captureOptionalMarkerContext(ctx context.Context, path string) (graphSourceFile, error) {
	if err := ctx.Err(); err != nil {
		return graphSourceFile{}, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return graphSourceFile{}, nil
	}
	if err != nil {
		return graphSourceFile{}, fmt.Errorf("stating Graph migration marker: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return graphSourceFile{}, fmt.Errorf("inspecting Graph migration marker: non-regular marker is unsupported")
	}
	return captureGraphFileContext(ctx, path, info)
}

func captureGraphSHMFileContext(ctx context.Context, path string, info os.FileInfo) (graphSourceFile, error) {
	file := graphSourceFile{
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
		return graphSourceFile{}, fmt.Errorf("reading Graph source entry: %w", err)
	}
	file.Hash = hash
	file.Identity = physicalIdentity(path, info)
	return file, nil
}

func captureGraphFileContext(ctx context.Context, path string, info os.FileInfo) (file graphSourceFile, returnErr error) {
	if err := ctx.Err(); err != nil {
		return graphSourceFile{}, err
	}
	file = graphSourceFile{
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
		return graphSourceFile{}, fmt.Errorf("reading Graph source entry: %w", err)
	}
	defer func() {
		if err := closeSQLiteCensusFile(opened); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("closing Graph source census descriptor: %w", err))
		}
	}()
	openedInfo, err := opened.Stat()
	if err != nil {
		return graphSourceFile{}, fmt.Errorf("stating Graph source entry: %w", err)
	}
	if !sameGraphSourceCensusFile(info, openedInfo) {
		return graphSourceFile{}, fmt.Errorf("reading Graph source entry: %w", errSQLiteSourceChanged)
	}
	file.Hash, err = hashSQLiteSourceFile(ctx, opened)
	if err != nil {
		return graphSourceFile{}, fmt.Errorf("reading Graph source entry: %w", err)
	}
	finalInfo, err := opened.Stat()
	if err != nil {
		return graphSourceFile{}, fmt.Errorf("restating Graph source entry: %w", err)
	}
	if !sameGraphSourceCensusFile(info, finalInfo) {
		return graphSourceFile{}, fmt.Errorf("reading Graph source entry: %w", errSQLiteSourceChanged)
	}
	file.Identity = physicalIdentity(path, info)
	return file, nil
}

func captureGraphSequenceFloorFileContext(ctx context.Context, path string, info os.FileInfo) (file graphSourceFile, floor sqliteSequenceFloorState, returnErr error) {
	if err := ctx.Err(); err != nil {
		return graphSourceFile{}, sqliteSequenceFloorState{}, err
	}
	if !info.Mode().IsRegular() {
		return graphSourceFile{}, sqliteSequenceFloorState{}, fmt.Errorf("inspecting SQLite sequence floor: not a regular file")
	}
	file = graphSourceFile{
		Present: true,
		Mode:    info.Mode(),
		Size:    info.Size(),
		ModTime: info.ModTime(),
	}
	opened, err := os.Open(path)
	if err != nil {
		return graphSourceFile{}, sqliteSequenceFloorState{}, fmt.Errorf("reading Graph source entry: %w", err)
	}
	defer func() {
		if err := closeSQLiteCensusFile(opened); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("closing Graph sequence-floor census descriptor: %w", err))
		}
	}()
	openedInfo, err := opened.Stat()
	if err != nil {
		return graphSourceFile{}, sqliteSequenceFloorState{}, fmt.Errorf("stating Graph source entry: %w", err)
	}
	if !sameGraphSourceCensusFile(info, openedInfo) {
		return graphSourceFile{}, sqliteSequenceFloorState{}, fmt.Errorf("reading Graph source entry: %w", errSQLiteSourceChanged)
	}
	floor, file.Hash, err = captureSQLiteSequenceFloorCensusFromFile(ctx, opened)
	if err != nil {
		return graphSourceFile{}, sqliteSequenceFloorState{}, fmt.Errorf("reading Graph source entry: %w", err)
	}
	finalInfo, err := opened.Stat()
	if err != nil {
		return graphSourceFile{}, sqliteSequenceFloorState{}, fmt.Errorf("restating Graph source entry: %w", err)
	}
	if !sameGraphSourceCensusFile(info, finalInfo) {
		return graphSourceFile{}, sqliteSequenceFloorState{}, fmt.Errorf("reading Graph source entry: %w", errSQLiteSourceChanged)
	}
	file.Identity = physicalIdentity(path, info)
	return file, floor, nil
}

func sameGraphSourceCensusFile(left, right os.FileInfo) bool {
	return left.Mode() == right.Mode() && left.Size() == right.Size() && left.ModTime().Equal(right.ModTime()) && physicalIdentity("", left) == physicalIdentity("", right)
}

func newGraphTarget(path string, state graphSourceState) (storebinding.FenceTarget, error) {
	classes, err := graphClasses()
	if err != nil {
		return storebinding.FenceTarget{}, err
	}
	identity := graphComponentIdentity(path, state)
	locator, err := graphLocator(path)
	if err != nil {
		return storebinding.FenceTarget{}, err
	}
	return storebinding.NewFenceTarget(ProviderID, classes, []storebinding.FenceComponentTarget{{
		ID:               GraphComponentID,
		Locator:          locator,
		PhysicalIdentity: identity,
		Classes:          classes,
	}})
}

func graphTargetPath(target storebinding.FenceTarget) (string, storebinding.FenceComponentTarget, error) {
	if err := target.Validate(); err != nil {
		return "", storebinding.FenceComponentTarget{}, err
	}
	classes, err := graphClasses()
	if err != nil {
		return "", storebinding.FenceComponentTarget{}, err
	}
	if target.Provider != ProviderID || !target.Classes.Equal(classes) || len(target.Components) != 1 {
		return "", storebinding.FenceComponentTarget{}, ErrInvalidGraphTarget
	}
	component := target.Components[0]
	if component.ID != GraphComponentID || !component.Classes.Equal(classes) {
		return "", storebinding.FenceComponentTarget{}, ErrInvalidGraphTarget
	}
	parsed, err := url.Parse(string(component.Locator))
	if err != nil || parsed.Scheme != "file" || parsed.Host != "" || parsed.RawQuery != "" || parsed.Path == "" {
		return "", storebinding.FenceComponentTarget{}, ErrInvalidGraphTarget
	}
	path := filepath.Clean(parsed.Path)
	if !filepath.IsAbs(path) || filepath.Base(path) != graphFilename || filepath.Base(filepath.Dir(path)) != graphDirectoryName {
		return "", storebinding.FenceComponentTarget{}, ErrInvalidGraphTarget
	}
	canonicalPath, err := canonicalPath(path)
	if err != nil || canonicalPath != path {
		return "", storebinding.FenceComponentTarget{}, ErrInvalidGraphTarget
	}
	return path, component, nil
}

func graphLocator(path string) (storebinding.ComponentLocator, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return storebinding.ComponentLocator((&url.URL{Scheme: "file", Path: filepath.Clean(abs)}).String()), nil
}

func graphClasses() (storebinding.ClassSet, error) {
	return storebinding.NewClassSet(coordclass.ClassGraph)
}

func physicalIdentity(_ string, info os.FileInfo) storebinding.PhysicalIdentity {
	// Device/inode identity deliberately excludes the locator spelling. A
	// symlink or hard-link alias must be recognized as the same component by
	// overlap checks rather than minting a second fenceable identity.
	sum := sha256.Sum256([]byte("gascity.sqlite.component.v2\x00" + platformFileIdentity(info)))
	return storebinding.PhysicalIdentity("sha256:" + hex.EncodeToString(sum[:]))
}

func absentIdentity(path string) storebinding.PhysicalIdentity {
	sum := sha256.Sum256([]byte("gascity.sqlite.component.absent.v1\x00" + filepath.Clean(path)))
	return storebinding.PhysicalIdentity("sha256:" + hex.EncodeToString(sum[:]))
}

func graphComponentIdentity(path string, state graphSourceState) storebinding.PhysicalIdentity {
	database := state.database.Identity
	if !state.database.Present {
		database = absentIdentity(path)
	}
	marker := graphSourceFileIdentity(state.marker)
	directory := graphSourceFileIdentity(state.directory)
	floor := graphSourceFileIdentity(state.files[graphSequenceFloorFilename]) + "\x00" + state.sequenceFloor.identity()
	sum := sha256.Sum256([]byte("gascity.sqlite.graph-component.v4\x00" + string(database) + "\x00" + directory + "\x00" + marker + "\x00" + floor))
	return storebinding.PhysicalIdentity("sha256:" + hex.EncodeToString(sum[:]))
}

func graphSourceFileIdentity(file graphSourceFile) string {
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

type graphSchema struct{ version string }

func recoverGraphPrivateSnapshot(ctx context.Context, path string) (returnErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	db, err := openGraphInspectionDatabase("sqlite", graphPrivateSnapshotDSN(path))
	if err != nil {
		return fmt.Errorf("opening Graph private recovery database: %w", err)
	}
	db.SetMaxOpenConns(1)

	var schemaVersion int
	if err := db.QueryRowContext(ctx, `PRAGMA schema_version`).Scan(&schemaVersion); err != nil {
		returnErr = fmt.Errorf("probing Graph private recovery database: %w", err)
	}
	if err := closeGraphInspectionDatabase(db); err != nil {
		returnErr = errors.Join(returnErr, fmt.Errorf("closing Graph private recovery database: %w", err))
	}
	return returnErr
}

func inspectGraphSchema(ctx context.Context, path string, immutable bool) (graphSchema, error) {
	db, err := openGraphInspectionDatabase("sqlite", graphReadOnlyDSN(path, immutable))
	if err != nil {
		return graphSchema{}, fmt.Errorf("opening Graph schema for read-only inspection: %w", err)
	}
	db.SetMaxOpenConns(1)

	schema, inspectionErr := inspectGraphSchemaDatabase(ctx, db)
	if err := closeGraphInspectionDatabase(db); err != nil {
		inspectionErr = errors.Join(inspectionErr, fmt.Errorf("closing Graph read-only schema inspection database: %w", err))
	}
	if inspectionErr != nil {
		return graphSchema{}, inspectionErr
	}
	return schema, nil
}

func inspectGraphSchemaDatabase(ctx context.Context, db *sql.DB) (graphSchema, error) {
	if err := beads.ValidateSQLiteStoreSchema(ctx, db); err != nil {
		return graphSchema{}, fmt.Errorf("inspecting Graph schema: %w", err)
	}
	var userVersion int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&userVersion); err != nil {
		return graphSchema{}, fmt.Errorf("reading Graph schema version: %w", err)
	}
	rows, err := db.QueryContext(ctx, `SELECT type, name, tbl_name, COALESCE(sql, '') FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name`)
	if err != nil {
		return graphSchema{}, fmt.Errorf("reading Graph schema: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	var fingerprint strings.Builder
	fmt.Fprintf(&fingerprint, "user_version=%d\n", userVersion) //nolint:errcheck // strings.Builder cannot fail
	tables := make(map[string]bool)
	for rows.Next() {
		var kind, name, table, statement string
		if err := rows.Scan(&kind, &name, &table, &statement); err != nil {
			return graphSchema{}, fmt.Errorf("reading Graph schema row: %w", err)
		}
		if kind == "table" {
			tables[name] = true
		}
		fmt.Fprintf(&fingerprint, "%s|%s|%s|%s\n", kind, name, table, statement) //nolint:errcheck // strings.Builder cannot fail
	}
	if err := rows.Err(); err != nil {
		return graphSchema{}, fmt.Errorf("iterating Graph schema: %w", err)
	}
	if err := rows.Close(); err != nil {
		return graphSchema{}, fmt.Errorf("closing Graph schema rows: %w", err)
	}
	for table, required := range graphRequiredColumns {
		if !tables[table] {
			return graphSchema{}, fmt.Errorf("inspecting Graph schema: missing table %q", table)
		}
		columns, err := graphColumns(ctx, db, table)
		if err != nil {
			return graphSchema{}, err
		}
		for _, column := range required {
			if !columns[column] {
				return graphSchema{}, fmt.Errorf("inspecting Graph schema: table %q missing column %q", table, column)
			}
		}
	}
	sum := sha256.Sum256([]byte(fingerprint.String()))
	return graphSchema{version: fmt.Sprintf("graph-%d-%s", userVersion, hex.EncodeToString(sum[:8]))}, nil
}

var graphRequiredColumns = map[string][]string{
	"kv":       {"key", "value"},
	"beads":    {"id", "tier", "title", "status", "issue_type", "priority", "created_at", "updated_at", "assignee", "from_agent", "parent_id", "ref", "description", "bead_json"},
	"labels":   {"bead_id", "label"},
	"metadata": {"bead_id", "meta_key", "meta_value"},
	"deps":     {"issue_id", "depends_on_id", "dep_type"},
}

func graphColumns(ctx context.Context, db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return nil, fmt.Errorf("reading Graph table %q: %w", table, err)
	}
	defer rows.Close() //nolint:errcheck
	columns := make(map[string]bool)
	for rows.Next() {
		var (
			cid        int
			name       string
			columnType string
			notNull    int
			defaultVal sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultVal, &primaryKey); err != nil {
			return nil, fmt.Errorf("reading Graph table %q column: %w", table, err)
		}
		columns[name] = true
	}
	return columns, rows.Err()
}

func graphDescriptor(spec storebinding.BindingSpec, target storebinding.FenceTarget, schema graphSchema, state graphSourceState, writerFencing bool) (storebinding.Descriptor, error) {
	path, component, err := graphTargetPath(target)
	if err != nil {
		return storebinding.Descriptor{}, err
	}
	canonicalRoot := filepath.Dir(filepath.Dir(path))
	digest := sha256.Sum256([]byte("gascity.sqlite.binding-config.v2\x00" + canonicalRoot + "\x00" + string(spec.ConfigRef)))
	descriptor := storebinding.Descriptor{
		Version:                 1,
		SemanticContractVersion: graphContract,
		Provider:                ProviderID,
		ImplementationVersion:   "go-modernc-sqlite",
		ConfigRefDigest:         storebinding.ConfigRefDigest("sha256:" + hex.EncodeToString(digest[:])),
		Capabilities: storebinding.ClassCapabilities{
			Graph:         storebinding.ClassCapability{Available: true, Transactions: true, Claims: true},
			WriterFencing: writerFencing,
		},
		Components: []storebinding.ComponentDescriptor{{
			ID:               GraphComponentID,
			Locator:          component.Locator,
			PhysicalIdentity: component.PhysicalIdentity,
			Classes:          component.Classes,
			Format:           graphFormat,
			SchemaVersion:    schema.version,
			ABIVersion:       "modernc-sqlite",
			Marker:           storebinding.MarkerState{Name: graphMarkerFilename, Present: state.marker.Present},
		}},
	}
	return storebinding.NewDescriptor(descriptor)
}

func graphReadOnlyDSN(path string, immutable bool) string {
	query := url.Values{}
	query.Set("mode", "ro")
	if immutable {
		query.Set("immutable", "1")
	}
	return (&url.URL{Scheme: "file", Path: path, RawQuery: query.Encode()}).String()
}

// graphPrivateSnapshotDSN opens an already-copied private snapshot read-write.
// SQLite may need to rebuild a derived WAL index or roll back a hot journal;
// those recovery writes stay inside the disposable snapshot directory.
func graphPrivateSnapshotDSN(path string) string {
	query := url.Values{}
	query.Set("mode", "rw")
	return (&url.URL{Scheme: "file", Path: path, RawQuery: query.Encode()}).String()
}

func copyGraphSnapshot(ctx context.Context, _ string, destinationDir string, state graphSourceState, pinned sqliteSnapshotFiles) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(destinationDir, 0o700); err != nil {
		return fmt.Errorf("creating Graph inspection snapshot directory: %w", err)
	}
	for _, name := range []string{
		graphFilename,
		graphFilename + "-wal",
		graphFilename + "-journal",
		graphSequenceFloorFilename,
	} {
		file, present := state.files[name]
		if !present {
			continue
		}
		if !file.Mode.IsRegular() {
			return fmt.Errorf("copying Graph inspection snapshot: non-regular SQLite component %q", name)
		}
		destination := filepath.Join(destinationDir, name)
		if graphSnapshotComponent(name) {
			source, ok := pinned.component(name, graphFilename)
			if !ok {
				return fmt.Errorf("copying Graph inspection snapshot: SQLite component %q was not pinned", name)
			}
			if err := copyPinnedSQLiteSnapshotFile(ctx, source, destination, graphSnapshotExpectation(file)); err != nil {
				return err
			}
			continue
		}
		return fmt.Errorf("copying Graph inspection snapshot: unsupported unpinned component %q", name)
	}
	return nil
}

func graphSnapshotComponent(name string) bool {
	return graphSQLiteComponent(name) || name == graphSequenceFloorFilename
}

func graphSnapshotExpectation(file graphSourceFile) sqliteSnapshotExpectation {
	return sqliteSnapshotExpectation{
		mode:     file.Mode,
		size:     file.Size,
		modTime:  file.ModTime,
		hash:     file.Hash,
		identity: file.Identity,
	}
}
