package beads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	beadslib "github.com/steveyegge/beads"
)

const nativeDoltStoreActor = "gascity"

var nativeDoltOpenReadyStatuses = []beadslib.Status{
	beadslib.StatusOpen,
	beadslib.StatusBlocked,
	beadslib.StatusDeferred,
	beadslib.Status("pinned"),
	beadslib.Status("hooked"),
	beadslib.Status("review"),
	beadslib.Status("testing"),
}

var (
	nativeDoltOpenBestAvailable = beadslib.OpenBestAvailable
	nativeDoltOpenEnvMu         sync.Mutex
)

var nativeDoltOpenEnvKeys = []string{
	"BEADS_CREDENTIALS_FILE",
	"BEADS_DOLT_AUTO_START",
	"BEADS_DOLT_DATA_DIR",
	"BEADS_DOLT_PASSWORD",
	"BEADS_DOLT_PORT",
	"BEADS_DOLT_SERVER_DATABASE",
	"BEADS_DOLT_SERVER_HOST",
	"BEADS_DOLT_SERVER_MODE",
	"BEADS_DOLT_SERVER_PORT",
	"BEADS_DOLT_SERVER_SOCKET",
	"BEADS_DOLT_SERVER_TLS",
	"BEADS_DOLT_SERVER_USER",
	"BEADS_DOLT_SHARED_SERVER",
}

func nativeDoltOperationContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, bdCommandTimeout)
}

func nativeDoltCleanupContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), bdCommandTimeout)
}

// ProcessEnvSnapshotExcludingNativeDoltOpen returns a process environment
// snapshot after any in-flight native Dolt open has restored scoped BEADS_* env.
func ProcessEnvSnapshotExcludingNativeDoltOpen() []string {
	nativeDoltOpenEnvMu.Lock()
	defer nativeDoltOpenEnvMu.Unlock()
	return os.Environ()
}

func processEnvSnapshotExcludingNativeDoltOpen() []string {
	return ProcessEnvSnapshotExcludingNativeDoltOpen()
}

func withNativeDoltOpenEnv(env map[string]string) (func(), error) {
	nativeDoltOpenEnvMu.Lock()
	previous := make(map[string]*string, len(nativeDoltOpenEnvKeys))
	for _, key := range nativeDoltOpenEnvKeys {
		if value, ok := os.LookupEnv(key); ok {
			copied := value
			previous[key] = &copied
		} else {
			previous[key] = nil
		}
		value, ok := env[key]
		var err error
		if ok && strings.TrimSpace(value) != "" {
			err = os.Setenv(key, value)
		} else {
			err = os.Unsetenv(key)
		}
		if err != nil {
			restoreNativeDoltOpenEnv(previous)
			nativeDoltOpenEnvMu.Unlock()
			return nil, fmt.Errorf("projecting native Dolt open env %s: %w", key, err)
		}
	}
	return func() {
		restoreNativeDoltOpenEnv(previous)
		nativeDoltOpenEnvMu.Unlock()
	}, nil
}

func restoreNativeDoltOpenEnv(previous map[string]*string) {
	for _, key := range nativeDoltOpenEnvKeys {
		if value := previous[key]; value != nil {
			_ = os.Setenv(key, *value)
			continue
		}
		_ = os.Unsetenv(key)
	}
}

// NativeDoltStore is a Store implementation backed by the upstream beads
// library over Dolt. It is constructed by the store factory after native-store
// preflight gates pass.
type NativeDoltStore struct {
	mu       sync.RWMutex
	storage  beadslib.Storage
	actor    string
	idPrefix string
}

var _ Store = (*NativeDoltStore)(nil)

func newNativeDoltStoreWithStorage(storage beadslib.Storage, actor string) *NativeDoltStore {
	if actor == "" {
		actor = nativeDoltStoreActor
	}
	return &NativeDoltStore{storage: storage, actor: actor}
}

func newNativeDoltStoreWithStorageAndPrefix(storage beadslib.Storage, actor, idPrefix string) *NativeDoltStore {
	store := newNativeDoltStoreWithStorage(storage, actor)
	store.idPrefix = normalizeIDPrefix(idPrefix)
	return store
}

// OpenNativeDoltStoreAt opens a native Dolt-backed beads store at scopeRoot
// while projecting the supplied scoped Dolt environment for upstream beads.
func OpenNativeDoltStoreAt(ctx context.Context, scopeRoot string, env map[string]string) (*NativeDoltStore, error) {
	return newNativeDoltStoreAt(ctx, scopeRoot, env)
}

