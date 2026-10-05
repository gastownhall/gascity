package beads

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// nativeDoltLifecycleForTest stands in for the beads issue-lifecycle facade over
// this package's Storage test doubles.
//
// The real facade resolves a whole request inside one SQL transaction against a
// live Dolt server, which the unit tier deliberately does not run. This double
// reaches the same Storage/Transaction primitives through the double's own
// RunInTransaction, so a mid-request failure rolls back exactly as the facade's
// transaction does, and it reproduces the refusals the store's request mapping
// has to respect — most importantly that dependencies must arrive on the
// request rather than on the issue.
//
// It also reproduces the two refusals that are pure request validation and so
// need no server: a forced assignee transfer with no assignee edit, and a
// patched metadata key the backend would not accept.
//
// Semantics it does NOT model, because no store double can: the facade's
// serialization retry and its close policy, both of which read other rows.
// Those are covered against real storage in the integration tier.
type nativeDoltLifecycleForTest struct{ storage beadslib.Storage }

var _ issueops.Lifecycle = nativeDoltLifecycleForTest{}

func newNativeDoltLifecycleForTest(storage beadslib.Storage) (issueops.Lifecycle, error) {
	return nativeDoltLifecycleForTest{storage: storage}, nil
}

func nativeDoltLifecycleValidationError(format string, args ...any) error {
	return fmt.Errorf("%w: %s", issueops.ErrValidation, fmt.Sprintf(format, args...))
}

func (l nativeDoltLifecycleForTest) Create(ctx context.Context, request issueops.CreateRequest) (issueops.CreateResult, error) {
	if request.Issue == nil {
		return issueops.CreateResult{}, nativeDoltLifecycleValidationError("create: actor and issue are required")
	}
	if len(request.Issue.Dependencies) > 0 || len(request.Issue.Comments) > 0 {
		return issueops.CreateResult{}, nativeDoltLifecycleValidationError("create: issue comments and dependencies must be supplied through request fields")
	}
	issue := cloneNativeIssueForTest(request.Issue)
	var result issueops.CreateResult
	err := l.storage.RunInTransaction(ctx, "bd: create issue", func(tx beadslib.Transaction) error {
		if err := tx.CreateIssue(ctx, issue, request.Actor); err != nil {
			return err
		}
		edges := make([]*beadslib.Dependency, 0, len(request.Dependencies)+1)
		if request.ParentID != "" {
			edges = append(edges, &beadslib.Dependency{IssueID: issue.ID, DependsOnID: request.ParentID, Type: beadslib.DepParentChild})
		}
		for _, dependency := range request.Dependencies {
			edge := &beadslib.Dependency{IssueID: issue.ID, DependsOnID: dependency.TargetID, Type: dependency.Type}
			if dependency.Reverse {
				edge.IssueID, edge.DependsOnID = dependency.TargetID, issue.ID
			}
			edges = append(edges, edge)
		}
		for _, edge := range edges {
			if err := tx.AddDependency(ctx, edge, request.Actor); err != nil {
				return err
			}
		}
		result.Issue = l.hydrateForTest(ctx, tx, issue, edges)
		return nil
	})
	if err != nil {
		return issueops.CreateResult{}, err
	}
	return result, nil
}

// hydrateForTest re-reads the created issue the way the facade does, falling
// back to the in-flight issue for doubles that do not serve reads.
func (l nativeDoltLifecycleForTest) hydrateForTest(ctx context.Context, tx beadslib.Transaction, issue *beadslib.Issue, edges []*beadslib.Dependency) *beadslib.Issue {
	if stored, err := tx.GetIssue(ctx, issue.ID); err == nil && stored != nil {
		return stored
	}
	hydrated := cloneNativeIssueForTest(issue)
	hydrated.Dependencies = cloneNativeDependencies(edges)
	return hydrated
}

