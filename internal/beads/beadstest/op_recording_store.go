package beadstest

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
)

// Op kinds recorded by OpRecordingStore. Every Store method and forwarded
// capability records exactly one op under one of these kinds.
const (
	OpGet                      = "Get"
	OpList                     = "List"
	OpReady                    = "Ready"
	OpCreate                   = "Create"
	OpUpdate                   = "Update"
	OpClose                    = "Close"
	OpReopen                   = "Reopen"
	OpCloseAll                 = "CloseAll"
	OpSetMetadata              = "SetMetadata"
	OpSetMetadataBatch         = "SetMetadataBatch"
	OpSetLocalString           = "SetLocalString"
	OpGetLocalString           = "GetLocalString"
	OpTx                       = "Tx"
	OpDelete                   = "Delete"
	OpPing                     = "Ping"
	OpDepAdd                   = "DepAdd"
	OpDepRemove                = "DepRemove"
	OpDepList                  = "DepList"
	OpDepListBatch             = "DepListBatch"
	OpDepMetadata              = "DepMetadata"
	OpUpdateIfMatch            = "UpdateIfMatch"
	OpCloseIfMatch             = "CloseIfMatch"
	OpCloseWithMetadataIfMatch = "CloseWithMetadataIfMatch"
	OpDeleteIfMatch            = "DeleteIfMatch"
	OpCompareAndSetMetadataKey = "CompareAndSetMetadataKey"
	OpReleaseIfCurrent         = "ReleaseIfCurrent"
)

// RecordedOp is one Store operation captured by OpRecordingStore.
type RecordedOp struct {
	// Kind is one of the Op* constants.
	Kind string
	// ID is the bead the op addressed, when it addressed one. For Create it is
	// the id the delegate assigned.
	ID string
	// Query is the ListQuery a List-kind op asked the store. The legacy list
	// helpers (ListOpen, Children, ListByLabel, ListByAssignee, ListByMetadata)
	// record the query a List-composed store answers them with.
	Query beads.ListQuery
	// Method names the Store method that recorded a List-kind op.
	Method string
	// Err is the error the delegate returned, if any.
	Err error
}

// NotFound reports whether the op failed because its bead does not exist.
func (o RecordedOp) NotFound() bool {
	return o.Err != nil && errors.Is(o.Err, beads.ErrNotFound)
}

// OpRecordingStore is a beads.Store that records every operation, reads
// included, before returning the delegate's answer. Request-budget tests use it
// to count what one command asks of its ledger.
//
// It forwards the optional capabilities both the file and native stores
// implement (the conditional-write family, ReleaseIfCurrent, DepListBatch,
// ReadyContext and DepMetadata), and reports the delegate's ListRequestStatsOf
// counts through ListRequestStats. A forwarded capability the delegate lacks answers an
// error wrapping errors.ErrUnsupported. Capabilities only the native store has
// (Count, Claim, IDPrefix) are deliberately not forwarded, so a file-backed
// fixture keeps the code paths it takes without the recorder.
//
// It is safe for concurrent use. Construct one with NewOpRecordingStore.
type OpRecordingStore struct {
	delegate beads.Store

	mu  sync.Mutex
	ops []RecordedOp
}

// NewOpRecordingStore wraps delegate so every operation is recorded. A nil
// delegate is replaced by a fresh in-memory store.
func NewOpRecordingStore(delegate beads.Store) *OpRecordingStore {
	if delegate == nil {
		delegate = beads.NewMemStore()
	}
	return &OpRecordingStore{delegate: delegate}
}

// Delegate returns the wrapped store.
func (r *OpRecordingStore) Delegate() beads.Store { return r.delegate }

// Ops returns a copy of the recorded operations in invocation order.
func (r *OpRecordingStore) Ops() []RecordedOp {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]RecordedOp(nil), r.ops...)
}

// Reset clears the recorded operations without touching the delegate.
func (r *OpRecordingStore) Reset() {
	r.mu.Lock()
	r.ops = nil
	r.mu.Unlock()
}

