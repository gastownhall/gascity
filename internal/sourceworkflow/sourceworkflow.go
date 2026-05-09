// Package sourceworkflow provides primitives for enforcing the "one live
// graph workflow per source bead" invariant. It owns the singleton scanner
// (ListLiveRoots), the cross-process launch lock (WithLock), the conflict
// error type (ConflictError), and helpers for snapshotting / closing /
// restoring workflow subtrees during force-replacement flows. Callers in
// internal/sling and cmd/gc use this package to gate graph launches and
// to drive the `gc workflow delete-source` / `reopen-source` recovery
// commands.
package sourceworkflow

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/closeorder"
	"github.com/gastownhall/gascity/internal/citylayout"
)

// ConflictError is returned when a graph workflow launch is blocked by one
// or more already-live workflow roots for the same source bead. The CLI
// maps this to exit code 3 and renders a `gc workflow delete-source`
// cleanup hint; the API maps it to HTTP 409.
type ConflictError struct {
	SourceBeadID string
	WorkflowIDs  []string
}

// SourceStoreRefMetadataKey is the bead metadata key recording which store
// a workflow root's source bead lives in (e.g. "city:foo" or "rig:alpha").
// Used by WorkflowMatchesSource to scope cross-store singleton checks.
const SourceStoreRefMetadataKey = "gc.source_store_ref"

// IsWorkflowRoot reports whether a bead is a source-workflow root. It must
// stay in sync with sling.IsWorkflowAttachment: roots may be marked via the
// legacy gc.kind=workflow label, via gc.formula_contract=graph.v2, or both.
// Queries that only match one label miss graph.v2-only roots and allow
// --force to spawn duplicates.
func IsWorkflowRoot(b beads.Bead) bool {
	return strings.EqualFold(strings.TrimSpace(b.Metadata["gc.kind"]), "workflow") ||
		strings.EqualFold(strings.TrimSpace(b.Metadata["gc.formula_contract"]), "graph.v2")
}

func (e *ConflictError) Error() string {
	if e == nil {
		return "source workflow conflict"
	}
	if len(e.WorkflowIDs) == 0 {
		return fmt.Sprintf("source bead %s already has a live workflow", e.SourceBeadID)
	}
	return fmt.Sprintf(
		"source bead %s already has live workflow(s): %s",
		e.SourceBeadID,
		strings.Join(e.WorkflowIDs, ","),
	)
}

// NormalizeSourceBeadID trims whitespace from a source bead ID so equality
// checks don't fail on stray spaces from user-entered labels.
func NormalizeSourceBeadID(sourceBeadID string) string {
	return strings.TrimSpace(sourceBeadID)
}

// NormalizeSourceStoreRef trims whitespace from a store ref for comparison.
func NormalizeSourceStoreRef(sourceStoreRef string) string {
	return strings.TrimSpace(sourceStoreRef)
}

// LockScopeForStoreRef returns the filesystem scope used for source-workflow
// locks for a source bead's resident store ref.
func LockScopeForStoreRef(cityPath, defaultStorePath, storeRef string, rigPath func(string) (string, bool)) string {
	cityPath = strings.TrimSpace(cityPath)
	defaultStorePath = strings.TrimSpace(defaultStorePath)
	storeRef = strings.TrimSpace(storeRef)
	if storeRef == "" {
		switch {
		case defaultStorePath != "":
			return filepath.Clean(defaultStorePath)
		case cityPath != "":
			return filepath.Clean(cityPath)
		default:
			return ""
		}
	}
	if cityPath == "" {
		return filepath.Clean(storeRef)
	}
	switch {
	case strings.HasPrefix(storeRef, "city:"):
		return filepath.Clean(cityPath)
	case strings.HasPrefix(storeRef, "rig:"):
		rigName := strings.TrimSpace(strings.TrimPrefix(storeRef, "rig:"))
		if rigPath != nil {
			if path, ok := rigPath(rigName); ok {
				path = strings.TrimSpace(path)
				if path != "" {
					if !filepath.IsAbs(path) {
						path = filepath.Join(cityPath, path)
					}
					return filepath.Clean(path)
				}
			}
		}
	}
	return filepath.Clean(storeRef)
}