func (l nativeDoltLifecycleForTest) Update(ctx context.Context, request issueops.UpdateRequest) (issueops.UpdateResult, error) {
	if request.ForceAssigneeTransfer && !request.Patch.Assignee.Set {
		return issueops.UpdateResult{}, nativeDoltLifecycleValidationError("invalid forced assignee transfer")
	}
	// The facade validates every patched metadata key before it opens the
	// transaction; Create does not validate the same keys, and neither does the
	// map-based Tx write. That asymmetry is what lets a bead be created with a
	// key that jams every later update, so the double reproduces it.
	for key := range request.Patch.Metadata.Set {
		if !beadmeta.ValidKey(key) {
			return issueops.UpdateResult{}, nativeDoltLifecycleValidationError("invalid metadata key %q: must match %s", key, beadmeta.ValidKeyPattern)
		}
	}
	updates, err := nativeDoltLifecycleUpdatesForTest(request.Patch)
	if err != nil {
		return issueops.UpdateResult{}, err
	}
	var result issueops.UpdateResult
	err = l.storage.RunInTransaction(ctx, "bd: update "+request.IssueID, func(tx beadslib.Transaction) error {
		current, err := tx.GetIssue(ctx, request.IssueID)
		if err != nil {
			return err
		}
		if current == nil {
			return fmt.Errorf("not found: issue %s", request.IssueID)
		}
		// The three Expected* guards are evaluated AS-MODIFIED, against the row
		// as this transaction sees it right now -- which, inside a batch's
		// per-item sequential calls into this same double, is already the row
		// as earlier items in the request left it. A miss leaves this
		// transaction's body unrun: the double returns before any of the
		// writes below are issued.
		if request.ExpectedVersion != nil && current.RowVersion != *request.ExpectedVersion {
			return issueops.ErrVersionMismatch
		}
		if request.ExpectedStatus != nil && current.Status != *request.ExpectedStatus {
			return issueops.ErrStatusMismatch
		}
		if request.ExpectedAssignee != nil && current.Assignee != *request.ExpectedAssignee {
			return issueops.ErrAssigneeMismatch
		}
		if request.Patch.ParentID.Set {
			if err := nativeDoltLifecycleReparentForTest(ctx, tx, request); err != nil {
				return err
			}
		}
		if len(updates) > 0 {
			if err := tx.UpdateIssue(ctx, request.IssueID, updates, request.Actor); err != nil {
				return err
			}
			result.Changed = true
		}
		for _, label := range request.Patch.Labels.Add {
			if err := tx.AddLabel(ctx, request.IssueID, label, request.Actor); err != nil {
				return err
			}
			result.Changed = true
		}
		for _, label := range request.Patch.Labels.Remove {
			if err := tx.RemoveLabel(ctx, request.IssueID, label, request.Actor); err != nil {
				return err
			}
			result.Changed = true
		}
		result.Issue, _ = tx.GetIssue(ctx, request.IssueID)
		return nil
	})
	if err != nil {
		return issueops.UpdateResult{}, err
	}
	return result, nil
}

// nativeDoltLifecycleReparentForTest replaces every parent edge with the one the
// patch names, refusing a target that does not exist.
func nativeDoltLifecycleReparentForTest(ctx context.Context, tx beadslib.Transaction, request issueops.UpdateRequest) error {
	parentID := strings.TrimSpace(request.Patch.ParentID.Value)
	namespace, _ := tx.GetConfig(ctx, "issue_prefix")
	if parentID != "" && !nativeDoltTargetIsExternalForTest(namespace, parentID) {
		parent, err := tx.GetIssue(ctx, parentID)
		if err != nil {
			return err
		}
		if parent == nil {
			return fmt.Errorf("issue %s not found", parentID)
		}
	}
	existing, err := tx.GetDependencyRecords(ctx, request.IssueID)
	if err != nil {
		return err
	}
	for _, dependency := range existing {
		if dependency == nil || dependency.Type != beadslib.DepParentChild {
			continue
		}
		if err := tx.RemoveDependency(ctx, request.IssueID, dependency.DependsOnID, request.Actor); err != nil {
			return err
		}
	}
	if parentID == "" {
		return nil
	}
	return tx.AddDependency(ctx, &beadslib.Dependency{
		IssueID:     request.IssueID,
		DependsOnID: parentID,
		Type:        beadslib.DepParentChild,
	}, request.Actor)
}

// nativeDoltLifecycleUpdatesForTest maps an issue patch onto the storage-layer
// update map. Metadata rides the merge operation rather than the metadata
// column, which is how the facade merges keys instead of replacing the document.
func nativeDoltLifecycleUpdatesForTest(patch issueops.IssuePatch) (map[string]interface{}, error) {
	updates := make(map[string]interface{})
	if patch.Title.Set {
		updates["title"] = patch.Title.Value
	}
	if patch.Status.Set {
		updates["status"] = string(patch.Status.Value)
	}
	if patch.IssueType.Set {
		updates["issue_type"] = string(patch.IssueType.Value)
	}
	if patch.Priority.Set {
		updates["priority"] = patch.Priority.Value
	}
	if patch.Description.Set {
		updates["description"] = patch.Description.Value
	}
	if patch.Assignee.Set {
		updates["assignee"] = patch.Assignee.Value
	}
	if len(patch.Metadata.Set) > 0 {
		raw, err := json.Marshal(patch.Metadata.Set)
		if err != nil {
			return nil, err
		}
		updates[nativeDoltMergeMetadataOp] = json.RawMessage(raw)
	}
	return updates, nil
}

// nativeDoltMergeMetadataOp mirrors the beads issueops.OpMergeMetadata update
// key: a merge operation resolved against the current row inside the write
// transaction rather than a plain column write.
const nativeDoltMergeMetadataOp = "_merge_metadata"

