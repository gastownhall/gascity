package beads

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	beadslib "github.com/steveyegge/beads"
)

func TestNativeDoltStoreCreateDelegatesToUpstreamStorage(t *testing.T) {
	createdAt := time.Date(2026, 5, 17, 10, 30, 0, 0, time.UTC)
	priority := 1
	var captured *beadslib.Issue
	var capturedActor string
	storage := &nativeDoltStorageSpy{
		createIssue: func(_ context.Context, issue *beadslib.Issue, actor string) error {
			captured = cloneNativeIssueForTest(issue)
			capturedActor = actor
			issue.ID = "gc-native"
			issue.CreatedAt = createdAt
			issue.UpdatedAt = createdAt
			return nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	got, err := store.Create(Bead{
		Title:       "native create",
		Priority:    &priority,
		Description: "created through native store",
		Assignee:    "gascity/builder",
		Labels:      []string{"native", "dolt"},
		Metadata:    map[string]string{"gc.step_ref": "build"},
		Needs:       []string{"blocks:ga-parent"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if capturedActor == "" {
		t.Fatal("CreateIssue actor was empty")
	}
	if captured.Title != "native create" {
		t.Fatalf("upstream title = %q, want native create", captured.Title)
	}
	if captured.Status != beadslib.StatusOpen {
		t.Fatalf("upstream status = %q, want open", captured.Status)
	}
	if captured.IssueType != beadslib.TypeTask {
		t.Fatalf("upstream issue type = %q, want task", captured.IssueType)
	}
	if len(captured.Dependencies) != 1 || captured.Dependencies[0].DependsOnID != "ga-parent" || captured.Dependencies[0].Type != beadslib.DepBlocks {
		t.Fatalf("upstream dependencies = %#v, want blocks:ga-parent", captured.Dependencies)
	}
	if !json.Valid(captured.Metadata) {
		t.Fatalf("upstream metadata is invalid JSON: %q", captured.Metadata)
	}
	if got.ID != "gc-native" {
		t.Fatalf("created ID = %q, want gc-native", got.ID)
	}
	if got.Status != "open" {
		t.Fatalf("created status = %q, want open", got.Status)
	}
	if got.Type != "task" {
		t.Fatalf("created type = %q, want task", got.Type)
	}
	if got.Metadata["gc.step_ref"] != "build" {
		t.Fatalf("created metadata = %#v, want gc.step_ref=build", got.Metadata)
	}
}

func TestNativeDoltStoreCreatePropagatesUpstreamError(t *testing.T) {
	wantErr := errors.New("create failed")
	storage := &nativeDoltStorageSpy{
		createIssue: func(context.Context, *beadslib.Issue, string) error {
			return wantErr
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if _, err := store.Create(Bead{Title: "native create"}); !errors.Is(err, wantErr) {
		t.Fatalf("Create error = %v, want %v", err, wantErr)
	}
}

func TestNativeDoltStoreCreateEmitsBeadCreatedEvent(t *testing.T) {
	createdAt := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	priority := 1
	rec := events.NewFake()
	storage := &nativeDoltStorageSpy{
		createIssue: func(_ context.Context, issue *beadslib.Issue, _ string) error {
			issue.ID = "gc-native"
			issue.CreatedAt = createdAt
			issue.UpdatedAt = createdAt
			return nil
		},
	}
	store := newNativeDoltStoreWithStorageAndRecorder(storage, "native-test", rec)

	created, err := store.Create(Bead{
		Title:    "native create",
		Priority: &priority,
		Metadata: map[string]string{"gc.step_ref": "build"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	event := singleNativeBeadEvent(t, rec)
	if event.Type != events.BeadCreated {
		t.Fatalf("event type = %q, want %q", event.Type, events.BeadCreated)
	}
	if event.Actor != "native-test" {
		t.Fatalf("event actor = %q, want native-test", event.Actor)
	}
	if event.Subject != created.ID {
		t.Fatalf("event subject = %q, want %q", event.Subject, created.ID)
	}
	payload := nativeBeadEventPayload(t, event)
	if payload.ID != created.ID || payload.Title != "native create" || payload.Status != "open" {
		t.Fatalf("event payload bead = %#v, want created bead snapshot", payload)
	}
	if payload.Metadata["gc.step_ref"] != "build" {
		t.Fatalf("event payload metadata = %#v, want gc.step_ref=build", payload.Metadata)
	}
}

func TestNativeDoltStoreGetPropagatesUpstreamError(t *testing.T) {
	wantErr := errors.New("get failed")
	storage := &nativeDoltStorageSpy{
		getIssue: func(context.Context, string) (*beadslib.Issue, error) {
			return nil, wantErr
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if _, err := store.Get("gc-missing"); !errors.Is(err, wantErr) {
		t.Fatalf("Get error = %v, want %v", err, wantErr)
	}
}

func TestNativeDoltStoreGetRejectsInvalidMetadata(t *testing.T) {
	storage := &nativeDoltStorageSpy{
		getIssue: func(context.Context, string) (*beadslib.Issue, error) {
			return &beadslib.Issue{
				ID:        "gc-corrupt",
				Title:     "corrupt metadata",
				Status:    beadslib.StatusOpen,
				IssueType: beadslib.TypeTask,
				Priority:  2,
				Metadata:  json.RawMessage(`{"gc.step_ref":`),
			}, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if _, err := store.Get("gc-corrupt"); err == nil {
		t.Fatal("Get error = nil, want invalid metadata error")
	} else if !strings.Contains(err.Error(), `parsing metadata for bead "gc-corrupt"`) {
		t.Fatalf("Get error = %v, want bead metadata context", err)
	}
}

func TestNativeDoltStoreListDelegatesAndConvertsIssues(t *testing.T) {
	createdAt := time.Date(2026, 5, 17, 11, 0, 0, 0, time.UTC)
	var capturedFilter beadslib.IssueFilter
	storage := &nativeDoltStorageSpy{
		searchIssues: func(_ context.Context, _ string, filter beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			capturedFilter = filter
			return []*beadslib.Issue{{
				ID:          "gc-listed",
				Title:       "listed through native store",
				Status:      beadslib.StatusOpen,
				IssueType:   beadslib.TypeTask,
				Priority:    2,
				CreatedAt:   createdAt,
				Assignee:    "gascity/builder",
				Labels:      []string{"native"},
				Metadata:    json.RawMessage(`{"gc.step_ref":"list"}`),
				Description: "native list",
			}}, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	got, err := store.List(ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(capturedFilter.ExcludeStatus) != 1 || capturedFilter.ExcludeStatus[0] != beadslib.StatusClosed {
		t.Fatalf("ExcludeStatus = %#v, want [closed]", capturedFilter.ExcludeStatus)
	}
	if len(got) != 1 {
		t.Fatalf("List len = %d, want 1", len(got))
	}
	if got[0].ID != "gc-listed" || got[0].Title != "listed through native store" {
		t.Fatalf("listed bead = %#v, want converted upstream issue", got[0])
	}
	if got[0].Metadata["gc.step_ref"] != "list" {
		t.Fatalf("metadata = %#v, want gc.step_ref=list", got[0].Metadata)
	}
}

func TestNativeDoltStoreUpdateEmitsBeadUpdatedEvent(t *testing.T) {
	rec := events.NewFake()
	issue := &beadslib.Issue{
		ID:        "gc-native",
		Title:     "before update",
		Status:    beadslib.StatusOpen,
		IssueType: beadslib.TypeTask,
		Priority:  2,
		Metadata:  json.RawMessage(`{"gc.step_ref":"build"}`),
	}
	storage := &nativeDoltStorageSpy{
		getIssue: func(_ context.Context, id string) (*beadslib.Issue, error) {
			if id != issue.ID {
				t.Fatalf("GetIssue id = %q, want %q", id, issue.ID)
			}
			return cloneNativeIssueForTest(issue), nil
		},
		updateIssue: func(_ context.Context, id string, updates map[string]interface{}, _ string) error {
			if id != issue.ID {
				t.Fatalf("UpdateIssue id = %q, want %q", id, issue.ID)
			}
			if title, ok := updates["title"].(string); ok {
				issue.Title = title
			}
			if status, ok := updates["status"].(string); ok {
				issue.Status = beadslib.Status(status)
			}
			return nil
		},
	}
	store := newNativeDoltStoreWithStorageAndRecorder(storage, "native-test", rec)
	title := "after update"
	status := "in_progress"

	if err := store.Update(issue.ID, UpdateOpts{Title: &title, Status: &status}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	event := singleNativeBeadEvent(t, rec)
	if event.Type != events.BeadUpdated {
		t.Fatalf("event type = %q, want %q", event.Type, events.BeadUpdated)
	}
	if event.Subject != issue.ID {
		t.Fatalf("event subject = %q, want %q", event.Subject, issue.ID)
	}
	payload := nativeBeadEventPayload(t, event)
	if payload.Title != title || payload.Status != status {
		t.Fatalf("event payload bead = %#v, want updated title/status", payload)
	}
	if payload.Metadata["gc.step_ref"] != "build" {
		t.Fatalf("event payload metadata = %#v, want gc.step_ref=build", payload.Metadata)
	}
}

func TestNativeDoltStoreCloseEmitsBeadClosedEvent(t *testing.T) {
	rec := events.NewFake()
	issue := &beadslib.Issue{
		ID:        "gc-native",
		Title:     "close me",
		Status:    beadslib.StatusOpen,
		IssueType: beadslib.TypeTask,
		Priority:  2,
	}
	storage := &nativeDoltStorageSpy{
		getIssue: func(_ context.Context, id string) (*beadslib.Issue, error) {
			if id != issue.ID {
				t.Fatalf("GetIssue id = %q, want %q", id, issue.ID)
			}
			return cloneNativeIssueForTest(issue), nil
		},
		closeIssue: func(_ context.Context, id, _ string, _ string, _ string) error {
			if id != issue.ID {
				t.Fatalf("CloseIssue id = %q, want %q", id, issue.ID)
			}
			issue.Status = beadslib.StatusClosed
			return nil
		},
	}
	store := newNativeDoltStoreWithStorageAndRecorder(storage, "native-test", rec)

	if err := store.Close(issue.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	event := singleNativeBeadEvent(t, rec)
	if event.Type != events.BeadClosed {
		t.Fatalf("event type = %q, want %q", event.Type, events.BeadClosed)
	}
	payload := nativeBeadEventPayload(t, event)
	if payload.ID != issue.ID || payload.Status != "closed" {
		t.Fatalf("event payload bead = %#v, want closed bead snapshot", payload)
	}
}

func TestNativeDoltStoreDeleteEmitsBeadDeletedEvent(t *testing.T) {
	rec := events.NewFake()
	issue := &beadslib.Issue{
		ID:        "gc-native",
		Title:     "delete me",
		Status:    beadslib.StatusOpen,
		IssueType: beadslib.TypeTask,
		Priority:  2,
	}
	deleted := false
	storage := &nativeDoltStorageSpy{
		getIssue: func(_ context.Context, id string) (*beadslib.Issue, error) {
			if id != issue.ID {
				t.Fatalf("GetIssue id = %q, want %q", id, issue.ID)
			}
			return cloneNativeIssueForTest(issue), nil
		},
		deleteIssue: func(_ context.Context, id string) error {
			if id != issue.ID {
				t.Fatalf("DeleteIssue id = %q, want %q", id, issue.ID)
			}
			deleted = true
			return nil
		},
	}
	store := newNativeDoltStoreWithStorageAndRecorder(storage, "native-test", rec)

	if err := store.Delete(issue.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !deleted {
		t.Fatal("DeleteIssue was not called")
	}

	event := singleNativeBeadEvent(t, rec)
	if event.Type != events.BeadDeleted {
		t.Fatalf("event type = %q, want %q", event.Type, events.BeadDeleted)
	}
	payload := nativeBeadEventPayload(t, event)
	if payload.ID != issue.ID || payload.Title != "delete me" {
		t.Fatalf("event payload bead = %#v, want deleted bead snapshot", payload)
	}
}

func TestNativeDoltStoreWriteFailuresEmitNoEvents(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*NativeDoltStore) error
	}{
		{
			name: "create",
			run: func(store *NativeDoltStore) error {
				_, err := store.Create(Bead{Title: "create fails"})
				return err
			},
		},
		{
			name: "update",
			run: func(store *NativeDoltStore) error {
				title := "update fails"
				return store.Update("gc-native", UpdateOpts{Title: &title})
			},
		},
		{
			name: "close",
			run: func(store *NativeDoltStore) error {
				return store.Close("gc-native")
			},
		},
		{
			name: "delete",
			run: func(store *NativeDoltStore) error {
				return store.Delete("gc-native")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wantErr := errors.New(tc.name + " failed")
			rec := events.NewFake()
			issue := &beadslib.Issue{
				ID:        "gc-native",
				Title:     "write fails",
				Status:    beadslib.StatusOpen,
				IssueType: beadslib.TypeTask,
				Priority:  2,
			}
			storage := &nativeDoltStorageSpy{
				createIssue: func(context.Context, *beadslib.Issue, string) error {
					return wantErr
				},
				getIssue: func(context.Context, string) (*beadslib.Issue, error) {
					return cloneNativeIssueForTest(issue), nil
				},
				updateIssue: func(context.Context, string, map[string]interface{}, string) error {
					return wantErr
				},
				closeIssue: func(context.Context, string, string, string, string) error {
					return wantErr
				},
				deleteIssue: func(context.Context, string) error {
					return wantErr
				},
			}
			store := newNativeDoltStoreWithStorageAndRecorder(storage, "native-test", rec)

			if err := tc.run(store); !errors.Is(err, wantErr) {
				t.Fatalf("%s error = %v, want %v", tc.name, err, wantErr)
			}
			if len(rec.Events) != 0 {
				t.Fatalf("recorded %d events, want 0: %#v", len(rec.Events), rec.Events)
			}
		})
	}
}

func TestNativeDoltStoreSetMetadataBatchRejectsInvalidExistingMetadata(t *testing.T) {
	updateCalled := false
	storage := &nativeDoltStorageSpy{
		getIssue: func(context.Context, string) (*beadslib.Issue, error) {
			return &beadslib.Issue{
				ID:        "gc-corrupt",
				Title:     "corrupt metadata",
				Status:    beadslib.StatusOpen,
				IssueType: beadslib.TypeTask,
				Priority:  2,
				Metadata:  json.RawMessage(`{"existing":`),
			}, nil
		},
		updateIssue: func(context.Context, string, map[string]interface{}, string) error {
			updateCalled = true
			return nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if err := store.SetMetadataBatch("gc-corrupt", map[string]string{"gc.step_ref": "build"}); err == nil {
		t.Fatal("SetMetadataBatch error = nil, want invalid metadata error")
	} else if !strings.Contains(err.Error(), `parsing metadata for bead "gc-corrupt"`) {
		t.Fatalf("SetMetadataBatch error = %v, want bead metadata context", err)
	}
	if updateCalled {
		t.Fatal("UpdateIssue was called after invalid metadata")
	}
}

type nativeDoltStorageSpy struct {
	beadslib.Storage
	createIssue  func(context.Context, *beadslib.Issue, string) error
	getIssue     func(context.Context, string) (*beadslib.Issue, error)
	updateIssue  func(context.Context, string, map[string]interface{}, string) error
	closeIssue   func(context.Context, string, string, string, string) error
	deleteIssue  func(context.Context, string) error
	searchIssues func(context.Context, string, beadslib.IssueFilter) ([]*beadslib.Issue, error)
}

func (s *nativeDoltStorageSpy) CreateIssue(ctx context.Context, issue *beadslib.Issue, actor string) error {
	return s.createIssue(ctx, issue, actor)
}

func (s *nativeDoltStorageSpy) GetIssue(ctx context.Context, id string) (*beadslib.Issue, error) {
	return s.getIssue(ctx, id)
}

func (s *nativeDoltStorageSpy) UpdateIssue(ctx context.Context, id string, updates map[string]interface{}, actor string) error {
	return s.updateIssue(ctx, id, updates, actor)
}

func (s *nativeDoltStorageSpy) CloseIssue(ctx context.Context, id string, reason string, actor string, session string) error {
	return s.closeIssue(ctx, id, reason, actor, session)
}

func (s *nativeDoltStorageSpy) DeleteIssue(ctx context.Context, id string) error {
	return s.deleteIssue(ctx, id)
}

func (s *nativeDoltStorageSpy) SearchIssues(ctx context.Context, query string, filter beadslib.IssueFilter) ([]*beadslib.Issue, error) {
	return s.searchIssues(ctx, query, filter)
}

func singleNativeBeadEvent(t *testing.T, rec *events.Fake) events.Event {
	t.Helper()
	if len(rec.Events) != 1 {
		t.Fatalf("recorded %d events, want 1: %#v", len(rec.Events), rec.Events)
	}
	return rec.Events[0]
}

func nativeBeadEventPayload(t *testing.T, event events.Event) Bead {
	t.Helper()
	var payload struct {
		Bead Bead `json:"bead"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatalf("unmarshal event payload: %v", err)
	}
	return payload.Bead
}

func cloneNativeIssueForTest(issue *beadslib.Issue) *beadslib.Issue {
	cloned := *issue
	cloned.Metadata = append(json.RawMessage(nil), issue.Metadata...)
	cloned.Labels = append([]string(nil), issue.Labels...)
	cloned.Dependencies = append([]*beadslib.Dependency(nil), issue.Dependencies...)
	return &cloned
}