// WorkflowMatchesSource reports whether a workflow root belongs to the
// given source bead and (optionally) a specific source store ref. Legacy
// roots without SourceStoreRefMetadataKey are treated as belonging to the
// store they physically live in (rootStoreRef).
func WorkflowMatchesSource(root beads.Bead, sourceBeadID, sourceStoreRef, rootStoreRef string) bool {
	sourceBeadID = NormalizeSourceBeadID(sourceBeadID)
	if sourceBeadID == "" {
		return false
	}
	if NormalizeSourceBeadID(root.Metadata["gc.source_bead_id"]) != sourceBeadID {
		return false
	}
	sourceStoreRef = NormalizeSourceStoreRef(sourceStoreRef)
	if sourceStoreRef == "" {
		return true
	}
	rootSourceStoreRef := NormalizeSourceStoreRef(root.Metadata[SourceStoreRefMetadataKey])
	if rootSourceStoreRef != "" {
		return rootSourceStoreRef == sourceStoreRef
	}
	rootStoreRef = NormalizeSourceStoreRef(rootStoreRef)
	if rootStoreRef == "" {
		return false
	}
	return rootStoreRef == sourceStoreRef
}

// ListLiveRoots returns the live (not-closed) workflow roots in store that
// belong to sourceBeadID, scoped to sourceStoreRef when set. The query
// indexes on gc.source_bead_id and filters via IsWorkflowRoot so both
// legacy gc.kind=workflow roots and graph.v2-only roots are visible.
func ListLiveRoots(store beads.Store, sourceBeadID, sourceStoreRef, rootStoreRef string) ([]beads.Bead, error) {
	sourceBeadID = NormalizeSourceBeadID(sourceBeadID)
	if store == nil || sourceBeadID == "" {
		return nil, nil
	}
	roots, err := store.List(beads.ListQuery{
		Metadata: map[string]string{
			"gc.source_bead_id": sourceBeadID,
		},
	})
	if err != nil {
		return nil, err
	}
	roots = slices.DeleteFunc(roots, func(root beads.Bead) bool {
		if !IsWorkflowRoot(root) {
			return true
		}
		return !WorkflowMatchesSource(root, sourceBeadID, sourceStoreRef, rootStoreRef)
	})
	slices.SortFunc(roots, func(a, b beads.Bead) int {
		return strings.Compare(a.ID, b.ID)
	})
	return roots, nil
}

// BlockingWorkflowIDs extracts sorted root IDs from a list of blocking
// workflows for rendering in ConflictError messages and cleanup hints.
func BlockingWorkflowIDs(roots []beads.Bead) []string {
	ids := make([]string, 0, len(roots))
	for _, root := range roots {
		if root.ID == "" {
			continue
		}
		ids = append(ids, root.ID)
	}
	slices.Sort(ids)
	return ids
}

var (
	localLocksMu sync.Mutex
	localLocks   = map[string]*localLock{}
)

const fileLockRetryInterval = 25 * time.Millisecond

type localLock struct {
	token chan struct{}
	refs  int
}

// WithLock acquires a per-source-bead lock (in-process mutex + on-disk
// flock) rooted at cityPath before invoking fn. Guarantees at-most-one
// concurrent graph-workflow launch or recovery per (scopeRef, sourceBeadID)
// across processes. Honors ctx cancellation for both mutex and flock waits.
func WithLock(ctx context.Context, cityPath, scopeRef, sourceBeadID string, fn func() error) error {
	sourceBeadID = NormalizeSourceBeadID(sourceBeadID)
	if sourceBeadID == "" {
		return fn()
	}
	lockPath, key, err := lockIdentity(cityPath, scopeRef, sourceBeadID)
	if err != nil {
		return err
	}
	mu := inProcessMutex(key)
	defer releaseInProcessMutex(key, mu)
	if err := mu.Lock(ctx); err != nil {
		return err
	}
	defer mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return fmt.Errorf("create source workflow lock dir: %w", err)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open source workflow lock: %w", err)
	}
	defer f.Close() //nolint:errcheck // best-effort cleanup
	if err := lockFile(ctx, f, sourceBeadID); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:errcheck // best-effort unlock
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn()
}

