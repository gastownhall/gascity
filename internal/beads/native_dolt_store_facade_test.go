package beads

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

func openIssueForFacadeTest(id string) *beadslib.Issue {
	return &beadslib.Issue{ID: id, Title: "existing", Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2}
}

// TestNativeDoltCreateRequestMovesDependenciesOffTheIssue pins the mapping the
// facade requires: it refuses outright a create whose issue still carries edges
// (ValidatePublicCreateRequest, "issue comments and dependencies must be
// supplied through request fields").
func TestNativeDoltCreateRequestMovesDependenciesOffTheIssue(t *testing.T) {
	issue, err := nativeIssueFromBead(Bead{
		ID:           "gc-child",
		Title:        "child",
		ParentID:     "gc-parent",
		Dependencies: []Dep{{DependsOnID: "gc-blocker", Type: "blocks"}},
		Needs:        []string{"waits-for:gc-waiter"},
	})
	if err != nil {
		t.Fatalf("nativeIssueFromBead: %v", err)
	}
	request, err := nativeCreateRequestFromIssue("gascity", issue)
	if err != nil {
		t.Fatalf("nativeCreateRequestFromIssue: %v", err)
	}
	if len(request.Issue.Dependencies) != 0 {
		t.Fatalf("request issue kept %d dependencies; the facade refuses any", len(request.Issue.Dependencies))
	}
	want := []issueops.CreateDependency{
		{TargetID: "gc-blocker", Type: beadslib.DepBlocks},
		{TargetID: "gc-parent", Type: beadslib.DepParentChild},
		{TargetID: "gc-waiter", Type: beadslib.DependencyType("waits-for")},
	}
	if !slices.Equal(request.Dependencies, want) {
		t.Fatalf("request dependencies = %#v, want %#v", request.Dependencies, want)
	}
}

// TestNativeDoltCreateRequestKeepsParentOffTheParentIDField guards the ID
// scheme. CreateRequest.ParentID mints hierarchical "<parent>.N" child IDs;
// the same edge supplied as a parent-child dependency keeps the flat minted ID
// Gas City has always used.
func TestNativeDoltCreateRequestKeepsParentOffTheParentIDField(t *testing.T) {
	issue, err := nativeIssueFromBead(Bead{Title: "child", ParentID: "gc-parent"})
	if err != nil {
		t.Fatalf("nativeIssueFromBead: %v", err)
	}
	request, err := nativeCreateRequestFromIssue("gascity", issue)
	if err != nil {
		t.Fatalf("nativeCreateRequestFromIssue: %v", err)
	}
	if request.ParentID != "" {
		t.Fatalf("request ParentID = %q; setting it switches ID minting to the hierarchical scheme", request.ParentID)
	}
	if len(request.Dependencies) != 1 || request.Dependencies[0].Type != beadslib.DepParentChild {
		t.Fatalf("request dependencies = %#v, want one parent-child edge", request.Dependencies)
	}
}

// TestNativeDoltCreateRequestForcesIDPrefix pins the prefix behavior of the
// storage-layer create this replaced: dolt.CreateIssue passes
// SkipPrefixValidation for the single-issue path, and Gas City mints gc-, gcg-
// and gcs- IDs against one store.
func TestNativeDoltCreateRequestForcesIDPrefix(t *testing.T) {
	issue, err := nativeIssueFromBead(Bead{ID: "gcg-17", Title: "graph bead"})
	if err != nil {
		t.Fatalf("nativeIssueFromBead: %v", err)
	}
	request, err := nativeCreateRequestFromIssue("gascity", issue)
	if err != nil {
		t.Fatalf("nativeCreateRequestFromIssue: %v", err)
	}
	if !request.ForceIDPrefix {
		t.Fatal("request ForceIDPrefix = false; an off-prefix explicit ID that used to create is refused with ErrPrefixMismatch")
	}
}

