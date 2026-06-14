// Package beads provides the bead store abstraction — the universal persistence
// substrate for Gas City work units (tasks, messages, molecules, etc.).
package beads

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned when a bead ID does not exist in the store.
var ErrNotFound = errors.New("bead not found")

// ErrCacheUnavailable is returned by cache-only read handles when the cache
// cannot answer without consulting the backing store.
var ErrCacheUnavailable = errors.New("bead cache unavailable")

// ErrStoreClosed is returned when a caller uses a bead store after its backing
// handle has been closed.
var ErrStoreClosed = errors.New("bead store closed")

// ErrParentProjectionSuperseded reports that a parent update was overtaken by a
// concurrent reparent before the caller's projection wait could converge.
var ErrParentProjectionSuperseded = errors.New("parent projection superseded by concurrent update")

// ErrConditionalReleaseUnsupported reports that a store cannot atomically
// release an assignment based on the current status and assignee.
var ErrConditionalReleaseUnsupported = errors.New("conditional assignment release unsupported")

// ErrBDSilentFallback reports that a bd-backed store operation saw bd exit
// successfully after falling back to on-disk JSONL auto-import mode. BdStore
// surfaces this as an error for reads and writes because the command may have
// observed or mutated an empty fallback database instead of the configured
// backend. Detection requires bd's paired fallback markers: "auto-importing"
// and "into empty database".
var ErrBDSilentFallback = errors.New("bd silent fallback to on-disk auto-import")

// Bead is a single unit of work in Gas City. Everything is a bead: tasks,
// mail, molecules, convoys.
type Bead struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Status    string    `json:"status"`     // "open", "in_progress", "closed"
	Type      string    `json:"issue_type"` // "task" default; matches bd wire format
	Priority  *int      `json:"priority,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt is zero for legacy beads; UpdatedBefore falls back to CreatedAt.
	UpdatedAt    time.Time         `json:"updated_at,omitempty,omitzero"`
	Assignee     string            `json:"assignee,omitempty"`
	From         string            `json:"from,omitempty"`
	ParentID     string            `json:"parent,omitempty"`      // step → molecule; matches bd wire format
	Ref          string            `json:"ref,omitempty"`         // formula step ID or formula name
	Needs        []string          `json:"needs,omitempty"`       // dependency step refs
	Description  string            `json:"description,omitempty"` // step instructions
	Labels       []string          `json:"labels,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
	Dependencies []Dep             `json:"dependencies,omitempty"`
	// Ephemeral routes the bead to the wisps tier on Create. Wisps live in
	// a separate Dolt table, are not git-synced, and are eligible for TTL
	// garbage collection. Reads must opt in via ListQuery.TierMode (or the
	// WithEphemeral/WithBothTiers QueryOpts on the legacy label helpers).
	Ephemeral bool `json:"ephemeral,omitempty"`
	// NoHistory routes the bead to durable no-history storage on Create. These
	// rows are visible in normal durable reads but do not add Dolt history.
	NoHistory bool `json:"no_history,omitempty"`
	// DeferUntil hides the bead from ready/claimable views until this time,
	// mirroring bd's defer_until column (a future value means "not yet ready";
	// nil or past means ready). Create paths preserve it; UpdateOpts does not
	// mutate it.
	DeferUntil *time.Time `json:"defer_until,omitempty"`
	// IsBlocked carries bd's denormalized ready-work projection. Nil means the
	// store did not provide the projection and cached ready falls back to
	// dependency-derived readiness for backward compatibility.
	IsBlocked *bool `json:"is_blocked,omitempty"`
}

// UpdateOpts specifies which fields to change. Nil pointers are skipped.
type UpdateOpts struct {
	Title        *string // set title (nil = no change)
	Status       *string // set status (nil = no change)
	Type         *string // set issue type (nil = no change)
	Priority     *int    // set priority (nil = no change)
	Description  *string
	ParentID     *string
	Assignee     *string  // set assignee (nil = no change)
	Labels       []string // append these labels (nil = no change)
	RemoveLabels []string // remove these labels (nil = no change)
	Metadata     map[string]string
}

// ConditionalAssignmentReleaser is implemented by stores that can release an
// in-progress assignment only when the current status and assignee still match
// the expected snapshot.
type ConditionalAssignmentReleaser interface {
	ReleaseIfCurrent(id, expectedAssignee string) (bool, error)
}

