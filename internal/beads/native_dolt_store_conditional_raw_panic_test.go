package beads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

// nativeDoltRawPanicStorage proves the conditional-write cluster
// (UpdateIfMatch, CloseIfMatch, DeleteIfMatch, CloseWithMetadataIfMatch) never
// falls through to a raw storage primitive. Its embedded beadslib.Storage is
// left nil, so ANY method the five role accessors below do not satisfy --
// GetIssue, UpdateIssue, UpdateIssueChecked, CloseIssue, CloseIssueChecked,
// RunInTransaction, DeleteIssue, ReopenIssue, and everything else the real
// interface carries -- panics with a nil pointer dereference the instant
// NativeDoltStore reaches for it instead of going through the role.
//
// The role fakes below are deliberately self-contained: they read and write
// only the fixture issue held directly on this struct, never bouncing back
// through a raw call on the embedded (nil) Storage. The edge door is
// edgelessReader, because the fixture issue carries no edges.
type nativeDoltRawPanicStorage struct {
	beadslib.Storage
	issue *beadslib.Issue
}

func newNativeDoltRawPanicStorage(issue *beadslib.Issue) *nativeDoltRawPanicStorage {
	return &nativeDoltRawPanicStorage{issue: cloneNativeIssueForTest(issue)}
}

func (s *nativeDoltRawPanicStorage) IssueLifecycle() (issueops.Lifecycle, error) {
	return rawPanicLifecycle{storage: s}, nil
}

func (s *nativeDoltRawPanicStorage) Deleter() (issueops.Deleter, error) {
	return rawPanicDeleter{storage: s}, nil
}

func (s *nativeDoltRawPanicStorage) BatchApplier() (issueops.BatchApplier, error) {
	return rawPanicBatchApplier{storage: s}, nil
}

func (s *nativeDoltRawPanicStorage) IssueReader() (issueops.Reader, error) {
	return rawPanicReader{storage: s}, nil
}

func (s *nativeDoltRawPanicStorage) EdgeReader() (issueops.EdgeReader, error) {
	return edgelessReader{}, nil
}

// RunInTransaction answers the way a backend with no native transaction does:
// a typed *beadslib.ErrUnsupported, raised BEFORE the callback ever runs. This
// is what routes Store.Tx onto runTxAsBatch (native_dolt_store_batch_tx.go),
// which records writes through IssueReader/BatchApplier above instead of a
// raw tx.* call — the door this double exists to prove nothing bypasses.
func (s *nativeDoltRawPanicStorage) RunInTransaction(context.Context, string, func(beadslib.Transaction) error) error {
	return &beadslib.ErrUnsupported{Op: "RunInTransaction", Backend: "raw-panic-double"}
}

type rawPanicLifecycle struct{ storage *nativeDoltRawPanicStorage }

var _ issueops.Lifecycle = rawPanicLifecycle{}

func (l rawPanicLifecycle) Create(context.Context, issueops.CreateRequest) (issueops.CreateResult, error) {
	panic("rawPanicLifecycle: Create not exercised by the conditional-write cluster")
}

func (l rawPanicLifecycle) Update(_ context.Context, req issueops.UpdateRequest) (issueops.UpdateResult, error) {
	issue := l.storage.issue
	if issue == nil || issue.ID != req.IssueID {
		return issueops.UpdateResult{}, issueops.ErrNotFound
	}
	if req.ExpectedVersion != nil && issue.RowVersion != *req.ExpectedVersion {
		return issueops.UpdateResult{}, issueops.ErrVersionMismatch
	}
	if req.Patch.Title.Set {
		issue.Title = req.Patch.Title.Value
	}
	if req.Patch.Status.Set {
		issue.Status = req.Patch.Status.Value
	}
	if req.Patch.Assignee.Set {
		issue.Assignee = req.Patch.Assignee.Value
	}
	issue.RowVersion++
	return issueops.UpdateResult{Changed: true, Issue: cloneNativeIssueForTest(issue)}, nil
}

