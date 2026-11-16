package beads

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Guarded batches: atomic multi-row writes as a declarative capability.
//
// A GuardedBatcher applies a list of pinned-id creates and revision-guarded
// updates as one all-or-nothing write. It exists beside Store.Tx rather than as
// guards on it because Tx is a callback: on a served backend the callback is
// recorded, not run, so it can neither hand back a minted id nor carry a guard
// per write. A declarative item list maps onto the native batch applier's
// UpdateItem.ExpectedVersion directly, and every in-tree store with a
// transaction (or a single lock) can apply one atomically.
//
// Stores that cannot apply one atomically do not implement it: BdStore and the
// exec provider (no CLI sits behind the native batch applier), and the
// read-only DoltLite store. GuardedBatcherFor reports false for them, so a
// caller that requires atomic batches can refuse at startup instead of at the
// first write.

// MaxGuardedItems caps one guarded batch, matching the native batch applier's
// per-request bound. A caller that adds its own bookkeeping row to a batch of
// content items carries at most MaxGuardedItems-1 of them.
const MaxGuardedItems = 100

// GuardedBatcher applies a list of guarded writes as one all-or-nothing
// transaction: every item lands, or none does.
type GuardedBatcher interface {
	// ApplyGuarded validates b, then applies every item or none. A refusal
	// raised by an item is a *BatchItemError naming it; a batch refused by
	// validation matches ErrInvalidGuardedBatch and writes nothing. When
	// several items would refuse, which one is named depends on the backend.
	//
	// Those refusals, and ErrGuardedBatchUnsupported, wrote nothing. Any other
	// error (a dropped connection, a failed save, a context canceled mid-write)
	// may or may not have committed: re-read the rows before retrying, since a
	// committed batch replayed refuses its own pinned creates with
	// ErrAlreadyExists.
	ApplyGuarded(ctx context.Context, b GuardedBatch) (GuardedResult, error)
}

// GuardedBatcherHandleProvider lets a store or wrapper expose a guarded batcher
// only when it can actually apply one: a cache whose backing has none, or a
// SQLite layout without the revision column, answers false rather than handing
// out a batcher that always refuses. It is also how a wrapper opts in to
// forwarding guarded batches at all; see GuardedBatcherFor.
type GuardedBatcherHandleProvider interface {
	GuardedBatcherHandle() (GuardedBatcher, bool)
}

// GuardedBatch is one atomic request.
type GuardedBatch struct {
	// Reason labels the write's history entry where the store records one.
	// Empty uses the store default.
	Reason string
	// Items are applied in order. A batch holds 1 to MaxGuardedItems items,
	// and a row may appear in at most one of them.
	Items []GuardedItem
}

// GuardedItem is exactly one of Create or Update.
type GuardedItem struct {
	// Create inserts a new row under its pinned ID. The ID is required and the
	// create is create-only: an ID that already names a row refuses the whole
	// batch with ErrAlreadyExists. A guarded create carries no edges (ParentID,
	// Needs, Dependencies) and is written as given: a wrapper's create policy
	// (the storage tier a policy wrapper picks for Create, say) does not apply,
	// so the caller sets NoHistory and Ephemeral itself. CloseReason follows
	// the Bead contract rather than the request: a row created closed records
	// the trimmed metadata.close_reason, any other row none.
	Create *Bead
	// Update patches one existing row.
	Update *GuardedUpdate
}

// GuardedUpdate patches one existing row. Opts follows UpdateIfMatch's rules,
// except that labels are always accepted, since every guarded batcher applies
// them inside the batch: ParentID is refused and an empty Opts is
// ErrEmptyConditionalUpdate. Status "closed" closes the row.
type GuardedUpdate struct {
	ID   string
	Opts UpdateOpts
	// ExpectedRevision guards the update: the batch is refused unless the row's
	// revision equals it. Nil applies the update unguarded, with no ordering
	// against a concurrent writer. On the native store an unguarded label edit
	// derives its label revision from a read outside the batch's transaction,
	// so two concurrent unguarded label edits can stamp the same revision;
	// guard a label edit whose revision the caller relies on.
	ExpectedRevision *int64
}

// GuardedResult carries each touched row's revision after the batch.
type GuardedResult struct {
	// Revisions maps every row the batch created or updated to its revision
	// after the batch, equal to what a later Get reports. Revisions follow the
	// ConditionalWriter revision contract: compare them, never order them.
	// A newly created row's revision may be 0 (SQLite reports 0); it is still
	// a valid equality guard for the next batch.
	Revisions map[string]int64
}