// Tx is the write surface available inside a Store.Tx callback.
// Keep this interface limited to methods needed by current transactional
// write pairs; do not add Store methods speculatively.
type Tx interface {
	Update(id string, opts UpdateOpts) error
	SetMetadataBatch(id string, kvs map[string]string) error
	Close(id string) error
}

func runSequentialTx(tx Tx, fn func(Tx) error) error {
	if fn == nil {
		return errors.New("beads tx: nil callback")
	}
	return fn(tx)
}

func cloneIntPtr(v *int) *int {
	if v == nil {
		return nil
	}
	cloned := *v
	return &cloned
}

func cloneTimePtr(v *time.Time) *time.Time {
	if v == nil {
		return nil
	}
	cloned := *v
	return &cloned
}

func cloneBoolPtr(v *bool) *bool {
	if v == nil {
		return nil
	}
	cloned := *v
	return &cloned
}

// containerTypes enumerates bead types that group child beads for
// batch expansion during dispatch.
var containerTypes = map[string]bool{
	"convoy": true,
}

// IsContainerType reports whether the bead type groups child beads
// that should be expanded during dispatch.
func IsContainerType(t string) bool {
	return containerTypes[t]
}

// moleculeTypes enumerates bead types that represent attached or
// standalone molecules (wisps, full molecules).
var moleculeTypes = map[string]bool{
	"molecule": true,
	"wisp":     true,
}

// IsMoleculeType reports whether the bead type represents a molecule
// or wisp attached to a parent bead.
func IsMoleculeType(t string) bool {
	return moleculeTypes[t]
}

// readyExcludeTypes enumerates bead types that Ready() excludes by
// default. These are infrastructure or workflow-container types that
// represent internal bookkeeping rather than actionable work. This
// matches the exclusion list in the bd CLI's GetReadyWork query.
var readyExcludeTypes = map[string]bool{
	"merge-request": true, // processed by automation
	"gate":          true, // async wait conditions
	"molecule":      true, // workflow containers
	"step":          true, // non-root formula steps; parent molecule is the actionable unit (#1039)
	"message":       true, // mail/communication items
	"session":       true, // runtime/session continuity beads, never actionable work
	"agent":         true, // identity/state tracking beads
	"role":          true, // agent role definitions
	"rig":           true, // rig identity beads
}

var readyBlockingDependencyTypes = map[string]bool{
	"blocks":             true,
	"waits-for":          true,
	"conditional-blocks": true,
}

// IsReadyBlockingDependencyType reports whether a dependency type blocks a
// bead from Ready() until the dependency target closes.
func IsReadyBlockingDependencyType(t string) bool {
	return readyBlockingDependencyTypes[t]
}

// IsReadyExcludedType reports whether the bead type is excluded from
// Ready() results by default.
func IsReadyExcludedType(t string) bool {
	return readyExcludeTypes[t]
}

// IsReadyCandidate reports whether a bead passes the store-independent default
// Ready filters: open status, main tier, actionable type, and no future
// defer_until. Dependency and assignee checks are store-specific and happen
// separately.
func IsReadyCandidate(b Bead, now time.Time) bool {
	return IsReadyCandidateForTier(b, now, TierIssues)
}

// IsReadyCandidateForTier reports whether a bead passes the store-independent
// Ready filters for the requested storage tier.
func IsReadyCandidateForTier(b Bead, now time.Time, tier TierMode) bool {
	switch tier {
	case TierWisps:
		if !b.Ephemeral && !b.NoHistory {
			return false
		}
	case TierBoth:
		// no tier filter
	default: // TierIssues
		if b.Ephemeral {
			return false
		}
	}
	return b.Status == "open" &&
		!IsReadyExcludedBead(b) &&
		!IsDeferred(b, now)
}

// IsReadyExcludedBead reports whether a bead is infrastructure rather than
// actionable Ready work.
func IsReadyExcludedBead(b Bead) bool {
	if IsReadyExcludedType(b.Type) {
		return true
	}
	for _, label := range b.Labels {
		switch label {
		case "gc:session", "gc:order-tracking", "order-tracking":
			return true
		}
	}
	return false
}

