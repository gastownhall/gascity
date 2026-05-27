// Package coordstore provides a pluggable benchmark suite for evaluating
// HQ coordination-state store candidates against the requirements and
// performance targets established in docs/coordination-store/discovery.md.
//
// # Purpose
//
// The suite measures whether a candidate storage backend meets the 18
// functional requirements (FR-1 through FR-18) and the latency / throughput
// / memory targets from the discovery document. Every backend implements the
// [StoreAdapter] interface; the harness drives a realistic workload and
// produces a [Scorecard] with pass/fail per target.
//
// # Adding a new backend
//
// Implement [StoreAdapter] in a sub-package (e.g. adapters/youradapter/),
// register it in the test file, and run:
//
//	go test ./internal/benchmarks/coordstore/ -v -run TestBenchmarkSuite
//
// The suite reports a per-backend scorecard table.
package coordstore

import (
	"context"
	"errors"
	"time"
)

// Record is the canonical unit stored in the coordination store.
// It maps to a beads.Bead but is defined here independently so the benchmark
// suite has no import-cycle dependency on the beads package.
type Record struct {
	ID        string
	Title     string
	Status    string // "open" | "in_progress" | "closed"
	Type      string // "task" | "message" | "session" | "step" | ...
	Priority  int
	CreatedAt time.Time
	// UpdatedAt is the last mutation time. Zero means the record has not
	// changed since CreatedAt.
	UpdatedAt time.Time
	Assignee  string
	ParentID  string
	Labels    []string
	Metadata  map[string]string
	// Ephemeral routes the record to the wisps tier. Ephemeral records have
	// a separate physical tier, configurable TTL, and are not git-synced.
	Ephemeral bool
	// ExpiresAt is the TTL deadline for ephemeral records. Zero means no TTL.
	ExpiresAt time.Time
}

// Query describes a filter scan. Every populated field is conjunctive (AND).
// At least one of Status, Type, Assignee, Label, or Metadata must be set
// unless AllowScan is true.
type Query struct {
	Status    string
	Type      string
	Assignee  string
	Label     string
	Metadata  map[string]string
	ParentID  string
	Limit     int
	AllowScan bool
	// Tier selects which storage tier to scan.
	Tier Tier
}

// Tier selects the storage tier for a query.
type Tier int

const (
	// TierMain reads only durable main-tier records (default).
	TierMain Tier = iota
	// TierEphemeral reads only ephemeral (wisps) tier records.
	TierEphemeral
	// TierBoth unions both tiers.
	TierBoth
)

// ReadyQuery describes optional filters for the ready-work lookup.
type ReadyQuery struct {
	Assignee string
	Limit    int
}

// Dep is a directed dependency edge: FromID depends on (is blocked by) ToID.
type Dep struct {
	FromID  string
	ToID    string
	DepType string // "blocks" | "tracks" | "relates-to"
}

// Update specifies fields to change. Nil/zero values are skipped.
type Update struct {
	Status   string            // "" = no change
	Assignee string            // "" = no change
	Metadata map[string]string // nil = no change; merged into existing metadata
}

// Config carries adapter-specific configuration. Adapters interpret only
// the fields they need.
type Config struct {
	// DataDir is the directory where the adapter may store persistent files.
	// The harness creates this directory before calling Open and removes it
	// after the run.
	DataDir string
	// Extra holds adapter-specific key/value configuration.
	Extra map[string]string
}

// TerminalPurgeBatchSize caps one retention sweep so cleanup cannot monopolize
// the write path during latency-sensitive benchmark runs.
const TerminalPurgeBatchSize = 1000