func (l rawPanicLifecycle) Close(_ context.Context, req issueops.CloseRequest) (issueops.CloseResult, error) {
	issue := l.storage.issue
	if issue == nil || issue.ID != req.IssueID {
		return issueops.CloseResult{}, issueops.ErrNotFound
	}
	if req.ExpectedVersion != nil && issue.RowVersion != *req.ExpectedVersion {
		return issueops.CloseResult{}, issueops.ErrVersionMismatch
	}
	if issue.Status == beadslib.StatusClosed {
		return issueops.CloseResult{Issue: cloneNativeIssueForTest(issue)}, nil
	}
	issue.Status = beadslib.StatusClosed
	issue.CloseReason = req.Reason
	issue.RowVersion++
	return issueops.CloseResult{Changed: true, Issue: cloneNativeIssueForTest(issue)}, nil
}

func (l rawPanicLifecycle) Reopen(_ context.Context, req issueops.ReopenRequest) (issueops.ReopenResult, error) {
	issue := l.storage.issue
	if issue == nil || issue.ID != req.IssueID {
		return issueops.ReopenResult{}, issueops.ErrNotFound
	}
	issue.Status = beadslib.StatusOpen
	issue.RowVersion++
	return issueops.ReopenResult{Changed: true, Issue: cloneNativeIssueForTest(issue)}, nil
}

type rawPanicDeleter struct{ storage *nativeDoltRawPanicStorage }

var _ issueops.Deleter = rawPanicDeleter{}

func (d rawPanicDeleter) Delete(_ context.Context, req issueops.DeleteRequest) (issueops.DeleteResult, error) {
	issue := d.storage.issue
	if issue == nil || len(req.IDs) != 1 || req.IDs[0] != issue.ID {
		return issueops.DeleteResult{}, issueops.ErrNotFound
	}
	if req.ExpectedVersion != nil && issue.RowVersion != *req.ExpectedVersion {
		return issueops.DeleteResult{}, issueops.ErrVersionMismatch
	}
	d.storage.issue = nil
	return issueops.DeleteResult{Deleted: 1}, nil
}

type rawPanicBatchApplier struct{ storage *nativeDoltRawPanicStorage }

var _ issueops.BatchApplier = rawPanicBatchApplier{}

func (a rawPanicBatchApplier) ApplyBatch(_ context.Context, req issueops.ApplyBatchRequest) (issueops.ApplyBatchResult, error) {
	issue := a.storage.issue
	if issue == nil {
		return issueops.ApplyBatchResult{}, issueops.ErrNotFound
	}
	result := issueops.ApplyBatchResult{Items: make([]issueops.ItemResult, 0, len(req.Items))}
	for i, item := range req.Items {
		switch item.Kind {
		case issueops.ItemCreate:
			// Graph apply at or under issueops.MaxApplyBatchItems composes the
			// whole plan as create items through this same role door. This
			// double mints a deterministic id per key rather than modeling a
			// multi-row store, which is all ValidateGraphApplyResult checks:
			// every node key maps to a non-empty id.
			if result.Keys == nil {
				result.Keys = make(map[string]string, len(req.Items))
			}
			mintedID := fmt.Sprintf("gc-rawpanic-%d", i)
			if item.Create.Key != "" {
				result.Keys[item.Create.Key] = mintedID
			}
			result.Items = append(result.Items, issueops.ItemResult{Kind: item.Kind, IssueID: mintedID, Changed: true})
		case issueops.ItemUpdate:
			u := item.Update
			if u.ExpectedVersion != nil && issue.RowVersion != *u.ExpectedVersion {
				return issueops.ApplyBatchResult{}, &issueops.ItemError{Index: i, Kind: item.Kind, Err: issueops.ErrVersionMismatch}
			}
			if u.Patch.Status.Set {
				issue.Status = u.Patch.Status.Value
			}
			if u.Patch.Assignee.Set {
				issue.Assignee = u.Patch.Assignee.Value
			}
			if len(u.Patch.Metadata.Set) > 0 {
				merged, err := metadataMapFromNative(issue.Metadata)
				if err != nil {
					return issueops.ApplyBatchResult{}, err
				}
				if merged == nil {
					merged = make(map[string]string, len(u.Patch.Metadata.Set))
				}
				for key, raw := range u.Patch.Metadata.Set {
					var value string
					if err := json.Unmarshal(raw, &value); err != nil {
						return issueops.ApplyBatchResult{}, err
					}
					merged[key] = value
				}
				rawMetadata, err := metadataRawFromMap(merged)
				if err != nil {
					return issueops.ApplyBatchResult{}, err
				}
				issue.Metadata = rawMetadata
			}
			issue.RowVersion++
			result.Items = append(result.Items, issueops.ItemResult{Kind: item.Kind, IssueID: issue.ID, Changed: true, RowVersion: issue.RowVersion})
		case issueops.ItemClose:
			c := item.Close
			if c.ExpectedVersion != nil && issue.RowVersion != *c.ExpectedVersion {
				return issueops.ApplyBatchResult{}, &issueops.ItemError{Index: i, Kind: item.Kind, Err: issueops.ErrVersionMismatch}
			}
			issue.Status = beadslib.StatusClosed
			issue.CloseReason = c.Reason
			issue.RowVersion++
			result.Items = append(result.Items, issueops.ItemResult{Kind: item.Kind, IssueID: issue.ID, Changed: true, RowVersion: issue.RowVersion})
		default:
			return issueops.ApplyBatchResult{}, fmt.Errorf("rawPanicBatchApplier: unsupported item kind %q", item.Kind)
		}
	}
	return result, nil
}