func (r *OpRecordingStore) record(op RecordedOp) {
	if op.Kind == OpList && op.Query.Metadata != nil {
		op.Query.Metadata = cloneMeta(op.Query.Metadata)
	}
	if op.Kind == OpList {
		op.Query.Assignees = cloneStrings(op.Query.Assignees)
		op.Query.IDs = cloneStrings(op.Query.IDs)
		op.Query.ParentIDs = cloneStrings(op.Query.ParentIDs)
	}
	r.mu.Lock()
	r.ops = append(r.ops, op)
	r.mu.Unlock()
}

func (r *OpRecordingStore) recordList(method string, query beads.ListQuery, err error) {
	r.record(RecordedOp{Kind: OpList, Method: method, Query: query, Err: err})
}

// Create implements beads.Store.
func (r *OpRecordingStore) Create(b beads.Bead) (beads.Bead, error) {
	created, err := r.delegate.Create(b)
	r.record(RecordedOp{Kind: OpCreate, ID: created.ID, Err: err})
	return created, err
}

// Get implements beads.Store.
func (r *OpRecordingStore) Get(id string) (beads.Bead, error) {
	b, err := r.delegate.Get(id)
	r.record(RecordedOp{Kind: OpGet, ID: id, Err: err})
	return b, err
}

// Update implements beads.Store.
func (r *OpRecordingStore) Update(id string, opts beads.UpdateOpts) error {
	err := r.delegate.Update(id, opts)
	r.record(RecordedOp{Kind: OpUpdate, ID: id, Err: err})
	return err
}

// Close implements beads.Store.
func (r *OpRecordingStore) Close(id string) error {
	err := r.delegate.Close(id)
	r.record(RecordedOp{Kind: OpClose, ID: id, Err: err})
	return err
}

// Reopen implements beads.Store.
func (r *OpRecordingStore) Reopen(id string) error {
	err := r.delegate.Reopen(id)
	r.record(RecordedOp{Kind: OpReopen, ID: id, Err: err})
	return err
}

// CloseAll implements beads.Store.
func (r *OpRecordingStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	n, err := r.delegate.CloseAll(ids, metadata)
	r.record(RecordedOp{Kind: OpCloseAll, Err: err})
	return n, err
}

// List implements beads.Store.
func (r *OpRecordingStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	out, err := r.delegate.List(query)
	r.recordList("List", query, err)
	return out, err
}

// ListOpen implements beads.Store.
func (r *OpRecordingStore) ListOpen(status ...string) ([]beads.Bead, error) {
	out, err := r.delegate.ListOpen(status...)
	r.recordList("ListOpen", beads.ListOpenQuery(status...), err)
	return out, err
}

// Ready implements beads.Store.
func (r *OpRecordingStore) Ready(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	out, err := r.delegate.Ready(query...)
	r.record(RecordedOp{Kind: OpReady, Err: err})
	return out, err
}

// ReadyContext forwards the context-aware ready read, recorded as a Ready. A
// delegate without it answers beads.ErrReadyContextUnsupported, the veto the
// capability's callers already handle.
func (r *OpRecordingStore) ReadyContext(ctx context.Context, query ...beads.ReadyQuery) ([]beads.Bead, error) {
	var (
		out []beads.Bead
		err error
	)
	if reader, ok := r.delegate.(beads.ContextReadyReader); ok {
		out, err = reader.ReadyContext(ctx, query...)
	} else {
		err = fmt.Errorf("%T: %w", r.delegate, beads.ErrReadyContextUnsupported)
	}
	r.record(RecordedOp{Kind: OpReady, Method: "ReadyContext", Err: err})
	return out, err
}

// Children implements beads.Store.
func (r *OpRecordingStore) Children(parentID string, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	out, err := r.delegate.Children(parentID, opts...)
	r.recordList("Children", beads.ChildrenQuery(parentID, opts...), err)
	return out, err
}

// ListByLabel implements beads.Store.
func (r *OpRecordingStore) ListByLabel(label string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	out, err := r.delegate.ListByLabel(label, limit, opts...)
	r.recordList("ListByLabel", beads.ListByLabelQuery(label, limit, opts...), err)
	return out, err
}

// ListByAssignee implements beads.Store.
func (r *OpRecordingStore) ListByAssignee(assignee, status string, limit int) ([]beads.Bead, error) {
	out, err := r.delegate.ListByAssignee(assignee, status, limit)
	r.recordList("ListByAssignee", beads.ListByAssigneeQuery(assignee, status, limit), err)
	return out, err
}