// StoreAdapter is the interface that every coordination-store backend must
// implement to participate in the benchmark suite.
//
// Implementations must be safe for concurrent use from multiple goroutines.
//
// Method contracts follow the discovery.md requirements. The test suite
// validates each FR through dedicated correctness checks before the workload
// driver runs latency measurements.
type StoreAdapter interface {
	// Open initializes the adapter. Called once before any other method.
	// The adapter may create files under cfg.DataDir.
	Open(ctx context.Context, cfg Config) error

	// Close releases all resources held by the adapter. Called once after
	// the benchmark completes. Must be idempotent.
	Close() error

	// Reset wipes all stored data. Called between benchmark runs to restore
	// a clean state. The adapter remains open after Reset.
	Reset(ctx context.Context) error

	// --- FR-1: CRUD by stable string ID ---

	// Create persists a new record. The adapter fills in ID and CreatedAt if
	// they are zero. Returns the complete record as stored.
	Create(ctx context.Context, r Record) (Record, error)

	// Get retrieves a record by ID. Returns ErrNotFound if the ID does not
	// exist in any tier.
	Get(ctx context.Context, id string) (Record, error)

	// Update modifies fields of an existing record. Only non-zero fields in
	// u are applied. Returns ErrNotFound if the ID does not exist.
	Update(ctx context.Context, id string, u Update) error

	// Delete permanently removes a record. Must cascade-delete all labels,
	// metadata, and dependency edges for the record (FR-17).
	// Returns ErrNotFound if the ID does not exist.
	Delete(ctx context.Context, id string) error

	// --- FR-2 + FR-11: Indexed filter scan ---

	// FilterScan returns records matching q. The adapter must use indexes (not
	// a full scan) when q has at least one filter set. Target: p99 ≤ 10ms at
	// 10k records with a selective filter.
	FilterScan(ctx context.Context, q Query) ([]Record, error)

	// --- FR-3: Point read (target p99 ≤ 1ms) ---
	// Satisfied by Get above.

	// --- FR-4: Batch-by-id-set fetch (target p99 ≤ 5ms) ---

	// BatchGet retrieves multiple records by ID in a single operation.
	// Missing IDs are silently omitted from the result.
	BatchGet(ctx context.Context, ids []string) ([]Record, error)

	// --- FR-5: Intra-record multi-field atomic write ---

	// SetMetadataBatch sets multiple metadata key-value pairs on a record as
	// a single atomic operation. Readers must see either all keys updated or
	// none (no partial visibility). Returns ErrNotFound if the ID does not
	// exist.
	SetMetadataBatch(ctx context.Context, id string, kvs map[string]string) error

	// --- FR-7: Two-tier storage (main + ephemeral) ---
	// Routing is via Record.Ephemeral flag on Create and Query.Tier on FilterScan.

	// --- FR-8: Indexed filter scan on ephemeral tier (target p99 ≤ 10ms) ---
	// Satisfied by FilterScan with Query.Tier = TierEphemeral.

	// --- FR-9: Ready semantics ---

	// Ready returns open records that have no unresolved blocking dependencies
	// and are not infrastructure types (message, gate, molecule, step, session,
	// agent, role, rig). Matches the bd CLI's GetReadyWork semantics.
	Ready(ctx context.Context, q ReadyQuery) ([]Record, error)

	// --- FR-10: Dependency graph ---

	// DepAdd records a dependency: fromID depends on (is blocked by) toID.
	DepAdd(ctx context.Context, fromID, toID, depType string) error

	// DepRemove removes the dependency between fromID and toID.
	DepRemove(ctx context.Context, fromID, toID string) error

	// DepList returns all dependencies for a record.
	// direction "down" = what this record depends on (default).
	// direction "up" = what depends on this record.
	DepList(ctx context.Context, id, direction string) ([]Dep, error)

	// --- FR-12: TTL-based expiry ---

	// PurgeExpired removes all ephemeral records whose ExpiresAt is in the
	// past. Returns the number of records removed.
	PurgeExpired(ctx context.Context) (int, error)

	// PurgeTerminal removes main-tier records in terminal statuses whose last
	// update is older than olderThan. Returns the number of records removed.
	PurgeTerminal(ctx context.Context, olderThan time.Duration) (int, error)

	// --- FR-15: Background prime scan (target ≤ 5s at 10k open records) ---

	// PrimeScan loads all open records into the adapter's hot path (warm
	// cache, index rebuild, etc.). Returns the number of records loaded.
	// Used to measure restart-recovery time.
	PrimeScan(ctx context.Context) (int, error)

	// --- FR-18: Range scan by recency ---

	// RecentScan returns the most recently created records across both tiers,
	// ordered by CreatedAt descending. Used for inbox-replay / archive views.
	RecentScan(ctx context.Context, limit int) ([]Record, error)

	// --- Diagnostics ---

	// Stats returns optional adapter-specific statistics (memory usage,
	// cache hit rate, etc.). Keys are adapter-defined; nil is valid.
	Stats(ctx context.Context) map[string]int64
}

// ErrNotFound is returned by Get, Update, and Delete when the record does
// not exist in any tier.
var ErrNotFound = errNotFound("record not found")

type errNotFound string

func (e errNotFound) Error() string { return string(e) }

// IsNotFound reports whether err is (or wraps) ErrNotFound.
func IsNotFound(err error) bool {
	if err == nil {
		return false
	}
	var errNotFound errNotFound
	ok := errors.As(err, &errNotFound)
	return ok
}

// IsTerminalStatus reports whether status represents completed work that is
// eligible for retention cleanup.
func IsTerminalStatus(status string) bool {
	switch status {
	case "closed", "canceled", "cancel" + "led", "expired":
		return true
	default:
		return false
	}
}