// IsDeferred reports whether a bead is hidden by a future defer_until,
// mirroring bd ready's server-side filter (defer_until IS NULL OR <= now is
// ready) and cmd_hook.isFutureDeferredHookCandidate.
func IsDeferred(b Bead, now time.Time) bool {
	return b.DeferUntil != nil && b.DeferUntil.After(now)
}

func isReadyBlockingDependencyType(t string) bool {
	return IsReadyBlockingDependencyType(t)
}

// Dep represents a dependency relationship between two beads. The IssueID
// depends on (is blocked by) DependsOnID. Type describes the relationship
// kind (e.g. "blocks", "tracks", "relates-to").
type Dep struct {
	IssueID     string `json:"issue_id"`
	DependsOnID string `json:"depends_on_id"`
	Type        string `json:"type"` // "blocks", "tracks", "relates-to", etc.
}

// QueryOpt controls query behavior for list methods.
type QueryOpt int

const (
	// IncludeClosed extends the query to include closed beads.
	// Without this, cached queries only return non-closed beads.
	IncludeClosed QueryOpt = iota + 1
	// WithEphemeral routes the legacy label helpers (ListByLabel,
	// ListByMetadata) at the wisps tier instead of the default issues tier.
	WithEphemeral
	// WithBothTiers unions the issues and wisps tiers in a single query.
	// Mutually exclusive with WithEphemeral; if both are passed,
	// WithBothTiers wins.
	WithBothTiers
)

// HasOpt returns true if opts contains the given option.
func HasOpt(opts []QueryOpt, want QueryOpt) bool {
	for _, o := range opts {
		if o == want {
			return true
		}
	}
	return false
}

// Store is the interface for bead persistence. Implementations must assign
// unique non-empty IDs, default Status to "open", default Type to "task",
// and set CreatedAt on Create. The ID format is implementation-specific
// (e.g. "gc-1" for FileStore, "bd-XXXX" for BdStore).
type Store interface {
	// Create persists a new bead. The caller provides Title and optionally
	// Type; the store fills in ID, Status, and CreatedAt. Returns the
	// complete bead.
	Create(b Bead) (Bead, error)

	// Get retrieves a bead by ID. Returns ErrNotFound (possibly wrapped)
	// if the ID does not exist.
	Get(id string) (Bead, error)

	// Update modifies fields of an existing bead. Only non-nil fields in opts
	// are applied. Returns ErrNotFound if the bead does not exist.
	Update(id string, opts UpdateOpts) error

	// Close sets a bead's status to "closed". Returns ErrNotFound if the ID
	// does not exist. Closing an already-closed bead is a no-op.
	Close(id string) error

	// Reopen sets a closed bead's status back to "open". Returns ErrNotFound
	// if the ID does not exist.
	Reopen(id string) error

	// CloseAll closes multiple beads in a single batch operation and sets
	// the given metadata on each. Already-closed beads are skipped.
	// Returns the number of beads actually closed.
	CloseAll(ids []string, metadata map[string]string) (int, error)

	// List returns beads matching the query. Queries must include at least
	// one filter unless AllowScan is set explicitly.
	List(query ListQuery) ([]Bead, error)

	// Legacy helper; prefer List with ListQuery in new code.
	// ListOpen returns non-closed beads by default. With a status argument
	// (e.g., "in_progress" or "closed"), returns only beads matching that
	// status. In-process stores return creation order; external stores may not
	// guarantee order.
	ListOpen(status ...string) ([]Bead, error)

	// Ready returns open, unblocked beads representing actionable work.
	// Infrastructure types (molecule, message, gate, etc.) are excluded
	// to match the bd CLI's GetReadyWork semantics. Same ordering note
	// as List. Pass ReadyQuery to constrain the ready lookup.
	Ready(query ...ReadyQuery) ([]Bead, error)

	// Legacy helper; prefer List with ListQuery in new code.
	// Children returns all beads whose ParentID matches the given ID,
	// in creation order. Pass IncludeClosed to include closed children.
	Children(parentID string, opts ...QueryOpt) ([]Bead, error)

	// Legacy helper; prefer List with ListQuery in new code.
	// ListByLabel returns beads matching an exact label string.
	// Limit controls max results (0 = unlimited). Results are ordered
	// newest first where supported; in-process stores return creation order.
	// Pass IncludeClosed to include closed beads.
	ListByLabel(label string, limit int, opts ...QueryOpt) ([]Bead, error)

	// Legacy helper; prefer List with ListQuery in new code.
	// ListByAssignee returns beads assigned to the given agent with the
	// specified status. Limit controls max results (0 = unlimited).
	ListByAssignee(assignee, status string, limit int) ([]Bead, error)

	// Legacy helper; prefer List with ListQuery in new code.
	// ListByMetadata returns beads whose metadata contains all key-value pairs
	// in filters. Limit controls max results (0 = unlimited). Pass
	// IncludeClosed to include closed beads.
	ListByMetadata(filters map[string]string, limit int, opts ...QueryOpt) ([]Bead, error)

	// SetMetadata sets a key-value metadata pair on a bead. Returns
	// ErrNotFound if the bead does not exist.
	SetMetadata(id, key, value string) error

	// SetMetadataBatch sets multiple key-value metadata pairs on a bead.
	// In-memory stores (MemStore, FileStore) apply all writes atomically.
	// External stores (BdStore, exec) apply writes sequentially; partial
	// application is possible on mid-batch failure. Callers should design
	// batch contents to be idempotent and tolerate partial writes.
	// Returns ErrNotFound if the bead does not exist.
	SetMetadataBatch(id string, kvs map[string]string) error

	// Tx executes fn inside a single logical transaction identified by
	// commitMsg. Implementations without native transaction support may execute
	// writes sequentially or stage them until fn returns; outside observers
	// should not depend on seeing partial writes before Tx returns. fn must not
	// retain the Tx after it returns.
	Tx(commitMsg string, fn func(tx Tx) error) error

	// Delete permanently removes a bead from the store. The bead should be
	// closed first. Returns ErrNotFound if the bead does not exist.
	Delete(id string) error

	// Ping verifies that the store is operational. Returns nil on success,
	// or an error describing why the store is unavailable.
	Ping() error

	// DepAdd records a dependency: issueID depends on (is blocked by)
	// dependsOnID. The depType describes the relationship ("blocks",
	// "tracks", "relates-to", etc.).
	DepAdd(issueID, dependsOnID, depType string) error

	// DepRemove removes a dependency between two beads.
	DepRemove(issueID, dependsOnID string) error

	// DepList returns dependencies for a bead. Direction controls the
	// query: "down" returns what this bead depends on (default),
	// "up" returns what depends on this bead.
	DepList(id, direction string) ([]Dep, error)
}