func newNativeDoltStoreAt(parent context.Context, scopeRoot string, env map[string]string) (*NativeDoltStore, error) {
	ctx, cancel := nativeDoltOperationContext(parent)
	defer cancel()
	restoreEnv, err := withNativeDoltOpenEnv(env)
	if err != nil {
		return nil, err
	}
	defer restoreEnv()
	storage, err := nativeDoltOpenBestAvailable(ctx, filepath.Join(scopeRoot, ".beads"))
	if err != nil {
		return nil, err
	}
	prefix, err := storage.GetConfig(ctx, "issue_prefix")
	if err != nil {
		_ = storage.Close()
		return nil, fmt.Errorf("reading native issue prefix: %w", err)
	}
	return newNativeDoltStoreWithStorageAndPrefix(storage, nativeDoltStoreActor, prefix), nil
}

func newNativeDoltStoreForTest(storage beadslib.Storage) *NativeDoltStore {
	return newNativeDoltStoreWithStorage(storage, "native-test")
}

// IDPrefix returns the bead ID prefix owned by this store, without trailing "-".
func (s *NativeDoltStore) IDPrefix() string {
	if s == nil {
		return ""
	}
	return s.idPrefix
}

func (s *NativeDoltStore) listIncludesCompleteDependencies() bool {
	return true
}

func (s *NativeDoltStore) acquireStorage() (beadslib.Storage, func(), error) {
	if s == nil {
		return nil, nil, fmt.Errorf("native Dolt store: %w", ErrStoreClosed)
	}
	s.mu.RLock()
	if s.storage == nil {
		s.mu.RUnlock()
		return nil, nil, fmt.Errorf("native Dolt store: %w", ErrStoreClosed)
	}
	return s.storage, s.mu.RUnlock, nil
}

// CloseStore releases the underlying native beads storage handle.
func (s *NativeDoltStore) CloseStore() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	storage := s.storage
	s.storage = nil
	s.mu.Unlock()
	if storage == nil {
		return nil
	}
	return storage.Close()
}

// Create persists a new bead through the upstream beads storage layer.
func (s *NativeDoltStore) Create(b Bead) (Bead, error) {
	issue, err := nativeIssueFromBead(b)
	if err != nil {
		return Bead{}, err
	}
	storage, release, err := s.acquireStorage()
	if err != nil {
		return Bead{}, err
	}
	defer release()
	ctx, cancel := nativeDoltOperationContext(context.TODO())
	defer cancel()
	pendingDependencies := cloneNativeDependencies(issue.Dependencies)
	if err := s.validateCreatedDependencies(ctx, storage, issue.ID, pendingDependencies); err != nil {
		return Bead{}, err
	}
	if err := storage.CreateIssue(ctx, issue, s.actor); err != nil {
		return Bead{}, err
	}
	createdDependencies, err := s.persistCreatedDependencies(ctx, storage, issue.ID, pendingDependencies)
	if err != nil {
		cleanupCtx, cleanupCancel := nativeDoltCleanupContext()
		cleanupErr := s.compensateFailedCreate(cleanupCtx, storage, issue.ID, createdDependencies)
		cleanupCancel()
		if cleanupErr != nil {
			return Bead{}, errors.Join(err, cleanupErr)
		}
		return Bead{}, err
	}
	issue.Dependencies = createdDependencies
	return beadFromNativeIssue(issue)
}

// Get retrieves a bead by ID from the upstream beads storage layer.
func (s *NativeDoltStore) Get(id string) (Bead, error) {
	storage, release, err := s.acquireStorage()
	if err != nil {
		return Bead{}, err
	}
	defer release()
	ctx, cancel := nativeDoltOperationContext(context.TODO())
	defer cancel()
	issues, err := storage.SearchIssues(ctx, "", beadslib.IssueFilter{
		IDs:                 []string{id},
		IncludeDependencies: true,
	})
	if err != nil {
		return Bead{}, nativeStoreError(id, err)
	}
	for _, issue := range issues {
		if issue != nil && issue.ID == id {
			return beadFromNativeIssue(issue)
		}
	}
	return Bead{}, fmt.Errorf("bead %q: %w", id, ErrNotFound)
}

