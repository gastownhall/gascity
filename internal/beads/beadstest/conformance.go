// Package beadstest provides a conformance test suite for beads.Store
// implementations. Each implementation's test file calls RunStoreTests
// with its own factory function.
package beadstest

import (
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// RunStoreTests runs the full conformance suite against a Store implementation.
// The newStore function must return a fresh, empty store for each call.
func RunStoreTests(t *testing.T, newStore func() beads.Store) {
	t.Helper()

	t.Run("CreateAssignsUniqueNonEmptyID", func(t *testing.T) {
		s := newStore()
		b1, err := s.Create(beads.Bead{Title: "first"})
		if err != nil {
			t.Fatal(err)
		}
		b2, err := s.Create(beads.Bead{Title: "second"})
		if err != nil {
			t.Fatal(err)
		}
		if b1.ID == "" {
			t.Error("first bead ID is empty")
		}
		if b2.ID == "" {
			t.Error("second bead ID is empty")
		}
		if b1.ID == b2.ID {
			t.Errorf("bead IDs are not unique: both %q", b1.ID)
		}
	})

	t.Run("CreateSetsStatusOpen", func(t *testing.T) {
		s := newStore()
		b, err := s.Create(beads.Bead{Title: "test"})
		if err != nil {
			t.Fatal(err)
		}
		if b.Status != "open" {
			t.Errorf("Status = %q, want %q", b.Status, "open")
		}
	})

	t.Run("CreateDefaultsTypeToTask", func(t *testing.T) {
		s := newStore()
		b, err := s.Create(beads.Bead{Title: "test"})
		if err != nil {
			t.Fatal(err)
		}
		if b.Type != "task" {
			t.Errorf("Type = %q, want %q", b.Type, "task")
		}
	})

	t.Run("CreatePreservesExplicitType", func(t *testing.T) {
		s := newStore()
		b, err := s.Create(beads.Bead{Title: "test", Type: "bug"})
		if err != nil {
			t.Fatal(err)
		}
		if b.Type != "bug" {
			t.Errorf("Type = %q, want %q", b.Type, "bug")
		}
	})

	t.Run("CreateSetsCreatedAt", func(t *testing.T) {
		s := newStore()
		b, err := s.Create(beads.Bead{Title: "test"})
		if err != nil {
			t.Fatal(err)
		}
		// Sanity check: CreatedAt should be recent (within 1 hour).
		// We use a wide window because external stores have second-precision
		// timestamps with rounding, and timezone handling can vary.
		if time.Since(b.CreatedAt).Abs() > time.Hour {
			t.Errorf("CreatedAt = %v, want within 1 hour of now (%v)", b.CreatedAt, time.Now())
		}
	})

	t.Run("CreatePreservesTitle", func(t *testing.T) {
		s := newStore()
		b, err := s.Create(beads.Bead{Title: "Build a Tower of Hanoi app"})
		if err != nil {
			t.Fatal(err)
		}
		if b.Title != "Build a Tower of Hanoi app" {
			t.Errorf("Title = %q, want %q", b.Title, "Build a Tower of Hanoi app")
		}
	})

	t.Run("CreateAssigneeRoundTrips", func(t *testing.T) {
		s := newStore()
		created, err := s.Create(beads.Bead{Title: "test", Assignee: "mayor"})
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.Get(created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Assignee != "mayor" {
			t.Errorf("Assignee = %q after Get, want %q from Create", got.Assignee, "mayor")
		}
	})

	t.Run("CreateFromRoundTrips", func(t *testing.T) {
		s := newStore()
		created, err := s.Create(beads.Bead{Title: "test", From: "priya"})
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.Get(created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.From != "priya" {
			t.Errorf("From = %q after Get, want %q from Create", got.From, "priya")
		}
	})

	t.Run("GetExistingBead", func(t *testing.T) {
		s := newStore()
		created, err := s.Create(beads.Bead{Title: "round trip", Type: "bug"})
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.Get(created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != created.ID {
			t.Errorf("ID = %q, want %q", got.ID, created.ID)
		}
		if got.Title != created.Title {
			t.Errorf("Title = %q, want %q", got.Title, created.Title)
		}
		if got.Status != created.Status {
			t.Errorf("Status = %q, want %q", got.Status, created.Status)
		}
		if got.Type != created.Type {
			t.Errorf("Type = %q, want %q", got.Type, created.Type)
		}
		// Wide tolerance: dolt stores at second precision with rounding,
		// so create vs show can differ. Just verify it round-trips close.
		if got.CreatedAt.Sub(created.CreatedAt).Abs() > time.Hour {
			t.Errorf("CreatedAt = %v, want within 1h of %v", got.CreatedAt, created.CreatedAt)
		}
		if got.Assignee != created.Assignee {
			t.Errorf("Assignee = %q, want %q", got.Assignee, created.Assignee)
		}
	})

	t.Run("GetNotFound", func(t *testing.T) {
		s := newStore()
		// Create one bead so the store isn't empty, then look up a wrong ID.
		if _, err := s.Create(beads.Bead{Title: "exists"}); err != nil {
			t.Fatal(err)
		}
		_, err := s.Get("nonexistent-999")
		if err == nil {
			t.Fatal("Get(nonexistent-999) should return error")
		}
		if !errors.Is(err, beads.ErrNotFound) {
			t.Errorf("error = %v, want ErrNotFound", err)
		}
	})

	t.Run("GetNotFoundEmptyStore", func(t *testing.T) {
		s := newStore()
		_, err := s.Get("nonexistent-999")
		if err == nil {
			t.Fatal("Get on empty store should return error")
		}
		if !errors.Is(err, beads.ErrNotFound) {
			t.Errorf("error = %v, want ErrNotFound", err)
		}
	})

	t.Run("CloseSuccess", func(t *testing.T) {
		s := newStore()
		b, err := s.Create(beads.Bead{Title: "closeable"})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Close(b.ID); err != nil {
			t.Fatal(err)
		}
		got, err := s.Get(b.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "closed" {
			t.Errorf("Status = %q, want %q", got.Status, "closed")
		}
	})

	t.Run("CloseNotFound", func(t *testing.T) {
		s := newStore()
		err := s.Close("nonexistent-999")
		if err == nil {
			t.Fatal("Close(nonexistent-999) should return error")
		}
		if !errors.Is(err, beads.ErrNotFound) {
			t.Errorf("error = %v, want ErrNotFound", err)
		}
	})

	t.Run("CloseIdempotent", func(t *testing.T) {
		s := newStore()
		b, err := s.Create(beads.Bead{Title: "close twice"})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Close(b.ID); err != nil {
			t.Fatal(err)
		}
		// Closing again should succeed (no-op).
		if err := s.Close(b.ID); err != nil {
			t.Errorf("second Close returned error: %v", err)
		}
		got, err := s.Get(b.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "closed" {
			t.Errorf("Status = %q, want %q", got.Status, "closed")
		}
	})

	t.Run("CloseRemovesFromReady", func(t *testing.T) {
		s := newStore()
		b1, err := s.Create(beads.Bead{Title: "first"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Create(beads.Bead{Title: "second"}); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(b1.ID); err != nil {
			t.Fatal(err)
		}
		ready, err := s.Ready()
		if err != nil {
			t.Fatal(err)
		}
		if len(ready) != 1 {
			t.Fatalf("Ready() returned %d beads, want 1", len(ready))
		}
		if ready[0].Title != "second" {
			t.Errorf("ready[0].Title = %q, want %q", ready[0].Title, "second")
		}
	})

	t.Run("ListReturnsAllBeads", func(t *testing.T) {
		s := newStore()
		_, err := s.Create(beads.Bead{Title: "first"})
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.Create(beads.Bead{Title: "second"})
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.ListOpen()
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("List() returned %d beads, want 2", len(got))
		}
		titles := titlesOf(got)
		if !containsAll(titles, "first", "second") {
			t.Errorf("List() titles = %v, want [first second]", titles)
		}
	})

	t.Run("ListEmptyStore", func(t *testing.T) {
		s := newStore()
		got, err := s.ListOpen()
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("List() on empty store returned %d beads, want 0", len(got))
		}
	})

	t.Run("ListCount", func(t *testing.T) {
		s := newStore()
		for _, title := range []string{"alpha", "beta", "gamma"} {
			if _, err := s.Create(beads.Bead{Title: title}); err != nil {
				t.Fatal(err)
			}
		}
		got, err := s.ListOpen()
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 3 {
			t.Fatalf("List() returned %d beads, want 3", len(got))
		}
		titles := titlesOf(got)
		if !containsAll(titles, "alpha", "beta", "gamma") {
			t.Errorf("List() titles = %v, want [alpha beta gamma]", titles)
		}
	})

	t.Run("ReadyReturnsOpenBeads", func(t *testing.T) {
		s := newStore()
		_, err := s.Create(beads.Bead{Title: "first"})
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.Create(beads.Bead{Title: "second"})
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.Ready()
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("Ready() returned %d beads, want 2", len(got))
		}
		titles := titlesOf(got)
		if !containsAll(titles, "first", "second") {
			t.Errorf("Ready() titles = %v, want [first second]", titles)
		}
	})

	t.Run("UpdateDescription", func(t *testing.T) {
		s := newStore()
		b, err := s.Create(beads.Bead{Title: "updatable", Description: "original"})
		if err != nil {
			t.Fatal(err)
		}
		newDesc := "updated description"
		if err := s.Update(b.ID, beads.UpdateOpts{Description: &newDesc}); err != nil {
			t.Fatal(err)
		}
		got, err := s.Get(b.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Description != "updated description" {
			t.Errorf("Description = %q, want %q", got.Description, "updated description")
		}
	})

	t.Run("UpdateNotFound", func(t *testing.T) {
		s := newStore()
		desc := "whatever"
		err := s.Update("nonexistent-999", beads.UpdateOpts{Description: &desc})
		if err == nil {
			t.Fatal("Update(nonexistent) should return error")
		}
		if !errors.Is(err, beads.ErrNotFound) {
			t.Errorf("error = %v, want ErrNotFound", err)
		}
	})

	t.Run("UpdateNilField", func(t *testing.T) {
		s := newStore()
		b, err := s.Create(beads.Bead{Title: "keep desc", Description: "original"})
		if err != nil {
			t.Fatal(err)
		}
		// Update with nil Description — should leave field unchanged.
		if err := s.Update(b.ID, beads.UpdateOpts{}); err != nil {
			t.Fatal(err)
		}
		got, err := s.Get(b.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Description != "original" {
			t.Errorf("Description = %q, want %q (unchanged)", got.Description, "original")
		}
	})

	t.Run("ChildrenEmpty", func(t *testing.T) {
		s := newStore()
		children, err := s.Children("nonexistent")
		if err != nil {
			t.Fatal(err)
		}
		if len(children) != 0 {
			t.Errorf("Children(nonexistent) returned %d beads, want 0", len(children))
		}
	})

	t.Run("ChildrenReturnsMatching", func(t *testing.T) {
		s := newStore()
		parent, err := s.Create(beads.Bead{Title: "parent", Type: "molecule"})
		if err != nil {
			t.Fatal(err)
		}
		c1, err := s.Create(beads.Bead{Title: "child-1", ParentID: parent.ID})
		if err != nil {
			t.Fatal(err)
		}
		c2, err := s.Create(beads.Bead{Title: "child-2", ParentID: parent.ID})
		if err != nil {
			t.Fatal(err)
		}
		// Unrelated bead — should not appear.
		if _, err := s.Create(beads.Bead{Title: "other"}); err != nil {
			t.Fatal(err)
		}

		children, err := s.Children(parent.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(children) != 2 {
			t.Fatalf("Children returned %d beads, want 2", len(children))
		}
		if children[0].ID != c1.ID {
			t.Errorf("children[0].ID = %q, want %q", children[0].ID, c1.ID)
		}
		if children[1].ID != c2.ID {
			t.Errorf("children[1].ID = %q, want %q", children[1].ID, c2.ID)
		}
	})

	t.Run("ChildrenWrongParent", func(t *testing.T) {
		s := newStore()
		p1, err := s.Create(beads.Bead{Title: "parent-1"})
		if err != nil {
			t.Fatal(err)
		}
		p2, err := s.Create(beads.Bead{Title: "parent-2"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Create(beads.Bead{Title: "child-of-1", ParentID: p1.ID}); err != nil {
			t.Fatal(err)
		}

		children, err := s.Children(p2.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(children) != 0 {
			t.Errorf("Children(p2) returned %d beads, want 0", len(children))
		}
	})

	t.Run("ReadyEmptyStore", func(t *testing.T) {
		s := newStore()
		got, err := s.Ready()
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("Ready() on empty store returned %d beads, want 0", len(got))
		}
	})

	t.Run("ReadyCount", func(t *testing.T) {
		s := newStore()
		for _, title := range []string{"alpha", "beta", "gamma"} {
			if _, err := s.Create(beads.Bead{Title: title}); err != nil {
				t.Fatal(err)
			}
		}
		got, err := s.Ready()
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 3 {
			t.Fatalf("Ready() returned %d beads, want 3", len(got))
		}
		titles := titlesOf(got)
		if !containsAll(titles, "alpha", "beta", "gamma") {
			t.Errorf("Ready() titles = %v, want [alpha beta gamma]", titles)
		}
	})

	t.Run("ListByLabelMatch", func(t *testing.T) {
		s := newStore()
		if _, err := s.Create(beads.Bead{Title: "no-label"}); err != nil {
			t.Fatal(err)
		}
		b2, err := s.Create(beads.Bead{Title: "labeled", Labels: []string{"role:worker"}})
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.ListByLabel("role:worker", 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("ListByLabel returned %d beads, want 1", len(got))
		}
		if got[0].ID != b2.ID {
			t.Errorf("got[0].ID = %q, want %q", got[0].ID, b2.ID)
		}
	})

	t.Run("ListByLabelNoMatch", func(t *testing.T) {
		s := newStore()
		if _, err := s.Create(beads.Bead{Title: "other", Labels: []string{"role:worker"}}); err != nil {
			t.Fatal(err)
		}
		got, err := s.ListByLabel("role:mayor", 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("ListByLabel returned %d beads, want 0", len(got))
		}
	})

	t.Run("ListByLabelLimit", func(t *testing.T) {
		s := newStore()
		for _, title := range []string{"a", "b", "c"} {
			if _, err := s.Create(beads.Bead{Title: title, Labels: []string{"batch:1"}}); err != nil {
				t.Fatal(err)
			}
		}
		got, err := s.ListByLabel("batch:1", 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("ListByLabel with limit 2 returned %d beads, want 2", len(got))
		}
	})

	t.Run("ListByLabelEmpty", func(t *testing.T) {
		s := newStore()
		got, err := s.ListByLabel("anything", 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("ListByLabel on empty store returned %d beads, want 0", len(got))
		}
	})
}

// RunMetadataTests runs conformance tests for metadata absent-vs-empty
// semantics. Call this only for Store implementations that preserve
// empty-string metadata values (MemStore, BdStore). External script-backed
// stores (ExecStore) may not preserve this invariant.
func RunMetadataTests(t *testing.T, newStore func() beads.Store) {
	t.Helper()

	t.Run("MetadataAbsentVsEmpty", func(t *testing.T) {
		s := newStore()
		b, err := s.Create(beads.Bead{Title: "test"})
		if err != nil {
			t.Fatal(err)
		}
		// Set metadata to empty string.
		if err := s.SetMetadata(b.ID, "key", ""); err != nil {
			t.Fatal(err)
		}
		got, err := s.Get(b.ID)
		if err != nil {
			t.Fatal(err)
		}
		// Key present with empty value — comma-ok must distinguish from absent.
		val, ok := got.Metadata["key"]
		if !ok {
			t.Fatal("Metadata[\"key\"] absent, want present with empty value")
		}
		if val != "" {
			t.Errorf("Metadata[\"key\"] = %q, want empty string", val)
		}
		// Absent key must return !ok.
		_, ok = got.Metadata["nonexistent"]
		if ok {
			t.Error("Metadata[\"nonexistent\"] present, want absent")
		}
	})
}

// RunSequentialIDTests runs tests that assert gc-N sequential IDs. Call this
// only for Store implementations that use sequential IDs (MemStore, FileStore).
func RunSequentialIDTests(t *testing.T, newStore func() beads.Store) {
	t.Helper()

	t.Run("CreateAssignsSequentialID", func(t *testing.T) {
		s := newStore()
		b1, err := s.Create(beads.Bead{Title: "first"})
		if err != nil {
			t.Fatal(err)
		}
		b2, err := s.Create(beads.Bead{Title: "second"})
		if err != nil {
			t.Fatal(err)
		}
		if b1.ID != "gc-1" {
			t.Errorf("first bead ID = %q, want %q", b1.ID, "gc-1")
		}
		if b2.ID != "gc-2" {
			t.Errorf("second bead ID = %q, want %q", b2.ID, "gc-2")
		}
	})
}

// RunCreationOrderTests runs tests that assert List/Ready return beads in
// creation order. Only valid for in-process stores (MemStore, FileStore)
// where creation order can be tracked with sub-second precision.
func RunCreationOrderTests(t *testing.T, newStore func() beads.Store) {
	t.Helper()

	t.Run("ListOrder", func(t *testing.T) {
		s := newStore()
		for _, title := range []string{"alpha", "beta", "gamma"} {
			if _, err := s.Create(beads.Bead{Title: title}); err != nil {
				t.Fatal(err)
			}
		}
		got, err := s.ListOpen()
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 3 {
			t.Fatalf("List() returned %d beads, want 3", len(got))
		}
		want := []string{"alpha", "beta", "gamma"}
		for i, w := range want {
			if got[i].Title != w {
				t.Errorf("got[%d].Title = %q, want %q", i, got[i].Title, w)
			}
		}
	})

	t.Run("ReadyOrder", func(t *testing.T) {
		s := newStore()
		for _, title := range []string{"alpha", "beta", "gamma"} {
			if _, err := s.Create(beads.Bead{Title: title}); err != nil {
				t.Fatal(err)
			}
		}
		got, err := s.Ready()
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 3 {
			t.Fatalf("Ready() returned %d beads, want 3", len(got))
		}
		want := []string{"alpha", "beta", "gamma"}
		for i, w := range want {
			if got[i].Title != w {
				t.Errorf("got[%d].Title = %q, want %q", i, got[i].Title, w)
			}
		}
	})
}

// RunDepTests runs conformance tests for dependency operations.
func RunDepTests(t *testing.T, newStore func() beads.Store) {
	t.Helper()

	t.Run("DepAddAndListDown", func(t *testing.T) {
		s := newStore()
		if err := s.DepAdd("a", "b", "blocks"); err != nil {
			t.Fatal(err)
		}
		deps, err := s.DepList("a", "down")
		if err != nil {
			t.Fatal(err)
		}
		if len(deps) != 1 {
			t.Fatalf("DepList(a, down) = %d deps, want 1", len(deps))
		}
		if deps[0].DependsOnID != "b" {
			t.Errorf("dep.DependsOnID = %q, want %q", deps[0].DependsOnID, "b")
		}
	})

	t.Run("DepAddIdempotent", func(t *testing.T) {
		s := newStore()
		if err := s.DepAdd("a", "b", "blocks"); err != nil {
			t.Fatal(err)
		}
		if err := s.DepAdd("a", "b", "blocks"); err != nil {
			t.Fatal(err)
		}
		deps, _ := s.DepList("a", "down")
		if len(deps) != 1 {
			t.Errorf("DepList after duplicate DepAdd = %d deps, want 1", len(deps))
		}
	})

	t.Run("DepAddUpdatesType", func(t *testing.T) {
		s := newStore()
		if err := s.DepAdd("a", "b", "blocks"); err != nil {
			t.Fatal(err)
		}
		if err := s.DepAdd("a", "b", "tracks"); err != nil {
			t.Fatal(err)
		}
		deps, _ := s.DepList("a", "down")
		if len(deps) != 1 {
			t.Fatalf("DepList after type update = %d deps, want 1", len(deps))
		}
		if deps[0].Type != "tracks" {
			t.Errorf("dep.Type = %q, want %q", deps[0].Type, "tracks")
		}
	})

	t.Run("DepListUp", func(t *testing.T) {
		s := newStore()
		if err := s.DepAdd("a", "b", "blocks"); err != nil {
			t.Fatal(err)
		}
		deps, err := s.DepList("b", "up")
		if err != nil {
			t.Fatal(err)
		}
		if len(deps) != 1 {
			t.Fatalf("DepList(b, up) = %d deps, want 1", len(deps))
		}
		if deps[0].IssueID != "a" {
			t.Errorf("dep.IssueID = %q, want %q", deps[0].IssueID, "a")
		}
	})

	t.Run("DepRemove", func(t *testing.T) {
		s := newStore()
		_ = s.DepAdd("a", "b", "blocks")
		_ = s.DepAdd("a", "c", "blocks")
		if err := s.DepRemove("a", "b"); err != nil {
			t.Fatal(err)
		}
		deps, _ := s.DepList("a", "down")
		if len(deps) != 1 {
			t.Fatalf("DepList after remove = %d deps, want 1", len(deps))
		}
		if deps[0].DependsOnID != "c" {
			t.Errorf("remaining dep = %q, want %q", deps[0].DependsOnID, "c")
		}
	})

	t.Run("DepRemoveNonexistent", func(t *testing.T) {
		s := newStore()
		if err := s.DepRemove("x", "y"); err != nil {
			t.Errorf("DepRemove nonexistent should not error: %v", err)
		}
	})

	t.Run("DepListEmpty", func(t *testing.T) {
		s := newStore()
		deps, err := s.DepList("nonexistent", "down")
		if err != nil {
			t.Fatal(err)
		}
		if len(deps) != 0 {
			t.Errorf("DepList on empty store = %d deps, want 0", len(deps))
		}
	})

	t.Run("SetMetadataBatch", func(t *testing.T) {
		s := newStore()
		b, err := s.Create(beads.Bead{Title: "batch-test"})
		if err != nil {
			t.Fatal(err)
		}
		kvs := map[string]string{
			"key1": "value1",
			"key2": "value2",
			"key3": "value3",
		}
		if err := s.SetMetadataBatch(b.ID, kvs); err != nil {
			t.Fatalf("SetMetadataBatch: %v", err)
		}
		got, err := s.Get(b.ID)
		if err != nil {
			t.Fatal(err)
		}
		for k, want := range kvs {
			if got.Metadata[k] != want {
				t.Errorf("Metadata[%q] = %q, want %q", k, got.Metadata[k], want)
			}
		}
	})

	t.Run("SetMetadataBatchNotFound", func(t *testing.T) {
		s := newStore()
		err := s.SetMetadataBatch("nonexistent-999", map[string]string{"k": "v"})
		if err == nil {
			t.Fatal("SetMetadataBatch on nonexistent bead should return error")
		}
		if !errors.Is(err, beads.ErrNotFound) {
			t.Errorf("error = %v, want wrapped ErrNotFound", err)
		}
	})

	t.Run("SetMetadataBatchEmpty", func(t *testing.T) {
		s := newStore()
		b, err := s.Create(beads.Bead{Title: "empty-batch"})
		if err != nil {
			t.Fatal(err)
		}
		// Empty batch should succeed without error.
		if err := s.SetMetadataBatch(b.ID, map[string]string{}); err != nil {
			t.Fatalf("SetMetadataBatch with empty map: %v", err)
		}
	})

	t.Run("Ping", func(t *testing.T) {
		s := newStore()
		if err := s.Ping(); err != nil {
			t.Fatalf("Ping on fresh store should succeed: %v", err)
		}
	})
}

// titlesOf extracts titles from a slice of beads.
func titlesOf(bs []beads.Bead) []string {
	titles := make([]string, len(bs))
	for i, b := range bs {
		titles[i] = b.Title
	}
	sort.Strings(titles)
	return titles
}

// containsAll checks that sorted has all the expected values.
func containsAll(sorted []string, want ...string) bool {
	expected := make([]string, len(want))
	copy(expected, want)
	sort.Strings(expected)
	if len(sorted) != len(expected) {
		return false
	}
	for i := range sorted {
		if sorted[i] != expected[i] {
			return false
		}
	}
	return true
}