// ListByMetadata implements beads.Store.
func (r *OpRecordingStore) ListByMetadata(filters map[string]string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	out, err := r.delegate.ListByMetadata(filters, limit, opts...)
	r.recordList("ListByMetadata", beads.ListByMetadataQuery(filters, limit, opts...), err)
	return out, err
}

// SetMetadata implements beads.Store.
func (r *OpRecordingStore) SetMetadata(id, key, value string) error {
	err := r.delegate.SetMetadata(id, key, value)
	r.record(RecordedOp{Kind: OpSetMetadata, ID: id, Err: err})
	return err
}

// SetMetadataBatch implements beads.Store.
func (r *OpRecordingStore) SetMetadataBatch(id string, kvs map[string]string) error {
	err := r.delegate.SetMetadataBatch(id, kvs)
	r.record(RecordedOp{Kind: OpSetMetadataBatch, ID: id, Err: err})
	return err
}

// SetLocalString implements beads.Store.
func (r *OpRecordingStore) SetLocalString(id, key, value string) error {
	err := r.delegate.SetLocalString(id, key, value)
	r.record(RecordedOp{Kind: OpSetLocalString, ID: id, Err: err})
	return err
}

// GetLocalString implements beads.Store.
func (r *OpRecordingStore) GetLocalString(id, key string) (string, error) {
	v, err := r.delegate.GetLocalString(id, key)
	r.record(RecordedOp{Kind: OpGetLocalString, ID: id, Err: err})
	return v, err
}

// Tx implements beads.Store. The transaction body runs against the delegate's
// transaction handle, so its inner writes are recorded as the one Tx op.
func (r *OpRecordingStore) Tx(commitMsg string, fn func(tx beads.Tx) error) error {
	err := r.delegate.Tx(commitMsg, fn)
	r.record(RecordedOp{Kind: OpTx, Err: err})
	return err
}

// Delete implements beads.Store.
func (r *OpRecordingStore) Delete(id string) error {
	err := r.delegate.Delete(id)
	r.record(RecordedOp{Kind: OpDelete, ID: id, Err: err})
	return err
}

// Ping implements beads.Store.
func (r *OpRecordingStore) Ping() error {
	err := r.delegate.Ping()
	r.record(RecordedOp{Kind: OpPing, Err: err})
	return err
}

// DepAdd implements beads.Store.
func (r *OpRecordingStore) DepAdd(issueID, dependsOnID, depType string) error {
	err := r.delegate.DepAdd(issueID, dependsOnID, depType)
	r.record(RecordedOp{Kind: OpDepAdd, ID: issueID, Err: err})
	return err
}

// DepRemove implements beads.Store.
func (r *OpRecordingStore) DepRemove(issueID, dependsOnID string) error {
	err := r.delegate.DepRemove(issueID, dependsOnID)
	r.record(RecordedOp{Kind: OpDepRemove, ID: issueID, Err: err})
	return err
}

// DepList implements beads.Store.
func (r *OpRecordingStore) DepList(id, direction string) ([]beads.Dep, error) {
	out, err := r.delegate.DepList(id, direction)
	r.record(RecordedOp{Kind: OpDepList, ID: id, Err: err})
	return out, err
}

// DepMetadata forwards the edge-payload read.
func (r *OpRecordingStore) DepMetadata(issueID, dependsOnID string) (string, bool, error) {
	var (
		payload string
		carries bool
		err     error
	)
	if reader, ok := r.delegate.(beads.DepMetadataReader); ok {
		payload, carries, err = reader.DepMetadata(issueID, dependsOnID)
	} else {
		err = unsupported("DepMetadata", r.delegate)
	}
	r.record(RecordedOp{Kind: OpDepMetadata, ID: issueID, Err: err})
	return payload, carries, err
}

func unsupported(capability string, delegate beads.Store) error {
	return fmt.Errorf("%T does not implement %s: %w", delegate, capability, errors.ErrUnsupported)
}