// StorageClass selects the physical bead storage tier for adapters that
// support table-specific creates. It is adapter plumbing, not a domain-level
// behavior knob; normal callers should use Store.Create and let the policy
// wrapper classify semantic beads from config.
type StorageClass string

const (
	// StorageDefault lets the concrete store use its normal create behavior.
	StorageDefault StorageClass = ""
	// StorageHistory stores a bead in the normal history-tracked issues table.
	StorageHistory StorageClass = "history"
	// StorageNoHistory stores a bead in durable no-history storage.
	StorageNoHistory StorageClass = "no_history"
	// StorageEphemeral stores a bead in ephemeral wisp storage.
	StorageEphemeral StorageClass = "ephemeral"
)

// StorageCreateStore is an optional adapter capability for create calls whose
// physical storage tier has already been selected by policy middleware.
type StorageCreateStore interface {
	CreateWithStorage(b Bead, storage StorageClass) (Bead, error)
}

// StorageGraphApplyStore is an optional adapter capability for graph creates
// whose physical storage tier has already been selected by policy middleware.
type StorageGraphApplyStore interface {
	ApplyGraphPlanWithStorage(ctx context.Context, plan *GraphApplyPlan, storage StorageClass) (*GraphApplyResult, error)
}

// ParentProjectionWaiter is an optional capability for stores whose
// parent-child listing path may lag a successful parent update. Callers that
// need strict read-after-write semantics for parent projections can type-assert
// this interface after a successful Update.
type ParentProjectionWaiter interface {
	// WaitForParentProjection blocks until the store's parent-child listing
	// view reflects a reparent from oldParentID to newParentID for id, or
	// returns an error if the projection does not converge.
	WaitForParentProjection(ctx context.Context, id, oldParentID, newParentID string) error
}
