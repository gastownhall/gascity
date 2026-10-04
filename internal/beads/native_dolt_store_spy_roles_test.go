package beads

import (
	"context"
	"encoding/json"
	"fmt"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

// The role accessors the in-package storage doubles answer with, layered over
// the raw hooks their existing fixtures already set.
//
// The store's graph, delete and reachability front doors moved onto the role
// accessors because the raw methods are off the v0 served surface. The doubles'
// fixtures describe BEHAVIOR — this delete fails, these dependents exist —
// rather than a wire shape, so forwarding the roles onto the same hooks keeps
// every one of those fixtures meaningful instead of restating a hundred facts
// as role doubles.

// rawGraphStorage is the slice of beadslib.Storage the adapters below need. Both
// in-package doubles implement it.
type rawGraphStorage interface {
	DeleteIssue(context.Context, string) error
	AddDependency(context.Context, *beadslib.Dependency, string) error
	RemoveDependency(context.Context, string, string, string) error
	GetDependenciesWithMetadata(context.Context, string) ([]*beadslib.IssueWithDependencyMetadata, error)
	GetDependentsWithMetadata(context.Context, string) ([]*beadslib.IssueWithDependencyMetadata, error)
}

type rawDeleter struct{ raw rawGraphStorage }

func (d rawDeleter) Delete(ctx context.Context, req issueops.DeleteRequest) (issueops.DeleteResult, error) {
	for _, id := range req.IDs {
		if err := d.raw.DeleteIssue(ctx, id); err != nil {
			return issueops.DeleteResult{}, err
		}
	}
	return issueops.DeleteResult{Deleted: len(req.IDs)}, nil
}

type rawDependencyEditor struct{ raw rawGraphStorage }

func (e rawDependencyEditor) AddDependencies(ctx context.Context, req issueops.AddDependenciesRequest) (issueops.AddDependenciesResult, error) {
	for _, edge := range req.Edges {
		dep := &beadslib.Dependency{IssueID: edge.IssueID, DependsOnID: edge.DependsOnID, Type: edge.Type}
		if err := e.raw.AddDependency(ctx, dep, req.Actor); err != nil {
			return issueops.AddDependenciesResult{}, err
		}
	}
	return issueops.AddDependenciesResult{Added: req.Edges}, nil
}

func (e rawDependencyEditor) RemoveDependency(ctx context.Context, req issueops.RemoveDependencyRequest) (issueops.RemoveDependencyResult, error) {
	if err := e.raw.RemoveDependency(ctx, req.IssueID, req.DependsOnID, req.Actor); err != nil {
		return issueops.RemoveDependencyResult{}, err
	}
	return issueops.RemoveDependencyResult{Removed: true}, nil
}

type rawEdgeReader struct{ raw rawGraphStorage }

// ReadEdges answers from the same downward fixture the raw read used. The
// doubles' hook hands back the NEIGHBORS rather than the edge rows, so each is
// turned back into the edge it came from — the projection the store performed
// inline before the split.
func (r rawEdgeReader) ReadEdges(ctx context.Context, req issueops.EdgeReadRequest) (issueops.EdgeReadResult, error) {
	result := issueops.EdgeReadResult{}
	for _, id := range req.IDs {
		issues, err := r.raw.GetDependenciesWithMetadata(ctx, id)
		if err != nil {
			return issueops.EdgeReadResult{}, err
		}
		anchor := issueops.AnchorEdges{ID: id}
		for _, issue := range issues {
			anchor.Edges = append(anchor.Edges, &beadslib.Dependency{
				IssueID:     id,
				DependsOnID: issue.ID,
				Type:        issue.DependencyType,
			})
		}
		result.Anchors = append(result.Anchors, anchor)
	}
	return result, nil
}

type rawRelations struct{ raw rawGraphStorage }

func (r rawRelations) Related(ctx context.Context, req issueops.RelatedRequest) ([]*issueops.RelatedIssue, error) {
	if req.Direction == issueops.RelationIn {
		return r.raw.GetDependentsWithMetadata(ctx, req.ID)
	}
	return r.raw.GetDependenciesWithMetadata(ctx, req.ID)
}

// reachableStatsReporter stands in for the reachability probe: the doubles hold
// no statistics fixture, so a reachable store answers and nothing is asserted.
type reachableStatsReporter struct{}

func (reachableStatsReporter) Stats(context.Context, issueops.StatsRequest) (issueops.StatsResult, error) {
	return issueops.StatsResult{}, nil
}

func (reachableStatsReporter) AssigneeStats(context.Context, issueops.AssigneeStatsRequest) (issueops.StatsResult, error) {
	return issueops.StatsResult{}, nil
}

func (s *nativeDoltStorageSpy) Deleter() (issueops.Deleter, error) { return rawDeleter{raw: s}, nil }
func (s *nativeDoltStorageSpy) DependencyEditor() (issueops.DependencyEditor, error) {
	return rawDependencyEditor{raw: s}, nil
}

func (s *nativeDoltStorageSpy) EdgeReader() (issueops.EdgeReader, error) {
	return rawEdgeReader{raw: s}, nil
}

func (s *nativeDoltStorageSpy) IssueRelations() (issueops.Relations, error) {
	return rawRelations{raw: s}, nil
}

func (s *nativeDoltStorageSpy) StatsReporter() (issueops.StatsReporter, error) {
	return reachableStatsReporter{}, nil
}

func (s *nativeDoltMemStorage) Deleter() (issueops.Deleter, error) { return rawDeleter{raw: s}, nil }
func (s *nativeDoltMemStorage) DependencyEditor() (issueops.DependencyEditor, error) {
	return rawDependencyEditor{raw: s}, nil
}

func (s *nativeDoltMemStorage) EdgeReader() (issueops.EdgeReader, error) {
	return rawEdgeReader{raw: s}, nil
}

func (s *nativeDoltMemStorage) IssueRelations() (issueops.Relations, error) {
	return rawRelations{raw: s}, nil
}

func (s *nativeDoltMemStorage) StatsReporter() (issueops.StatsReporter, error) {
	return reachableStatsReporter{}, nil
}

var (
	_ rawGraphStorage = (*nativeDoltStorageSpy)(nil)
	_ rawGraphStorage = (*nativeDoltMemStorage)(nil)
)

// rawMetadataCASStorage is the read-modify-write pair the CAS adapter needs.
type rawMetadataCASStorage interface {
	GetIssue(context.Context, string) (*beadslib.Issue, error)
	UpdateIssue(context.Context, string, map[string]interface{}, string) error
}

// rawMetadataCAS reproduces issueops.MetadataCAS over the doubles' raw
// read-modify-write pair, INCLUDING the role's distinction between an absent
// key and one present holding JSON null or the empty string. That distinction
// is the whole point of the adapter: the store's two-arm normalization is only
// exercised by a double that actually has the two states.
type rawMetadataCAS struct{ raw rawMetadataCASStorage }

func (c rawMetadataCAS) CompareAndSetKey(ctx context.Context, req issueops.CompareAndSetKeyRequest) (issueops.CompareAndSetKeyResult, error) {
	issue, err := c.raw.GetIssue(ctx, req.IssueID)
	if err != nil {
		return issueops.CompareAndSetKeyResult{}, err
	}
	if issue == nil {
		return issueops.CompareAndSetKeyResult{}, fmt.Errorf("%w: %s", issueops.ErrNotFound, req.IssueID)
	}
	object := map[string]json.RawMessage{}
	if len(issue.Metadata) > 0 {
		if err := json.Unmarshal(issue.Metadata, &object); err != nil {
			return issueops.CompareAndSetKeyResult{}, err
		}
	}
	stored, present := object[req.Key]
	current := func() *json.RawMessage {
		if !present {
			return nil
		}
		value := append(json.RawMessage(nil), stored...)
		return &value
	}
	matched := false
	switch {
	case req.Expected == nil:
		matched = !present
	case present:
		matched = string(*req.Expected) == string(stored)
	}
	if !matched {
		return issueops.CompareAndSetKeyResult{Current: current()}, nil
	}
	if req.Value == nil {
		delete(object, req.Key)
	} else {
		object[req.Key] = append(json.RawMessage(nil), *req.Value...)
	}
	raw, err := json.Marshal(object)
	if err != nil {
		return issueops.CompareAndSetKeyResult{}, err
	}
	if err := c.raw.UpdateIssue(ctx, req.IssueID, map[string]interface{}{"metadata": json.RawMessage(raw)}, req.Actor); err != nil {
		return issueops.CompareAndSetKeyResult{}, err
	}
	// Re-read through the same closure so Current reports the POST-write state,
	// which is what the role promises and what a retry loop feeds back.
	stored, present = object[req.Key]
	return issueops.CompareAndSetKeyResult{Swapped: true, Current: current()}, nil
}

func (s *nativeDoltStorageSpy) MetadataCAS() (issueops.MetadataCAS, error) {
	return rawMetadataCAS{raw: s}, nil
}

func (s *nativeDoltMemStorage) MetadataCAS() (issueops.MetadataCAS, error) {
	return rawMetadataCAS{raw: s}, nil
}

// rawReleaser reproduces issueops.Releaser over the doubles' read-modify-write
// pair, including the refusal taxonomy the store's front door maps onto its
// boolean verdict. The comparison here is BYTE-EXACT: the role's own
// separator-insensitivity is the substrate's, and a double that forgave
// separators would hide a store that stopped sending the expectation at all.
type rawReleaser struct{ raw rawMetadataCASStorage }

func (r rawReleaser) Release(ctx context.Context, req issueops.ReleaseRequest) (issueops.ReleaseResult, error) {
	issue, err := r.raw.GetIssue(ctx, req.IssueID)
	if err != nil {
		return issueops.ReleaseResult{}, err
	}
	if issue == nil {
		return issueops.ReleaseResult{}, fmt.Errorf("%w: %s", issueops.ErrNotFound, req.IssueID)
	}
	if issue.Status != beadslib.StatusOpen && issue.Status != beadslib.StatusInProgress {
		return issueops.ReleaseResult{}, issueops.ErrNotReleasable
	}
	if issue.Assignee == "" {
		return issueops.ReleaseResult{}, issueops.ErrNotClaimed
	}
	if req.ExpectedAssignee != nil && *req.ExpectedAssignee != issue.Assignee {
		return issueops.ReleaseResult{}, issueops.ErrAssigneeMismatch
	}
	if err := r.raw.UpdateIssue(ctx, req.IssueID, map[string]interface{}{
		"status":   "open",
		"assignee": "",
	}, req.Actor); err != nil {
		return issueops.ReleaseResult{}, err
	}
	return issueops.ReleaseResult{Changed: true}, nil
}

func (s *nativeDoltStorageSpy) Releaser() (issueops.Releaser, error) {
	return rawReleaser{raw: s}, nil
}

func (s *nativeDoltMemStorage) Releaser() (issueops.Releaser, error) {
	return rawReleaser{raw: s}, nil
}

// rawBatchApplier reproduces issueops.BatchApplier over the double's own
// lifecycle role, so a fixture that records or applies UpdateRequests keeps
// seeing the requests the store composed even when the store routes a patch
// through the batch door.
//
// It is deliberately NOT atomic: the doubles it serves have no transaction to
// roll back, and inventing one here would let a test pass against a store that
// dialed a partial batch.
type rawBatchApplier struct{ storage beadslib.Storage }

// ApplyBatch applies each item and answers ONE ItemResult per item, in request
// order, carrying the Changed the underlying operation reported.
//
// The per-item outcomes are not decoration: a caller that composes a batch and
// counts what it changed — CloseAll does exactly that — reads its answer off
// them, and a double that returned none would report every batch as having
// changed nothing while the rows moved. Changed is taken from the operation
// rather than assumed true for the same reason in the other direction: a double
// that over-reported it would let a caller count rows nothing changed.
//
// ItemResult.Issue stays NIL, matching what the served applier answers rather
// than what the in-process role could. The http client's result carries no
// post-item snapshot at all (ledger row L-apply-snapshot: hooks never fire on
// that surface and a hundred hydrated issues would dwarf the request), so a
// double that hydrated it would let a caller depend on a member the wire never
// sends — the oracle drift this file exists to avoid.
func (a rawBatchApplier) ApplyBatch(ctx context.Context, req issueops.ApplyBatchRequest) (issueops.ApplyBatchResult, error) {
	lifecycle, err := a.storage.IssueLifecycle()
	if err != nil {
		return issueops.ApplyBatchResult{}, err
	}
	result := issueops.ApplyBatchResult{Keys: map[string]string{}, Items: make([]issueops.ItemResult, 0, len(req.Items))}
	for _, item := range req.Items {
		switch item.Kind {
		case issueops.ItemUpdate:
			updated, err := lifecycle.Update(ctx, issueops.UpdateRequest{
				Actor:                 req.Actor,
				IssueID:               item.Update.Target.ID,
				Patch:                 item.Update.Patch,
				ForceAssigneeTransfer: item.Update.ForceAssigneeTransfer,
				ForceClosePolicy:      item.Update.ForceClosePolicy,
				ExpectedVersion:       item.Update.ExpectedVersion,
				ExpectedAssignee:      item.Update.ExpectedAssignee,
				ExpectedStatus:        item.Update.ExpectedStatus,
			})
			if err != nil {
				return issueops.ApplyBatchResult{}, err
			}
			result.Items = append(result.Items, issueops.ItemResult{
				Kind: item.Kind, IssueID: item.Update.Target.ID, Changed: updated.Changed,
			})
		case issueops.ItemClose:
			closed, err := lifecycle.Close(ctx, issueops.CloseRequest{
				Actor:           req.Actor,
				IssueID:         item.Close.Target.ID,
				Reason:          item.Close.Reason,
				Session:         item.Close.Session,
				Force:           item.Close.Force,
				ExpectedVersion: item.Close.ExpectedVersion,
			})
			if err != nil {
				return issueops.ApplyBatchResult{}, err
			}
			result.Items = append(result.Items, issueops.ItemResult{
				Kind: item.Kind, IssueID: item.Close.Target.ID, Changed: closed.Changed,
			})
		case issueops.ItemCreate:
			created, err := lifecycle.Create(ctx, issueops.CreateRequest{Actor: req.Actor, Issue: item.Create.Issue})
			if err != nil {
				return issueops.ApplyBatchResult{}, err
			}
			id := ""
			if created.Issue != nil {
				id = created.Issue.ID
			}
			if item.Create.Key != "" && id != "" {
				result.Keys[item.Create.Key] = id
			}
			result.Items = append(result.Items, issueops.ItemResult{
				Kind: item.Kind, IssueID: id, Changed: true,
			})
		default:
			return issueops.ApplyBatchResult{}, fmt.Errorf("rawBatchApplier: unsupported item kind %q", item.Kind)
		}
	}
	return result, nil
}

func (s *nativeDoltStorageSpy) BatchApplier() (issueops.BatchApplier, error) {
	return rawBatchApplier{storage: s}, nil
}

func (s *nativeDoltMemStorage) BatchApplier() (issueops.BatchApplier, error) {
	return rawBatchApplier{storage: s}, nil
}

// The failing-label double gets its own batch applier for the reason embedding
// exists to make awkward: rawBatchApplier resolves the lifecycle from the
// storage it was handed, and one built from the EMBEDDED mem storage would run
// the inner AddLabel rather than this double's failing override — turning a
// rollback test into a test of nothing.
func (s *nativeDoltFailingLabelStorage) BatchApplier() (issueops.BatchApplier, error) {
	return rawBatchApplier{storage: s}, nil
}