// DepListBatch forwards the batched dependency read.
func (r *OpRecordingStore) DepListBatch(ids []string) (map[string][]beads.Dep, error) {
	batcher, ok := r.delegate.(interface {
		DepListBatch([]string) (map[string][]beads.Dep, error)
	})
	var (
		out map[string][]beads.Dep
		err error
	)
	if ok {
		out, err = batcher.DepListBatch(ids)
	} else {
		err = unsupported("DepListBatch", r.delegate)
	}
	r.record(RecordedOp{Kind: OpDepListBatch, Err: err})
	return out, err
}

// UpdateIfMatch forwards the conditional update.
func (r *OpRecordingStore) UpdateIfMatch(id string, expectedRevision int64, opts beads.UpdateOpts) error {
	err := unsupported("UpdateIfMatch", r.delegate)
	if w, ok := r.delegate.(interface {
		UpdateIfMatch(string, int64, beads.UpdateOpts) error
	}); ok {
		err = w.UpdateIfMatch(id, expectedRevision, opts)
	}
	r.record(RecordedOp{Kind: OpUpdateIfMatch, ID: id, Err: err})
	return err
}

// CloseIfMatch forwards the conditional close.
func (r *OpRecordingStore) CloseIfMatch(id string, expectedRevision int64) error {
	err := unsupported("CloseIfMatch", r.delegate)
	if w, ok := r.delegate.(interface {
		CloseIfMatch(string, int64) error
	}); ok {
		err = w.CloseIfMatch(id, expectedRevision)
	}
	r.record(RecordedOp{Kind: OpCloseIfMatch, ID: id, Err: err})
	return err
}

// CloseWithMetadataIfMatch forwards the conditional close with metadata.
func (r *OpRecordingStore) CloseWithMetadataIfMatch(id string, expectedRevision int64, metadata map[string]string) (beads.Bead, error) {
	var out beads.Bead
	err := unsupported("CloseWithMetadataIfMatch", r.delegate)
	if w, ok := r.delegate.(interface {
		CloseWithMetadataIfMatch(string, int64, map[string]string) (beads.Bead, error)
	}); ok {
		out, err = w.CloseWithMetadataIfMatch(id, expectedRevision, metadata)
	}
	r.record(RecordedOp{Kind: OpCloseWithMetadataIfMatch, ID: id, Err: err})
	return out, err
}

// DeleteIfMatch forwards the conditional delete.
func (r *OpRecordingStore) DeleteIfMatch(id string, expectedRevision int64) error {
	err := unsupported("DeleteIfMatch", r.delegate)
	if w, ok := r.delegate.(interface {
		DeleteIfMatch(string, int64) error
	}); ok {
		err = w.DeleteIfMatch(id, expectedRevision)
	}
	r.record(RecordedOp{Kind: OpDeleteIfMatch, ID: id, Err: err})
	return err
}

// CompareAndSetMetadataKey forwards the single-key metadata compare-and-set.
func (r *OpRecordingStore) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	var swapped bool
	err := unsupported("CompareAndSetMetadataKey", r.delegate)
	if w, ok := r.delegate.(interface {
		CompareAndSetMetadataKey(string, string, string, string) (bool, error)
	}); ok {
		swapped, err = w.CompareAndSetMetadataKey(id, key, expected, next)
	}
	r.record(RecordedOp{Kind: OpCompareAndSetMetadataKey, ID: id, Err: err})
	return swapped, err
}

// ReleaseIfCurrent forwards the assignee-fenced release.
func (r *OpRecordingStore) ReleaseIfCurrent(id, expectedAssignee string) (bool, error) {
	var released bool
	err := unsupported("ReleaseIfCurrent", r.delegate)
	if w, ok := r.delegate.(interface {
		ReleaseIfCurrent(string, string) (bool, error)
	}); ok {
		released, err = w.ReleaseIfCurrent(id, expectedAssignee)
	}
	r.record(RecordedOp{Kind: OpReleaseIfCurrent, ID: id, Err: err})
	return released, err
}

// ListRequestStats forwards the delegate's backend listing counters, or zero
// counts when the delegate does not keep them.
func (r *OpRecordingStore) ListRequestStats() beads.ListRequestStats {
	stats, _ := beads.ListRequestStatsOf(r.delegate)
	return stats
}

var (
	_ beads.Store              = (*OpRecordingStore)(nil)
	_ beads.ContextReadyReader = (*OpRecordingStore)(nil)
	_ beads.DepMetadataReader  = (*OpRecordingStore)(nil)
)