func inProcessMutex(key string) *localLock {
	localLocksMu.Lock()
	defer localLocksMu.Unlock()
	mu := localLocks[key]
	if mu == nil {
		mu = newLocalLock()
		localLocks[key] = mu
	}
	mu.refs++
	return mu
}

func releaseInProcessMutex(key string, mu *localLock) {
	localLocksMu.Lock()
	defer localLocksMu.Unlock()
	current := localLocks[key]
	if current == nil || current != mu {
		return
	}
	if current.refs > 0 {
		current.refs--
	}
	if current.refs == 0 {
		delete(localLocks, key)
	}
}

func newLocalLock() *localLock {
	lock := &localLock{token: make(chan struct{}, 1)}
	lock.token <- struct{}{}
	return lock
}

func (l *localLock) Lock(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-l.token:
		return nil
	}
}

func (l *localLock) Unlock() {
	l.token <- struct{}{}
}

func lockFile(ctx context.Context, f *os.File, sourceBeadID string) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return fmt.Errorf("lock source workflow %q: %w", sourceBeadID, err)
		}
		timer := time.NewTimer(fileLockRetryInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func lockIdentity(cityPath, scopeRef, sourceBeadID string) (lockPath, key string, _ error) {
	cityPath, err := canonicalCityPath(cityPath)
	if err != nil {
		return "", "", err
	}
	scopeRef = canonicalScopeRef(scopeRef)
	if scopeRef == "" {
		scopeRef = "city"
	}
	hash := sha256.Sum256([]byte(scopeRef + "\x00" + sourceBeadID))
	key = cityPath + "\x00" + scopeRef + "\x00" + sourceBeadID
	lockPath = filepath.Join(
		citylayout.RuntimeDataDir(cityPath),
		"sling-source-locks",
		hex.EncodeToString(hash[:])+".lock",
	)
	return lockPath, key, nil
}

func canonicalScopeRef(scopeRef string) string {
	scopeRef = strings.TrimSpace(scopeRef)
	if scopeRef == "" {
		return ""
	}
	scopeRef = filepath.Clean(scopeRef)
	if resolved, err := filepath.EvalSymlinks(scopeRef); err == nil && strings.TrimSpace(resolved) != "" {
		return resolved
	}
	return scopeRef
}

// ListWorkflowBeads returns the root and all descendant beads tagged with
// gc.root_bead_id=rootID (closed included). Used by CloseWorkflowSubtree
// and force-replacement snapshot/restore.
func ListWorkflowBeads(store beads.Store, rootID string) ([]beads.Bead, error) {
	rootID = strings.TrimSpace(rootID)
	if store == nil || rootID == "" {
		return nil, nil
	}
	root, err := store.Get(rootID)
	if err != nil {
		return nil, err
	}
	descendants, err := store.List(beads.ListQuery{
		IncludeClosed: true,
		Metadata: map[string]string{
			"gc.root_bead_id": rootID,
		},
	})
	if err != nil {
		return nil, err
	}
	beadsByID := map[string]beads.Bead{
		root.ID: root,
	}
	for _, bead := range descendants {
		beadsByID[bead.ID] = bead
	}
	out := make([]beads.Bead, 0, len(beadsByID))
	for _, bead := range beadsByID {
		out = append(out, bead)
	}
	slices.SortFunc(out, func(a, b beads.Bead) int {
		return strings.Compare(a.ID, b.ID)
	})
	return out, nil
}

// CloseWorkflowSubtree closes the root and every open descendant of a
// workflow, marking each gc.outcome=skipped. It closes descendants before the
// root and honors in-batch "blocks" dependencies so strict stores can close
// workflow step chains without rejecting blocked-before-blocker order. Returns
// the count of newly closed beads.
func CloseWorkflowSubtree(store beads.Store, rootID string) (int, error) {
	matched, err := ListWorkflowBeads(store, rootID)
	if err != nil {
		return 0, err
	}
	byID := make(map[string]beads.Bead, len(matched))
	for _, bead := range matched {
		byID[bead.ID] = bead
	}
	depthMemo := make(map[string]int, len(matched))
	const visitingDepth = -1
	var depth func(string) int
	depth = func(id string) int {
		if d, ok := depthMemo[id]; ok {
			if d == visitingDepth {
				return 0
			}
			return d
		}
		bead, ok := byID[id]
		if !ok {
			return 0
		}
		parentID := strings.TrimSpace(bead.ParentID)
		if parentID == "" || parentID == id {
			depthMemo[id] = 0
			return 0
		}
		parent, ok := byID[parentID]
		if !ok || parent.ID == "" {
			depthMemo[id] = 0
			return 0
		}
		depthMemo[id] = visitingDepth
		d := depth(parentID) + 1
		depthMemo[id] = d
		return d
	}
	slices.SortFunc(matched, func(a, b beads.Bead) int {
		if da, db := depth(a.ID), depth(b.ID); da != db {
			return cmp.Compare(db, da)
		}
		return cmp.Compare(a.ID, b.ID)
	})
	ids := make([]string, 0, len(matched))
	for _, bead := range matched {
		if bead.ID == "" || bead.Status == "closed" {
			continue
		}
		ids = append(ids, bead.ID)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	ordered, err := closeorder.Order(store, ids)
	if err != nil {
		return 0, err
	}
	return store.CloseAll(ordered, map[string]string{"gc.outcome": "skipped"})
}

// WorkflowBeadSnapshot captures the mutable fields of a workflow subtree
// bead so force-replacement can restore them if the replacement's finalize
// or post-finalize invariant check fails.
type WorkflowBeadSnapshot struct {
	ID       string
	Status   string
	Assignee string
	Outcome  string
}

// SnapshotOpenWorkflowBeads records the status/assignee/outcome of every
// open bead in a workflow subtree, used to roll back a force-replacement
// on finalize failure.
func SnapshotOpenWorkflowBeads(store beads.Store, rootID string) ([]WorkflowBeadSnapshot, error) {
	matched, err := ListWorkflowBeads(store, rootID)
	if err != nil {
		return nil, err
	}
	out := make([]WorkflowBeadSnapshot, 0, len(matched))
	for _, bead := range matched {
		if bead.ID == "" || bead.Status == "closed" {
			continue
		}
		out = append(out, WorkflowBeadSnapshot{
			ID:       bead.ID,
			Status:   bead.Status,
			Assignee: bead.Assignee,
			Outcome:  bead.Metadata["gc.outcome"],
		})
	}
	return out, nil
}

// RestoreWorkflowBeads re-applies a prior WorkflowBeadSnapshot set.
// Continues past individual failures and joins them into one error so the
// caller sees every restoration problem at once.
func RestoreWorkflowBeads(store beads.Store, snapshots []WorkflowBeadSnapshot) error {
	var restoreErr error
	for _, snapshot := range snapshots {
		if strings.TrimSpace(snapshot.ID) == "" {
			continue
		}
		status := snapshot.Status
		assignee := snapshot.Assignee
		if err := store.Update(snapshot.ID, beads.UpdateOpts{
			Status:   &status,
			Assignee: &assignee,
		}); err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("restore bead %s: %w", snapshot.ID, err))
			continue
		}
		if err := store.SetMetadata(snapshot.ID, "gc.outcome", snapshot.Outcome); err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("restore bead %s outcome: %w", snapshot.ID, err))
		}
	}
	return restoreErr
}

func canonicalCityPath(cityPath string) (string, error) {
	cleaned := filepath.Clean(strings.TrimSpace(cityPath))
	if cleaned == "" || cleaned == "." {
		return "", fmt.Errorf("source workflow lock requires city path")
	}
	abs, err := filepath.Abs(cleaned)
	if err != nil {
		return "", fmt.Errorf("canonicalize city path: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil && strings.TrimSpace(resolved) != "" {
		return resolved, nil
	}
	return abs, nil
}
