package main

import (
	"errors"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func TestControlReadyCandidates_OrderDedupEmpty(t *testing.T) {
	got := controlReadyCandidates([]string{"", "sess-1", "alias", "sess-1", "target"})
	want := []string{"sess-1", "alias", "target"}
	if !slices.Equal(got, want) {
		t.Fatalf("controlReadyCandidates() = %#v, want %#v", got, want)
	}
}

func TestControlReadyCandidates_LegacySuffixExpansion(t *testing.T) {
	got := controlReadyCandidates([]string{"gascity/control-dispatcher", "control-dispatcher"})
	want := []string{
		"gascity/control-dispatcher",
		"gascity/workflow-control",
		"control-dispatcher",
		"workflow-control",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("controlReadyCandidates() = %#v, want %#v", got, want)
	}
}

func TestControlReadyCandidates_TargetIncludedWhenSessionsEmpty(t *testing.T) {
	got := controlReadyCandidates([]string{"", "", "", "gascity/control-dispatcher"})
	want := []string{"gascity/control-dispatcher", "gascity/workflow-control"}
	if !slices.Equal(got, want) {
		t.Fatalf("controlReadyCandidates() = %#v, want %#v", got, want)
	}
}

func TestControlReadyCandidates_NoLegacyForNonControlSuffix(t *testing.T) {
	got := controlReadyCandidates([]string{"foo/workflow-control"})
	want := []string{"foo/workflow-control"}
	if !slices.Equal(got, want) {
		t.Fatalf("controlReadyCandidates() = %#v, want %#v", got, want)
	}
}

func TestInProcessReady_AssigneeFirstCandidateWins(t *testing.T) {
	store := beads.NewMemStore()
	for i := 0; i < 25; i++ {
		createReadyBead(t, store, "A", nil)
	}
	for i := 0; i < 5; i++ {
		createReadyBead(t, store, "B", nil)
	}

	got := runInProcessReadyForTest(t, store, "target", "", []string{"A", "B"}, 20)
	if len(got) != 20 {
		t.Fatalf("len(got) = %d, want 20", len(got))
	}
	for i, b := range got {
		if b.ID != "gc-"+strconv.Itoa(i+1) {
			t.Fatalf("got[%d].ID = %q, want gc-%d from first candidate only", i, b.ID, i+1)
		}
	}
}

func TestInProcessReady_LegacyAssigneeFallback(t *testing.T) {
	store := beads.NewMemStore()
	want := createReadyBead(t, store, "gascity/workflow-control", nil)

	got := runInProcessReadyForTest(t, store, "gascity/control-dispatcher", "", []string{"gascity/control-dispatcher"}, 20)
	if gotIDs(got) != want.ID {
		t.Fatalf("got IDs = %q, want %q", gotIDs(got), want.ID)
	}
}

func TestInProcessReady_RoutedToFallback_RequiresEmptyAssignee(t *testing.T) {
	store := beads.NewMemStore()
	createReadyBead(t, store, "someone-else", map[string]string{"gc.routed_to": "gascity/control-dispatcher"})
	want := createReadyBead(t, store, "", map[string]string{"gc.routed_to": "gascity/control-dispatcher"})

	got := runInProcessReadyForTest(t, store, "gascity/control-dispatcher", "", []string{"no-match"}, 20)
	if gotIDs(got) != want.ID {
		t.Fatalf("got IDs = %q, want only unassigned routed bead %q", gotIDs(got), want.ID)
	}
}

func TestInProcessReady_LegacyRoutedToFallback(t *testing.T) {
	store := beads.NewMemStore()
	want := createReadyBead(t, store, "", map[string]string{"gc.routed_to": "gascity/workflow-control"})

	got := runInProcessReadyForTest(t, store, "gascity/control-dispatcher", "gascity/workflow-control", []string{"no-match"}, 20)
	if gotIDs(got) != want.ID {
		t.Fatalf("got IDs = %q, want %q", gotIDs(got), want.ID)
	}
}

func TestInProcessReady_LimitPerStage(t *testing.T) {
	store := beads.NewMemStore()
	for i := 0; i < 25; i++ {
		createReadyBead(t, store, "A", nil)
	}

	got := runInProcessReadyForTest(t, store, "target", "", []string{"A"}, 7)
	if len(got) != 7 {
		t.Fatalf("len(got) = %d, want 7", len(got))
	}
}

func TestInProcessReady_UsesFilteredReadyQueries(t *testing.T) {
	store := &recordingReadyQueryStore{
		responses: [][]beads.Bead{
			nil,
			{{ID: "gc-legacy", Status: "open", Type: "task", Assignee: "gascity/workflow-control"}},
		},
	}
	prev := openControlStoreForReady
	openControlStoreForReady = func(string, string, *config.City) (beads.Store, error) {
		return store, nil
	}
	t.Cleanup(func() { openControlStoreForReady = prev })

	got, err := nextWorkflowServeBeadsInProcess(
		"/store",
		"/city",
		nil,
		"gascity/control-dispatcher",
		"gascity/workflow-control",
		[]string{"gascity/control-dispatcher"},
		20,
	)
	if err != nil {
		t.Fatalf("nextWorkflowServeBeadsInProcess: %v", err)
	}
	if gotIDs(got) != "gc-legacy" {
		t.Fatalf("got IDs = %q, want gc-legacy", gotIDs(got))
	}
	want := []beads.ReadyQuery{
		{Assignee: "gascity/control-dispatcher", Limit: 20},
		{Assignee: "gascity/workflow-control", Limit: 20},
	}
	if !slices.EqualFunc(store.queries, want, readyQueryEqual) {
		t.Fatalf("queries = %#v, want %#v", store.queries, want)
	}
}

func TestInProcessReady_StoreOpenError_Surfaces(t *testing.T) {
	openErr := errors.New("open failed")
	prev := openControlStoreForReady
	openControlStoreForReady = func(string, string, *config.City) (beads.Store, error) {
		return nil, openErr
	}
	t.Cleanup(func() { openControlStoreForReady = prev })

	_, err := nextWorkflowServeBeadsInProcess("/store", "/city", nil, "target", "", nil, 20)
	if !errors.Is(err, openErr) {
		t.Fatalf("err = %v, want %v", err, openErr)
	}
}

func TestInProcessReady_ReadyQueryError_Surfaces(t *testing.T) {
	readyErr := errors.New("ready failed")
	prev := openControlStoreForReady
	openControlStoreForReady = func(string, string, *config.City) (beads.Store, error) {
		return unavailableStore{err: readyErr}, nil
	}
	t.Cleanup(func() { openControlStoreForReady = prev })

	_, err := nextWorkflowServeBeadsInProcess("/store", "/city", nil, "target", "", nil, 20)
	if !errors.Is(err, readyErr) {
		t.Fatalf("err = %v, want %v", err, readyErr)
	}
}

type recordingReadyQueryStore struct {
	unavailableStore
	queries   []beads.ReadyQuery
	responses [][]beads.Bead
}

func (s *recordingReadyQueryStore) ReadyQuery(query beads.ReadyQuery) ([]beads.Bead, error) {
	s.queries = append(s.queries, query)
	if len(s.responses) == 0 {
		return nil, nil
	}
	next := s.responses[0]
	s.responses = s.responses[1:]
	return next, nil
}

func readyQueryEqual(a, b beads.ReadyQuery) bool {
	return a.Assignee == b.Assignee &&
		a.Unassigned == b.Unassigned &&
		a.Limit == b.Limit &&
		maps.Equal(a.Metadata, b.Metadata)
}

func TestDrainWorkflowServeWork_ControlAgentRoutesInProcess(t *testing.T) {
	prevInProcess := workflowServeListInProcess
	prevShell := workflowServeList
	prevControl := controlDispatcherServe
	t.Cleanup(func() {
		workflowServeListInProcess = prevInProcess
		workflowServeList = prevShell
		controlDispatcherServe = prevControl
	})

	inProcessCalls := 0
	workflowServeListInProcess = func(_, _ string, _ *config.City, _, _ string, _ []string, _ int) ([]hookBead, error) {
		inProcessCalls++
		return nil, nil
	}
	workflowServeList = func(string, string, map[string]string) ([]hookBead, error) {
		t.Fatal("shell workflowServeList called for control-dispatcher")
		return nil, nil
	}
	controlDispatcherServe = func(_, _, _ string, _ io.Writer, _ io.Writer) error {
		t.Fatal("controlDispatcherServe should not run with empty queue")
		return nil
	}

	_, err := drainWorkflowServeWork(config.Agent{Name: config.ControlDispatcherAgentName}, "/city", "/store", nil, "", nil, io.Discard)
	if err != nil {
		t.Fatalf("drainWorkflowServeWork: %v", err)
	}
	if inProcessCalls != 1 {
		t.Fatalf("inProcessCalls = %d, want 1", inProcessCalls)
	}
}

func TestDrainWorkflowServeWork_NonControlAgentUsesShell(t *testing.T) {
	prevInProcess := workflowServeListInProcess
	prevShell := workflowServeList
	prevControl := controlDispatcherServe
	t.Cleanup(func() {
		workflowServeListInProcess = prevInProcess
		workflowServeList = prevShell
		controlDispatcherServe = prevControl
	})

	shellCalls := 0
	workflowServeListInProcess = func(_, _ string, _ *config.City, _, _ string, _ []string, _ int) ([]hookBead, error) {
		t.Fatal("in-process path called for non-control agent")
		return nil, nil
	}
	workflowServeList = func(string, string, map[string]string) ([]hookBead, error) {
		shellCalls++
		return nil, nil
	}
	controlDispatcherServe = func(_, _, _ string, _ io.Writer, _ io.Writer) error {
		t.Fatal("controlDispatcherServe should not run with empty queue")
		return nil
	}

	_, err := drainWorkflowServeWork(config.Agent{Name: "worker", WorkQuery: "bd ready --assignee=worker"}, "/city", "/store", nil, "bd ready --assignee=worker", nil, io.Discard)
	if err != nil {
		t.Fatalf("drainWorkflowServeWork: %v", err)
	}
	if shellCalls != 1 {
		t.Fatalf("shellCalls = %d, want 1", shellCalls)
	}
}

func createReadyBead(t *testing.T, store beads.Store, assignee string, metadata map[string]string) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{Title: "ready", Metadata: metadata})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if assignee != "" {
		if err := store.Update(b.ID, beads.UpdateOpts{Assignee: &assignee}); err != nil {
			t.Fatalf("Update assignee: %v", err)
		}
	}
	got, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return got
}

func runInProcessReadyForTest(t *testing.T, store beads.Store, target, legacyTarget string, sessionVals []string, limit int) []hookBead {
	t.Helper()
	prev := openControlStoreForReady
	openControlStoreForReady = func(string, string, *config.City) (beads.Store, error) {
		return store, nil
	}
	t.Cleanup(func() { openControlStoreForReady = prev })

	got, err := nextWorkflowServeBeadsInProcess("/store", "/city", nil, target, legacyTarget, sessionVals, limit)
	if err != nil {
		t.Fatalf("nextWorkflowServeBeadsInProcess: %v", err)
	}
	return got
}

func gotIDs(beads []hookBead) string {
	ids := make([]string, 0, len(beads))
	for _, b := range beads {
		ids = append(ids, b.ID)
	}
	return strings.Join(ids, ",")
}