// Close and Reopen are single writes, so they read and write the store directly
// rather than through RunInTransaction: beadslib.Transaction carries no reopen
// verb (that lives on the internal IssueLifecycleTransaction lane), and neither
// operation has a second write to roll back.
func (l nativeDoltLifecycleForTest) Close(ctx context.Context, request issueops.CloseRequest) (issueops.CloseResult, error) {
	current, err := l.storage.GetIssue(ctx, request.IssueID)
	if err != nil {
		return issueops.CloseResult{}, err
	}
	if current == nil {
		return issueops.CloseResult{}, fmt.Errorf("not found: issue %s", request.IssueID)
	}
	// ExpectedVersion is checked BEFORE the idempotent-close short-circuit
	// below, per CloseRequest.ExpectedVersion's own doc: a re-close of an
	// already-closed issue still refuses on a stale token rather than
	// reporting the no-op it would otherwise be.
	if request.ExpectedVersion != nil && current.RowVersion != *request.ExpectedVersion {
		return issueops.CloseResult{}, issueops.ErrVersionMismatch
	}
	if current.Status == beadslib.StatusClosed {
		return issueops.CloseResult{Issue: current}, nil
	}
	if err := l.storage.CloseIssue(ctx, request.IssueID, request.Reason, request.Actor, request.Session); err != nil {
		return issueops.CloseResult{}, err
	}
	closed, _ := l.storage.GetIssue(ctx, request.IssueID)
	return issueops.CloseResult{Issue: closed, Changed: true}, nil
}

func (l nativeDoltLifecycleForTest) Reopen(ctx context.Context, request issueops.ReopenRequest) (issueops.ReopenResult, error) {
	current, err := l.storage.GetIssue(ctx, request.IssueID)
	if err != nil {
		return issueops.ReopenResult{}, err
	}
	if current == nil {
		return issueops.ReopenResult{}, fmt.Errorf("not found: issue %s", request.IssueID)
	}
	if current.Status == beadslib.StatusOpen {
		return issueops.ReopenResult{Issue: current}, nil
	}
	if err := l.storage.ReopenIssue(ctx, request.IssueID, request.Reason, request.Actor); err != nil {
		return issueops.ReopenResult{}, err
	}
	reopened, _ := l.storage.GetIssue(ctx, request.IssueID)
	return issueops.ReopenResult{Issue: reopened, Changed: true}, nil
}

func (s *nativeDoltStorageSpy) IssueLifecycle() (issueops.Lifecycle, error) {
	if s.issueLifecycle != nil {
		return s.issueLifecycle()
	}
	return newNativeDoltLifecycleForTest(s)
}

// nativeDoltRecordingLifecycle records the facade requests a store issues
// without applying them, so a test can assert the request the store built
// rather than the state some double happened to reach.
type nativeDoltRecordingLifecycle struct {
	creates []issueops.CreateRequest
	updates []issueops.UpdateRequest
	closes  []issueops.CloseRequest
	reopens []issueops.ReopenRequest
	err     error
}

var _ issueops.Lifecycle = (*nativeDoltRecordingLifecycle)(nil)

func (l *nativeDoltRecordingLifecycle) Create(_ context.Context, request issueops.CreateRequest) (issueops.CreateResult, error) {
	l.creates = append(l.creates, request)
	if l.err != nil {
		return issueops.CreateResult{}, l.err
	}
	issue := cloneNativeIssueForTest(request.Issue)
	if issue.ID == "" {
		issue.ID = "gc-recorded"
	}
	return issueops.CreateResult{Issue: issue}, nil
}

func (l *nativeDoltRecordingLifecycle) Update(_ context.Context, request issueops.UpdateRequest) (issueops.UpdateResult, error) {
	l.updates = append(l.updates, request)
	return issueops.UpdateResult{}, l.err
}

func (l *nativeDoltRecordingLifecycle) Close(_ context.Context, request issueops.CloseRequest) (issueops.CloseResult, error) {
	l.closes = append(l.closes, request)
	return issueops.CloseResult{}, l.err
}

func (l *nativeDoltRecordingLifecycle) Reopen(_ context.Context, request issueops.ReopenRequest) (issueops.ReopenResult, error) {
	l.reopens = append(l.reopens, request)
	return issueops.ReopenResult{}, l.err
}

// newRecordingNativeDoltStore returns a store whose writes land in a recording
// lifecycle, with reads served from issue.
func newRecordingNativeDoltStore(issue *beadslib.Issue) (*NativeDoltStore, *nativeDoltRecordingLifecycle) {
	recorder := &nativeDoltRecordingLifecycle{}
	storage := &nativeDoltStorageSpy{
		getIssue: func(context.Context, string) (*beadslib.Issue, error) {
			return cloneNativeIssueForTest(issue), nil
		},
		issueLifecycle: func() (issueops.Lifecycle, error) { return recorder, nil },
	}
	return newNativeDoltStoreForTest(storage), recorder
}

func (s *nativeDoltMemStorage) IssueLifecycle() (issueops.Lifecycle, error) {
	return newNativeDoltLifecycleForTest(s)
}

func (s *commitCountingMemStorage) IssueLifecycle() (issueops.Lifecycle, error) {
	return newNativeDoltLifecycleForTest(s)
}

func (s *nativeDoltCloseCapturingStorage) IssueLifecycle() (issueops.Lifecycle, error) {
	return newNativeDoltLifecycleForTest(s)
}

func (s *nativeDoltFailingDependencyStorage) IssueLifecycle() (issueops.Lifecycle, error) {
	return newNativeDoltLifecycleForTest(s)
}

func (s *nativeDoltFailingLabelStorage) IssueLifecycle() (issueops.Lifecycle, error) {
	return newNativeDoltLifecycleForTest(s)
}
