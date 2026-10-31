package beads

import (
	"context"
	"errors"
	"testing"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

func TestNativeAssignmentGuardCarriesRawStatusAndExactVersion(t *testing.T) {
	fixture := openIssueForConditionalTest()
	fixture.Status = beadslib.StatusBlocked
	fixture.Assignee = "dead-session"
	fixture.RowVersion = 17
	store, rec := newRecordingNativeDoltStore(fixture)
	empty := ""
	ok, err := store.UpdateIfAssignment(fixture.ID, "blocked", "dead-session", UpdateOpts{Assignee: &empty})
	if err != nil || !ok || len(rec.updates) != 1 {
		t.Fatalf("guard = %v, %v, requests %+v", ok, err, rec.updates)
	}
	req := rec.updates[0]
	if req.ExpectedStatus == nil || *req.ExpectedStatus != issueops.Status("blocked") || req.ExpectedAssignee == nil || *req.ExpectedAssignee != "dead-session" || req.ExpectedVersion == nil || *req.ExpectedVersion != 17 {
		t.Fatalf("missing raw/exact guards: %+v", req)
	}
	if !req.Patch.Assignee.Set || req.Patch.Assignee.Value != "" || req.Patch.Status.Set || len(req.Patch.Metadata.Set) != 0 || req.ForceAssigneeTransfer {
		t.Fatalf("release patch changed hold: %+v", req)
	}
}

func TestNativeAssignmentGuardRefusesChangedRow(t *testing.T) {
	for _, tc := range []struct {
		name     string
		change   func(*beadslib.Issue)
		writeErr error
	}{
		{name: "owner", change: func(b *beadslib.Issue) { b.Assignee = "live-session" }},
		{name: "alias spelling", change: func(b *beadslib.Issue) { b.Assignee = "dead_session" }},
		{name: "status", change: func(b *beadslib.Issue) { b.Status = beadslib.StatusInProgress }},
		{name: "resolved different ID", change: func(b *beadslib.Issue) { b.ID = "gc-other" }},
		{name: "race version", writeErr: issueops.ErrVersionMismatch},
		{name: "race owner", writeErr: issueops.ErrAssigneeMismatch},
		{name: "race status", writeErr: issueops.ErrStatusMismatch},
		{name: "gone", writeErr: issueops.ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := openIssueForConditionalTest()
			fixture.Status = beadslib.StatusBlocked
			fixture.Assignee = "dead-session"
			fixture.RowVersion = 17
			if tc.change != nil {
				tc.change(fixture)
			}
			store, rec := newRecordingNativeDoltStore(fixture)
			rec.err = tc.writeErr
			empty := ""
			ok, err := store.UpdateIfAssignment("gc-1", "blocked", "dead-session", UpdateOpts{Assignee: &empty})
			if err != nil || ok {
				t.Fatalf("guard = %v, %v, want miss", ok, err)
			}
			if tc.change != nil && len(rec.updates) != 0 {
				t.Fatalf("write on changed read: %+v", rec.updates)
			}
			if tc.writeErr != nil && len(rec.updates) != 1 {
				t.Fatalf("guard mismatch replayed: %+v", rec.updates)
			}
		})
	}
}

