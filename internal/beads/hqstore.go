package beads

import (
	"fmt"
	"os"
	"sync"
	"time"
)

const (
	hqExpiresAtMetadataKey = "expires_at"
	hqExpiresAtMetadataAlt = "gc.expires_at"
	hqClosedAtMetadataKey  = "gc.hqstore.closed_at"

	hqDefaultClosedTaskRetention = 7 * 24 * time.Hour
)

// HQStore is a dormant, snapshot-backed in-process Store implementation for the
// coordination-store migration experiments. Writes mutate an in-memory indexed
// core with no per-write fsync; durability comes from an async background
// snapshotter that periodically serializes the whole store to a gzip-compressed
// JSONL file and publishes it via atomic rename. It is not wired into live city
// storage; callers must opt in by opening it directly.
type HQStore struct {
	mu sync.RWMutex

	dir    string
	prefix string
	seq    int

	closed bool

	main      map[string]Bead
	wisps     map[string]Bead
	order     []string
	orderSeen map[string]bool
	deps      []Dep
	mainIdx   hqTierIndex
	wispIdx   hqTierIndex

	ttlInterval time.Duration
	ttlStop     chan struct{}
	ttlDone     chan struct{}

	closedTaskRetention time.Duration

	snapshotInterval time.Duration
	snapStop         chan struct{}
	snapDone         chan struct{}
	snapWriteMu      sync.Mutex // serializes concurrent snapshot writers
	snapErrMu        sync.Mutex
	snapErr          error
}

type hqStoreOptions struct {
	prefix           string
	ttlInterval      time.Duration
	closedRetention  time.Duration
	snapshotInterval time.Duration
}

// HQStoreOption customizes OpenHQStore.
type HQStoreOption func(*hqStoreOptions)

// WithHQStoreTTLInterval starts a background TTL sweeper at the given interval.
// A non-positive interval leaves TTL purge explicit via PurgeExpired.
func WithHQStoreTTLInterval(d time.Duration) HQStoreOption {
	return func(o *hqStoreOptions) {
		o.ttlInterval = d
	}
}

// WithHQStoreIDPrefix sets the generated ID prefix. Empty keeps the default.
func WithHQStoreIDPrefix(prefix string) HQStoreOption {
	return func(o *hqStoreOptions) {
		if prefix != "" {
			o.prefix = prefix
		}
	}
}

// WithHQStoreClosedTaskRetention sets how long closed main-tier beads remain
// queryable before the TTL sweeper can delete them. A non-positive duration
// disables closed-task retention sweeping.
func WithHQStoreClosedTaskRetention(d time.Duration) HQStoreOption {
	return func(o *hqStoreOptions) {
		o.closedRetention = d
	}
}

// WithHQStoreSnapshotInterval sets the background snapshot cadence. A
// non-positive interval disables periodic snapshots; Shutdown still flushes a
// final snapshot so an orderly close is always durable.
func WithHQStoreSnapshotInterval(d time.Duration) HQStoreOption {
	return func(o *hqStoreOptions) {
		o.snapshotInterval = d
	}
}

// OpenHQStore opens or creates a dormant HQStore rooted at dir. If a snapshot
// is present it is loaded to rebuild in-memory state and indexes.
func OpenHQStore(dir string, opts ...HQStoreOption) (*HQStore, error) {
	cfg := hqStoreOptions{
		prefix:           "hq",
		closedRetention:  hqDefaultClosedTaskRetention,
		snapshotInterval: hqDefaultSnapshotInterval,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("opening hqstore: %w", err)
	}

	store := &HQStore{
		dir:                 dir,
		prefix:              cfg.prefix,
		ttlInterval:         cfg.ttlInterval,
		closedTaskRetention: cfg.closedRetention,
		snapshotInterval:    cfg.snapshotInterval,
	}
	store.resetCoreLocked()

	if err := store.loadSnapshot(); err != nil {
		return nil, err
	}
	store.startSnapshotter()
	store.startTTLSweeper()
	return store, nil
}

// Shutdown stops the background goroutines, flushes a final snapshot, and marks
// the store closed. It is idempotent.
func (s *HQStore) Shutdown() error {
	s.stopTTLSweeper()
	s.stopSnapshotter()

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	// Final flush after marking closed: writeSnapshot takes only a read lock
	// internally via ExportAll, and no further writes can land because callers
	// hit ensureOpenLocked. snapWriteMu guards against a late periodic flush.
	if err := s.writeSnapshot(); err != nil {
		return fmt.Errorf("shutting down hqstore: %w", err)
	}
	return nil
}

// Ping verifies that the store is open.
func (s *HQStore) Ping() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return fmt.Errorf("pinging hqstore: closed")
	}
	return nil
}

// Tx executes fn against the HQStore write surface.
func (s *HQStore) Tx(_ string, fn func(tx Tx) error) error {
	return runSequentialTx(s, fn)
}

func (s *HQStore) ensureOpenLocked() error {
	if s.closed {
		return fmt.Errorf("hqstore is closed")
	}
	return nil
}