func TestNativeDoltCreateRequestReversesInboundDependency(t *testing.T) {
	issue, err := nativeIssueFromBead(Bead{
		ID:           "gc-child",
		Title:        "child",
		Dependencies: []Dep{{IssueID: "gc-other", DependsOnID: "gc-child", Type: "blocks"}},
	})
	if err != nil {
		t.Fatalf("nativeIssueFromBead: %v", err)
	}
	request, err := nativeCreateRequestFromIssue("gascity", issue)
	if err != nil {
		t.Fatalf("nativeCreateRequestFromIssue: %v", err)
	}
	want := issueops.CreateDependency{TargetID: "gc-other", Type: beadslib.DepBlocks, Reverse: true}
	if len(request.Dependencies) != 1 || request.Dependencies[0] != want {
		t.Fatalf("request dependencies = %#v, want %#v", request.Dependencies, want)
	}
}

func TestNativeDoltCreateRequestRejectsThirdPartyDependency(t *testing.T) {
	issue, err := nativeIssueFromBead(Bead{
		ID:           "gc-child",
		Title:        "child",
		Dependencies: []Dep{{IssueID: "gc-a", DependsOnID: "gc-b", Type: "blocks"}},
	})
	if err != nil {
		t.Fatalf("nativeIssueFromBead: %v", err)
	}
	_, err = nativeCreateRequestFromIssue("gascity", issue)
	if err == nil || !strings.Contains(err.Error(), "names neither end of the new bead") {
		t.Fatalf("nativeCreateRequestFromIssue error = %v, want a refusal naming the unrelated edge", err)
	}
}

func TestNativeDoltCreateRequestRejectsEmptyDependencyTarget(t *testing.T) {
	issue, err := nativeIssueFromBead(Bead{ID: "gc-child", Title: "child", Dependencies: []Dep{{Type: "blocks"}}})
	if err != nil {
		t.Fatalf("nativeIssueFromBead: %v", err)
	}
	if _, err := nativeCreateRequestFromIssue("gascity", issue); err == nil {
		t.Fatal("nativeCreateRequestFromIssue error = nil, want empty depends_on_id refusal")
	}
}