func TestNativeAssignmentGuardPreservesRetryFence(t *testing.T) {
	for _, tc := range []struct {
		name              string
		first, errorAfter error
		wantCalls         int
		wantOK            bool
	}{
		{name: "serialization", first: errors.New(serializationConflictErr), wantCalls: 2, wantOK: true},
		{name: "claim during retry", first: errors.New(serializationConflictErr), errorAfter: issueops.ErrVersionMismatch, wantCalls: 2},
		{name: "ambiguous", first: errors.New("connection reset by peer"), wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := openIssueForConditionalTest()
			fixture.Status = beadslib.StatusBlocked
			fixture.Assignee = "dead-session"
			fixture.RowVersion = 17
			calls := 0
			lifecycle := retryCountingLifecycle{update: func(req issueops.UpdateRequest) (issueops.UpdateResult, error) {
				calls++
				if req.ExpectedVersion == nil || *req.ExpectedVersion != 17 {
					t.Fatal("retry acquired newer fence")
				}
				if calls == 1 {
					return issueops.UpdateResult{}, tc.first
				}
				return issueops.UpdateResult{}, tc.errorAfter
			}}
			spy := &nativeDoltStorageSpy{getIssue: func(context.Context, string) (*beadslib.Issue, error) { return cloneNativeIssueForTest(fixture), nil }, issueLifecycle: func() (issueops.Lifecycle, error) { return lifecycle, nil }}
			store := newNativeDoltStoreForTest(spy)
			empty := ""
			ok, err := store.UpdateIfAssignment("gc-1", "blocked", "dead-session", UpdateOpts{Assignee: &empty})
			if ok != tc.wantOK || calls != tc.wantCalls {
				t.Fatalf("guard = %v, %v after %d calls", ok, err, calls)
			}
			if tc.name == "ambiguous" && !errors.Is(err, tc.first) {
				t.Fatalf("lost ambiguous error: %v", err)
			}
		})
	}
}

func TestNativeAssignmentGuardRefusesUnavailableRolesAndFields(t *testing.T) {
	fixture := openIssueForConditionalTest()
	fixture.Status = beadslib.StatusBlocked
	fixture.Assignee = "dead-session"
	fixture.RowVersion = 17
	sentinel := errors.New("lifecycle unsupported")
	spy := &nativeDoltStorageSpy{getIssue: func(context.Context, string) (*beadslib.Issue, error) { return cloneNativeIssueForTest(fixture), nil }, issueLifecycle: func() (issueops.Lifecycle, error) { return nil, sentinel }}
	store := newNativeDoltStoreForTest(spy)
	empty := ""
	if ok, err := store.UpdateIfAssignment("gc-1", "blocked", "dead-session", UpdateOpts{Assignee: &empty}); ok || !errors.Is(err, sentinel) {
		t.Fatalf("unsupported lifecycle = %v, %v", ok, err)
	}
	store = newNativeDoltStoreForTest(&assignmentReaderRefusal{nativeDoltStorageSpy: spy, err: sentinel})
	if ok, err := store.UpdateIfAssignment("gc-1", "blocked", "dead-session", UpdateOpts{Assignee: &empty}); ok || !errors.Is(err, sentinel) {
		t.Fatalf("unsupported reader = %v, %v", ok, err)
	}
	for _, opts := range []UpdateOpts{{Labels: []string{"hold"}}, {ParentID: &empty}, {}} {
		if ok, err := store.UpdateIfAssignment("gc-1", "blocked", "dead-session", opts); ok || err == nil {
			t.Fatalf("unsupported options %+v = %v, %v", opts, ok, err)
		}
	}
}

type assignmentReaderRefusal struct {
	*nativeDoltStorageSpy
	err error
}

func (s *assignmentReaderRefusal) IssueReader() (issueops.Reader, error) { return nil, s.err }

func TestNativeAssignmentGuardUsesPublishedRevisionToken(t *testing.T) {
	for _, token := range []string{"23", "invalid"} {
		t.Run(token, func(t *testing.T) {
			fixture := openIssueForConditionalTest()
			fixture.Status = beadslib.StatusBlocked
			fixture.Assignee = "dead-session"
			rec := &nativeDoltRecordingLifecycle{}
			spy := &nativeDoltStorageSpy{issueLifecycle: func() (issueops.Lifecycle, error) { return rec, nil }}
			store := newNativeDoltStoreForTest(&assignmentDetailsStorage{nativeDoltStorageSpy: spy, details: &issueops.IssueDetails{Issue: *fixture, Revision: token}})
			empty := ""
			ok, err := store.UpdateIfAssignment("gc-1", "blocked", "dead-session", UpdateOpts{Assignee: &empty})
			if token == "invalid" {
				if ok || err == nil || len(rec.updates) != 0 {
					t.Fatalf("invalid token reached writer: %v, %v, %+v", ok, err, rec.updates)
				}
				return
			}
			if !ok || err != nil || len(rec.updates) != 1 || *rec.updates[0].ExpectedVersion != 23 {
				t.Fatalf("published token lost: %v, %v, %+v", ok, err, rec.updates)
			}
		})
	}
}