type rawPanicReader struct{ storage *nativeDoltRawPanicStorage }

var _ issueops.Reader = rawPanicReader{}

func (r rawPanicReader) Ready(context.Context, issueops.ReadyRequest) (issueops.IssuePage, error) {
	panic("rawPanicReader: Ready not exercised by the conditional-write cluster")
}

func (r rawPanicReader) List(context.Context, issueops.ListRequest) (issueops.IssuePage, error) {
	panic("rawPanicReader: List not exercised by the conditional-write cluster")
}

func (r rawPanicReader) Get(_ context.Context, req issueops.GetRequest) (*issueops.IssueDetails, error) {
	issue := r.storage.issue
	if issue == nil || issue.ID != req.ID {
		return nil, issueops.ErrNotFound
	}
	return &issueops.IssueDetails{Issue: *cloneNativeIssueForTest(issue)}, nil
}

// TestNativeDoltStoreConditionalWritesNeverReachRawStorage pins the whole
// point of the G3 port: every one of these four methods must reach its
// backend ONLY through an issueops role. A storage double whose embedded
// beadslib.Storage is nil cannot silently succeed a raw call the way the
// no-op spy hooks elsewhere in this package would -- it panics -- so a
// regression that reintroduces a raw storage.GetIssue/UpdateIssueChecked/
// CloseIssueChecked/RunInTransaction/DeleteIssue call on any of these paths
// fails this test with a nil-pointer panic rather than passing silently.
func TestNativeDoltStoreConditionalWritesNeverReachRawStorage(t *testing.T) {
	t.Run("UpdateIfMatch plain door", func(t *testing.T) {
		storage := newNativeDoltRawPanicStorage(openIssueForConditionalTest())
		store := newNativeDoltStoreForTest(storage)
		title := "retitled"
		if err := store.UpdateIfMatch("gc-1", 0, UpdateOpts{Title: &title}); err != nil {
			t.Fatalf("UpdateIfMatch: %v", err)
		}
	})

	t.Run("UpdateIfMatch batch door", func(t *testing.T) {
		storage := newNativeDoltRawPanicStorage(openIssueForConditionalTest())
		store := newNativeDoltStoreForTest(storage)
		status := "in_progress"
		if err := store.UpdateIfMatch("gc-1", 0, UpdateOpts{Status: &status}); err != nil {
			t.Fatalf("UpdateIfMatch: %v", err)
		}
	})

	t.Run("CloseIfMatch", func(t *testing.T) {
		storage := newNativeDoltRawPanicStorage(openIssueForConditionalTest())
		store := newNativeDoltStoreForTest(storage)
		if err := store.CloseIfMatch("gc-1", 0); err != nil {
			t.Fatalf("CloseIfMatch: %v", err)
		}
	})

	t.Run("DeleteIfMatch", func(t *testing.T) {
		storage := newNativeDoltRawPanicStorage(openIssueForConditionalTest())
		store := newNativeDoltStoreForTest(storage)
		if err := store.DeleteIfMatch("gc-1", 0); err != nil {
			t.Fatalf("DeleteIfMatch: %v", err)
		}
	})

	t.Run("CloseWithMetadataIfMatch", func(t *testing.T) {
		storage := newNativeDoltRawPanicStorage(openIssueForConditionalTest())
		store := newNativeDoltStoreForTest(storage)
		if _, err := store.CloseWithMetadataIfMatch("gc-1", 0, map[string]string{"close_reason": "done"}); err != nil {
			t.Fatalf("CloseWithMetadataIfMatch: %v", err)
		}
	})
}

