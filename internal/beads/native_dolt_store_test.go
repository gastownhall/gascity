package beads

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
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
		getIssue: func(_ context.Context, id string) (*beadslib.Issue, error) {
			return &beadslib.Issue{ID: id, Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2}, nil
		},
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

func TestNativeDoltStoreCreateGetPreservesDeferUntil(t *testing.T) {
	store := newNativeDoltStoreForTest(newNativeDoltMemStorage())
	deferUntil := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)

	created, err := store.Create(Bead{Title: "native deferred", DeferUntil: &deferUntil})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.DeferUntil == nil || !created.DeferUntil.Equal(deferUntil) {
		t.Fatalf("created.DeferUntil = %v, want %s", created.DeferUntil, deferUntil.Format(time.RFC3339))
	}

	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.DeferUntil == nil || !got.DeferUntil.Equal(deferUntil) {
		t.Fatalf("got.DeferUntil = %v, want %s", got.DeferUntil, deferUntil.Format(time.RFC3339))
	}
}

func TestNativeDoltStoreCreateGetPreservesNoHistory(t *testing.T) {
	store := newNativeDoltStoreForTest(newNativeDoltMemStorage())

	created, err := store.Create(Bead{Title: "native no history", NoHistory: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !created.NoHistory {
		t.Fatalf("created.NoHistory = false, want true")
	}
	if created.Ephemeral {
		t.Fatalf("created.Ephemeral = true, want false for no-history bead")
	}

	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.NoHistory {
		t.Fatalf("got.NoHistory = false, want true")
	}
	if got.Ephemeral {
		t.Fatalf("got.Ephemeral = true, want false for no-history bead")
	}
}

func TestNativeDoltStoreReleaseIfCurrent(t *testing.T) {
	store := newNativeDoltStoreForTest(newNativeDoltMemStorage())
	created, err := store.Create(Bead{Title: "native release", Assignee: "worker-1"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	status := "in_progress"
	if err := store.Update(created.ID, UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("Update status: %v", err)
	}

	released, err := store.ReleaseIfCurrent(created.ID, "worker-2")
	if err != nil {
		t.Fatalf("ReleaseIfCurrent wrong assignee: %v", err)
	}
	if released {
		t.Fatal("ReleaseIfCurrent released a bead with the wrong assignee")
	}
	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get after skipped release: %v", err)
	}
	if got.Status != "in_progress" || got.Assignee != "worker-1" {
		t.Fatalf("skipped release mutated bead: %+v", got)
	}

	released, err = store.ReleaseIfCurrent(created.ID, "worker-1")
	if err != nil {
		t.Fatalf("ReleaseIfCurrent matching assignee: %v", err)
	}
	if !released {
		t.Fatal("ReleaseIfCurrent did not release matching in-progress assignment")
	}
	got, err = store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get after release: %v", err)
	}
	if got.Status != "open" || got.Assignee != "" {
		t.Fatalf("released bead = %+v, want open and unassigned", got)
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
		searchIssues: func(context.Context, string, beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			return nil, wantErr
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if _, err := store.Get("gc-missing"); !errors.Is(err, wantErr) {
		t.Fatalf("Get error = %v, want %v", err, wantErr)
	}
}

func TestNativeDoltStoreConvertsDefaultPriorityAsUnset(t *testing.T) {
	bead, err := beadFromNativeIssue(&beadslib.Issue{
		ID:        "gc-unset-priority",
		Title:     "unset priority",
		Status:    beadslib.StatusOpen,
		IssueType: beadslib.TypeTask,
		Priority:  2,
	})
	if err != nil {
		t.Fatalf("beadFromNativeIssue: %v", err)
	}
	if bead.Priority != nil {
		t.Fatalf("Priority = %v, want nil for upstream default priority", *bead.Priority)
	}

	bead, err = beadFromNativeIssue(&beadslib.Issue{
		ID:        "gc-explicit-priority",
		Title:     "explicit priority",
		Status:    beadslib.StatusOpen,
		IssueType: beadslib.TypeTask,
		Priority:  1,
	})
	if err != nil {
		t.Fatalf("beadFromNativeIssue explicit: %v", err)
	}
	if bead.Priority == nil || *bead.Priority != 1 {
		t.Fatalf("Priority = %v, want explicit P1", bead.Priority)
	}
}

func TestNativeDoltStoreMapsUpstreamStatusesToGasCityContract(t *testing.T) {
	tests := []struct {
		upstream           beadslib.Status
		want               string
		wantIndefiniteHold bool
	}{
		{beadslib.StatusOpen, "open", false},
		{beadslib.StatusInProgress, "in_progress", false},
		{beadslib.StatusClosed, "closed", false},
		{beadslib.Status("blocked"), "open", false},
		{beadslib.Status("deferred"), "open", true},
		{beadslib.Status("pinned"), "open", false},
		{beadslib.Status("hooked"), "open", false},
	}

	for _, tt := range tests {
		t.Run(string(tt.upstream), func(t *testing.T) {
			bead, err := beadFromNativeIssue(&beadslib.Issue{
				ID:        "gc-status",
				Title:     "status mapping",
				Status:    tt.upstream,
				IssueType: beadslib.TypeTask,
			})
			if err != nil {
				t.Fatalf("beadFromNativeIssue: %v", err)
			}
			if bead.Status != tt.want {
				t.Fatalf("Status = %q, want %q", bead.Status, tt.want)
			}
			if bead.IndefinitelyDeferred != tt.wantIndefiniteHold {
				t.Fatalf("IndefinitelyDeferred = %v, want %v", bead.IndefinitelyDeferred, tt.wantIndefiniteHold)
			}
		})
	}
}

func TestNativeDoltStoreListStatusOpenMatchesOpenNormalizedUpstreamStatuses(t *testing.T) {
	issues := []*beadslib.Issue{
		{ID: "gc-open", Title: "open", Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2},
		{ID: "gc-blocked", Title: "blocked", Status: beadslib.StatusBlocked, IssueType: beadslib.TypeTask, Priority: 2},
		{ID: "gc-deferred", Title: "deferred", Status: beadslib.StatusDeferred, IssueType: beadslib.TypeTask, Priority: 2},
		{ID: "gc-pinned", Title: "pinned", Status: beadslib.Status("pinned"), IssueType: beadslib.TypeTask, Priority: 2},
		{ID: "gc-hooked", Title: "hooked", Status: beadslib.Status("hooked"), IssueType: beadslib.TypeTask, Priority: 2},
		{ID: "gc-review", Title: "review", Status: beadslib.Status("review"), IssueType: beadslib.TypeTask, Priority: 2},
		{ID: "gc-active", Title: "active", Status: beadslib.StatusInProgress, IssueType: beadslib.TypeTask, Priority: 2},
		{ID: "gc-closed", Title: "closed", Status: beadslib.StatusClosed, IssueType: beadslib.TypeTask, Priority: 2},
	}
	storage := &nativeDoltStorageSpy{
		searchIssues: func(_ context.Context, _ string, filter beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			return filterNativeIssuesForTest(issues, filter), nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	got, err := store.List(ListQuery{AllowScan: true, Status: "open", TierMode: TierBoth})
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	wantIDs := map[string]bool{
		"gc-open": true, "gc-blocked": true, "gc-deferred": true,
		"gc-pinned": true, "gc-hooked": true, "gc-review": true,
	}
	if len(got) != len(wantIDs) {
		t.Fatalf("List(Status: open) len = %d, want %d; got %+v", len(got), len(wantIDs), got)
	}
	for _, bead := range got {
		if !wantIDs[bead.ID] {
			t.Fatalf("List(Status: open) returned unexpected bead %q from %+v", bead.ID, got)
		}
		if bead.Status != "open" {
			t.Fatalf("List(Status: open) bead %q status = %q, want normalized open", bead.ID, bead.Status)
		}
	}
}

// TestNativeDoltStoreListStatusOpenExcludesClosedBeadsFromUpstreamDrift guards
// against Dolt status-index drift (gcy-1on) where SearchIssues returns a bead
// with status="closed" even though the ExcludeStatus filter asked to exclude it.
// ApplyListQuery must catch leaked closed beads so List(Status: "open") never
// returns them regardless of upstream inconsistency.
func TestNativeDoltStoreListStatusOpenExcludesClosedBeadsFromUpstreamDrift(t *testing.T) {
	// Spy that ignores the ExcludeStatus filter and returns a closed bead,
	// simulating Dolt status-index drift.
	storage := &nativeDoltStorageSpy{
		searchIssues: func(_ context.Context, _ string, _ beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			return []*beadslib.Issue{
				{ID: "gc-closed-drift", Title: "closed but leaking from index", Status: beadslib.StatusClosed, IssueType: beadslib.TypeTask, Priority: 2},
				{ID: "gc-open", Title: "genuinely open", Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2},
			}, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	got, err := store.List(ListQuery{AllowScan: true, Status: "open", TierMode: TierBoth})
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("List(Status: open) len = %d, want 1 (closed drift bead must be excluded); got %+v", len(got), got)
	}
	if got[0].ID != "gc-open" {
		t.Fatalf("List(Status: open) returned unexpected bead %q, want gc-open", got[0].ID)
	}
}

func TestNativeDoltStoreReadyOnlyIncludesOpenAndDeferredUpstreamStatuses(t *testing.T) {
	// bd's own status-category table (vendored beads internal/types.
	// BuiltInStatusCategory) marks blocked/hooked as "wip" and pinned as
	// "frozen" — both excluded from bd's own ready semantics. Only "open"
	// (category active) and deferred (once DeferUntil has passed, handled
	// via IsReadyCandidateForTier's IsDeferred check) belong here. This
	// issue set intentionally includes a blocked bead whose dependency
	// graph the spy treats as fully satisfied (it is returned unconditionally
	// whenever queried by status), to prove Ready() must never surface it
	// even when GetReadyWork would happily return it if asked. gc-deferred
	// carries a past DeferUntil to represent an expired time-bound deferral;
	// the no-DeferUntil (indefinite) case is covered separately by
	// TestNativeDoltStoreReadyExcludesIndefinitelyDeferredBeads.
	past := time.Now().UTC().Add(-24 * time.Hour)
	issues := []*beadslib.Issue{
		{ID: "gc-open", Title: "open", Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2},
		{ID: "gc-blocked", Title: "blocked", Status: beadslib.StatusBlocked, IssueType: beadslib.TypeTask, Priority: 2},
		{ID: "gc-deferred", Title: "deferred", Status: beadslib.StatusDeferred, IssueType: beadslib.TypeTask, Priority: 2, DeferUntil: &past},
		{ID: "gc-pinned", Title: "pinned", Status: beadslib.Status("pinned"), IssueType: beadslib.TypeTask, Priority: 2},
		{ID: "gc-hooked", Title: "hooked", Status: beadslib.Status("hooked"), IssueType: beadslib.TypeTask, Priority: 2},
		{ID: "gc-review", Title: "review", Status: beadslib.Status("review"), IssueType: beadslib.TypeTask, Priority: 2},
		{ID: "gc-active", Title: "active", Status: beadslib.StatusInProgress, IssueType: beadslib.TypeTask, Priority: 2},
		{ID: "gc-closed", Title: "closed", Status: beadslib.StatusClosed, IssueType: beadslib.TypeTask, Priority: 2},
	}
	storage := &nativeDoltStorageSpy{
		getReadyWork: func(_ context.Context, filter beadslib.WorkFilter) ([]*beadslib.Issue, error) {
			var result []*beadslib.Issue
			for _, issue := range issues {
				if !workFilterMatchesStatus(filter, issue.Status) {
					continue
				}
				result = append(result, cloneNativeIssueForTest(issue))
			}
			return result, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	got, err := store.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}

	wantIDs := map[string]bool{
		"gc-open": true, "gc-deferred": true,
	}
	if len(got) != len(wantIDs) {
		t.Fatalf("Ready len = %d, want %d; got %+v", len(got), len(wantIDs), got)
	}
	for _, bead := range got {
		if !wantIDs[bead.ID] {
			t.Fatalf("Ready returned unexpected bead %q from %+v — blocked/pinned/hooked/review must never surface as ready even when their dependency graph is satisfied", bead.ID, got)
		}
		if bead.Status != "open" {
			t.Fatalf("Ready bead %q status = %q, want normalized open", bead.ID, bead.Status)
		}
	}
}

func TestNativeDoltStoreReadyExcludesFutureDeferredBeads(t *testing.T) {
	store := newNativeDoltStoreForTest(newNativeDoltMemStorage())

	ready, err := store.Create(Bead{Title: "ready"})
	if err != nil {
		t.Fatalf("Create(ready): %v", err)
	}
	future := time.Now().UTC().Add(24 * time.Hour)
	futureDeferred, err := store.Create(Bead{Title: "future", DeferUntil: &future})
	if err != nil {
		t.Fatalf("Create(future): %v", err)
	}
	past := time.Now().UTC().Add(-24 * time.Hour)
	pastDeferred, err := store.Create(Bead{Title: "past", DeferUntil: &past})
	if err != nil {
		t.Fatalf("Create(past): %v", err)
	}

	got, err := store.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	ids := map[string]bool{}
	for _, bead := range got {
		ids[bead.ID] = true
	}
	if !ids[ready.ID] || !ids[pastDeferred.ID] {
		t.Fatalf("Ready() ids = %v, want ready and past-deferred beads", ids)
	}
	if ids[futureDeferred.ID] {
		t.Fatalf("Ready() ids = %v, future-deferred bead %s must be hidden", ids, futureDeferred.ID)
	}
}

// TestNativeDoltStoreReadyExcludesIndefinitelyDeferredBeads covers bd defer
// <id> without --until: a first-class, documented "status-based" indefinite
// deferral (upstream cmd/bd/defer.go) that sets status=deferred and leaves
// defer_until NULL, distinct from bd defer <id> --until=<time>'s time-bound
// snooze. nativeDoltOpenReadyStatuses must keep querying StatusDeferred so an
// *expired* time-bound deferral (defer_until in the past) can resurface, but
// an issue that was never time-bound (defer_until nil) must not fall through
// IsReadyCandidateForTier's nil-DeferUntil case as if it were an ordinary
// open bead that was never deferred at all.
func TestNativeDoltStoreReadyExcludesIndefinitelyDeferredBeads(t *testing.T) {
	past := time.Now().UTC().Add(-24 * time.Hour)
	issues := []*beadslib.Issue{
		{ID: "gc-open", Title: "open", Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2},
		{ID: "gc-deferred-indefinite", Title: "indefinite", Status: beadslib.StatusDeferred, IssueType: beadslib.TypeTask, Priority: 2},
		{ID: "gc-deferred-expired", Title: "expired", Status: beadslib.StatusDeferred, IssueType: beadslib.TypeTask, Priority: 2, DeferUntil: &past},
	}
	storage := &nativeDoltStorageSpy{
		getReadyWork: func(_ context.Context, filter beadslib.WorkFilter) ([]*beadslib.Issue, error) {
			var result []*beadslib.Issue
			for _, issue := range issues {
				if !workFilterMatchesStatus(filter, issue.Status) {
					continue
				}
				result = append(result, cloneNativeIssueForTest(issue))
			}
			return result, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	got, err := store.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}

	wantIDs := map[string]bool{"gc-open": true, "gc-deferred-expired": true}
	if len(got) != len(wantIDs) {
		t.Fatalf("Ready len = %d, want %d; got %+v", len(got), len(wantIDs), got)
	}
	for _, bead := range got {
		if !wantIDs[bead.ID] {
			t.Fatalf("Ready returned unexpected bead %q from %+v — an indefinitely status-deferred bead (status=deferred, defer_until=NULL) must never surface as ready", bead.ID, got)
		}
	}
}

func TestNativeDoltStoreNormalizesUpstreamNotFoundErrors(t *testing.T) {
	upstreamNotFound := errors.New("not found")
	storage := &nativeDoltStorageSpy{
		getIssue: func(context.Context, string) (*beadslib.Issue, error) {
			return nil, fmt.Errorf("get issue: %w", upstreamNotFound)
		},
		updateIssue: func(context.Context, string, map[string]interface{}, string) error {
			return fmt.Errorf("update issue: %w", upstreamNotFound)
		},
		closeIssue: func(context.Context, string, string, string, string) error {
			return fmt.Errorf("close issue: %w", upstreamNotFound)
		},
		reopenIssue: func(context.Context, string, string, string) error {
			return fmt.Errorf("reopen issue: %w", upstreamNotFound)
		},
		deleteIssue: func(context.Context, string) error {
			return fmt.Errorf("delete issue: %w", upstreamNotFound)
		},
		addLabel: func(context.Context, string, string, string) error {
			return fmt.Errorf("add label: %w", upstreamNotFound)
		},
		addDependency: func(context.Context, *beadslib.Dependency, string) error {
			return fmt.Errorf("add dependency: %w", upstreamNotFound)
		},
		removeDependency: func(context.Context, string, string, string) error {
			return fmt.Errorf("remove dependency: %w", upstreamNotFound)
		},
		getDependenciesWithMetadata: func(context.Context, string) ([]*beadslib.IssueWithDependencyMetadata, error) {
			return nil, fmt.Errorf("deps down: %w", upstreamNotFound)
		},
		getDependentsWithMetadata: func(context.Context, string) ([]*beadslib.IssueWithDependencyMetadata, error) {
			return nil, fmt.Errorf("deps up: %w", upstreamNotFound)
		},
	}
	store := newNativeDoltStoreForTest(storage)
	title := "changed"

	checks := []struct {
		name string
		call func() error
	}{
		{name: "Get", call: func() error {
			_, err := store.Get("gc-missing")
			return err
		}},
		{name: "Update", call: func() error {
			return store.Update("gc-missing", UpdateOpts{Title: &title})
		}},
		{name: "Close", call: func() error {
			return store.Close("gc-missing")
		}},
		{name: "Reopen", call: func() error {
			return store.Reopen("gc-missing")
		}},
		{name: "SetMetadataBatch", call: func() error {
			return store.SetMetadataBatch("gc-missing", map[string]string{"k": "v"})
		}},
		{name: "Delete", call: func() error {
			return store.Delete("gc-missing")
		}},
		{name: "DepAdd", call: func() error {
			return store.DepAdd("gc-missing", "gc-target", "blocks")
		}},
		{name: "DepRemove", call: func() error {
			return store.DepRemove("gc-missing", "gc-target")
		}},
		{name: "DepListDown", call: func() error {
			_, err := store.DepList("gc-missing", "down")
			return err
		}},
		{name: "DepListUp", call: func() error {
			_, err := store.DepList("gc-missing", "up")
			return err
		}},
	}
	for _, tc := range checks {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, ErrNotFound) {
				t.Fatalf("%s error = %v, want ErrNotFound", tc.name, err)
			}
		})
	}

	if err := nativeStoreError("gc-schema", errors.New("label not found in schema: open")); errors.Is(err, ErrNotFound) {
		t.Fatalf("non-missing upstream error was normalized to ErrNotFound: %v", err)
	}
}

func TestNativeDoltStoreCloseForwardsMetadataCloseReason(t *testing.T) {
	const wantReason = "convoy autoclose: all children closed"
	var gotReason string
	storage := &nativeDoltStorageSpy{
		getIssue: func(context.Context, string) (*beadslib.Issue, error) {
			raw, err := metadataRawFromMap(map[string]string{
				"close_reason": "  " + wantReason + "  \n",
			})
			if err != nil {
				t.Fatalf("metadataRawFromMap: %v", err)
			}
			return &beadslib.Issue{
				ID:        "gc-close",
				Title:     "close me",
				Status:    beadslib.StatusOpen,
				IssueType: beadslib.TypeTask,
				Priority:  2,
				Metadata:  raw,
			}, nil
		},
		closeIssue: func(_ context.Context, _ string, reason string, _ string, _ string) error {
			gotReason = reason
			return nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if err := store.Close("gc-close"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if gotReason != wantReason {
		t.Fatalf("Close reason = %q, want %q", gotReason, wantReason)
	}
}

func TestNativeDoltStoreCloseTreatsMalformedMetadataAsEmptyReason(t *testing.T) {
	closeCalled := false
	var gotReason string
	storage := &nativeDoltStorageSpy{
		getIssue: func(context.Context, string) (*beadslib.Issue, error) {
			return &beadslib.Issue{
				ID:        "gc-close",
				Title:     "close me",
				Status:    beadslib.StatusOpen,
				IssueType: beadslib.TypeTask,
				Priority:  2,
				Metadata:  json.RawMessage(`{"close_reason":`),
			}, nil
		},
		closeIssue: func(_ context.Context, _ string, reason string, _ string, _ string) error {
			closeCalled = true
			gotReason = reason
			return nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if err := store.Close("gc-close"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !closeCalled {
		t.Fatal("CloseIssue was not called")
	}
	if gotReason != "" {
		t.Fatalf("Close reason = %q, want empty reason for malformed metadata", gotReason)
	}
}

func TestNativeDoltStoreCloseAllForwardsMetadataCloseReason(t *testing.T) {
	const wantReason = "order-tracking sweep: stale beyond watchdog window"
	storage := &nativeDoltCloseCapturingStorage{nativeDoltMemStorage: newNativeDoltMemStorage()}
	store := newNativeDoltStoreForTest(storage)
	first, err := store.Create(Bead{Title: "first"})
	if err != nil {
		t.Fatalf("Create first: %v", err)
	}
	second, err := store.Create(Bead{Title: "second"})
	if err != nil {
		t.Fatalf("Create second: %v", err)
	}

	closed, err := store.CloseAll([]string{first.ID, second.ID}, map[string]string{
		"close_reason": "  " + wantReason + "  \n",
	})
	if err != nil {
		t.Fatalf("CloseAll: %v", err)
	}
	if closed != 2 {
		t.Fatalf("closed = %d, want 2", closed)
	}
	if got := fmt.Sprint(storage.closeReasons); got != fmt.Sprint([]string{wantReason, wantReason}) {
		t.Fatalf("close reasons = %v, want %q for each close", storage.closeReasons, wantReason)
	}
}

func TestNativeDoltStoreTranslationTableNormalizesNotFoundText(t *testing.T) {
	for _, msg := range []string{
		"not found",
		"not found: issue gc-missing",
		"issue gc-missing: not found",
		"issue not found: gc-missing",
		"issue gc-missing not found",
		"no rows in result set",
		"sql: no rows in result set",
	} {
		t.Run(msg, func(t *testing.T) {
			if err := nativeStoreError("gc-missing", errors.New(msg)); !errors.Is(err, ErrNotFound) {
				t.Fatalf("nativeStoreError(%q) = %v, want ErrNotFound", msg, err)
			}
		})
	}
}

func TestNativeDoltStoreNormalizesRealUpstreamMissingIssueErrors(t *testing.T) {
	ctx := context.Background()
	storage, err := beadslib.Open(ctx, filepath.Join(t.TempDir(), ".beads"))
	if err != nil {
		t.Skipf("upstream beads storage unavailable: %v", err)
	}
	t.Cleanup(func() {
		if err := storage.Close(); err != nil {
			t.Fatalf("close upstream storage: %v", err)
		}
	})
	store := newNativeDoltStoreWithStorageAndPrefix(storage, "native-test", "gc")
	title := "changed"

	checks := []struct {
		name string
		call func() error
	}{
		{name: "Get", call: func() error {
			_, err := store.Get("gc-missing")
			return err
		}},
		{name: "Close", call: func() error {
			return store.Close("gc-missing")
		}},
		{name: "Update", call: func() error {
			return store.Update("gc-missing", UpdateOpts{Title: &title})
		}},
		{name: "SetMetadataBatch", call: func() error {
			return store.SetMetadataBatch("gc-missing", map[string]string{"k": "v"})
		}},
		{name: "DepAdd", call: func() error {
			return store.DepAdd("gc-missing", "gc-target", "blocks")
		}},
	}
	for _, tc := range checks {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, ErrNotFound) {
				t.Fatalf("%s error = %v, want ErrNotFound", tc.name, err)
			}
		})
	}
}

func TestNativeDoltStoreCloseStoreWaitsForInFlightOperation(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	closed := make(chan struct{})
	storage := &nativeDoltStorageSpy{
		getIssue: func(context.Context, string) (*beadslib.Issue, error) {
			close(entered)
			<-release
			return &beadslib.Issue{ID: "gc-open", Status: beadslib.StatusOpen}, nil
		},
		closeIssue: func(context.Context, string, string, string, string) error {
			return nil
		},
		close: func() error {
			close(closed)
			return nil
		},
	}
	store := newNativeDoltStoreForTest(storage)
	closeErr := make(chan error, 1)
	opErr := make(chan error, 1)

	go func() {
		opErr <- store.Close("gc-open")
	}()
	<-entered
	go func() {
		closeErr <- store.CloseStore()
	}()

	select {
	case <-closed:
		t.Fatal("CloseStore closed the backing while an operation still held it")
	case <-time.After(20 * time.Millisecond):
	}

	close(release)
	if err := <-opErr; err != nil {
		t.Fatalf("in-flight Close: %v", err)
	}
	if err := <-closeErr; err != nil {
		t.Fatalf("CloseStore: %v", err)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("CloseStore did not close the backing after the operation finished")
	}
	if _, err := store.Get("gc-open"); !errors.Is(err, ErrStoreClosed) {
		t.Fatalf("Get after CloseStore = %v, want ErrStoreClosed", err)
	}
}

func TestNativeDoltStoreReopenAlreadyOpenSkipsUpstreamCall(t *testing.T) {
	var reopened bool
	storage := &nativeDoltStorageSpy{
		getIssue: func(context.Context, string) (*beadslib.Issue, error) {
			return &beadslib.Issue{ID: "gc-open", Status: beadslib.StatusOpen}, nil
		},
		reopenIssue: func(context.Context, string, string, string) error {
			reopened = true
			return errors.New("unexpected reopen")
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if err := store.Reopen("gc-open"); err != nil {
		t.Fatalf("Reopen(open): %v", err)
	}
	if reopened {
		t.Fatal("Reopen(open) called upstream ReopenIssue")
	}
}

func TestNativeDoltStoreGetRejectsInvalidMetadata(t *testing.T) {
	storage := &nativeDoltStorageSpy{
		searchIssues: func(context.Context, string, beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			return []*beadslib.Issue{{
				ID:        "gc-corrupt",
				Title:     "corrupt metadata",
				Status:    beadslib.StatusOpen,
				IssueType: beadslib.TypeTask,
				Priority:  2,
				Metadata:  json.RawMessage(`{"gc.step_ref":`),
			}}, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if _, err := store.Get("gc-corrupt"); err == nil {
		t.Fatal("Get error = nil, want invalid metadata error")
	} else if !errors.Is(err, ErrMetadataParse) {
		t.Fatalf("Get error = %v, want ErrMetadataParse", err)
	} else if !strings.Contains(err.Error(), `parsing metadata for bead "gc-corrupt"`) {
		t.Fatalf("Get error = %v, want bead metadata context", err)
	}
}

func TestNativeDoltStorePreservesNonStringMetadataAsJSONText(t *testing.T) {
	storage := &nativeDoltStorageSpy{
		searchIssues: func(context.Context, string, beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			return []*beadslib.Issue{{
				ID:        "gc-metadata",
				Title:     "metadata",
				Status:    beadslib.StatusOpen,
				IssueType: beadslib.TypeTask,
				Priority:  2,
				Metadata:  json.RawMessage(`{"count":42,"flag":true,"nested":{"k":"v"},"none":null}`),
			}}, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	got, err := store.Get("gc-metadata")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	want := map[string]string{
		"count":  "42",
		"flag":   "true",
		"nested": `{"k":"v"}`,
		"none":   "null",
	}
	for key, value := range want {
		if got.Metadata[key] != value {
			t.Fatalf("Metadata[%q] = %q, want %q; all metadata=%#v", key, got.Metadata[key], value, got.Metadata)
		}
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

func TestNativeDoltStoreListSkipsInvalidMetadataRows(t *testing.T) {
	corrupt := &beadslib.Issue{
		ID:        "gc-corrupt",
		Title:     "corrupt metadata",
		Status:    beadslib.StatusOpen,
		IssueType: beadslib.IssueType("convoy"),
		Priority:  2,
		Metadata:  json.RawMessage(`metadata is not json`),
	}
	storage := &nativeDoltStorageSpy{
		searchIssues: func(_ context.Context, query string, _ beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			if query == "gc-corrupt" {
				return []*beadslib.Issue{corrupt}, nil
			}
			return []*beadslib.Issue{
				corrupt,
				{
					ID:        "gc-listed",
					Title:     "valid convoy",
					Status:    beadslib.StatusOpen,
					IssueType: beadslib.IssueType("convoy"),
					Priority:  2,
					Metadata:  json.RawMessage(`{"gc.step_ref":"list"}`),
				},
			}, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	got, err := store.List(ListQuery{AllowScan: true, Type: "convoy"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("List len = %d, want only valid rows: %#v", len(got), got)
	}
	if got[0].ID != "gc-listed" {
		t.Fatalf("List[0].ID = %q, want gc-listed", got[0].ID)
	}
	if _, err := store.Get("gc-corrupt"); err == nil {
		t.Fatal("Get error = nil, want invalid metadata error")
	} else if !strings.Contains(err.Error(), `parsing metadata for bead "gc-corrupt"`) {
		t.Fatalf("Get error = %v, want bead metadata context", err)
	}
}

// A limit may reach the backing search ONLY together with a pushed-down sort
// (IssueFilter.SortBy) — a bare limit on an unsorted query truncates an
// arbitrary subset. Created-order sorts push down (sr-dp9o: keeping the limit
// stops the dispatcher's history read from materializing the whole retained
// corpus); the backing then pages in the requested order and the local
// ApplyListQuery re-sort is a stable no-op over the returned page.
func TestNativeDoltStoreListPushesLimitOnlyWithSort(t *testing.T) {
	createdAt := time.Date(2026, 5, 17, 11, 0, 0, 0, time.UTC)
	newIssue := &beadslib.Issue{ID: "gc-new", Title: "new", Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2, CreatedAt: createdAt.Add(2 * time.Minute)}
	oldIssue := &beadslib.Issue{ID: "gc-old", Title: "old", Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2, CreatedAt: createdAt}
	midIssue := &beadslib.Issue{ID: "gc-mid", Title: "mid", Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2, CreatedAt: createdAt.Add(time.Minute)}
	storage := &nativeDoltStorageSpy{
		searchIssues: func(_ context.Context, _ string, filter beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			if filter.Limit != 0 && filter.SortBy == "" {
				t.Fatalf("upstream list limit = %d pushed without a sort; a bare limit truncates an arbitrary subset", filter.Limit)
			}
			switch {
			case filter.SortBy == "created" && !filter.SortDesc:
				return []*beadslib.Issue{cloneNativeIssueForTest(newIssue), cloneNativeIssueForTest(midIssue), cloneNativeIssueForTest(oldIssue)}, nil
			case filter.SortBy == "created" && filter.SortDesc:
				return []*beadslib.Issue{cloneNativeIssueForTest(oldIssue), cloneNativeIssueForTest(midIssue), cloneNativeIssueForTest(newIssue)}, nil
			}
			return []*beadslib.Issue{cloneNativeIssueForTest(newIssue), cloneNativeIssueForTest(oldIssue), cloneNativeIssueForTest(midIssue)}, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	asc, err := store.List(ListQuery{AllowScan: true, Sort: SortCreatedAsc, Limit: 1})
	if err != nil {
		t.Fatalf("List asc: %v", err)
	}
	if len(asc) != 1 || asc[0].ID != "gc-old" {
		t.Fatalf("List asc = %+v, want oldest bead", asc)
	}

	desc, err := store.List(ListQuery{AllowScan: true, Sort: SortCreatedDesc, Limit: 1})
	if err != nil {
		t.Fatalf("List desc: %v", err)
	}
	if len(desc) != 1 || desc[0].ID != "gc-new" {
		t.Fatalf("List desc = %+v, want newest bead", desc)
	}
}

func TestNativeDoltStoreListTierWispsIncludesNoHistoryAndEphemeralRows(t *testing.T) {
	issues, err := nativeIssuesFromBeads([]Bead{
		{ID: "gc-history", Title: "history-backed row"},
		{ID: "gc-no-history", Title: "no-history wisp", NoHistory: true},
		{ID: "gc-ephemeral", Title: "ephemeral wisp", Ephemeral: true},
	})
	if err != nil {
		t.Fatalf("nativeIssuesFromBeads: %v", err)
	}
	storage := &nativeDoltStorageSpy{
		searchIssues: func(_ context.Context, _ string, filter beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			return filterNativeIssuesForTest(issues, filter), nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	got, err := store.List(ListQuery{AllowScan: true, TierMode: TierWisps, Limit: 2})
	if err != nil {
		t.Fatalf("List(TierWisps): %v", err)
	}
	if len(got) != 2 || got[0].ID != "gc-no-history" || got[1].ID != "gc-ephemeral" {
		t.Fatalf("List(TierWisps) = %+v, want no-history and ephemeral rows", got)
	}
	if !got[0].NoHistory || got[0].Ephemeral {
		t.Fatalf("first row storage = ephemeral:%v no_history:%v, want no-history", got[0].Ephemeral, got[0].NoHistory)
	}
	if !got[1].Ephemeral || got[1].NoHistory {
		t.Fatalf("second row storage = ephemeral:%v no_history:%v, want ephemeral", got[1].Ephemeral, got[1].NoHistory)
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

func TestNativeDoltStoreSetMetadataBatchRetriesSerializationConflictFromFreshState(t *testing.T) {
	getCalls := 0
	updateCalls := 0
	var writtenMetadata json.RawMessage
	storage := &nativeDoltStorageSpy{
		getIssue: func(context.Context, string) (*beadslib.Issue, error) {
			getCalls++
			metadata := json.RawMessage(`{"existing":"before-conflict"}`)
			if getCalls > 1 {
				metadata = json.RawMessage(`{"concurrent":"preserved"}`)
			}
			return &beadslib.Issue{
				ID:        "gc-conflict",
				Title:     "metadata conflict",
				Status:    beadslib.StatusOpen,
				IssueType: beadslib.TypeTask,
				Priority:  2,
				Metadata:  metadata,
			}, nil
		},
		updateIssue: func(_ context.Context, _ string, updates map[string]interface{}, _ string) error {
			updateCalls++
			if updateCalls == 1 {
				return errors.New("dolt commit: Error 1213 (40001): serialization failure: this transaction conflicts with a committed transaction, try restarting transaction")
			}
			raw, ok := updates["metadata"].(json.RawMessage)
			if !ok {
				t.Fatalf("metadata update type = %T, want json.RawMessage", updates["metadata"])
			}
			writtenMetadata = slices.Clone(raw)
			return nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if err := store.SetMetadataBatch("gc-conflict", map[string]string{"requested": "written"}); err != nil {
		t.Fatalf("SetMetadataBatch: %v", err)
	}
	if getCalls != 2 {
		t.Fatalf("GetIssue calls = %d, want 2 so retry re-reads current metadata", getCalls)
	}
	if updateCalls != 2 {
		t.Fatalf("UpdateIssue calls = %d, want 2", updateCalls)
	}
	var got map[string]string
	if err := json.Unmarshal(writtenMetadata, &got); err != nil {
		t.Fatalf("unmarshal written metadata: %v", err)
	}
	want := map[string]string{"concurrent": "preserved", "requested": "written"}
	if !maps.Equal(got, want) {
		t.Fatalf("written metadata = %#v, want %#v", got, want)
	}
}

func TestNativeDoltStoreSetMetadataBatchDoesNotRetryPermanentWriteError(t *testing.T) {
	wantErr := errors.New("metadata write denied")
	getCalls := 0
	updateCalls := 0
	storage := &nativeDoltStorageSpy{
		getIssue: func(context.Context, string) (*beadslib.Issue, error) {
			getCalls++
			return &beadslib.Issue{ID: "gc-permanent", Metadata: json.RawMessage(`{"existing":"kept"}`)}, nil
		},
		updateIssue: func(context.Context, string, map[string]interface{}, string) error {
			updateCalls++
			return wantErr
		},
	}
	store := newNativeDoltStoreForTest(storage)

	err := store.SetMetadataBatch("gc-permanent", map[string]string{"requested": "written"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("SetMetadataBatch error = %v, want %v", err, wantErr)
	}
	if getCalls != 1 || updateCalls != 1 {
		t.Fatalf("calls = GetIssue:%d UpdateIssue:%d, want 1 each", getCalls, updateCalls)
	}
}

func TestNativeDoltStoreSetMetadataBatchStopsAfterThreeSerializationConflicts(t *testing.T) {
	wantErr := errors.New("commit failed (SQLSTATE 40001): serialization failure")
	getCalls := 0
	updateCalls := 0
	storage := &nativeDoltStorageSpy{
		getIssue: func(context.Context, string) (*beadslib.Issue, error) {
			getCalls++
			return &beadslib.Issue{ID: "gc-persistent-conflict"}, nil
		},
		updateIssue: func(context.Context, string, map[string]interface{}, string) error {
			updateCalls++
			return wantErr
		},
	}
	store := newNativeDoltStoreForTest(storage)

	err := store.SetMetadataBatch("gc-persistent-conflict", map[string]string{"requested": "written"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("SetMetadataBatch error = %v, want %v", err, wantErr)
	}
	if getCalls != 3 || updateCalls != 3 {
		t.Fatalf("calls = GetIssue:%d UpdateIssue:%d, want 3 each", getCalls, updateCalls)
	}
}

func TestNativeDoltSerializationConflictClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "mysql error and SQLSTATE", err: errors.New("Error 1213 (40001): serialization failure"), want: true},
		{name: "mysql lock wait timeout", err: errors.New("Error 1205 (HY000): lock wait timeout exceeded"), want: true},
		{name: "SQLSTATE", err: errors.New("commit failed (SQLSTATE 40001)"), want: true},
		{name: "Dolt conflict wording", err: errors.New("this transaction conflicts with a committed transaction"), want: true},
		{name: "unrelated serialization wording", err: errors.New("serialization failed while encoding metadata"), want: false},
		{name: "permanent error", err: errors.New("permission denied"), want: false},
		{name: "nil", err: nil, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isNativeDoltSerializationConflict(tt.err); got != tt.want {
				t.Fatalf("isNativeDoltSerializationConflict(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestNativeDoltStoreCloseWithMetadataIfMatchRetriesWholeTransaction(t *testing.T) {
	for _, conflict := range []error{
		errors.New("Error 1213 (40001): deadlock"),
		errors.New("Error 1205 (HY000): lock wait timeout exceeded"),
	} {
		t.Run(conflict.Error(), func(t *testing.T) {
			storage := &retryingNativeDoltStorage{
				nativeDoltMemStorage: newNativeDoltMemStorage(),
				txErrors:             []error{conflict},
			}
			store := newNativeDoltStoreForTest(storage)
			created, err := store.Create(Bead{Title: "retry whole transaction"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}

			closed, err := store.CloseWithMetadataIfMatch(created.ID, created.Revision, map[string]string{"state": "drained"})
			if err != nil {
				t.Fatalf("CloseWithMetadataIfMatch: %v", err)
			}
			if storage.txCalls != 2 {
				t.Fatalf("RunInTransaction calls = %d, want 2", storage.txCalls)
			}
			if closed.Status != "closed" || closed.Metadata["state"] != "drained" {
				t.Fatalf("returned bead = %#v, want closed row from replay", closed)
			}
		})
	}
}

func TestNativeDoltStoreCloseWithMetadataIfMatchRetryRereadsFence(t *testing.T) {
	storage := &retryingNativeDoltStorage{nativeDoltMemStorage: newNativeDoltMemStorage()}
	store := newNativeDoltStoreForTest(storage)
	created, err := store.Create(Bead{Title: "retry stale fence"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	storage.txErrors = []error{errors.New("Error 1213 (40001): deadlock")}
	storage.afterConflict = func() {
		if err := storage.store.SetMetadata(created.ID, "intervening", "write"); err != nil {
			t.Fatalf("intervening write: %v", err)
		}
	}

	closed, err := store.CloseWithMetadataIfMatch(created.ID, created.Revision, map[string]string{"state": "drained"})
	if !IsPreconditionFailed(err) {
		t.Fatalf("CloseWithMetadataIfMatch error = %v, want precondition failure", err)
	}
	if !reflect.DeepEqual(closed, Bead{}) {
		t.Fatalf("failed replay returned %#v, want zero bead", closed)
	}
	if storage.txCalls != 2 {
		t.Fatalf("RunInTransaction calls = %d, want 2", storage.txCalls)
	}
	fresh, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if fresh.Status != "open" || fresh.Metadata["state"] != "" || fresh.Metadata["intervening"] != "write" {
		t.Fatalf("replay fence result = %#v, want later open row without close", fresh)
	}
}

func TestNativeDoltStoreCloseWithMetadataIfMatchDoesNotRetryAmbiguousFailure(t *testing.T) {
	sentinel := errors.New("connection reset by peer")
	storage := &retryingNativeDoltStorage{
		nativeDoltMemStorage: newNativeDoltMemStorage(),
		txErrors:             []error{sentinel},
	}
	store := newNativeDoltStoreForTest(storage)
	created, err := store.Create(Bead{Title: "ambiguous close"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	closed, err := store.CloseWithMetadataIfMatch(created.ID, created.Revision, nil)
	if !errors.Is(err, sentinel) {
		t.Fatalf("CloseWithMetadataIfMatch error = %v, want %v", err, sentinel)
	}
	if !reflect.DeepEqual(closed, Bead{}) {
		t.Fatalf("ambiguous failure returned %#v, want zero bead", closed)
	}
	if storage.txCalls != 1 {
		t.Fatalf("RunInTransaction calls = %d, want 1", storage.txCalls)
	}
}

func TestNativeDoltStoreCloseWithMetadataIfMatchReturnsZeroAfterRetryExhaustion(t *testing.T) {
	conflict := errors.New("Error 1213 (40001): deadlock")
	storage := &retryingNativeDoltStorage{
		nativeDoltMemStorage: newNativeDoltMemStorage(),
		txErrors:             []error{conflict, conflict, conflict},
	}
	store := newNativeDoltStoreForTest(storage)
	created, err := store.Create(Bead{Title: "exhaust close retries"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	closed, err := store.CloseWithMetadataIfMatch(created.ID, created.Revision, nil)
	if !errors.Is(err, conflict) {
		t.Fatalf("CloseWithMetadataIfMatch error = %v, want %v", err, conflict)
	}
	if !reflect.DeepEqual(closed, Bead{}) {
		t.Fatalf("exhausted retry returned %#v, want zero bead", closed)
	}
	if storage.txCalls != nativeWriteAttempts {
		t.Fatalf("RunInTransaction calls = %d, want %d", storage.txCalls, nativeWriteAttempts)
	}
}

// DeleteIfMatch runs its fence check and delete inside one transaction, so a
// serialization conflict must replay the WHOLE transaction — re-reading the row
// version each attempt — exactly like the close path. Retrying only the delete
// would fence against a RowVersion the rolled-back read already invalidated.
func TestNativeDoltStoreDeleteIfMatchRetriesWholeTransaction(t *testing.T) {
	for _, conflict := range []error{
		errors.New("Error 1213 (40001): deadlock"),
		errors.New("Error 1205 (HY000): lock wait timeout exceeded"),
	} {
		t.Run(conflict.Error(), func(t *testing.T) {
			storage := &retryingNativeDoltStorage{
				nativeDoltMemStorage: newNativeDoltMemStorage(),
				txErrors:             []error{conflict},
			}
			store := newNativeDoltStoreForTest(storage)
			created, err := store.Create(Bead{Title: "retry whole delete transaction"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}

			if err := store.DeleteIfMatch(created.ID, created.Revision); err != nil {
				t.Fatalf("DeleteIfMatch: %v", err)
			}
			if storage.txCalls != 2 {
				t.Fatalf("RunInTransaction calls = %d, want 2", storage.txCalls)
			}
			if _, err := store.Get(created.ID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("Get after replayed delete = %v, want ErrNotFound", err)
			}
		})
	}
}

// An ambiguous failure — one that may have committed — must NOT replay a delete,
// or a delete that already applied would run again against a moved fence. This
// pins the same transient/ambiguous split the close path draws.
func TestNativeDoltStoreDeleteIfMatchDoesNotRetryAmbiguousFailure(t *testing.T) {
	sentinel := errors.New("connection reset by peer")
	storage := &retryingNativeDoltStorage{
		nativeDoltMemStorage: newNativeDoltMemStorage(),
		txErrors:             []error{sentinel},
	}
	store := newNativeDoltStoreForTest(storage)
	created, err := store.Create(Bead{Title: "ambiguous delete"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := store.DeleteIfMatch(created.ID, created.Revision); !errors.Is(err, sentinel) {
		t.Fatalf("DeleteIfMatch error = %v, want %v", err, sentinel)
	}
	if storage.txCalls != 1 {
		t.Fatalf("RunInTransaction calls = %d, want 1", storage.txCalls)
	}
}

func TestNativeDoltStoreReadyFiltersGasCityExcludedTypesBeforeLimit(t *testing.T) {
	storage := &nativeDoltStorageSpy{
		getReadyWork: func(_ context.Context, filter beadslib.WorkFilter) ([]*beadslib.Issue, error) {
			if filter.Limit != 0 {
				t.Fatalf("upstream ready limit = %d, want 0 so Gas City filters before limiting", filter.Limit)
			}
			return []*beadslib.Issue{
				{ID: "gc-step", Title: "step", Status: beadslib.StatusOpen, IssueType: "step", Priority: 2},
				{ID: "gc-session", Title: "session", Status: beadslib.StatusOpen, IssueType: "session", Priority: 2},
				{ID: "gc-task-1", Title: "task 1", Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2},
				{ID: "gc-task-2", Title: "task 2", Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2},
			}, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	got, err := store.Ready(ReadyQuery{Limit: 1})
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if len(got) != 1 || got[0].ID != "gc-task-1" {
		t.Fatalf("Ready(limit=1) = %+v, want only gc-task-1 after filtering infra types", got)
	}
}

func TestNativeDoltStoreTxAppliesCallbackWrites(t *testing.T) {
	store := newNativeDoltStoreForTest(newNativeDoltMemStorage())
	created, err := store.Create(Bead{
		Title:    "native tx",
		Metadata: map[string]string{"initial": "true"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	title := "native tx updated"

	if err := store.Tx("native tx test", func(tx Tx) error {
		if err := tx.Update(created.ID, UpdateOpts{
			Title:    &title,
			Metadata: map[string]string{"phase": "updated"},
		}); err != nil {
			return err
		}
		if err := tx.SetMetadataBatch(created.ID, map[string]string{"step": "done"}); err != nil {
			return err
		}
		return tx.Close(created.ID)
	}); err != nil {
		t.Fatalf("Tx: %v", err)
	}

	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != title {
		t.Fatalf("Title = %q, want %q", got.Title, title)
	}
	if got.Status != "closed" {
		t.Fatalf("Status = %q, want closed", got.Status)
	}
	if got.Metadata["initial"] != "true" || got.Metadata["phase"] != "updated" || got.Metadata["step"] != "done" {
		t.Fatalf("Metadata = %#v, want merged tx metadata", got.Metadata)
	}
}

// commitCountingMemStorage counts how many RunInTransaction (i.e. DOLT_COMMIT)
// boundaries a sequence of store writes crosses.
type commitCountingMemStorage struct {
	*nativeDoltMemStorage
	commits int
}

func (s *commitCountingMemStorage) RunInTransaction(ctx context.Context, msg string, fn func(beadslib.Transaction) error) error {
	s.commits++
	return s.nativeDoltMemStorage.RunInTransaction(ctx, msg, fn)
}

func TestNativeDoltStoreTxCoalescesWritesIntoSingleCommit(t *testing.T) {
	counting := &commitCountingMemStorage{nativeDoltMemStorage: newNativeDoltMemStorage()}
	store := newNativeDoltStoreForTest(counting)

	seed, err := store.Create(Bead{Title: "seed", Metadata: map[string]string{"k": "v"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	commitsBeforeTx := counting.commits
	var membershipID string
	if err := store.Tx("coalesced bind", func(tx Tx) error {
		created, err := tx.Create(Bead{Title: "membership", Metadata: map[string]string{"role": "member"}})
		if err != nil {
			return err
		}
		membershipID = created.ID
		if err := tx.SetMetadataBatch(seed.ID, map[string]string{"touched": "yes"}); err != nil {
			return err
		}
		if err := tx.Update(seed.ID, UpdateOpts{Metadata: map[string]string{"phase": "bound"}}); err != nil {
			return err
		}
		return tx.Close(membershipID)
	}); err != nil {
		t.Fatalf("Tx: %v", err)
	}

	if got := counting.commits - commitsBeforeTx; got != 1 {
		t.Fatalf("Tx with 4 writes issued %d commits, want exactly 1", got)
	}

	gotSeed, err := store.Get(seed.ID)
	if err != nil {
		t.Fatalf("Get seed: %v", err)
	}
	if gotSeed.Metadata["touched"] != "yes" || gotSeed.Metadata["phase"] != "bound" || gotSeed.Metadata["k"] != "v" {
		t.Fatalf("seed metadata = %#v, want merged tx writes", gotSeed.Metadata)
	}
	gotMembership, err := store.Get(membershipID)
	if err != nil {
		t.Fatalf("Get membership: %v", err)
	}
	if gotMembership.Status != "closed" {
		t.Fatalf("membership status = %q, want closed", gotMembership.Status)
	}
	if gotMembership.Metadata["role"] != "member" {
		t.Fatalf("membership metadata = %#v, want created-in-tx fields", gotMembership.Metadata)
	}
}

// TestNativeDoltStoreTxRollsBackOnError verifies the coalesced transaction is
// atomic: a failure mid-callback leaves none of the writes committed.
func TestNativeDoltStoreTxRollsBackOnError(t *testing.T) {
	store := newNativeDoltStoreForTest(newNativeDoltMemStorage())
	seed, err := store.Create(Bead{Title: "seed", Metadata: map[string]string{"phase": "initial"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	sentinel := errors.New("boom")
	if err := store.Tx("rollback", func(tx Tx) error {
		if err := tx.SetMetadataBatch(seed.ID, map[string]string{"phase": "mutated"}); err != nil {
			return err
		}
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatalf("Tx error = %v, want sentinel", err)
	}

	got, err := store.Get(seed.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata["phase"] != "initial" {
		t.Fatalf("phase = %q, want initial (rolled back)", got.Metadata["phase"])
	}
}

func TestNativeDoltStoreCloseWithMetadataIfMatchCommitsOneFencedTerminalState(t *testing.T) {
	storage := &commitCountingMemStorage{nativeDoltMemStorage: newNativeDoltMemStorage()}
	store := newNativeDoltStoreForTest(storage)
	created, err := store.Create(Bead{
		Title:    "atomic terminal state",
		Metadata: map[string]string{"sibling": "preserved"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	commitsBeforeClose := storage.commits
	closed, err := store.CloseWithMetadataIfMatch(created.ID, created.Revision, map[string]string{
		"state":        "drained",
		"close_reason": "reconciler stop",
	})
	if err != nil {
		t.Fatalf("CloseWithMetadataIfMatch: %v", err)
	}
	if closed.Status != "closed" || closed.Metadata["state"] != "drained" || closed.Metadata["sibling"] != "preserved" {
		t.Fatalf("returned closed bead = %#v, want exact merged terminal row", closed)
	}
	if got := storage.commits - commitsBeforeClose; got != 1 {
		t.Fatalf("atomic metadata close issued %d commits, want exactly one", got)
	}

	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("status = %q, want closed", got.Status)
	}
	for key, want := range map[string]string{
		"sibling":      "preserved",
		"state":        "drained",
		"close_reason": "reconciler stop",
	} {
		if got.Metadata[key] != want {
			t.Fatalf("metadata[%q] = %q, want %q (metadata and close must share one commit)", key, got.Metadata[key], want)
		}
	}
}

func TestNativeDoltStoreCloseWithMetadataIfMatchRejectsStaleRevisionWithoutMutation(t *testing.T) {
	storage := newNativeDoltMemStorage()
	store := newNativeDoltStoreForTest(storage)
	created, err := store.Create(Bead{Title: "stale fenced close", Metadata: map[string]string{"sibling": "before"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.SetMetadata(created.ID, "intervening", "write"); err != nil {
		t.Fatalf("intervening SetMetadata: %v", err)
	}
	before, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get before stale close: %v", err)
	}
	beforeIssue, err := storage.GetIssue(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("GetIssue before stale close: %v", err)
	}
	beforeRawMetadata := bytes.Clone(beforeIssue.Metadata)

	closed, err := store.CloseWithMetadataIfMatch(created.ID, created.Revision, map[string]string{"state": "drained"})
	if !IsPreconditionFailed(err) {
		t.Fatalf("CloseWithMetadataIfMatch stale error = %v, want precondition failure", err)
	}
	if !reflect.DeepEqual(closed, Bead{}) {
		t.Fatalf("stale close result = %#v, want zero bead", closed)
	}
	after, getErr := store.Get(created.ID)
	if getErr != nil {
		t.Fatalf("Get after stale close: %v", getErr)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("stale close mutated bead:\n got: %#v\nwant: %#v", after, before)
	}
	afterIssue, err := storage.GetIssue(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("GetIssue after stale close: %v", err)
	}
	if !bytes.Equal(afterIssue.Metadata, beforeRawMetadata) {
		t.Fatalf("stale close changed raw metadata bytes:\n got: %q\nwant: %q", afterIssue.Metadata, beforeRawMetadata)
	}
}

func TestNativeDoltStoreCloseWithMetadataIfMatchReturnsZeroOnMalformedMetadata(t *testing.T) {
	storage := &nativeDoltStorageSpy{
		getIssue: func(_ context.Context, id string) (*beadslib.Issue, error) {
			return &beadslib.Issue{
				ID:         id,
				Status:     beadslib.StatusOpen,
				IssueType:  beadslib.TypeTask,
				Metadata:   json.RawMessage(`{"broken":`),
				RowVersion: 17,
			}, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	closed, err := store.CloseWithMetadataIfMatch("gc-malformed", 17, map[string]string{"state": "drained"})
	if err == nil {
		t.Fatal("CloseWithMetadataIfMatch error = nil, want malformed metadata error")
	}
	if !reflect.DeepEqual(closed, Bead{}) {
		t.Fatalf("malformed close result = %#v, want zero bead", closed)
	}
}

func TestNativeDoltStoreCloseWithMetadataIfMatchRollsBackMetadataWhenCloseFails(t *testing.T) {
	sentinel := errors.New("injected close failure")
	storage := &nativeDoltFailingCloseStorage{
		nativeDoltMemStorage: newNativeDoltMemStorage(),
		closeIssue: func(context.Context, string, string, string, string) error {
			return sentinel
		},
	}
	store := newNativeDoltStoreForTest(storage)
	created, err := store.Create(Bead{Title: "rollback fenced close", Metadata: map[string]string{"sibling": "before"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	before, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get before close: %v", err)
	}
	beforeIssue, err := storage.GetIssue(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("GetIssue before close: %v", err)
	}
	beforeRawMetadata := bytes.Clone(beforeIssue.Metadata)

	closed, err := store.CloseWithMetadataIfMatch(created.ID, created.Revision, map[string]string{"state": "drained"})
	if !reflect.DeepEqual(closed, Bead{}) {
		t.Fatalf("CloseWithMetadataIfMatch result = %#v, want zero bead on failure", closed)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("CloseWithMetadataIfMatch error = %v, want injected close failure", err)
	}
	after, getErr := store.Get(created.ID)
	if getErr != nil {
		t.Fatalf("Get after failed close: %v", getErr)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("failed close left partial mutation:\n got: %#v\nwant: %#v", after, before)
	}
	afterIssue, err := storage.GetIssue(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("GetIssue after failed close: %v", err)
	}
	if !bytes.Equal(afterIssue.Metadata, beforeRawMetadata) {
		t.Fatalf("failed close changed raw metadata bytes:\n got: %q\nwant: %q", afterIssue.Metadata, beforeRawMetadata)
	}
}

func TestNativeDoltStoreCloseWithMetadataIfMatchRejectsUnclosedResult(t *testing.T) {
	storage := &nativeDoltFailingCloseStorage{
		nativeDoltMemStorage: newNativeDoltMemStorage(),
		closeIssue: func(context.Context, string, string, string, string) error {
			return nil
		},
	}
	store := newNativeDoltStoreForTest(storage)
	created, err := store.Create(Bead{Title: "close postcondition"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	closed, err := store.CloseWithMetadataIfMatch(created.ID, created.Revision, map[string]string{"state": "drained"})
	if err == nil {
		t.Fatal("CloseWithMetadataIfMatch error = nil, want unclosed-result refusal")
	}
	if !reflect.DeepEqual(closed, Bead{}) {
		t.Fatalf("unclosed transaction returned %#v, want zero bead", closed)
	}
	fresh, getErr := store.Get(created.ID)
	if getErr != nil {
		t.Fatalf("Get after refused close: %v", getErr)
	}
	if fresh.Status != "open" || fresh.Metadata["state"] != "" {
		t.Fatalf("refused close left a partial mutation: %#v", fresh)
	}
}

func TestNativeDoltStoreCloseWithMetadataIfMatchHasOneSameRevisionWinner(t *testing.T) {
	store := newNativeDoltStoreForTest(newNativeDoltMemStorage())
	created, err := store.Create(Bead{Title: "racing fenced close"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	type closeResult struct {
		bead Bead
		err  error
	}
	results := make(chan closeResult, 2)
	for _, value := range []string{"first", "second"} {
		value := value
		go func() {
			bead, err := store.CloseWithMetadataIfMatch(created.ID, created.Revision, map[string]string{"winner": value})
			results <- closeResult{bead: bead, err: err}
		}()
	}
	var wins, losses int
	for range 2 {
		result := <-results
		err := result.err
		switch {
		case err == nil:
			wins++
			if result.bead.Status != "closed" || result.bead.Metadata["winner"] == "" {
				t.Fatalf("winning result = %#v, want exact closed winner", result.bead)
			}
		case IsPreconditionFailed(err):
			losses++
			if !reflect.DeepEqual(result.bead, Bead{}) {
				t.Fatalf("losing result = %#v, want zero bead", result.bead)
			}
		default:
			t.Fatalf("same-revision close error = %v, want nil or precondition failure", err)
		}
	}
	if wins != 1 || losses != 1 {
		t.Fatalf("same-revision results wins=%d losses=%d, want exactly one of each", wins, losses)
	}
	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get after race: %v", err)
	}
	if got.Status != "closed" || (got.Metadata["winner"] != "first" && got.Metadata["winner"] != "second") {
		t.Fatalf("winner state = %#v, want one complete terminal result", got)
	}
}

func TestNativeDoltStoreDependencyRoundTrip(t *testing.T) {
	store := newNativeDoltStoreForTest(newNativeDoltMemStorage())
	parent, err := store.Create(Bead{Title: "dependency parent"})
	if err != nil {
		t.Fatalf("Create parent: %v", err)
	}
	child, err := store.Create(Bead{Title: "dependency child"})
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}

	if err := store.DepAdd(child.ID, parent.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}
	down, err := store.DepList(child.ID, "down")
	if err != nil {
		t.Fatalf("DepList down: %v", err)
	}
	if len(down) != 1 || down[0].IssueID != child.ID || down[0].DependsOnID != parent.ID || down[0].Type != "blocks" {
		t.Fatalf("DepList down = %#v, want child blocks parent", down)
	}
	up, err := store.DepList(parent.ID, "up")
	if err != nil {
		t.Fatalf("DepList up: %v", err)
	}
	if len(up) != 1 || up[0].IssueID != child.ID || up[0].DependsOnID != parent.ID || up[0].Type != "blocks" {
		t.Fatalf("DepList up = %#v, want child blocks parent", up)
	}
}

func TestNativeDoltStoreCreatePersistsDependenciesAfterUpstreamCreate(t *testing.T) {
	store := newNativeDoltStoreForTest(newNativeDoltMemStorage())
	parent, err := store.Create(Bead{Title: "create parent"})
	if err != nil {
		t.Fatalf("Create parent: %v", err)
	}
	blocker, err := store.Create(Bead{Title: "create blocker"})
	if err != nil {
		t.Fatalf("Create blocker: %v", err)
	}
	waiter, err := store.Create(Bead{Title: "create waiter"})
	if err != nil {
		t.Fatalf("Create waiter: %v", err)
	}

	child, err := store.Create(Bead{
		Title:    "create child",
		ParentID: parent.ID,
		Dependencies: []Dep{{
			DependsOnID: blocker.ID,
			Type:        "blocks",
		}},
		Needs: []string{"waits-for:" + waiter.ID},
	})
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}
	if child.ParentID != parent.ID {
		t.Fatalf("created ParentID = %q, want %q", child.ParentID, parent.ID)
	}

	got, err := store.Get(child.ID)
	if err != nil {
		t.Fatalf("Get child: %v", err)
	}
	if got.ParentID != parent.ID {
		t.Fatalf("fresh ParentID = %q, want %q", got.ParentID, parent.ID)
	}
	assertNativeDependency(t, got.Dependencies, child.ID, parent.ID, string(beadslib.DepParentChild))
	assertNativeDependency(t, got.Dependencies, child.ID, blocker.ID, "blocks")
	assertNativeDependency(t, got.Dependencies, child.ID, waiter.ID, "waits-for")

	children, err := store.Children(parent.ID)
	if err != nil {
		t.Fatalf("Children: %v", err)
	}
	if len(children) != 1 || children[0].ID != child.ID {
		t.Fatalf("Children(%q) = %#v, want child %q", parent.ID, children, child.ID)
	}
}

func TestNativeDoltStoreCreateWithMissingDependencyDoesNotLeavePartialIssue(t *testing.T) {
	store := newNativeDoltStoreForTest(newNativeDoltMemStorage())

	_, err := store.Create(Bead{
		Title: "create child",
		Needs: []string{"blocks:gc-missing"},
	})
	if err == nil {
		t.Fatal("Create error = nil, want missing dependency error")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Create error = %v, want ErrNotFound", err)
	}
	all, listErr := store.List(ListQuery{AllowScan: true, IncludeClosed: true, TierMode: TierBoth})
	if listErr != nil {
		t.Fatalf("List after failed create: %v", listErr)
	}
	for _, bead := range all {
		if bead.Title == "create child" {
			t.Fatalf("failed create left durable bead: %+v", bead)
		}
	}
}

func TestNativeDoltStoreFailedReparentKeepsOldParent(t *testing.T) {
	store := newNativeDoltStoreForTest(newNativeDoltMemStorage())
	oldParent, err := store.Create(Bead{Title: "old parent"})
	if err != nil {
		t.Fatalf("Create old parent: %v", err)
	}
	child, err := store.Create(Bead{Title: "child", ParentID: oldParent.ID})
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}

	missingParent := "gc-missing"
	err = store.Update(child.ID, UpdateOpts{ParentID: &missingParent})
	if err == nil {
		t.Fatal("Update reparent error = nil, want missing parent error")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Update reparent error = %v, want ErrNotFound", err)
	}
	got, err := store.Get(child.ID)
	if err != nil {
		t.Fatalf("Get child after failed reparent: %v", err)
	}
	if got.ParentID != oldParent.ID {
		t.Fatalf("ParentID after failed reparent = %q, want old parent %q", got.ParentID, oldParent.ID)
	}
}

func TestNativeDoltStoreMixedUpdateWithMissingParentLeavesBeadUnchanged(t *testing.T) {
	store := newNativeDoltStoreForTest(newNativeDoltMemStorage())
	oldParent, err := store.Create(Bead{Title: "old parent"})
	if err != nil {
		t.Fatalf("Create old parent: %v", err)
	}
	child, err := store.Create(Bead{
		Title:    "child",
		ParentID: oldParent.ID,
		Assignee: "old-agent",
		Labels:   []string{"keep", "remove"},
		Metadata: map[string]string{"phase": "old"},
	})
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}

	newTitle := "new title"
	newAssignee := "new-agent"
	missingParent := "gc-missing"
	err = store.Update(child.ID, UpdateOpts{
		Title:        &newTitle,
		Assignee:     &newAssignee,
		ParentID:     &missingParent,
		Labels:       []string{"new-label"},
		RemoveLabels: []string{"remove"},
		Metadata:     map[string]string{"phase": "new"},
	})
	if err == nil {
		t.Fatal("Update error = nil, want missing parent error")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Update error = %v, want ErrNotFound", err)
	}
	got, err := store.Get(child.ID)
	if err != nil {
		t.Fatalf("Get child after failed update: %v", err)
	}
	if got.Title != "child" {
		t.Fatalf("Title after failed update = %q, want child", got.Title)
	}
	if got.Assignee != "old-agent" {
		t.Fatalf("Assignee after failed update = %q, want old-agent", got.Assignee)
	}
	if got.ParentID != oldParent.ID {
		t.Fatalf("ParentID after failed update = %q, want %q", got.ParentID, oldParent.ID)
	}
	if got.Metadata["phase"] != "old" {
		t.Fatalf("Metadata after failed update = %#v, want phase=old", got.Metadata)
	}
	if slices.Contains(got.Labels, "new-label") || !slices.Contains(got.Labels, "remove") || !slices.Contains(got.Labels, "keep") {
		t.Fatalf("Labels after failed update = %#v, want original labels", got.Labels)
	}
}

func TestNativeDoltStoreUpdateRollsBackScalarOnLabelFailure(t *testing.T) {
	labelErr := errors.New("add label failed")
	storage := &nativeDoltFailingLabelStorage{
		nativeDoltMemStorage: newNativeDoltMemStorage(),
		addLabel: func(context.Context, string) error {
			return labelErr
		},
	}
	store := newNativeDoltStoreForTest(storage)
	child, err := store.Create(Bead{
		Title:    "child",
		Assignee: "old-agent",
		Labels:   []string{"keep"},
		Metadata: map[string]string{"phase": "old"},
	})
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}

	newTitle := "new title"
	newAssignee := "new-agent"
	err = store.Update(child.ID, UpdateOpts{
		Title:    &newTitle,
		Assignee: &newAssignee,
		Labels:   []string{"new-label"},
		Metadata: map[string]string{"phase": "new"},
	})
	if !errors.Is(err, labelErr) {
		t.Fatalf("Update error = %v, want %v", err, labelErr)
	}
	got, err := store.Get(child.ID)
	if err != nil {
		t.Fatalf("Get child after failed update: %v", err)
	}
	if got.Title != "child" {
		t.Fatalf("Title after failed update = %q, want child", got.Title)
	}
	if got.Assignee != "old-agent" {
		t.Fatalf("Assignee after failed update = %q, want old-agent", got.Assignee)
	}
	if got.Metadata["phase"] != "old" {
		t.Fatalf("Metadata after failed update = %#v, want phase=old", got.Metadata)
	}
	if slices.Contains(got.Labels, "new-label") || !slices.Contains(got.Labels, "keep") {
		t.Fatalf("Labels after failed update = %#v, want original labels", got.Labels)
	}
}

func TestNativeDoltStoreUpdateRollsBackScalarOnValidReparentFailure(t *testing.T) {
	reparentErr := errors.New("add parent dependency failed")
	storage := &nativeDoltFailingDependencyStorage{
		nativeDoltMemStorage: newNativeDoltMemStorage(),
	}
	store := newNativeDoltStoreForTest(storage)
	oldParent, err := store.Create(Bead{Title: "old parent"})
	if err != nil {
		t.Fatalf("Create old parent: %v", err)
	}
	newParent, err := store.Create(Bead{Title: "new parent"})
	if err != nil {
		t.Fatalf("Create new parent: %v", err)
	}
	child, err := store.Create(Bead{Title: "child", ParentID: oldParent.ID})
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}
	storage.addDependency = func(_ context.Context, dep *beadslib.Dependency) error {
		if dep.Type == beadslib.DepParentChild && dep.DependsOnID == newParent.ID {
			return reparentErr
		}
		return storage.nativeDoltMemStorage.AddDependency(context.Background(), dep, "test")
	}

	newTitle := "new title"
	err = store.Update(child.ID, UpdateOpts{
		Title:    &newTitle,
		ParentID: &newParent.ID,
	})
	if !errors.Is(err, reparentErr) {
		t.Fatalf("Update error = %v, want %v", err, reparentErr)
	}
	got, err := store.Get(child.ID)
	if err != nil {
		t.Fatalf("Get child after failed update: %v", err)
	}
	if got.Title != "child" {
		t.Fatalf("Title after failed update = %q, want child", got.Title)
	}
	if got.ParentID != oldParent.ID {
		t.Fatalf("ParentID after failed update = %q, want %q", got.ParentID, oldParent.ID)
	}
}

func TestNativeDoltStoreCreateDependencyFailureDeletesPartialIssue(t *testing.T) {
	failingAdd := errors.New("add dependency failed")
	storage := &nativeDoltFailingDependencyStorage{
		nativeDoltMemStorage: newNativeDoltMemStorage(),
		addDependency: func(context.Context, *beadslib.Dependency) error {
			return failingAdd
		},
	}
	store := newNativeDoltStoreForTest(storage)
	parent, err := store.Create(Bead{Title: "parent"})
	if err != nil {
		t.Fatalf("Create parent: %v", err)
	}

	_, err = store.Create(Bead{
		Title: "child",
		Needs: []string{"blocks:" + parent.ID},
	})
	if !errors.Is(err, failingAdd) {
		t.Fatalf("Create error = %v, want %v", err, failingAdd)
	}
	all, err := store.List(ListQuery{AllowScan: true, IncludeClosed: true, TierMode: TierBoth})
	if err != nil {
		t.Fatalf("List after failed create: %v", err)
	}
	for _, bead := range all {
		if bead.Title == "child" {
			t.Fatalf("failed create left durable bead: %+v", bead)
		}
	}
}

func TestNativeDoltStoreCreateDependencyTimeoutCleansUpWithFreshContext(t *testing.T) {
	oldTimeout := bdCommandTimeout
	bdCommandTimeout = time.Millisecond
	t.Cleanup(func() {
		bdCommandTimeout = oldTimeout
	})
	storage := &nativeDoltFailingDependencyStorage{
		nativeDoltMemStorage: newNativeDoltMemStorage(),
		addDependency: func(ctx context.Context, _ *beadslib.Dependency) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}
	store := newNativeDoltStoreForTest(storage)
	parent, err := store.Create(Bead{Title: "parent"})
	if err != nil {
		t.Fatalf("Create parent: %v", err)
	}

	_, err = store.Create(Bead{
		Title: "child",
		Needs: []string{"blocks:" + parent.ID},
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Create error = %v, want context deadline exceeded", err)
	}
	all, err := store.List(ListQuery{AllowScan: true, IncludeClosed: true, TierMode: TierBoth})
	if err != nil {
		t.Fatalf("List after timed-out create: %v", err)
	}
	for _, bead := range all {
		if bead.Title == "child" {
			t.Fatalf("timed-out create left durable bead: %+v", bead)
		}
	}
}

func TestNativeDoltStoreUsesBoundedOperationContext(t *testing.T) {
	storage := &nativeDoltStorageSpy{
		searchIssues: func(ctx context.Context, _ string, _ beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("native storage context has no deadline")
			}
			return []*beadslib.Issue{{ID: "gc-1", Title: "bounded", Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2}}, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if _, err := store.Get("gc-1"); err != nil {
		t.Fatalf("Get: %v", err)
	}
}

func TestOpenNativeDoltStoreAtProjectsScopedEnvDuringOpen(t *testing.T) {
	t.Setenv("BEADS_DOLT_SERVER_HOST", "ambient.example.com")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "9999")
	oldOpen := nativeDoltOpenBestAvailable
	t.Cleanup(func() {
		nativeDoltOpenBestAvailable = oldOpen
	})
	nativeDoltOpenBestAvailable = func(ctx context.Context, beadsDir string) (beadslib.Storage, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("native open context has no deadline")
		}
		if got := os.Getenv("BEADS_DOLT_SERVER_HOST"); got != "scoped.example.com" {
			t.Fatalf("BEADS_DOLT_SERVER_HOST during open = %q, want scoped.example.com", got)
		}
		if got := os.Getenv("BEADS_DOLT_SERVER_PORT"); got != "4407" {
			t.Fatalf("BEADS_DOLT_SERVER_PORT during open = %q, want 4407", got)
		}
		if !strings.HasSuffix(beadsDir, filepath.Join("scope", ".beads")) {
			t.Fatalf("beadsDir = %q, want scope .beads path", beadsDir)
		}
		return &nativeDoltStorageSpy{
			getConfig: func(context.Context, string) (string, error) {
				return "gc", nil
			},
		}, nil
	}

	store, err := OpenNativeDoltStoreAt(context.Background(), filepath.Join(t.TempDir(), "scope"), map[string]string{
		"BEADS_DOLT_SERVER_HOST": "scoped.example.com",
		"BEADS_DOLT_SERVER_PORT": "4407",
	})
	if err != nil {
		t.Fatalf("OpenNativeDoltStoreAt: %v", err)
	}
	if store.IDPrefix() != "gc" {
		t.Fatalf("IDPrefix = %q, want gc", store.IDPrefix())
	}
	if got := os.Getenv("BEADS_DOLT_SERVER_HOST"); got != "ambient.example.com" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST after open = %q, want ambient restored", got)
	}
	if got := os.Getenv("BEADS_DOLT_SERVER_PORT"); got != "9999" {
		t.Fatalf("BEADS_DOLT_SERVER_PORT after open = %q, want ambient restored", got)
	}
}

// TestOpenNativeDoltStoreAtPersistsLocalStringsAcrossReopen exercises the
// real newNativeDoltStoreAt wiring (not the in-memory-only default used by
// newNativeDoltStoreForTest / the conformance factory): it swaps only the
// external Dolt-open seam and proves SetLocalString/GetLocalString survive a
// second open of the same scopeRoot, i.e. clone-local data really lives on
// disk at <scopeRoot>/.beads/local-strings.json rather than in process memory.
func TestOpenNativeDoltStoreAtPersistsLocalStringsAcrossReopen(t *testing.T) {
	scopeRoot := filepath.Join(t.TempDir(), "scope")
	oldOpen := nativeDoltOpenBestAvailable
	t.Cleanup(func() {
		nativeDoltOpenBestAvailable = oldOpen
	})
	nativeDoltOpenBestAvailable = func(context.Context, string) (beadslib.Storage, error) {
		return &nativeDoltStorageSpy{
			getConfig: func(context.Context, string) (string, error) { return "gc", nil },
		}, nil
	}

	store, err := OpenNativeDoltStoreAt(context.Background(), scopeRoot, nil)
	if err != nil {
		t.Fatalf("OpenNativeDoltStoreAt (first open): %v", err)
	}
	if err := store.SetLocalString("gc-1", "last_woke_at", "2026-07-14T00:00:00Z"); err != nil {
		t.Fatalf("SetLocalString: %v", err)
	}

	sidecarPath := filepath.Join(scopeRoot, ".beads", "local-strings.json")
	if _, err := os.Stat(sidecarPath); err != nil {
		t.Fatalf("expected sidecar file at %s: %v", sidecarPath, err)
	}

	reopened, err := OpenNativeDoltStoreAt(context.Background(), scopeRoot, nil)
	if err != nil {
		t.Fatalf("OpenNativeDoltStoreAt (reopen): %v", err)
	}
	got, err := reopened.GetLocalString("gc-1", "last_woke_at")
	if err != nil {
		t.Fatalf("GetLocalString after reopen: %v", err)
	}
	if got != "2026-07-14T00:00:00Z" {
		t.Fatalf("GetLocalString after reopen = %q, want persisted value", got)
	}
}

func TestProcessEnvSnapshotWaitsForNativeDoltOpenEnvRestore(t *testing.T) {
	t.Setenv("BEADS_DOLT_SERVER_HOST", "ambient.example.com")
	restoreEnv, err := withNativeDoltOpenEnv(map[string]string{
		"BEADS_DOLT_SERVER_HOST": "scoped.example.com",
	})
	if err != nil {
		t.Fatalf("withNativeDoltOpenEnv: %v", err)
	}
	envCh := make(chan []string, 1)
	go func() {
		envCh <- processEnvSnapshotExcludingNativeDoltOpen()
	}()
	select {
	case env := <-envCh:
		t.Fatalf("process env snapshot completed during native open with host %q", beadsDoltServerHostFromEnv(env))
	case <-time.After(10 * time.Millisecond):
	}
	restoreEnv()
	var env []string
	select {
	case env = <-envCh:
	case <-time.After(time.Second):
		t.Fatal("process env snapshot did not complete after native open env restored")
	}
	if got := beadsDoltServerHostFromEnv(env); got != "ambient.example.com" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST in snapshot = %q, want ambient.example.com", got)
	}
}

func TestProcessEnvSnapshotWaitsForWholeBeadsNamespaceRestore(t *testing.T) {
	t.Setenv("BEADS_FUTURE_AUTHORITY", "ambient-authority")
	restoreEnv, err := withWithheldBeadsEnv()
	if err != nil {
		t.Fatalf("withWithheldBeadsEnv: %v", err)
	}
	restored := false
	t.Cleanup(func() {
		if !restored {
			restoreEnv()
		}
	})

	envCh := make(chan []string, 1)
	go func() {
		envCh <- ProcessEnvSnapshotExcludingNativeDoltOpen()
	}()
	select {
	case env := <-envCh:
		t.Fatalf("process env snapshot completed while BEADS_ was withheld: %v", envValues(env, "BEADS_FUTURE_AUTHORITY"))
	case <-time.After(10 * time.Millisecond):
	}

	restoreEnv()
	restored = true
	select {
	case env := <-envCh:
		if got := envValues(env, "BEADS_FUTURE_AUTHORITY"); len(got) != 1 || got[0] != "ambient-authority" {
			t.Fatalf("BEADS_FUTURE_AUTHORITY after restore = %v, want [ambient-authority]", got)
		}
	case <-time.After(time.Second):
		t.Fatal("process env snapshot did not complete after the BEADS_ namespace was restored")
	}
}

func TestOrdinaryNativeOpenWaitsForWholeBeadsNamespaceRestore(t *testing.T) {
	t.Setenv("BEADS_FUTURE_AUTHORITY", "ambient-authority")
	restoreEnv, err := withWithheldBeadsEnv()
	if err != nil {
		t.Fatalf("withWithheldBeadsEnv: %v", err)
	}
	restored := false
	t.Cleanup(func() {
		if !restored {
			restoreEnv()
		}
	})

	oldOpen := nativeDoltOpenBestAvailable
	t.Cleanup(func() { nativeDoltOpenBestAvailable = oldOpen })
	seen := make(chan string, 1)
	nativeDoltOpenBestAvailable = func(context.Context, string) (beadslib.Storage, error) {
		seen <- os.Getenv("BEADS_FUTURE_AUTHORITY")
		return &nativeDoltStorageSpy{
			getConfig: func(context.Context, string) (string, error) { return "gc", nil },
		}, nil
	}

	openDone := make(chan error, 1)
	scopeRoot := t.TempDir()
	go func() {
		store, openErr := OpenNativeDoltStoreAt(context.Background(), scopeRoot, nil)
		if store != nil {
			_ = store.CloseStore()
		}
		openDone <- openErr
	}()
	select {
	case got := <-seen:
		t.Fatalf("ordinary native open ran while BEADS_ was withheld (saw %q)", got)
	case <-time.After(10 * time.Millisecond):
	}

	restoreEnv()
	restored = true
	select {
	case got := <-seen:
		if got != "ambient-authority" {
			t.Fatalf("ordinary native open saw BEADS_FUTURE_AUTHORITY=%q, want ambient-authority", got)
		}
	case <-time.After(time.Second):
		t.Fatal("ordinary native open did not continue after the BEADS_ namespace was restored")
	}
	select {
	case err := <-openDone:
		if err != nil {
			t.Fatalf("OpenNativeDoltStoreAt: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ordinary native open did not return")
	}
}

// TestAmbientNativeDoltOpenEnvWaitsForNativeDoltOpenEnvRestore proves the guarded
// single-key ambient read serializes with an in-flight native Dolt open. A native
// open for a non-external scope unsets BEADS_DOLT_SERVER_TLS under nativeDoltOpenEnvMu
// for the duration of the open, so a bare os.Getenv could observe that transient unset.
// AmbientNativeDoltOpenEnv must block until restore and then observe the true ambient
// "1", never the concurrent scope's transient value.
func TestAmbientNativeDoltOpenEnvWaitsForNativeDoltOpenEnvRestore(t *testing.T) {
	t.Setenv("BEADS_DOLT_SERVER_TLS", "1")
	restoreEnv, err := withNativeDoltOpenEnv(map[string]string{
		"BEADS_DOLT_SERVER_HOST": "scoped.example.com",
	})
	if err != nil {
		t.Fatalf("withNativeDoltOpenEnv: %v", err)
	}
	restored := false
	t.Cleanup(func() {
		if !restored {
			restoreEnv()
		}
	})
	tlsCh := make(chan string, 1)
	go func() {
		tlsCh <- AmbientNativeDoltOpenEnv("BEADS_DOLT_SERVER_TLS")
	}()
	select {
	case got := <-tlsCh:
		t.Fatalf("ambient TLS read completed during native open (got %q); it must block on nativeDoltOpenEnvMu", got)
	case <-time.After(10 * time.Millisecond):
	}
	restoreEnv()
	restored = true
	select {
	case got := <-tlsCh:
		if got != "1" {
			t.Fatalf("ambient TLS after native open restore = %q, want 1", got)
		}
	case <-time.After(time.Second):
		t.Fatal("ambient TLS read did not complete after native open env restored")
	}
}

func TestBdStorePurgeWaitsForNativeDoltOpenEnvRestore(t *testing.T) {
	t.Setenv("BEADS_DOLT_SERVER_HOST", "ambient.example.com")
	restoreEnv, err := withNativeDoltOpenEnv(map[string]string{
		"BEADS_DOLT_SERVER_HOST": "scoped.example.com",
	})
	if err != nil {
		t.Fatalf("withNativeDoltOpenEnv: %v", err)
	}
	restored := false
	t.Cleanup(func() {
		if !restored {
			restoreEnv()
		}
	})
	done := make(chan []string, 1)
	store := NewBdStore(t.TempDir(), nil)
	store.SetPurgeRunner(func(_ string, env []string, _ ...string) ([]byte, error) {
		done <- env
		return []byte(`{"purged_count":0}`), nil
	})
	go func() {
		_, purgeErr := store.Purge(filepath.Join(t.TempDir(), ".beads"), true)
		if purgeErr != nil {
			t.Errorf("Purge: %v", purgeErr)
		}
	}()
	select {
	case env := <-done:
		t.Fatalf("Purge env snapshot completed during native open with host %q", beadsDoltServerHostFromEnv(env))
	case <-time.After(10 * time.Millisecond):
	}
	restoreEnv()
	restored = true
	var env []string
	select {
	case env = <-done:
	case <-time.After(time.Second):
		t.Fatal("Purge env snapshot did not complete after native open env restored")
	}
	if got := beadsDoltServerHostFromEnv(env); got != "ambient.example.com" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST in purge env = %q, want ambient.example.com", got)
	}
}

func TestCachingStoreCreateWithNativeDoltStoreHydratesDependencies(t *testing.T) {
	native := newNativeDoltStoreForTest(newNativeDoltMemStorage())
	parent, err := native.Create(Bead{Title: "cache parent"})
	if err != nil {
		t.Fatalf("Create parent: %v", err)
	}
	blocker, err := native.Create(Bead{Title: "cache blocker"})
	if err != nil {
		t.Fatalf("Create blocker: %v", err)
	}

	var notifications []cacheWriteNotification
	cache := NewCachingStoreForTest(native, func(eventType, beadID string, payload json.RawMessage) {
		notifications = append(notifications, cacheWriteNotification{eventType: eventType, beadID: beadID, payload: payload})
	})

	child, err := cache.Create(Bead{
		Title:    "cache child",
		ParentID: parent.ID,
		Dependencies: []Dep{{
			DependsOnID: blocker.ID,
			Type:        "blocks",
		}},
	})
	if err != nil {
		t.Fatalf("Create child through cache: %v", err)
	}
	assertNativeDependency(t, child.Dependencies, child.ID, parent.ID, string(beadslib.DepParentChild))
	assertNativeDependency(t, child.Dependencies, child.ID, blocker.ID, "blocks")

	cached, err := cache.Get(child.ID)
	if err != nil {
		t.Fatalf("cache Get child: %v", err)
	}
	if cached.ParentID != parent.ID {
		t.Fatalf("cached ParentID = %q, want %q", cached.ParentID, parent.ID)
	}
	assertNativeDependency(t, cached.Dependencies, child.ID, parent.ID, string(beadslib.DepParentChild))
	assertNativeDependency(t, cached.Dependencies, child.ID, blocker.ID, "blocks")

	if len(notifications) != 1 {
		t.Fatalf("notifications = %d, want 1: %#v", len(notifications), notifications)
	}
	if notifications[0].eventType != "bead.created" || notifications[0].beadID != child.ID {
		t.Fatalf("notification = %#v, want bead.created for %s", notifications[0], child.ID)
	}
	created, _, err := decodeCacheEvent(notifications[0].payload)
	if err != nil {
		t.Fatalf("decode create notification: %v", err)
	}
	assertNativeDependency(t, created.Dependencies, child.ID, parent.ID, string(beadslib.DepParentChild))
	assertNativeDependency(t, created.Dependencies, child.ID, blocker.ID, "blocks")
}

func TestNativeDoltStoreApplyGraphPlanWithStorageEphemeral(t *testing.T) {
	store := newNativeDoltStoreForTest(newNativeDoltMemStorage())

	result, err := store.ApplyGraphPlanWithStorage(t.Context(), &GraphApplyPlan{
		CommitMessage: "gc: native ephemeral graph",
		Nodes: []GraphApplyNode{
			{Key: "root", Title: "Root", Metadata: map[string]string{"gc.kind": "wisp"}},
			{Key: "blocker", Title: "Blocker"},
			{
				Key:               "child",
				Title:             "Child",
				ParentKey:         "root",
				Assignee:          "gascity/worker",
				AssignAfterCreate: true,
				MetadataRefs:      map[string]string{"gc.root_bead_id": "root", "gc.blocker_id": "blocker"},
			},
		},
		Edges: []GraphApplyEdge{
			{FromKey: "child", ToKey: "blocker"},
		},
	}, StorageEphemeral)
	if err != nil {
		t.Fatalf("ApplyGraphPlanWithStorage: %v", err)
	}

	root, err := store.Get(result.IDs["root"])
	if err != nil {
		t.Fatalf("Get root: %v", err)
	}
	blocker, err := store.Get(result.IDs["blocker"])
	if err != nil {
		t.Fatalf("Get blocker: %v", err)
	}
	child, err := store.Get(result.IDs["child"])
	if err != nil {
		t.Fatalf("Get child: %v", err)
	}

	for _, bead := range []Bead{root, blocker, child} {
		if !bead.Ephemeral {
			t.Fatalf("bead %s Ephemeral = false, want true", bead.ID)
		}
		if bead.NoHistory {
			t.Fatalf("bead %s NoHistory = true, want false", bead.ID)
		}
	}
	if child.Assignee != "gascity/worker" {
		t.Fatalf("child.Assignee = %q, want gascity/worker", child.Assignee)
	}
	if child.Metadata["gc.root_bead_id"] != root.ID || child.Metadata["gc.blocker_id"] != blocker.ID {
		t.Fatalf("child metadata refs = %#v, want root/blocker IDs", child.Metadata)
	}
	if child.ParentID != root.ID {
		t.Fatalf("child.ParentID = %q, want %q", child.ParentID, root.ID)
	}
	assertNativeDependency(t, child.Dependencies, child.ID, root.ID, string(beadslib.DepParentChild))
	assertNativeDependency(t, child.Dependencies, child.ID, blocker.ID, string(beadslib.DepBlocks))
}

func TestNativeDoltStoreApplyGraphPlanWithStorageNoHistory(t *testing.T) {
	store := newNativeDoltStoreForTest(newNativeDoltMemStorage())

	result, err := store.ApplyGraphPlanWithStorage(t.Context(), &GraphApplyPlan{
		Nodes: []GraphApplyNode{{Key: "node", Title: "No-history graph node"}},
	}, StorageNoHistory)
	if err != nil {
		t.Fatalf("ApplyGraphPlanWithStorage: %v", err)
	}

	got, err := store.Get(result.IDs["node"])
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.NoHistory {
		t.Fatalf("NoHistory = false, want true")
	}
	if got.Ephemeral {
		t.Fatalf("Ephemeral = true, want false")
	}
}

func assertNativeDependency(t *testing.T, deps []Dep, issueID, dependsOnID, depType string) {
	t.Helper()
	for _, dep := range deps {
		if dep.IssueID == issueID && dep.DependsOnID == dependsOnID && dep.Type == depType {
			return
		}
	}
	t.Fatalf("dependencies = %#v, missing %s -> %s (%s)", deps, issueID, dependsOnID, depType)
}

type nativeDoltTransactionTestStorage interface {
	CreateIssue(context.Context, *beadslib.Issue, string) error
	CreateIssues(context.Context, []*beadslib.Issue, string) error
	GetIssue(context.Context, string) (*beadslib.Issue, error)
	UpdateIssue(context.Context, string, map[string]interface{}, string) error
	CloseIssue(context.Context, string, string, string, string) error
	DeleteIssue(context.Context, string) error
	AddLabel(context.Context, string, string, string) error
	RemoveLabel(context.Context, string, string, string) error
	AddDependency(context.Context, *beadslib.Dependency, string) error
	RemoveDependency(context.Context, string, string, string) error
	GetDependencyRecords(context.Context, string) ([]*beadslib.Dependency, error)
}

type nativeDoltTransactionForTest struct {
	beadslib.Transaction
	storage nativeDoltTransactionTestStorage
}

func (tx nativeDoltTransactionForTest) CreateIssue(ctx context.Context, issue *beadslib.Issue, actor string) error {
	return tx.storage.CreateIssue(ctx, issue, actor)
}

func (tx nativeDoltTransactionForTest) CreateIssues(ctx context.Context, issues []*beadslib.Issue, actor string) error {
	return tx.storage.CreateIssues(ctx, issues, actor)
}

func (tx nativeDoltTransactionForTest) CloseIssue(ctx context.Context, id, reason, actor, session string) error {
	return tx.storage.CloseIssue(ctx, id, reason, actor, session)
}

func (tx nativeDoltTransactionForTest) DeleteIssue(ctx context.Context, id string) error {
	return tx.storage.DeleteIssue(ctx, id)
}

func (tx nativeDoltTransactionForTest) GetIssue(ctx context.Context, id string) (*beadslib.Issue, error) {
	return tx.storage.GetIssue(ctx, id)
}

func (tx nativeDoltTransactionForTest) UpdateIssue(ctx context.Context, id string, updates map[string]interface{}, actor string) error {
	return tx.storage.UpdateIssue(ctx, id, updates, actor)
}

func (tx nativeDoltTransactionForTest) AddLabel(ctx context.Context, issueID, label, actor string) error {
	return tx.storage.AddLabel(ctx, issueID, label, actor)
}

func (tx nativeDoltTransactionForTest) RemoveLabel(ctx context.Context, issueID, label, actor string) error {
	return tx.storage.RemoveLabel(ctx, issueID, label, actor)
}

func (tx nativeDoltTransactionForTest) AddDependency(ctx context.Context, dep *beadslib.Dependency, actor string) error {
	return tx.storage.AddDependency(ctx, dep, actor)
}

func (tx nativeDoltTransactionForTest) RemoveDependency(ctx context.Context, issueID, dependsOnID, actor string) error {
	return tx.storage.RemoveDependency(ctx, issueID, dependsOnID, actor)
}

func (tx nativeDoltTransactionForTest) GetDependencyRecords(ctx context.Context, issueID string) ([]*beadslib.Dependency, error) {
	return tx.storage.GetDependencyRecords(ctx, issueID)
}

type nativeDoltStorageSpy struct {
	beadslib.Storage
	createIssue                 func(context.Context, *beadslib.Issue, string) error
	createIssues                func(context.Context, []*beadslib.Issue, string) error
	getIssue                    func(context.Context, string) (*beadslib.Issue, error)
	updateIssue                 func(context.Context, string, map[string]interface{}, string) error
	updateIssueChecked          func(context.Context, string, map[string]interface{}, string, beadslib.UpdateIssueOptions) error
	runInTransaction            func(context.Context, string, func(beadslib.Transaction) error) error
	reopenIssue                 func(context.Context, string, string, string) error
	closeIssue                  func(context.Context, string, string, string, string) error
	closeIssueChecked           func(context.Context, string, string, beadslib.CloseIssueOptions) (beadslib.CloseIssueResult, error)
	deleteIssue                 func(context.Context, string) error
	searchIssues                func(context.Context, string, beadslib.IssueFilter) ([]*beadslib.Issue, error)
	countIssues                 func(context.Context, string, beadslib.IssueFilter) (int64, error)
	getReadyWork                func(context.Context, beadslib.WorkFilter) ([]*beadslib.Issue, error)
	addLabel                    func(context.Context, string, string, string) error
	removeLabel                 func(context.Context, string, string, string) error
	addDependency               func(context.Context, *beadslib.Dependency, string) error
	removeDependency            func(context.Context, string, string, string) error
	getDependencyRecords        func(context.Context, string) ([]*beadslib.Dependency, error)
	getDependenciesWithMetadata func(context.Context, string) ([]*beadslib.IssueWithDependencyMetadata, error)
	getDependentsWithMetadata   func(context.Context, string) ([]*beadslib.IssueWithDependencyMetadata, error)
	getConfig                   func(context.Context, string) (string, error)
	close                       func() error
}

func (s *nativeDoltStorageSpy) CreateIssue(ctx context.Context, issue *beadslib.Issue, actor string) error {
	if s.createIssue == nil {
		return nil
	}
	return s.createIssue(ctx, issue, actor)
}

func (s *nativeDoltStorageSpy) CreateIssues(ctx context.Context, issues []*beadslib.Issue, actor string) error {
	if s.createIssues != nil {
		return s.createIssues(ctx, issues, actor)
	}
	for _, issue := range issues {
		if err := s.CreateIssue(ctx, issue, actor); err != nil {
			return err
		}
	}
	return nil
}

func (s *nativeDoltStorageSpy) GetIssue(ctx context.Context, id string) (*beadslib.Issue, error) {
	if s.getIssue == nil {
		return nil, nil
	}
	return s.getIssue(ctx, id)
}

func (s *nativeDoltStorageSpy) UpdateIssue(ctx context.Context, id string, updates map[string]interface{}, actor string) error {
	if s.updateIssue == nil {
		return nil
	}
	return s.updateIssue(ctx, id, updates, actor)
}

func (s *nativeDoltStorageSpy) UpdateIssueChecked(ctx context.Context, id string, updates map[string]interface{}, actor string, opts beadslib.UpdateIssueOptions) error {
	if s.updateIssueChecked == nil {
		return nil
	}
	return s.updateIssueChecked(ctx, id, updates, actor, opts)
}

func (s *nativeDoltStorageSpy) RunInTransaction(ctx context.Context, commitMsg string, fn func(beadslib.Transaction) error) error {
	if s.runInTransaction != nil {
		return s.runInTransaction(ctx, commitMsg, fn)
	}
	return fn(nativeDoltTransactionForTest{storage: s})
}

func (s *nativeDoltStorageSpy) ReopenIssue(ctx context.Context, id string, reason string, actor string) error {
	if s.reopenIssue == nil {
		return nil
	}
	return s.reopenIssue(ctx, id, reason, actor)
}

func (s *nativeDoltStorageSpy) CloseIssue(ctx context.Context, id string, reason string, actor string, session string) error {
	if s.closeIssue == nil {
		return nil
	}
	return s.closeIssue(ctx, id, reason, actor, session)
}

func (s *nativeDoltStorageSpy) CloseIssueChecked(ctx context.Context, id string, actor string, opts beadslib.CloseIssueOptions) (beadslib.CloseIssueResult, error) {
	if s.closeIssueChecked == nil {
		return beadslib.CloseIssueResult{}, nil
	}
	return s.closeIssueChecked(ctx, id, actor, opts)
}

func (s *nativeDoltStorageSpy) DeleteIssue(ctx context.Context, id string) error {
	if s.deleteIssue == nil {
		return nil
	}
	return s.deleteIssue(ctx, id)
}

func (s *nativeDoltStorageSpy) SearchIssues(ctx context.Context, query string, filter beadslib.IssueFilter) ([]*beadslib.Issue, error) {
	if s.searchIssues == nil {
		return nil, nil
	}
	return s.searchIssues(ctx, query, filter)
}

func (s *nativeDoltStorageSpy) CountIssues(ctx context.Context, query string, filter beadslib.IssueFilter) (int64, error) {
	if s.countIssues == nil {
		return 0, nil
	}
	return s.countIssues(ctx, query, filter)
}

func (s *nativeDoltStorageSpy) GetReadyWork(ctx context.Context, filter beadslib.WorkFilter) ([]*beadslib.Issue, error) {
	if s.getReadyWork == nil {
		return nil, nil
	}
	return s.getReadyWork(ctx, filter)
}

func (s *nativeDoltStorageSpy) AddLabel(ctx context.Context, issueID, label, actor string) error {
	if s.addLabel == nil {
		return nil
	}
	return s.addLabel(ctx, issueID, label, actor)
}

func (s *nativeDoltStorageSpy) RemoveLabel(ctx context.Context, issueID, label, actor string) error {
	if s.removeLabel == nil {
		return nil
	}
	return s.removeLabel(ctx, issueID, label, actor)
}

func (s *nativeDoltStorageSpy) AddDependency(ctx context.Context, dep *beadslib.Dependency, actor string) error {
	if s.addDependency == nil {
		return nil
	}
	return s.addDependency(ctx, dep, actor)
}

func (s *nativeDoltStorageSpy) RemoveDependency(ctx context.Context, issueID, dependsOnID, actor string) error {
	if s.removeDependency == nil {
		return nil
	}
	return s.removeDependency(ctx, issueID, dependsOnID, actor)
}

func (s *nativeDoltStorageSpy) GetDependencyRecords(ctx context.Context, issueID string) ([]*beadslib.Dependency, error) {
	if s.getDependencyRecords == nil {
		return nil, nil
	}
	return s.getDependencyRecords(ctx, issueID)
}

func (s *nativeDoltStorageSpy) GetDependenciesWithMetadata(ctx context.Context, issueID string) ([]*beadslib.IssueWithDependencyMetadata, error) {
	if s.getDependenciesWithMetadata == nil {
		return nil, nil
	}
	return s.getDependenciesWithMetadata(ctx, issueID)
}

func (s *nativeDoltStorageSpy) GetDependentsWithMetadata(ctx context.Context, issueID string) ([]*beadslib.IssueWithDependencyMetadata, error) {
	if s.getDependentsWithMetadata == nil {
		return nil, nil
	}
	return s.getDependentsWithMetadata(ctx, issueID)
}

func (s *nativeDoltStorageSpy) GetConfig(ctx context.Context, key string) (string, error) {
	if s.getConfig == nil {
		return "", nil
	}
	return s.getConfig(ctx, key)
}

func (s *nativeDoltStorageSpy) AddComment(context.Context, string, string, string) error {
	return nil
}

func (s *nativeDoltStorageSpy) ImportIssueComment(context.Context, string, string, string, time.Time) (*beadslib.Comment, error) {
	return nil, nil
}

func (s *nativeDoltStorageSpy) Close() error {
	if s.close == nil {
		return nil
	}
	return s.close()
}

type nativeDoltMemStorage struct {
	beadslib.Storage
	store *MemStore
	txMu  sync.Mutex
}

func newNativeDoltMemStorage() *nativeDoltMemStorage {
	return &nativeDoltMemStorage{store: NewMemStore()}
}

func (s *nativeDoltMemStorage) RunInTransaction(_ context.Context, _ string, fn func(beadslib.Transaction) error) error {
	return runNativeDoltMemStorageTransactionForTest(s, func() error {
		return fn(nativeDoltTransactionForTest{storage: s})
	})
}

func runNativeDoltMemStorageTransactionForTest(storage *nativeDoltMemStorage, fn func() error) error {
	storage.txMu.Lock()
	defer storage.txMu.Unlock()
	storage.store.mu.Lock()
	seq, beads, deps := storage.store.snapshot()
	storage.store.mu.Unlock()
	if err := fn(); err != nil {
		storage.store.restoreFrom(seq, beads, deps)
		return err
	}
	return nil
}

func (s *nativeDoltMemStorage) CreateIssue(_ context.Context, issue *beadslib.Issue, _ string) error {
	withoutDependencies := *issue
	withoutDependencies.Dependencies = nil
	bead, err := beadFromNativeIssue(&withoutDependencies)
	if err != nil {
		return err
	}
	created, err := s.store.Create(bead)
	if err != nil {
		return err
	}
	converted, err := nativeIssueFromBead(created)
	if err != nil {
		return err
	}
	*issue = *converted
	return nil
}

func (s *nativeDoltMemStorage) CreateIssues(ctx context.Context, issues []*beadslib.Issue, actor string) error {
	for _, issue := range issues {
		if err := s.CreateIssue(ctx, issue, actor); err != nil {
			return err
		}
	}
	return nil
}

func (s *nativeDoltMemStorage) GetIssue(_ context.Context, id string) (*beadslib.Issue, error) {
	bead, err := s.store.Get(id)
	if err != nil {
		return nil, err
	}
	return nativeIssueFromBead(bead)
}

func (s *nativeDoltMemStorage) UpdateIssue(_ context.Context, id string, updates map[string]interface{}, _ string) error {
	opts, err := nativeDoltMemUpdateOpts(updates)
	if err != nil {
		return err
	}
	return s.store.Update(id, opts)
}

func (s *nativeDoltMemStorage) UpdateIssueChecked(
	_ context.Context,
	id string,
	updates map[string]interface{},
	_ string,
	opts beadslib.UpdateIssueOptions,
) error {
	updateOpts, err := nativeDoltMemUpdateOpts(updates)
	if err != nil {
		return err
	}
	if opts.ExpectedVersion == nil {
		return s.store.Update(id, updateOpts)
	}
	return nativeDoltMemCheckedError(s.store.UpdateIfMatch(id, *opts.ExpectedVersion, updateOpts))
}

func (s *nativeDoltMemStorage) ReopenIssue(_ context.Context, id string, _ string, _ string) error {
	return s.store.Reopen(id)
}

func (s *nativeDoltMemStorage) CloseIssue(_ context.Context, id string, _ string, _ string, _ string) error {
	return s.store.Close(id)
}

func (s *nativeDoltMemStorage) CloseIssueChecked(
	_ context.Context,
	id string,
	_ string,
	opts beadslib.CloseIssueOptions,
) (beadslib.CloseIssueResult, error) {
	current, err := s.store.Get(id)
	if err != nil {
		return beadslib.CloseIssueResult{}, err
	}
	if opts.ExpectedVersion == nil {
		err = s.store.Close(id)
	} else {
		err = s.store.CloseIfMatch(id, *opts.ExpectedVersion)
	}
	if err != nil {
		return beadslib.CloseIssueResult{}, nativeDoltMemCheckedError(err)
	}
	return beadslib.CloseIssueResult{Unchanged: current.Status == "closed"}, nil
}

func (s *nativeDoltMemStorage) DeleteIssue(_ context.Context, id string) error {
	return s.store.Delete(id)
}

func nativeDoltMemCheckedError(err error) error {
	if !IsPreconditionFailed(err) {
		return err
	}
	return fmt.Errorf("%w: %w", beadslib.ErrVersionMismatch, err)
}

func (s *nativeDoltMemStorage) SearchIssues(_ context.Context, _ string, filter beadslib.IssueFilter) ([]*beadslib.Issue, error) {
	beads, err := s.store.List(ListQuery{AllowScan: true, IncludeClosed: true, TierMode: TierBoth})
	if err != nil {
		return nil, err
	}
	issues := make([]*beadslib.Issue, 0, len(beads))
	for _, bead := range beads {
		issue, err := nativeIssueFromBead(bead)
		if err != nil {
			return nil, err
		}
		if filter.IncludeDependencies {
			issue, err = s.nativeIssueWithStoredDependencies(bead)
		}
		if err != nil {
			return nil, err
		}
		issues = append(issues, issue)
	}
	return issues, nil
}

func (s *nativeDoltMemStorage) GetReadyWork(_ context.Context, filter beadslib.WorkFilter) ([]*beadslib.Issue, error) {
	beads, err := s.store.List(ListQuery{AllowScan: true, Status: string(beadslib.StatusOpen), TierMode: TierBoth})
	if err != nil {
		return nil, err
	}
	statusByID := make(map[string]string, len(beads))
	all, err := s.store.List(ListQuery{AllowScan: true, IncludeClosed: true, TierMode: TierBoth})
	if err != nil {
		return nil, err
	}
	for _, bead := range all {
		statusByID[bead.ID] = bead.Status
	}
	ready := make([]Bead, 0, len(beads))
	for _, bead := range beads {
		if !filter.IncludeEphemeral && bead.Ephemeral {
			continue
		}
		if filter.Assignee != nil && bead.Assignee != *filter.Assignee {
			continue
		}
		deps, err := s.store.DepList(bead.ID, "down")
		if err != nil {
			return nil, err
		}
		blocked := false
		for _, dep := range deps {
			switch dep.Type {
			case "blocks", "waits-for", "conditional-blocks":
			default:
				continue
			}
			if statusByID[dep.DependsOnID] != "closed" {
				blocked = true
				break
			}
		}
		if blocked {
			continue
		}
		ready = append(ready, bead)
		if filter.Limit > 0 && len(ready) >= filter.Limit {
			break
		}
	}
	return nativeIssuesFromBeads(ready)
}

func (s *nativeDoltMemStorage) AddLabel(_ context.Context, issueID, label, _ string) error {
	return s.store.Update(issueID, UpdateOpts{Labels: []string{label}})
}

func (s *nativeDoltMemStorage) RemoveLabel(_ context.Context, issueID, label, _ string) error {
	return s.store.Update(issueID, UpdateOpts{RemoveLabels: []string{label}})
}

func (s *nativeDoltMemStorage) AddDependency(_ context.Context, dep *beadslib.Dependency, _ string) error {
	return s.store.DepAdd(dep.IssueID, dep.DependsOnID, string(dep.Type))
}

func (s *nativeDoltMemStorage) RemoveDependency(_ context.Context, issueID, dependsOnID string, _ string) error {
	return s.store.DepRemove(issueID, dependsOnID)
}

func (s *nativeDoltMemStorage) GetDependencyRecords(_ context.Context, issueID string) ([]*beadslib.Dependency, error) {
	deps, err := s.store.DepList(issueID, "down")
	if err != nil {
		return nil, err
	}
	records := make([]*beadslib.Dependency, 0, len(deps))
	for _, dep := range deps {
		records = append(records, &beadslib.Dependency{
			IssueID:     dep.IssueID,
			DependsOnID: dep.DependsOnID,
			Type:        beadslib.DependencyType(dep.Type),
		})
	}
	return records, nil
}

func (s *nativeDoltMemStorage) GetDependenciesWithMetadata(_ context.Context, issueID string) ([]*beadslib.IssueWithDependencyMetadata, error) {
	deps, err := s.store.DepList(issueID, "down")
	if err != nil {
		return nil, err
	}
	items := make([]*beadslib.IssueWithDependencyMetadata, 0, len(deps))
	for _, dep := range deps {
		issue := s.issueForDependency(dep.DependsOnID)
		items = append(items, &beadslib.IssueWithDependencyMetadata{
			Issue:          *issue,
			DependencyType: beadslib.DependencyType(dep.Type),
		})
	}
	return items, nil
}

func (s *nativeDoltMemStorage) GetDependentsWithMetadata(_ context.Context, issueID string) ([]*beadslib.IssueWithDependencyMetadata, error) {
	deps, err := s.store.DepList(issueID, "up")
	if err != nil {
		return nil, err
	}
	items := make([]*beadslib.IssueWithDependencyMetadata, 0, len(deps))
	for _, dep := range deps {
		issue := s.issueForDependency(dep.IssueID)
		items = append(items, &beadslib.IssueWithDependencyMetadata{
			Issue:          *issue,
			DependencyType: beadslib.DependencyType(dep.Type),
		})
	}
	return items, nil
}

func (s *nativeDoltMemStorage) GetConfig(context.Context, string) (string, error) {
	return "gc", nil
}

func (s *nativeDoltMemStorage) AddComment(context.Context, string, string, string) error {
	return nil
}

func (s *nativeDoltMemStorage) ImportIssueComment(context.Context, string, string, string, time.Time) (*beadslib.Comment, error) {
	return nil, nil
}

func (s *nativeDoltMemStorage) Close() error {
	return nil
}

type nativeDoltCloseCapturingStorage struct {
	*nativeDoltMemStorage
	closeReasons []string
}

type nativeDoltFailingCloseStorage struct {
	*nativeDoltMemStorage
	closeIssue func(context.Context, string, string, string, string) error
}

type retryingNativeDoltStorage struct {
	*nativeDoltMemStorage
	txCalls       int
	txErrors      []error
	afterConflict func()
}

func (s *retryingNativeDoltStorage) RunInTransaction(_ context.Context, _ string, fn func(beadslib.Transaction) error) error {
	s.txCalls++
	var txErr error
	if len(s.txErrors) > 0 {
		txErr = s.txErrors[0]
		s.txErrors = s.txErrors[1:]
	}
	err := runNativeDoltMemStorageTransactionForTest(s.nativeDoltMemStorage, func() error {
		if err := fn(nativeDoltTransactionForTest{storage: s}); err != nil {
			return err
		}
		return txErr
	})
	if txErr != nil && s.afterConflict != nil {
		s.afterConflict()
	}
	return err
}

func (s *nativeDoltFailingCloseStorage) RunInTransaction(_ context.Context, _ string, fn func(beadslib.Transaction) error) error {
	return runNativeDoltMemStorageTransactionForTest(s.nativeDoltMemStorage, func() error {
		return fn(nativeDoltTransactionForTest{storage: s})
	})
}

func (s *nativeDoltFailingCloseStorage) CloseIssue(ctx context.Context, id, reason, actor, session string) error {
	if s.closeIssue != nil {
		return s.closeIssue(ctx, id, reason, actor, session)
	}
	return s.nativeDoltMemStorage.CloseIssue(ctx, id, reason, actor, session)
}

func (s *nativeDoltCloseCapturingStorage) CloseIssue(ctx context.Context, id string, reason string, actor string, session string) error {
	s.closeReasons = append(s.closeReasons, reason)
	return s.nativeDoltMemStorage.CloseIssue(ctx, id, reason, actor, session)
}

func (s *nativeDoltMemStorage) issueForDependency(id string) *beadslib.Issue {
	bead, err := s.store.Get(id)
	if err != nil {
		return &beadslib.Issue{ID: id}
	}
	issue, err := nativeIssueFromBead(bead)
	if err != nil {
		return &beadslib.Issue{ID: id}
	}
	return issue
}

func (s *nativeDoltMemStorage) nativeIssueWithStoredDependencies(bead Bead) (*beadslib.Issue, error) {
	issue, err := nativeIssueFromBead(bead)
	if err != nil {
		return nil, err
	}
	deps, err := s.store.DepList(bead.ID, "down")
	if err != nil {
		return nil, err
	}
	issue.Dependencies = issue.Dependencies[:0]
	for _, dep := range deps {
		issue.Dependencies = append(issue.Dependencies, &beadslib.Dependency{
			IssueID:     dep.IssueID,
			DependsOnID: dep.DependsOnID,
			Type:        beadslib.DependencyType(dep.Type),
		})
	}
	return issue, nil
}

type nativeDoltFailingDependencyStorage struct {
	*nativeDoltMemStorage
	addDependency func(context.Context, *beadslib.Dependency) error
}

func (s *nativeDoltFailingDependencyStorage) RunInTransaction(_ context.Context, _ string, fn func(beadslib.Transaction) error) error {
	return runNativeDoltMemStorageTransactionForTest(s.nativeDoltMemStorage, func() error {
		return fn(nativeDoltTransactionForTest{storage: s})
	})
}

func (s *nativeDoltFailingDependencyStorage) AddDependency(ctx context.Context, dep *beadslib.Dependency, actor string) error {
	if s.addDependency != nil {
		return s.addDependency(ctx, dep)
	}
	return s.nativeDoltMemStorage.AddDependency(ctx, dep, actor)
}

func (s *nativeDoltFailingDependencyStorage) DeleteIssue(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.nativeDoltMemStorage.DeleteIssue(ctx, id)
}

type nativeDoltFailingLabelStorage struct {
	*nativeDoltMemStorage
	addLabel func(context.Context, string) error
}

func (s *nativeDoltFailingLabelStorage) RunInTransaction(_ context.Context, _ string, fn func(beadslib.Transaction) error) error {
	return runNativeDoltMemStorageTransactionForTest(s.nativeDoltMemStorage, func() error {
		return fn(nativeDoltTransactionForTest{storage: s})
	})
}

func (s *nativeDoltFailingLabelStorage) AddLabel(ctx context.Context, issueID, label, actor string) error {
	if s.addLabel != nil {
		return s.addLabel(ctx, label)
	}
	return s.nativeDoltMemStorage.AddLabel(ctx, issueID, label, actor)
}

func beadsDoltServerHostFromEnv(env []string) string {
	const prefix = "BEADS_DOLT_SERVER_HOST="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix)
		}
	}
	return ""
}

func nativeDoltMemUpdateOpts(updates map[string]interface{}) (UpdateOpts, error) {
	var opts UpdateOpts
	for key, value := range updates {
		switch key {
		case "title":
			text := fmt.Sprint(value)
			opts.Title = &text
		case "status":
			text := fmt.Sprint(value)
			opts.Status = &text
		case "issue_type":
			text := fmt.Sprint(value)
			opts.Type = &text
		case "priority":
			priority, ok := value.(int)
			if !ok {
				return UpdateOpts{}, fmt.Errorf("priority update has type %T, want int", value)
			}
			opts.Priority = &priority
		case "description":
			text := fmt.Sprint(value)
			opts.Description = &text
		case "assignee":
			text := fmt.Sprint(value)
			opts.Assignee = &text
		case "metadata":
			raw, ok := value.(json.RawMessage)
			if !ok {
				return UpdateOpts{}, fmt.Errorf("metadata update has type %T, want json.RawMessage", value)
			}
			metadata, err := metadataMapFromNative(raw)
			if err != nil {
				return UpdateOpts{}, err
			}
			opts.Metadata = metadata
		default:
			return UpdateOpts{}, fmt.Errorf("unsupported native update field %q", key)
		}
	}
	return opts, nil
}

func nativeIssuesFromBeads(beads []Bead) ([]*beadslib.Issue, error) {
	issues := make([]*beadslib.Issue, 0, len(beads))
	for _, bead := range beads {
		issue, err := nativeIssueFromBead(bead)
		if err != nil {
			return nil, err
		}
		issues = append(issues, issue)
	}
	return issues, nil
}

func cloneNativeIssueForTest(issue *beadslib.Issue) *beadslib.Issue {
	cloned := *issue
	cloned.Metadata = append(json.RawMessage(nil), issue.Metadata...)
	cloned.Labels = append([]string(nil), issue.Labels...)
	cloned.Dependencies = append([]*beadslib.Dependency(nil), issue.Dependencies...)
	return &cloned
}

func filterNativeIssuesForTest(issues []*beadslib.Issue, filter beadslib.IssueFilter) []*beadslib.Issue {
	filtered := make([]*beadslib.Issue, 0, len(issues))
	for _, issue := range issues {
		if filter.Status != nil && issue.Status != *filter.Status {
			continue
		}
		if len(filter.Statuses) > 0 && !slices.Contains(filter.Statuses, issue.Status) {
			continue
		}
		if len(filter.ExcludeStatus) > 0 && slices.Contains(filter.ExcludeStatus, issue.Status) {
			continue
		}
		if filter.Ephemeral != nil && issue.Ephemeral != *filter.Ephemeral {
			continue
		}
		if filter.Assignee != nil && issue.Assignee != *filter.Assignee {
			continue
		}
		filtered = append(filtered, cloneNativeIssueForTest(issue))
		if filter.Limit > 0 && len(filtered) >= filter.Limit {
			break
		}
	}
	return filtered
}

// TestOpenNativeDoltStoreAtWithoutAmbientEnvWithholdsTheWholeNamespace pins the
// hermetic open against the variable class that motivated it: the ones this
// package's scoped projection does not name.
//
// BEADS_DOLT_CREDENTIAL_COMMAND is the sharp one — the library runs it — and it
// is not in nativeDoltOpenEnvKeys, so the scoped projection leaves it standing.
// The assertion is made from inside the open, where the library would read it.
func TestOpenNativeDoltStoreAtWithoutAmbientEnvWithholdsTheWholeNamespace(t *testing.T) {
	t.Setenv("BEADS_DOLT_CREDENTIAL_COMMAND", "/poison/credential-command")
	t.Setenv("BEADS_DB", "/poison/db")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "ambient.example.com")
	oldOpen := nativeDoltOpenBestAvailable
	t.Cleanup(func() { nativeDoltOpenBestAvailable = oldOpen })

	var seen map[string]string
	nativeDoltOpenBestAvailable = func(context.Context, string) (beadslib.Storage, error) {
		seen = map[string]string{}
		for _, key := range []string{"BEADS_DOLT_CREDENTIAL_COMMAND", "BEADS_DB", "BEADS_DOLT_SERVER_HOST"} {
			seen[key] = os.Getenv(key)
		}
		return &nativeDoltStorageSpy{
			getConfig: func(context.Context, string) (string, error) { return "gcg", nil },
		}, nil
	}

	if _, err := OpenNativeDoltStoreAtWithoutAmbientEnv(context.Background(), filepath.Join(t.TempDir(), "scope")); err != nil {
		t.Fatalf("OpenNativeDoltStoreAtWithoutAmbientEnv: %v", err)
	}
	for key, value := range seen {
		if value != "" {
			t.Errorf("%s = %q during the open, want withheld", key, value)
		}
	}
	// Withheld for the open, not for the process: a caller that shares this
	// process must find its environment exactly as it left it.
	if got := os.Getenv("BEADS_DOLT_CREDENTIAL_COMMAND"); got != "/poison/credential-command" {
		t.Errorf("BEADS_DOLT_CREDENTIAL_COMMAND after the open = %q, want the ambient value restored", got)
	}
	if got := os.Getenv("BEADS_DOLT_SERVER_HOST"); got != "ambient.example.com" {
		t.Errorf("BEADS_DOLT_SERVER_HOST after the open = %q, want the ambient value restored", got)
	}
}

func TestOpenNativeDoltStoreAtWithoutAmbientEnvWithCredentialCommandProjectsOnlyTheSelectedCommand(t *testing.T) {
	t.Setenv("BEADS_DOLT_CREDENTIAL_COMMAND", "/poison/credential-command")
	t.Setenv("BEADS_DB", "/poison/db")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "ambient.example.com")
	oldOpen := nativeDoltOpenBestAvailable
	t.Cleanup(func() { nativeDoltOpenBestAvailable = oldOpen })

	var seen map[string]string
	nativeDoltOpenBestAvailable = func(context.Context, string) (beadslib.Storage, error) {
		seen = map[string]string{}
		for _, key := range []string{"BEADS_DOLT_CREDENTIAL_COMMAND", "BEADS_DB", "BEADS_DOLT_SERVER_HOST"} {
			seen[key] = os.Getenv(key)
		}
		return &nativeDoltStorageSpy{
			getConfig: func(context.Context, string) (string, error) { return "gcg", nil },
		}, nil
	}

	store, err := OpenNativeDoltStoreAtWithoutAmbientEnvWithCredentialCommand(
		context.Background(), filepath.Join(t.TempDir(), "scope"), "/selected/gc internal beads-credential")
	if err != nil {
		t.Fatalf("OpenNativeDoltStoreAtWithoutAmbientEnvWithCredentialCommand: %v", err)
	}
	if err := store.CloseStore(); err != nil {
		t.Fatalf("CloseStore: %v", err)
	}
	if got := seen["BEADS_DOLT_CREDENTIAL_COMMAND"]; got != "/selected/gc internal beads-credential" {
		t.Errorf("BEADS_DOLT_CREDENTIAL_COMMAND during the open = %q, want the selected command", got)
	}
	for _, key := range []string{"BEADS_DB", "BEADS_DOLT_SERVER_HOST"} {
		if got := seen[key]; got != "" {
			t.Errorf("%s = %q during the open, want withheld", key, got)
		}
	}
	if got := os.Getenv("BEADS_DOLT_CREDENTIAL_COMMAND"); got != "/poison/credential-command" {
		t.Errorf("BEADS_DOLT_CREDENTIAL_COMMAND after the open = %q, want the ambient value restored", got)
	}
	if got := os.Getenv("BEADS_DOLT_SERVER_HOST"); got != "ambient.example.com" {
		t.Errorf("BEADS_DOLT_SERVER_HOST after the open = %q, want the ambient value restored", got)
	}
}

// TestScopedNativeDoltOpenEnvIgnoresKeysOutsideItsList is the falsification the
// hermetic open exists for: the scoped projection honors exactly the keys it
// names, so passing an "empty environment" to it withholds nothing else.
func TestScopedNativeDoltOpenEnvIgnoresKeysOutsideItsList(t *testing.T) {
	t.Setenv("BEADS_DOLT_CREDENTIAL_COMMAND", "/poison/credential-command")
	restore, err := withNativeDoltOpenEnv(map[string]string{"BEADS_DOLT_CREDENTIAL_COMMAND": ""})
	if err != nil {
		t.Fatalf("withNativeDoltOpenEnv: %v", err)
	}
	got := os.Getenv("BEADS_DOLT_CREDENTIAL_COMMAND")
	restore()
	if got != "/poison/credential-command" {
		t.Fatalf("BEADS_DOLT_CREDENTIAL_COMMAND during a scoped projection = %q; if the projection now honors unlisted keys, the hermetic open's reason to exist has changed", got)
	}
}
