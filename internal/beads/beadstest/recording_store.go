package beadstest

import (
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
)

// RecordedCall is a single mutating Store call captured by RecordingStore.
// The fields that are not relevant to a given Op are left zero. RecordingStore
// records the arguments verbatim (deep-copying maps and slices so later
// mutation of the caller's argument cannot rewrite history), which lets a
// front-door test assert that a typed domain method emits byte-identical bead
// writes to the raw bead op it replaces.
type RecordedCall struct {
	// Op is the Store method that was invoked: "Create", "Update", "Close",
	// "Reopen", "CloseAll", "SetMetadata", "SetMetadataBatch", "Delete",
	// "DepAdd", or "DepRemove".
	Op string

	// ID is the target bead id for ops that address a single bead. For Create
	// it is the id the delegate assigned to the new bead.
	ID string

	// Bead is the argument to Create (verbatim, as passed by the caller).
	Bead beads.Bead

	// Opts is the argument to Update.
	Opts beads.UpdateOpts

	// Key/Value are the arguments to SetMetadata.
	Key   string
	Value string

	// Metadata is the argument to SetMetadataBatch, or the per-bead metadata
	// argument to CloseAll. It is a deep copy.
	Metadata map[string]string

	// IDs is the argument to CloseAll.
	IDs []string

	// DependsOnID / DepType carry the extra arguments to DepAdd / DepRemove.
	DependsOnID string
	DepType     string
}

// RecordingStore is a beads.Store that records every mutating call before
// delegating to an underlying store, so front-door tests can assert that a
// typed domain method produces the exact same bead writes (same keys, same
// empty-string-clear values, same labels, same NoHistory/Ephemeral flags) as
// the raw bead op it replaces.
//
// Reads are passed straight through to the delegate. Only mutating ops are
// recorded. RecordingStore is safe for concurrent use; the recorded log is
// guarded by a mutex.
//
// Construct one with NewRecordingStore. The zero value is not usable.
type RecordingStore struct {
	beads.Store

	mu    sync.Mutex
	calls []RecordedCall
}

// NewRecordingStore wraps delegate so every mutating call is recorded. If
// delegate is nil a fresh in-memory store is used.
func NewRecordingStore(delegate beads.Store) *RecordingStore {
	if delegate == nil {
		delegate = beads.NewMemStore()
	}
	return &RecordingStore{Store: delegate}
}

func (r *RecordingStore) record(c RecordedCall) {
	r.mu.Lock()
	r.calls = append(r.calls, c)
	r.mu.Unlock()
}

// Calls returns a copy of the recorded call log in invocation order.
func (r *RecordingStore) Calls() []RecordedCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]RecordedCall, len(r.calls))
	copy(out, r.calls)
	return out
}

// CallsForOp returns the recorded calls whose Op matches op, in order.
func (r *RecordingStore) CallsForOp(op string) []RecordedCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []RecordedCall
	for _, c := range r.calls {
		if c.Op == op {
			out = append(out, c)
		}
	}
	return out
}

// Reset clears the recorded call log without touching the delegate.
func (r *RecordingStore) Reset() {
	r.mu.Lock()
	r.calls = nil
	r.mu.Unlock()
}

func cloneMeta(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func cloneStrings(s []string) []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s))
	copy(out, s)
	return out
}

func cloneBead(b beads.Bead) beads.Bead {
	b.Labels = cloneStrings(b.Labels)
	b.Needs = cloneStrings(b.Needs)
	b.Metadata = cloneMeta(b.Metadata)
	if len(b.Dependencies) > 0 {
		deps := make([]beads.Dep, len(b.Dependencies))
		copy(deps, b.Dependencies)
		b.Dependencies = deps
	}
	return b
}

func cloneOpts(o beads.UpdateOpts) beads.UpdateOpts {
	o.Labels = cloneStrings(o.Labels)
	o.RemoveLabels = cloneStrings(o.RemoveLabels)
	o.Metadata = cloneMeta(o.Metadata)
	return o
}

// Create records the create then delegates, capturing the assigned id.
func (r *RecordingStore) Create(b beads.Bead) (beads.Bead, error) {
	created, err := r.Store.Create(b)
	r.record(RecordedCall{Op: "Create", ID: created.ID, Bead: cloneBead(b)})
	return created, err
}

// Update records the update then delegates.
func (r *RecordingStore) Update(id string, opts beads.UpdateOpts) error {
	r.record(RecordedCall{Op: "Update", ID: id, Opts: cloneOpts(opts)})
	return r.Store.Update(id, opts)
}

// Close records the close then delegates.
func (r *RecordingStore) Close(id string) error {
	r.record(RecordedCall{Op: "Close", ID: id})
	return r.Store.Close(id)
}

// Reopen records the reopen then delegates.
func (r *RecordingStore) Reopen(id string) error {
	r.record(RecordedCall{Op: "Reopen", ID: id})
	return r.Store.Reopen(id)
}

// CloseAll records the batch close then delegates.
func (r *RecordingStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	r.record(RecordedCall{Op: "CloseAll", IDs: cloneStrings(ids), Metadata: cloneMeta(metadata)})
	return r.Store.CloseAll(ids, metadata)
}

// SetMetadata records the single-key write then delegates.
func (r *RecordingStore) SetMetadata(id, key, value string) error {
	r.record(RecordedCall{Op: "SetMetadata", ID: id, Key: key, Value: value})
	return r.Store.SetMetadata(id, key, value)
}

// SetMetadataBatch records the batch write then delegates.
func (r *RecordingStore) SetMetadataBatch(id string, kvs map[string]string) error {
	r.record(RecordedCall{Op: "SetMetadataBatch", ID: id, Metadata: cloneMeta(kvs)})
	return r.Store.SetMetadataBatch(id, kvs)
}

// Delete records the delete then delegates.
func (r *RecordingStore) Delete(id string) error {
	r.record(RecordedCall{Op: "Delete", ID: id})
	return r.Store.Delete(id)
}

// DepAdd records the dependency add then delegates.
func (r *RecordingStore) DepAdd(issueID, dependsOnID, depType string) error {
	r.record(RecordedCall{Op: "DepAdd", ID: issueID, DependsOnID: dependsOnID, DepType: depType})
	return r.Store.DepAdd(issueID, dependsOnID, depType)
}

// DepRemove records the dependency remove then delegates.
func (r *RecordingStore) DepRemove(issueID, dependsOnID string) error {
	r.record(RecordedCall{Op: "DepRemove", ID: issueID, DependsOnID: dependsOnID})
	return r.Store.DepRemove(issueID, dependsOnID)
}