// TestNativeDoltStoreOtherPortedPathsNeverReachRawStorage extends the same
// raw-panic tripwire (a storage double whose embedded beadslib.Storage is nil,
// so any call the mocked roles do not satisfy panics) to the rest of G3's
// raw-GetIssue/raw-tx.* port: the version-mismatch read-back
// (conditionalWriteError), the conditional-write capability probe
// (probeConditionalWriteCapability), graph apply at or under
// issueops.MaxApplyBatchItems, and the newly ported Gets in closeOnce,
// reopenOnce, stampAndClose and nativeBatchTx.Close. The WorkspaceConfig-backed
// issue-prefix read (nativeReadIssuePrefix) is covered by its own subtest,
// added alongside that port.
func TestNativeDoltStoreOtherPortedPathsNeverReachRawStorage(t *testing.T) {
	t.Run("mismatch read-back", func(t *testing.T) {
		storage := newNativeDoltRawPanicStorage(openIssueForConditionalTest())
		store := newNativeDoltStoreForTest(storage)
		title := "retitled"
		err := store.UpdateIfMatch("gc-1", 99, UpdateOpts{Title: &title})
		var mismatch *PreconditionFailedError
		if !errors.As(err, &mismatch) {
			t.Fatalf("UpdateIfMatch with a stale revision = %v, want *PreconditionFailedError", err)
		}
	})

	t.Run("capability probe", func(t *testing.T) {
		storage := newNativeDoltRawPanicStorage(openIssueForConditionalTest())
		store := newNativeDoltStoreForTest(storage)
		ok, reason := store.probeConditionalWriteCapability()
		if !ok {
			t.Fatalf("probeConditionalWriteCapability = false (%s), want true: every role this double answers is implemented", reason)
		}
	})

	t.Run("graph apply at or under the cap", func(t *testing.T) {
		storage := newNativeDoltRawPanicStorage(openIssueForConditionalTest())
		store := newNativeDoltStoreForTest(storage)
		plan := &GraphApplyPlan{Nodes: []GraphApplyNode{{Key: "n1", Title: "a node"}}}
		result, err := store.ApplyGraphPlan(context.Background(), plan)
		if err != nil {
			t.Fatalf("ApplyGraphPlan: %v", err)
		}
		if result.IDs["n1"] == "" {
			t.Fatalf("ApplyGraphPlan result = %+v, want a minted id for key %q", result, "n1")
		}
	})

	t.Run("Close (closeOnce)", func(t *testing.T) {
		storage := newNativeDoltRawPanicStorage(openIssueForConditionalTest())
		store := newNativeDoltStoreForTest(storage)
		if err := store.Close("gc-1"); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})

	t.Run("Reopen (reopenOnce)", func(t *testing.T) {
		issue := openIssueForConditionalTest()
		issue.Status = beadslib.StatusClosed
		storage := newNativeDoltRawPanicStorage(issue)
		store := newNativeDoltStoreForTest(storage)
		if err := store.Reopen("gc-1"); err != nil {
			t.Fatalf("Reopen: %v", err)
		}
	})

	t.Run("stampAndClose", func(t *testing.T) {
		storage := newNativeDoltRawPanicStorage(openIssueForConditionalTest())
		store := newNativeDoltStoreForTest(storage)
		if err := store.stampAndClose("gc-1", map[string]string{"close_reason": "the sweep's reason"}); err != nil {
			t.Fatalf("stampAndClose: %v", err)
		}
	})

	t.Run("Tx Close (nativeBatchTx.Close)", func(t *testing.T) {
		storage := newNativeDoltRawPanicStorage(openIssueForConditionalTest())
		store := newNativeDoltStoreForTest(storage)
		if err := store.Tx("gc: test tx", func(tx Tx) error {
			return tx.Close("gc-1")
		}); err != nil {
			t.Fatalf("Tx: %v", err)
		}
	})
}