func TestNativeDoltStoreUpdateMapsOptionsToIssuePatch(t *testing.T) {
	store, recorder := newRecordingNativeDoltStore(openIssueForFacadeTest("gc-child"))
	title, status, issueType := "updated title", "in_progress", "bug"
	priority := 1
	description, assignee, parentID := "updated description", "native-test", "gc-parent"

	if err := store.Update("gc-child", UpdateOpts{
		Title:        &title,
		Status:       &status,
		Type:         &issueType,
		Priority:     &priority,
		Description:  &description,
		Assignee:     &assignee,
		ParentID:     &parentID,
		Labels:       []string{"add"},
		RemoveLabels: []string{"remove"},
		Metadata:     map[string]string{"gc.step_ref": "build"},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(recorder.updates) != 1 {
		t.Fatalf("facade updates = %d, want 1", len(recorder.updates))
	}
	got := recorder.updates[0]
	if got.Actor != "native-test" || got.IssueID != "gc-child" {
		t.Fatalf("update actor/id = %q/%q, want native-test/gc-child", got.Actor, got.IssueID)
	}
	patch := got.Patch
	if patch.Title.Value != title || patch.Status.Value != issueops.Status(status) ||
		patch.IssueType.Value != issueops.IssueType(issueType) || patch.Priority.Value != priority ||
		patch.Description.Value != description || patch.Assignee.Value != assignee || patch.ParentID.Value != parentID {
		t.Fatalf("patch = %#v, want the supplied scalar fields", patch)
	}
	if !slices.Equal(patch.Labels.Add, []string{"add"}) || !slices.Equal(patch.Labels.Remove, []string{"remove"}) {
		t.Fatalf("label patch = add:%#v remove:%#v", patch.Labels.Add, patch.Labels.Remove)
	}
	if patch.Metadata.Replace.Set {
		t.Fatal("metadata patch replaces the whole document; the storage-layer path merged keys")
	}
	if string(patch.Metadata.Set["gc.step_ref"]) != `"build"` {
		t.Fatalf("metadata patch = %#v, want the value marshaled as a JSON string", patch.Metadata.Set)
	}
}

// TestNativeDoltStoreUpdateForcesOnlyTheArmedGuards is the fix for the shape the
// upstream change carried. ForceAssigneeTransfer without an assignee edit is not
// a harmless no-op: the facade refuses it outright with "invalid forced assignee
// transfer", so setting it unconditionally would break every update that does
// not touch the assignee.
func TestNativeDoltStoreUpdateForcesOnlyTheArmedGuards(t *testing.T) {
	title, status, assignee := "retitled", "closed", "worker-b"
	tests := []struct {
		name              string
		opts              UpdateOpts
		wantForceAssignee bool
		wantForceClose    bool
	}{
		{name: "title only", opts: UpdateOpts{Title: &title}},
		{name: "assignee", opts: UpdateOpts{Assignee: &assignee}, wantForceAssignee: true},
		{name: "status", opts: UpdateOpts{Status: &status}, wantForceClose: true},
		{name: "both", opts: UpdateOpts{Assignee: &assignee, Status: &status}, wantForceAssignee: true, wantForceClose: true},
		{name: "metadata only", opts: UpdateOpts{Metadata: map[string]string{"k": "v"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, recorder := newRecordingNativeDoltStore(openIssueForFacadeTest("gc-1"))
			if err := store.Update("gc-1", tt.opts); err != nil {
				t.Fatalf("Update: %v", err)
			}
			got := recorder.updates[0]
			if got.ForceAssigneeTransfer != tt.wantForceAssignee {
				t.Fatalf("ForceAssigneeTransfer = %v, want %v", got.ForceAssigneeTransfer, tt.wantForceAssignee)
			}
			if got.ForceClosePolicy != tt.wantForceClose {
				t.Fatalf("ForceClosePolicy = %v, want %v", got.ForceClosePolicy, tt.wantForceClose)
			}
		})
	}
}

func TestNativeDoltStoreSetMetadataBatchMergesThroughTheFacade(t *testing.T) {
	store, recorder := newRecordingNativeDoltStore(openIssueForFacadeTest("gc-metadata"))
	if err := store.SetMetadataBatch("gc-metadata", map[string]string{"count": "3", "enabled": "true"}); err != nil {
		t.Fatalf("SetMetadataBatch: %v", err)
	}
	if len(recorder.updates) != 1 {
		t.Fatalf("facade updates = %d, want 1", len(recorder.updates))
	}
	got := recorder.updates[0]
	if got.ForceAssigneeTransfer || got.ForceClosePolicy {
		t.Fatalf("metadata write armed a force: assignee=%v close=%v", got.ForceAssigneeTransfer, got.ForceClosePolicy)
	}
	if got.Patch.Metadata.Replace.Set {
		t.Fatal("metadata write replaces the whole document instead of merging keys")
	}
	want := map[string]json.RawMessage{"count": json.RawMessage(`"3"`), "enabled": json.RawMessage(`"true"`)}
	for key, value := range want {
		if string(got.Patch.Metadata.Set[key]) != string(value) {
			t.Fatalf("metadata patch[%q] = %s, want %s", key, got.Patch.Metadata.Set[key], value)
		}
	}
}

func TestNativeDoltStoreSetMetadataBatchSkipsEmptyWrite(t *testing.T) {
	store, recorder := newRecordingNativeDoltStore(openIssueForFacadeTest("gc-metadata"))
	if err := store.SetMetadataBatch("gc-metadata", nil); err != nil {
		t.Fatalf("SetMetadataBatch(nil): %v", err)
	}
	if len(recorder.updates) != 0 {
		t.Fatalf("empty metadata write issued %d facade updates, want 0", len(recorder.updates))
	}
}

func TestNativeDoltStoreCloseForcesPolicyAndCarriesMetadataReason(t *testing.T) {
	issue := openIssueForFacadeTest("gc-close")
	issue.Metadata = json.RawMessage(`{"close_reason":"done"}`)
	store, recorder := newRecordingNativeDoltStore(issue)

	if err := store.Close("gc-close"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if len(recorder.closes) != 1 {
		t.Fatalf("facade closes = %d, want 1", len(recorder.closes))
	}
	got := recorder.closes[0]
	if got.IssueID != "gc-close" || got.Actor != "native-test" || got.Reason != "done" {
		t.Fatalf("close request = %#v, want gc-close/native-test/done", got)
	}
	if !got.Force {
		t.Fatal("close did not force policy; the storage-layer close it replaced applied none, and molecule roots close over open children")
	}
}

func TestNativeDoltStoreReopenSkipsFacadeForAnOpenBead(t *testing.T) {
	store, recorder := newRecordingNativeDoltStore(openIssueForFacadeTest("gc-open"))
	if err := store.Reopen("gc-open"); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	if len(recorder.reopens) != 0 {
		t.Fatalf("reopen of an open bead issued %d facade calls, want 0", len(recorder.reopens))
	}
}

func TestNativeDoltStoreReopenDelegatesForAClosedBead(t *testing.T) {
	issue := openIssueForFacadeTest("gc-closed")
	issue.Status = beadslib.StatusClosed
	store, recorder := newRecordingNativeDoltStore(issue)

	if err := store.Reopen("gc-closed"); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	if len(recorder.reopens) != 1 || recorder.reopens[0].IssueID != "gc-closed" || recorder.reopens[0].Actor != "native-test" {
		t.Fatalf("facade reopens = %#v, want one gc-closed/native-test request", recorder.reopens)
	}
}

// TestNativeDoltStoreMetadataKeyRuleSplitsByRoute pins the store's most
// surprising route asymmetry. The facade validates every patched metadata key;
// Create and the map-based Store.Tx write do not. A bead can therefore be
// created carrying a key that every later standalone write refuses, which is
// why internal/dispatch drops such keys instead of propagating them
// (beadmeta.CopyUserKeys). The pattern itself is checked against a live server
// in native_dolt_store_facade_integration_test.go.
func TestNativeDoltStoreMetadataKeyRuleSplitsByRoute(t *testing.T) {
	store := newNativeDoltStoreForTest(newNativeDoltMemStorage())
	created, err := store.Create(Bead{Title: "dashed", Metadata: map[string]string{"my-key": "planted"}})
	if err != nil {
		t.Fatalf("Create with a non-conforming metadata key: %v, want the create route to accept it", err)
	}

	err = store.SetMetadataBatch(created.ID, map[string]string{"my-key": "rewritten"})
	if err == nil {
		t.Fatal("standalone SetMetadataBatch accepted a non-conforming key; the facade refuses it")
	}
	if !strings.Contains(err.Error(), "my-key") {
		t.Fatalf("SetMetadataBatch error = %v, want it to name the refused key", err)
	}
	if err := store.Update(created.ID, UpdateOpts{Metadata: map[string]string{"my-key": "rewritten"}}); err == nil {
		t.Fatal("standalone Update accepted a non-conforming key; the facade refuses it")
	}

	if err := store.Tx("gc: tx", func(tx Tx) error {
		return tx.SetMetadataBatch(created.ID, map[string]string{"my-key": "rewritten"})
	}); err != nil {
		t.Fatalf("Tx SetMetadataBatch: %v, want the map-based route to accept the same key", err)
	}
	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata["my-key"] != "rewritten" {
		t.Fatalf("metadata = %#v, want the Tx write to have landed", got.Metadata)
	}
}