type assignmentDetailsStorage struct {
	*nativeDoltStorageSpy
	details *issueops.IssueDetails
}

func (s *assignmentDetailsStorage) IssueReader() (issueops.Reader, error) {
	return assignmentDetailsReader{details: s.details}, nil
}

type assignmentDetailsReader struct {
	issueops.Reader
	details *issueops.IssueDetails
}

func (r assignmentDetailsReader) Get(context.Context, issueops.GetRequest) (*issueops.IssueDetails, error) {
	return r.details, nil
}

func TestNativeBlockedReleaseLostGuardStaysHandled(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      beadslib.Status
		writeErr    error
		wantHandled bool
	}{
		{name: "ordinary open", status: beadslib.StatusOpen},
		{name: "blocked", status: beadslib.StatusBlocked, wantHandled: true},
		{name: "reopened after raw read", status: beadslib.StatusBlocked, writeErr: issueops.ErrStatusMismatch, wantHandled: true},
		{name: "owner changed after raw read", status: beadslib.StatusBlocked, writeErr: issueops.ErrAssigneeMismatch, wantHandled: true},
		{name: "version changed after raw read", status: beadslib.StatusBlocked, writeErr: issueops.ErrVersionMismatch, wantHandled: true},
		{name: "review hold", status: beadslib.Status("review"), wantHandled: true},
		{name: "testing hold", status: beadslib.Status("testing"), wantHandled: true},
		{name: "deferred hold", status: beadslib.Status("deferred"), wantHandled: true},
		{name: "custom hold", status: beadslib.Status("custom"), wantHandled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := openIssueForConditionalTest()
			fixture.Status = tc.status
			fixture.Assignee = "dead-session"
			fixture.RowVersion = 17
			store, rec := newRecordingNativeDoltStore(fixture)
			rec.err = tc.writeErr
			ok, handled, err := store.ReleaseBlockedIfCurrent("gc-1", "dead-session", map[string]string{"gc.continuation_group": "", "gc.run_target": "repo/worker"})
			wantOK := tc.status == beadslib.StatusBlocked && tc.writeErr == nil
			if err != nil || handled != tc.wantHandled || ok != wantOK {
				t.Fatalf("release = %v, %v, %v", ok, handled, err)
			}
			if tc.status == beadslib.StatusBlocked {
				if len(rec.updates) != 1 {
					t.Fatalf("combined update count = %d", len(rec.updates))
				}
				req := rec.updates[0]
				if req.ExpectedStatus == nil || *req.ExpectedStatus != issueops.Status("blocked") || req.ExpectedAssignee == nil || *req.ExpectedAssignee != "dead-session" || req.ExpectedVersion == nil || *req.ExpectedVersion != 17 || req.Patch.Status.Set || !req.Patch.Assignee.Set || req.Patch.Assignee.Value != "" || req.ForceAssigneeTransfer {
					t.Fatalf("combined release lost fencing or changed hold: %+v", req)
				}
				if len(req.Patch.Metadata.Set) != 2 || string(req.Patch.Metadata.Set["gc.continuation_group"]) != `""` || string(req.Patch.Metadata.Set["gc.run_target"]) != `"repo/worker"` {
					t.Fatalf("metadata patch = %+v", req.Patch.Metadata)
				}
			}
			if tc.status != beadslib.StatusBlocked && len(rec.updates) != 0 {
				t.Fatalf("wrote other raw status: %+v", rec.updates)
			}
		})
	}
}

func TestBlockedReleaseCapabilityDoesNotInventSupportOnCache(t *testing.T) {
	cache := NewCachingStore(NewMemStore(), nil)
	if _, ok := BlockedAssignmentReleaserFor(cache); ok {
		t.Fatal("cache invented backing capability")
	}
}