// ErrAlreadyExists reports a pinned create whose id already names a row.
var ErrAlreadyExists = errors.New("beads: id already exists")

// ErrGuardedBatchUnsupported reports a store with no guarded batches.
var ErrGuardedBatchUnsupported = errors.New("beads: store cannot apply guarded batches")

// ErrInvalidGuardedBatch reports a batch refused by validation, before any
// write: an empty or oversized batch, an item with not exactly one member, a
// create without an id, with edges or both Ephemeral and NoHistory, an update
// without an id, or a row named twice. A namespace-fenced store refuses a
// create pinned outside its namespaces the same way, and that refusal also
// matches ErrPinnedIDOutsideNamespace. Update options the guarded write cannot
// carry also match the conditional-update error they raise
// (ConditionalUpdateFieldUnsupportedError, ErrEmptyConditionalUpdate).
var ErrInvalidGuardedBatch = errors.New("beads: invalid guarded batch")

// BatchItemError names the item that refused the whole batch. It wraps
// ErrAlreadyExists, ErrNotFound or a *PreconditionFailedError, so
// IsPreconditionFailed and errors.Is keep working through it.
type BatchItemError struct {
	Index int
	ID    string
	Err   error
}

// Error reports the refusing item and its cause.
func (e *BatchItemError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("guarded batch item %d (%s): %v", e.Index, e.ID, e.Err)
}

