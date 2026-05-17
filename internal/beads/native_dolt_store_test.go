package beads

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

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

type nativeDoltStorageSpy struct {
	beadslib.Storage
	createIssue  func(context.Context, *beadslib.Issue, string) error
	getIssue     func(context.Context, string) (*beadslib.Issue, error)
	searchIssues func(context.Context, string, beadslib.IssueFilter) ([]*beadslib.Issue, error)
}

func (s *nativeDoltStorageSpy) CreateIssue(ctx context.Context, issue *beadslib.Issue, actor string) error {
	return s.createIssue(ctx, issue, actor)
}

func (s *nativeDoltStorageSpy) GetIssue(ctx context.Context, id string) (*beadslib.Issue, error) {
	return s.getIssue(ctx, id)
}

func (s *nativeDoltStorageSpy) SearchIssues(ctx context.Context, query string, filter beadslib.IssueFilter) ([]*beadslib.Issue, error) {
	return s.searchIssues(ctx, query, filter)
}

func cloneNativeIssueForTest(issue *beadslib.Issue) *beadslib.Issue {
	cloned := *issue
	cloned.Metadata = append(json.RawMessage(nil), issue.Metadata...)
	cloned.Labels = append([]string(nil), issue.Labels...)
	cloned.Dependencies = append([]*beadslib.Dependency(nil), issue.Dependencies...)
	return &cloned
}