// Update modifies an existing bead through the upstream beads storage layer.
func (s *NativeDoltStore) Update(id string, opts UpdateOpts) error {
	storage, release, err := s.acquireStorage()
	if err != nil {
		return err
	}
	defer release()
	ctx, cancel := nativeDoltOperationContext(context.TODO())
	defer cancel()
	err = storage.RunInTransaction(ctx, fmt.Sprintf("gc: update bead %s", id), func(tx beadslib.Transaction) error {
		if opts.ParentID != nil {
			if err := s.validateUpdateParent(ctx, tx, *opts.ParentID); err != nil {
				return err
			}
		}
		updates, err := s.nativeUpdates(ctx, tx, id, opts)
		if err != nil {
			return err
		}
		if len(updates) > 0 {
			if err := tx.UpdateIssue(ctx, id, updates, s.actor); err != nil {
				return nativeStoreError(id, err)
			}
		}
		for _, label := range opts.Labels {
			if err := tx.AddLabel(ctx, id, label, s.actor); err != nil {
				return nativeStoreError(id, err)
			}
		}
		for _, label := range opts.RemoveLabels {
			if err := tx.RemoveLabel(ctx, id, label, s.actor); err != nil {
				return nativeStoreError(id, err)
			}
		}
		if opts.ParentID != nil {
			if err := s.updateParentInTransaction(ctx, tx, id, *opts.ParentID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nativeStoreError(id, err)
	}
	return nil
}

// Close sets a bead's status to closed through the upstream beads storage layer.
func (s *NativeDoltStore) Close(id string) error {
	storage, release, err := s.acquireStorage()
	if err != nil {
		return err
	}
	defer release()
	ctx, cancel := nativeDoltOperationContext(context.TODO())
	defer cancel()
	current, err := storage.GetIssue(ctx, id)
	if err != nil {
		return nativeStoreError(id, err)
	}
	if current == nil {
		return fmt.Errorf("bead %q: %w", id, ErrNotFound)
	}
	if current.Status == beadslib.StatusClosed {
		return nil
	}
	reason := nativeCloseReasonFromIssue(current)
	if err := storage.CloseIssue(ctx, id, reason, s.actor, ""); err != nil {
		return nativeStoreError(id, err)
	}
	return nil
}

// Reopen sets a closed bead's status back to open.
func (s *NativeDoltStore) Reopen(id string) error {
	storage, release, err := s.acquireStorage()
	if err != nil {
		return err
	}
	defer release()
	ctx, cancel := nativeDoltOperationContext(context.TODO())
	defer cancel()
	current, err := storage.GetIssue(ctx, id)
	if err != nil {
		return nativeStoreError(id, err)
	}
	if current == nil {
		return fmt.Errorf("bead %q: %w", id, ErrNotFound)
	}
	if current.Status == beadslib.StatusOpen {
		return nil
	}
	return nativeStoreError(id, storage.ReopenIssue(ctx, id, "", s.actor))
}

// CloseAll closes multiple beads and sets metadata on each newly closed bead.
func (s *NativeDoltStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	closed := 0
	for _, id := range ids {
		current, err := s.Get(id)
		if err != nil {
			return closed, err
		}
		if current.Status == "closed" {
			continue
		}
		if len(metadata) > 0 {
			if err := s.SetMetadataBatch(id, metadata); err != nil {
				return closed, err
			}
		}
		if err := s.Close(id); err != nil {
			return closed, err
		}
		closed++
	}
	return closed, nil
}

// List returns beads matching the query.
func (s *NativeDoltStore) List(query ListQuery) ([]Bead, error) {
	if !query.HasFilter() && !query.AllowScan {
		return nil, fmt.Errorf("listing beads: %w", ErrQueryRequiresScan)
	}
	storage, release, err := s.acquireStorage()
	if err != nil {
		return nil, err
	}
	defer release()
	filter := nativeIssueFilterFromListQuery(query)
	ctx, cancel := nativeDoltOperationContext(context.TODO())
	defer cancel()
	issues, err := storage.SearchIssues(ctx, "", filter)
	if err != nil {
		return nil, err
	}
	beads := make([]Bead, 0, len(issues))
	for _, issue := range issues {
		bead, err := beadFromNativeIssue(issue)
		if err != nil {
			return nil, err
		}
		beads = append(beads, bead)
	}
	return ApplyListQuery(beads, query), nil
}

// ListOpen returns non-closed beads by default, or beads with the given status.
func (s *NativeDoltStore) ListOpen(status ...string) ([]Bead, error) {
	query := ListQuery{AllowScan: true}
	if len(status) > 0 {
		query.Status = status[0]
		if status[0] == "closed" {
			query.IncludeClosed = true
		}
	}
	return s.List(query)
}

// Ready returns open, unblocked actionable beads.
func (s *NativeDoltStore) Ready(queries ...ReadyQuery) ([]Bead, error) {
	q := readyQueryFromArgs(queries)
	storage, release, err := s.acquireStorage()
	if err != nil {
		return nil, err
	}
	defer release()
	ctx, cancel := nativeDoltOperationContext(context.TODO())
	defer cancel()
	var beads []Bead
	seen := make(map[string]bool)
statusLoop:
	for _, status := range nativeDoltOpenReadyStatuses {
		filter := beadslib.WorkFilter{Status: status}
		if q.Assignee != "" {
			filter.Assignee = &q.Assignee
		}
		issues, err := storage.GetReadyWork(ctx, filter)
		if err != nil {
			return nil, err
		}
		for _, issue := range issues {
			bead, err := beadFromNativeIssue(issue)
			if err != nil {
				return nil, err
			}
			if bead.Status != "open" || bead.Ephemeral || IsReadyExcludedType(bead.Type) || seen[bead.ID] {
				continue
			}
			seen[bead.ID] = true
			beads = append(beads, bead)
			if q.Limit > 0 && len(beads) >= q.Limit {
				break statusLoop
			}
		}
	}
	return beads, nil
}

// Children returns all beads whose parent-child dependency points at parentID.
func (s *NativeDoltStore) Children(parentID string, opts ...QueryOpt) ([]Bead, error) {
	return s.List(ListQuery{
		ParentID:      parentID,
		IncludeClosed: HasOpt(opts, IncludeClosed),
		AllowScan:     true,
		TierMode:      TierModeFromOpts(opts),
	})
}

// WaitForParentProjection blocks until native dependency queries reflect a
// successful reparent from oldParentID to newParentID for id.
func (s *NativeDoltStore) WaitForParentProjection(ctx context.Context, id, oldParentID, newParentID string) error {
	ticker := time.NewTicker(bdParentProjectionPollInterval)
	defer ticker.Stop()

	var lastErr error
	for {
		current, err := s.Get(id)
		if err == nil {
			switch current.ParentID {
			case newParentID:
				matches, matchErr := s.parentProjectionMatches(id, oldParentID, newParentID)
				if matchErr == nil && matches {
					return nil
				}
				lastErr = matchErr
			case oldParentID:
				lastErr = nil
			default:
				return fmt.Errorf("updating bead %q: %w", id, ErrParentProjectionSuperseded)
			}
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("updating bead %q: waiting for parent projection from %q to %q: %w (last check error: %w)", id, oldParentID, newParentID, ctx.Err(), lastErr)
			}
			return fmt.Errorf("updating bead %q: waiting for parent projection from %q to %q: %w", id, oldParentID, newParentID, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (s *NativeDoltStore) parentProjectionMatches(id, oldParentID, newParentID string) (bool, error) {
	if oldParentID != "" {
		oldChildren, err := s.Children(oldParentID)
		if err != nil {
			return false, fmt.Errorf("listing old parent %q children: %w", oldParentID, err)
		}
		if beadSliceContains(oldChildren, id) {
			return false, nil
		}
	}
	if newParentID != "" {
		newChildren, err := s.Children(newParentID)
		if err != nil {
			return false, fmt.Errorf("listing new parent %q children: %w", newParentID, err)
		}
		if !beadSliceContains(newChildren, id) {
			return false, nil
		}
	}
	return true, nil
}

// ListByLabel returns beads with an exact label match.
func (s *NativeDoltStore) ListByLabel(label string, limit int, opts ...QueryOpt) ([]Bead, error) {
	return s.List(ListQuery{
		Label:         label,
		Limit:         limit,
		IncludeClosed: HasOpt(opts, IncludeClosed),
		AllowScan:     true,
		TierMode:      TierModeFromOpts(opts),
	})
}

// ListByAssignee returns beads assigned to assignee with the requested status.
func (s *NativeDoltStore) ListByAssignee(assignee, status string, limit int) ([]Bead, error) {
	return s.List(ListQuery{Assignee: assignee, Status: status, Limit: limit, AllowScan: true})
}

// ListByMetadata returns beads whose metadata contains all filters.
func (s *NativeDoltStore) ListByMetadata(filters map[string]string, limit int, opts ...QueryOpt) ([]Bead, error) {
	return s.List(ListQuery{
		Metadata:      filters,
		Limit:         limit,
		IncludeClosed: HasOpt(opts, IncludeClosed),
		AllowScan:     true,
		TierMode:      TierModeFromOpts(opts),
	})
}

// SetMetadata sets a single metadata key on a bead.
func (s *NativeDoltStore) SetMetadata(id, key, value string) error {
	return s.SetMetadataBatch(id, map[string]string{key: value})
}

// SetMetadataBatch sets multiple metadata keys on a bead.
func (s *NativeDoltStore) SetMetadataBatch(id string, kvs map[string]string) error {
	storage, release, err := s.acquireStorage()
	if err != nil {
		return err
	}
	defer release()
	ctx, cancel := nativeDoltOperationContext(context.TODO())
	defer cancel()
	issue, err := storage.GetIssue(ctx, id)
	if err != nil {
		return nativeStoreError(id, err)
	}
	if issue == nil {
		return fmt.Errorf("bead %q: %w", id, ErrNotFound)
	}
	metadata, err := metadataMapFromNative(issue.Metadata)
	if err != nil {
		return fmt.Errorf("parsing metadata for bead %q: %w", id, err)
	}
	if metadata == nil {
		metadata = make(map[string]string, len(kvs))
	}
	for k, v := range kvs {
		metadata[k] = v
	}
	raw, err := metadataRawFromMap(metadata)
	if err != nil {
		return err
	}
	return nativeStoreError(id, storage.UpdateIssue(ctx, id, map[string]interface{}{"metadata": raw}, s.actor))
}

// Tx executes fn sequentially against the native Dolt store.
func (s *NativeDoltStore) Tx(_ string, fn func(Tx) error) error {
	_, release, err := s.acquireStorage()
	if err != nil {
		return err
	}
	release()
	return runSequentialTx(s, fn)
}

// Delete permanently removes a bead from the upstream beads storage layer.
func (s *NativeDoltStore) Delete(id string) error {
	storage, release, err := s.acquireStorage()
	if err != nil {
		return err
	}
	defer release()
	ctx, cancel := nativeDoltOperationContext(context.TODO())
	defer cancel()
	return nativeStoreError(id, storage.DeleteIssue(ctx, id))
}

// Ping verifies that the upstream storage is reachable.
func (s *NativeDoltStore) Ping() error {
	storage, release, err := s.acquireStorage()
	if err != nil {
		return err
	}
	defer release()
	ctx, cancel := nativeDoltOperationContext(context.TODO())
	defer cancel()
	_, err = storage.GetStatistics(ctx)
	return err
}

// DepAdd records a dependency between two beads.
func (s *NativeDoltStore) DepAdd(issueID, dependsOnID, depType string) error {
	storage, release, err := s.acquireStorage()
	if err != nil {
		return err
	}
	defer release()
	ctx, cancel := nativeDoltOperationContext(context.TODO())
	defer cancel()
	return nativeStoreError(issueID, storage.AddDependency(ctx, &beadslib.Dependency{
		IssueID:     issueID,
		DependsOnID: dependsOnID,
		Type:        beadslib.DependencyType(depType),
	}, s.actor))
}

// DepRemove removes a dependency between two beads.
func (s *NativeDoltStore) DepRemove(issueID, dependsOnID string) error {
	storage, release, err := s.acquireStorage()
	if err != nil {
		return err
	}
	defer release()
	ctx, cancel := nativeDoltOperationContext(context.TODO())
	defer cancel()
	return nativeStoreError(issueID, storage.RemoveDependency(ctx, issueID, dependsOnID, s.actor))
}

// DepList returns dependencies for a bead.
func (s *NativeDoltStore) DepList(id, direction string) ([]Dep, error) {
	storage, release, err := s.acquireStorage()
	if err != nil {
		return nil, err
	}
	defer release()
	ctx, cancel := nativeDoltOperationContext(context.TODO())
	defer cancel()
	return s.depList(ctx, storage, id, direction)
}

func (s *NativeDoltStore) depList(ctx context.Context, storage beadslib.Storage, id, direction string) ([]Dep, error) {
	if direction == "up" {
		issues, err := storage.GetDependentsWithMetadata(ctx, id)
		if err != nil {
			return nil, nativeStoreError(id, err)
		}
		deps := make([]Dep, 0, len(issues))
		for _, issue := range issues {
			deps = append(deps, Dep{
				IssueID:     issue.ID,
				DependsOnID: id,
				Type:        string(issue.DependencyType),
			})
		}
		return deps, nil
	}
	issues, err := storage.GetDependenciesWithMetadata(ctx, id)
	if err != nil {
		return nil, nativeStoreError(id, err)
	}
	deps := make([]Dep, 0, len(issues))
	for _, issue := range issues {
		deps = append(deps, Dep{
			IssueID:     id,
			DependsOnID: issue.ID,
			Type:        string(issue.DependencyType),
		})
	}
	return deps, nil
}

type nativeIssueGetter interface {
	GetIssue(context.Context, string) (*beadslib.Issue, error)
}

func (s *NativeDoltStore) nativeUpdates(ctx context.Context, storage nativeIssueGetter, id string, opts UpdateOpts) (map[string]interface{}, error) {
	updates := make(map[string]interface{})
	if opts.Title != nil {
		updates["title"] = *opts.Title
	}
	if opts.Status != nil {
		updates["status"] = *opts.Status
	}
	if opts.Type != nil {
		updates["issue_type"] = *opts.Type
	}
	if opts.Priority != nil {
		updates["priority"] = *opts.Priority
	}
	if opts.Description != nil {
		updates["description"] = *opts.Description
	}
	if opts.Assignee != nil {
		updates["assignee"] = *opts.Assignee
	}
	if len(opts.Metadata) > 0 {
		issue, err := storage.GetIssue(ctx, id)
		if err != nil {
			return nil, nativeStoreError(id, err)
		}
		if issue == nil {
			return nil, fmt.Errorf("bead %q: %w", id, ErrNotFound)
		}
		metadata, err := metadataMapFromNative(issue.Metadata)
		if err != nil {
			return nil, fmt.Errorf("parsing metadata for bead %q: %w", id, err)
		}
		if metadata == nil {
			metadata = make(map[string]string, len(opts.Metadata))
		}
		for k, v := range opts.Metadata {
			metadata[k] = v
		}
		raw, err := metadataRawFromMap(metadata)
		if err != nil {
			return nil, err
		}
		updates["metadata"] = raw
	}
	return updates, nil
}

func (s *NativeDoltStore) validateUpdateParent(ctx context.Context, storage nativeIssueGetter, parentID string) error {
	if strings.TrimSpace(parentID) == "" {
		return nil
	}
	issue, err := storage.GetIssue(ctx, parentID)
	if err != nil {
		return nativeStoreError(parentID, err)
	}
	if issue == nil {
		return fmt.Errorf("bead %q: %w", parentID, ErrNotFound)
	}
	return nil
}

func (s *NativeDoltStore) updateParentInTransaction(ctx context.Context, tx beadslib.Transaction, id, parentID string) error {
	if strings.TrimSpace(parentID) != "" {
		issue, err := tx.GetIssue(ctx, parentID)
		if err != nil {
			return nativeStoreError(parentID, err)
		}
		if issue == nil {
			return fmt.Errorf("bead %q: %w", parentID, ErrNotFound)
		}
	}
	deps, err := tx.GetDependencyRecords(ctx, id)
	if err != nil {
		return nativeStoreError(id, err)
	}
	for _, dep := range deps {
		if dep == nil || dep.Type != beadslib.DepParentChild {
			continue
		}
		if err := tx.RemoveDependency(ctx, id, dep.DependsOnID, s.actor); err != nil {
			return nativeStoreError(id, err)
		}
	}
	if parentID == "" {
		return nil
	}
	if err := tx.AddDependency(ctx, &beadslib.Dependency{
		IssueID:     id,
		DependsOnID: parentID,
		Type:        beadslib.DepParentChild,
	}, s.actor); err != nil {
		return nativeStoreError(id, err)
	}
	return nil
}

func (s *NativeDoltStore) persistCreatedDependencies(ctx context.Context, storage beadslib.Storage, issueID string, deps []*beadslib.Dependency) ([]*beadslib.Dependency, error) {
	if len(deps) == 0 {
		return nil, nil
	}
	if strings.TrimSpace(issueID) == "" {
		return nil, fmt.Errorf("persisting native create dependencies: upstream create did not assign an issue ID")
	}
	created := make([]*beadslib.Dependency, 0, len(deps))
	for _, dep := range deps {
		if dep == nil {
			continue
		}
		persisted := *dep
		if strings.TrimSpace(persisted.IssueID) == "" {
			persisted.IssueID = issueID
		}
		if err := storage.AddDependency(ctx, &persisted, s.actor); err != nil {
			return created, fmt.Errorf("persisting native create dependency %q -> %q: %w", persisted.IssueID, persisted.DependsOnID, nativeStoreError(persisted.IssueID, err))
		}
		depCopy := persisted
		created = append(created, &depCopy)
	}
	return created, nil
}

func (s *NativeDoltStore) validateCreatedDependencies(ctx context.Context, storage beadslib.Storage, issueID string, deps []*beadslib.Dependency) error {
	for _, dep := range deps {
		if dep == nil {
			continue
		}
		targetID := strings.TrimSpace(dep.DependsOnID)
		if targetID == "" {
			return fmt.Errorf("validating native create dependency for %q: depends_on_id is empty", issueID)
		}
		if !shouldPrevalidateNativeDependency(issueID, targetID, s.idPrefix) {
			continue
		}
		issue, err := storage.GetIssue(ctx, targetID)
		if err != nil {
			return fmt.Errorf("validating native create dependency %q -> %q: %w", issueID, targetID, nativeStoreError(targetID, err))
		}
		if issue == nil {
			return fmt.Errorf("validating native create dependency %q -> %q: bead %q: %w", issueID, targetID, targetID, ErrNotFound)
		}
	}
	return nil
}

func (s *NativeDoltStore) compensateFailedCreate(ctx context.Context, storage beadslib.Storage, issueID string, deps []*beadslib.Dependency) error {
	if strings.TrimSpace(issueID) == "" {
		return nil
	}
	var errs []error
	for _, dep := range deps {
		if dep == nil {
			continue
		}
		if err := storage.RemoveDependency(ctx, issueID, dep.DependsOnID, s.actor); err != nil {
			errs = append(errs, fmt.Errorf("removing partial native dependency %q -> %q: %w", issueID, dep.DependsOnID, nativeStoreError(issueID, err)))
		}
	}
	if err := storage.DeleteIssue(ctx, issueID); err != nil {
		errs = append(errs, fmt.Errorf("deleting partial native issue %q: %w", issueID, nativeStoreError(issueID, err)))
	}
	return errors.Join(errs...)
}

func nativeCloseReasonFromIssue(issue *beadslib.Issue) string {
	if issue == nil {
		return ""
	}
	metadata, err := metadataMapFromNative(issue.Metadata)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(metadata["close_reason"])
}

func shouldPrevalidateNativeDependency(issueID, targetID, storePrefix string) bool {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(targetID)), "external:") {
		return false
	}
	sourcePrefix := nativeBeadIDPrefix(issueID)
	if sourcePrefix == "" {
		sourcePrefix = normalizeIDPrefix(storePrefix)
	}
	targetPrefix := nativeBeadIDPrefix(targetID)
	return sourcePrefix == "" || targetPrefix == "" || sourcePrefix == targetPrefix
}

func nativeBeadIDPrefix(id string) string {
	before, _, ok := strings.Cut(strings.ToLower(strings.TrimSpace(id)), "-")
	if !ok {
		return ""
	}
	return normalizeIDPrefix(before)
}

func cloneNativeDependencies(deps []*beadslib.Dependency) []*beadslib.Dependency {
	if len(deps) == 0 {
		return nil
	}
	cloned := make([]*beadslib.Dependency, 0, len(deps))
	for _, dep := range deps {
		if dep == nil {
			continue
		}
		depCopy := *dep
		cloned = append(cloned, &depCopy)
	}
	return cloned
}

func nativeIssueFromBead(b Bead) (*beadslib.Issue, error) {
	status := b.Status
	if status == "" {
		status = "open"
	}
	issueType := b.Type
	if issueType == "" {
		issueType = "task"
	}
	issue := &beadslib.Issue{
		ID:          b.ID,
		Title:       b.Title,
		Description: b.Description,
		Status:      beadslib.Status(status),
		IssueType:   beadslib.IssueType(issueType),
		Assignee:    b.Assignee,
		Sender:      b.From,
		CreatedAt:   b.CreatedAt,
		Labels:      append([]string(nil), b.Labels...),
		Ephemeral:   b.Ephemeral,
	}
	if b.Priority != nil {
		issue.Priority = *b.Priority
	} else {
		issue.Priority = 2
	}
	raw, err := metadataRawFromMap(b.Metadata)
	if err != nil {
		return nil, err
	}
	issue.Metadata = raw
	for _, dep := range b.Dependencies {
		issue.Dependencies = append(issue.Dependencies, &beadslib.Dependency{
			IssueID:     dep.IssueID,
			DependsOnID: dep.DependsOnID,
			Type:        beadslib.DependencyType(dep.Type),
		})
	}
	if b.ParentID != "" {
		issue.Dependencies = append(issue.Dependencies, &beadslib.Dependency{
			IssueID:     b.ID,
			DependsOnID: b.ParentID,
			Type:        beadslib.DepParentChild,
		})
	}
	for _, need := range b.Needs {
		depType := "blocks"
		dependsOnID := need
		if before, after, ok := strings.Cut(need, ":"); ok && before != "" && after != "" {
			depType = before
			dependsOnID = after
		}
		issue.Dependencies = append(issue.Dependencies, &beadslib.Dependency{
			IssueID:     b.ID,
			DependsOnID: dependsOnID,
			Type:        beadslib.DependencyType(depType),
		})
	}
	return issue, nil
}

func beadFromNativeIssue(issue *beadslib.Issue) (Bead, error) {
	if issue == nil {
		return Bead{}, nil
	}
	metadata, err := metadataMapFromNative(issue.Metadata)
	if err != nil {
		return Bead{}, fmt.Errorf("parsing metadata for bead %q: %w", issue.ID, err)
	}
	b := Bead{
		ID:          issue.ID,
		Title:       issue.Title,
		Status:      mapBdStatus(string(issue.Status)),
		Type:        string(issue.IssueType),
		Priority:    nativePriorityFromIssue(issue),
		CreatedAt:   issue.CreatedAt,
		Assignee:    issue.Assignee,
		From:        issue.Sender,
		Description: issue.Description,
		Labels:      append([]string(nil), issue.Labels...),
		Metadata:    metadata,
		Ephemeral:   issue.Ephemeral,
	}
	for _, dep := range issue.Dependencies {
		if dep == nil {
			continue
		}
		converted := Dep{
			IssueID:     dep.IssueID,
			DependsOnID: dep.DependsOnID,
			Type:        string(dep.Type),
		}
		b.Dependencies = append(b.Dependencies, converted)
		if dep.Type == beadslib.DepParentChild && b.ParentID == "" {
			b.ParentID = dep.DependsOnID
		}
	}
	return b, nil
}

func nativePriorityFromIssue(issue *beadslib.Issue) *int {
	// Upstream beads stores omitted priority as P2. Gas City's Store surface
	// represents that unset/default state as nil, matching BdStore's sparse
	// JSON decode semantics for callers that distinguish unset from explicit.
	if issue.Priority == 2 {
		return nil
	}
	priority := issue.Priority
	return &priority
}

func nativeIssueFilterFromListQuery(query ListQuery) beadslib.IssueFilter {
	limit := query.Limit
	if query.Sort != SortDefault {
		limit = 0
	}
	filter := beadslib.IssueFilter{
		Limit:               limit,
		MetadataFields:      query.Metadata,
		CreatedBefore:       zeroTimePtr(query.CreatedBefore),
		IncludeDependencies: true,
	}
	switch query.TierMode {
	case TierWisps:
		ephemeral := true
		filter.Ephemeral = &ephemeral
	case TierBoth:
		// no tier filter
	default:
		ephemeral := false
		filter.Ephemeral = &ephemeral
	}
	if query.Status != "" {
		if query.Status == "open" {
			filter.ExcludeStatus = []beadslib.Status{beadslib.StatusClosed, beadslib.StatusInProgress}
		} else {
			status := beadslib.Status(query.Status)
			filter.Status = &status
		}
	} else if !query.IncludeClosed {
		filter.ExcludeStatus = []beadslib.Status{beadslib.StatusClosed}
	}
	if query.Type != "" {
		issueType := beadslib.IssueType(query.Type)
		filter.IssueType = &issueType
	}
	if query.Label != "" {
		filter.Labels = []string{query.Label}
	}
	if query.Assignee != "" {
		filter.Assignee = &query.Assignee
	}
	if query.ParentID != "" {
		filter.ParentID = &query.ParentID
	}
	return filter
}

func nativeStoreError(id string, err error) error {
	if err == nil || errors.Is(err, ErrNotFound) {
		return err
	}
	if !nativeUpstreamNotFound(err) {
		return err
	}
	if id == "" {
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	return fmt.Errorf("bead %q: %w: %w", id, ErrNotFound, err)
}

func nativeUpstreamNotFound(err error) bool {
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	return msg == "not found" ||
		strings.Contains(msg, "not found: issue ") ||
		strings.Contains(msg, "issue not found: ") ||
		((strings.HasPrefix(msg, "issue ") || strings.Contains(msg, " issue ")) && strings.HasSuffix(msg, " not found")) ||
		strings.HasSuffix(msg, ": not found") ||
		msg == "no rows in result set" ||
		strings.HasSuffix(msg, ": no rows in result set")
}

func zeroTimePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func metadataRawFromMap(metadata map[string]string) (json.RawMessage, error) {
	if len(metadata) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		return nil, fmt.Errorf("marshaling metadata: %w", err)
	}
	return raw, nil
}

func metadataMapFromNative(raw json.RawMessage) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var values map[string]interface{}
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("unmarshaling metadata: %w", err)
	}
	metadata := make(map[string]string, len(values))
	for k, v := range values {
		if s, ok := v.(string); ok {
			metadata[k] = s
			continue
		}
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("marshaling metadata value %q: %w", k, err)
		}
		metadata[k] = string(raw)
	}
	return metadata, nil
}
