package beads

import (
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/pathutil"
)

// cacheMutationScopeProvider identifies independently opened store handles
// that mutate the same durable scope. It is intentionally optional: stores
// without a stable scope receive an instance-local coordinator.
type cacheMutationScopeProvider interface {
	CacheMutationScope() string
}

// cacheMutationCoordination is shared by every CachingStore opened over the
// same durable scope. Controller config reloads deliberately overlap old and
// replacement store handles, so mutation locks and observer queues cannot be
// owned by one decorating cache instance.
type cacheMutationCoordination struct {
	mutationScopeMu sync.RWMutex
	// mutationGeneration advances whenever any cache handle over this durable
	// scope commits a local state change and retains the latest generation for
	// each affected bead. Reconciliation snapshots use the per-bead stamps so a
	// replacement handle cannot merge stale lifecycle state without discarding
	// unrelated fresh rows from the same scan.
	mutationGeneration cacheMutationGeneration

	closeStateMu    sync.Mutex
	closeStateLocks map[string]*cacheCloseStateLock

	orderedNotificationMu     sync.Mutex
	orderedNotificationQueues map[string]*cacheOrderedNotificationQueue

	pendingPublications cachePendingPublications
}

// cacheMutationGeneration groups the scope epoch and its per-bead stamps under
// one lock. Reading the epoch cannot observe a generation before its bead
// stamps are visible, which is required for replacement-handle reconciliation
// to fence a mutation that is still finalizing its cache state.
type cacheMutationGeneration struct {
	mu   sync.RWMutex
	next uint64
	byID map[string]uint64
}

func (g *cacheMutationGeneration) advance(ids ...string) uint64 {
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.next++
	if g.byID == nil {
		g.byID = make(map[string]uint64)
	}
	for _, id := range ids {
		if id != "" {
			g.byID[id] = g.next
		}
	}
	return g.next
}

func (g *cacheMutationGeneration) current() uint64 {
	if g == nil {
		return 0
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.next
}

func (g *cacheMutationGeneration) changedSince(id string, generation uint64) bool {
	if g == nil || id == "" {
		return false
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.byID[id] > generation
}

// Registry entries intentionally live for the controller process lifetime.
// StopReconciler does not fence request handlers that already captured an old
// cache, so deleting an entry on reload could split those in-flight handlers
// from the replacement coordinator. Keys are configured durable store scopes,
// not bead IDs or mutations.
var cacheMutationCoordinationRegistry = struct {
	sync.Mutex
	byScope map[string]*cacheMutationCoordination
}{byScope: make(map[string]*cacheMutationCoordination)}

func newCacheMutationCoordination() *cacheMutationCoordination {
	return &cacheMutationCoordination{
		mutationGeneration:        cacheMutationGeneration{byID: make(map[string]uint64)},
		closeStateLocks:           make(map[string]*cacheCloseStateLock),
		orderedNotificationQueues: make(map[string]*cacheOrderedNotificationQueue),
		pendingPublications:       cachePendingPublications{byID: make(map[string]cachePendingPublication)},
	}
}

func cacheMutationCoordinationFor(backing Store) *cacheMutationCoordination {
	scope := cacheMutationScopeFor(backing)
	if scope == "" {
		return newCacheMutationCoordination()
	}
	scope = pathutil.NormalizePathForCompare(scope)

	cacheMutationCoordinationRegistry.Lock()
	defer cacheMutationCoordinationRegistry.Unlock()
	coordination := cacheMutationCoordinationRegistry.byScope[scope]
	if coordination == nil {
		coordination = newCacheMutationCoordination()
		cacheMutationCoordinationRegistry.byScope[scope] = coordination
	}
	return coordination
}

func cacheMutationScopeFor(store Store) string {
	// Wrapper layers in this package expose ConditionalWritesResolveTarget;
	// follow a bounded chain so optional scope identity is not hidden by an
	// embedded Store interface. The bound also makes a malformed self-cycle
	// harmless without requiring dynamic Store values to be comparable.
	for range 8 {
		if provider, ok := store.(cacheMutationScopeProvider); ok {
			if scope := strings.TrimSpace(provider.CacheMutationScope()); scope != "" {
				return scope
			}
		}
		targeter, ok := store.(ConditionalWritesResolveTargeter)
		if !ok {
			return ""
		}
		store = targeter.ConditionalWritesResolveTarget()
		if store == nil {
			return ""
		}
	}
	return ""
}

// CacheMutationScope returns the durable scope shared by BdStore handles.
func (s *BdStore) CacheMutationScope() string {
	if s == nil {
		return ""
	}
	return s.dir
}

// CacheMutationScope returns the durable scope shared by native Dolt handles.
func (s *NativeDoltStore) CacheMutationScope() string {
	if s == nil {
		return ""
	}
	return s.scopeRoot
}

// CacheMutationScope returns the file whose mutations are shared by FileStore
// handles.
func (s *FileStore) CacheMutationScope() string {
	if s == nil {
		return ""
	}
	return s.path
}