// Unwrap exposes the item's refusal to errors.Is and errors.As.
func (e *BatchItemError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// GuardedBatcherFor returns the guarded batcher store applies batches through.
// A store that provides a handle answers through it; otherwise the store
// itself must be a GuardedBatcher.
//
// Discovery is opt-in per wrapper and deliberately does NOT follow
// ConditionalWritesResolveTarget. That target promises fenced capabilities
// only (UpdateIfMatch, CAS, the atomic closer), and a wrapper may declare it
// precisely because it refuses everything unfenced. A guarded batch can carry
// unguarded updates and pinned creates, so following the target would hand a
// caller blind writes through a wrapper that refuses them. A wrapper that
// forwards guarded batches says so by implementing GuardedBatcherHandle,
// usually by returning GuardedBatcherFor of the store it wraps; any other
// wrapper hides its inner batcher.
func GuardedBatcherFor(store Store) (GuardedBatcher, bool) {
	if store == nil {
		return nil, false
	}
	if provider, ok := store.(GuardedBatcherHandleProvider); ok {
		return provider.GuardedBatcherHandle()
	}
	batcher, ok := store.(GuardedBatcher)
	return batcher, ok
}

// id names the row the item writes.
func (it GuardedItem) id() string {
	switch {
	case it.Create != nil:
		return it.Create.ID
	case it.Update != nil:
		return it.Update.ID
	default:
		return ""
	}
}

// expectedRevision is the item's guard, or nil for a create or an unguarded
// update.
func (it GuardedItem) expectedRevision() *int64 {
	if it.Update == nil {
		return nil
	}
	return it.Update.ExpectedRevision
}

// closes reports an update that moves the row to closed.
func (it GuardedItem) closes() bool {
	return it.Update != nil && it.Update.Opts.Status != nil && *it.Update.Opts.Status == "closed"
}

// validateGuardedBatch checks the whole batch before any store reads or writes
// anything, so a refused batch writes nothing on every backend.
func validateGuardedBatch(b GuardedBatch) error {
	if len(b.Items) == 0 {
		return fmt.Errorf("%w: no items", ErrInvalidGuardedBatch)
	}
	if len(b.Items) > MaxGuardedItems {
		return fmt.Errorf("%w: %d items, at most %d", ErrInvalidGuardedBatch, len(b.Items), MaxGuardedItems)
	}
	seen := make(map[string]int, len(b.Items))
	for i, it := range b.Items {
		if err := validateGuardedItem(it); err != nil {
			return fmt.Errorf("guarded batch item %d: %w", i, err)
		}
		id := it.id()
		if first, dup := seen[id]; dup {
			return fmt.Errorf("%w: items %d and %d both write %q", ErrInvalidGuardedBatch, first, i, id)
		}
		seen[id] = i
	}
	return nil
}

// checkGuardedCreateNamespaces applies the pinned-id fence to every create in
// b, before the store reads or writes anything. A refusal is a validation
// refusal: it matches ErrInvalidGuardedBatch, so the caller knows the batch
// wrote nothing, and ErrPinnedIDOutsideNamespace, so it can route the id to a
// sibling binding.
func checkGuardedCreateNamespaces(op string, b GuardedBatch, namespaces []string) error {
	for i, it := range b.Items {
		if it.Create == nil {
			continue
		}
		if err := checkPinnedIDNamespace(op, it.Create.ID, namespaces); err != nil {
			return fmt.Errorf("guarded batch item %d: %w: %w", i, ErrInvalidGuardedBatch, err)
		}
	}
	return nil
}

func validateGuardedItem(it GuardedItem) error {
	switch {
	case it.Create == nil && it.Update == nil:
		return fmt.Errorf("%w: item sets neither Create nor Update", ErrInvalidGuardedBatch)
	case it.Create != nil && it.Update != nil:
		return fmt.Errorf("%w: item sets both Create and Update", ErrInvalidGuardedBatch)
	case it.Create != nil:
		return validateGuardedCreate(*it.Create)
	default:
		return validateGuardedUpdate(*it.Update)
	}
}

func validateGuardedCreate(b Bead) error {
	switch {
	case b.ID == "" || strings.TrimSpace(b.ID) != b.ID:
		return fmt.Errorf("%w: create needs a pinned id without surrounding space, got %q", ErrInvalidGuardedBatch, b.ID)
	case b.ParentID != "" || len(b.Needs) > 0 || len(b.Dependencies) > 0:
		return fmt.Errorf("%w: create %q carries edges; a guarded create writes one row", ErrInvalidGuardedBatch, b.ID)
	case b.Ephemeral && b.NoHistory:
		return fmt.Errorf("%w: create %q is both ephemeral and no-history; a row has one storage tier", ErrInvalidGuardedBatch, b.ID)
	default:
		return nil
	}
}

func validateGuardedUpdate(u GuardedUpdate) error {
	if u.ID == "" || strings.TrimSpace(u.ID) != u.ID {
		return fmt.Errorf("%w: update needs an id without surrounding space, got %q", ErrInvalidGuardedBatch, u.ID)
	}
	if err := validateConditionalUpdateOpts(u.Opts, true); err != nil {
		return fmt.Errorf("%w: update %q: %w", ErrInvalidGuardedBatch, u.ID, err)
	}
	return nil
}

// guardedBatchTouched lists the batch's rows in item order and the subset its
// updates close, for a cache refreshing after the batch.
func guardedBatchTouched(b GuardedBatch) ([]string, map[string]struct{}) {
	ids := make([]string, 0, len(b.Items))
	closed := make(map[string]struct{})
	for _, it := range b.Items {
		id := it.id()
		ids = append(ids, id)
		if it.closes() {
			closed[id] = struct{}{}
		}
	}
	return ids, closed
}

// guardedCreateCloseReason applies GuardedItem.Create's close-reason rule to
// the row a backend is about to insert: one created closed records the trimmed
// metadata.close_reason, as a close does (recordCloseReason), and any other
// row records none.
func guardedCreateCloseReason(b *Bead) {
	if b.Status == "closed" {
		recordCloseReason(b)
		return
	}
	b.CloseReason = ""
}

// guardedPreconditionError is the item refusal for a guard that missed.
func guardedPreconditionError(index int, id string, expected, current int64, raw string) *BatchItemError {
	return &BatchItemError{Index: index, ID: id, Err: &PreconditionFailedError{
		ID: id, Expected: expected, Current: current, Raw: raw,
	}}
}

// guardedNotFoundError is the item refusal for an update of a missing row.
func guardedNotFoundError(index int, id string, cause error) *BatchItemError {
	err := fmt.Errorf("bead %q: %w", id, ErrNotFound)
	if cause != nil && !errors.Is(cause, ErrNotFound) {
		err = fmt.Errorf("bead %q: %w: %w", id, ErrNotFound, cause)
	}
	return &BatchItemError{Index: index, ID: id, Err: err}
}

// guardedExistsError is the item refusal for a pinned create over a row.
func guardedExistsError(index int, id string, cause error) *BatchItemError {
	err := fmt.Errorf("bead %q: %w", id, ErrAlreadyExists)
	if cause != nil {
		err = fmt.Errorf("bead %q: %w: %w", id, ErrAlreadyExists, cause)
	}
	return &BatchItemError{Index: index, ID: id, Err: err}
}
