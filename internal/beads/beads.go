// Package beads provides the bead store abstraction — the universal persistence
// substrate for Gas City work units (tasks, messages, molecules, etc.).
package beads

import (
	"errors"
	"time"
)

// ErrNotFound is returned when a bead ID does not exist in the store.
var ErrNotFound = errors.New("bead not found")

// Bead is a single unit of work in Gas City. Everything is a bead: tasks,
// mail, molecules, convoys.
type Bead struct {
	ID           string            `json:"id"`
	Title        string            `json:"title"`
	Status       string            `json:"status"`     // "open", "in_progress", "closed"
	Type         string            `json:"issue_type"` // "task" default; matches bd wire format
	Priority     *int              `json:"priority,omitempty"`
	CreatedAt    time.Time         `json:"created_at"`
	Assignee     string            `json:"assignee,omitempty"`
	From         string            `json:"from,omitempty"`
	ParentID     string            `json:"parent,omitempty"`      // step → molecule; matches bd wire format
	Ref          string            `json:"ref,omitempty"`         // formula step ID or formula name
	Needs        []string          `json:"needs,omitempty"`       // dependency step refs
	Description  string            `json:"description,omitempty"` // step instructions
	Labels       []string          `json:"labels,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
	Dependencies []Dep             `json:"dependencies,omitempty"`
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

func cloneIntPtr(v *int) *int {
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
	"message":       true, // mail/communication items
	"session":       true, // runtime/session continuity beads, never actionable work
	"agent":         true, // identity/state tracking beads
	"role":          true, // agent role definitions
	"rig":           true, // rig identity beads
}

// IsReadyExcludedType reports whether the bead type is excluded from
// Ready() results by default.
func IsReadyExcludedType(t string) bool {
	return readyExcludeTypes[t]
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
	// as List.
	Ready() ([]Bead, error)

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
