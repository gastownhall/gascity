package api

import (
	"testing"

	"github.com/gastownhall/gascity/internal/api/genclient"
)

func TestBeadsFromGenList_Valid(t *testing.T) {
	priority := int64(2)
	assignee := "builder-1"
	desc := "work here"
	labels := []string{"ready-to-build", "source:actual-pm"}
	items := []genclient.Bead{
		{Id: "gc-1", Title: "first", IssueType: "task", Status: "open", Assignee: &assignee, Priority: &priority, Description: &desc, Labels: &labels},
		{Id: "gc-2", Title: "second", IssueType: "task", Status: "closed"},
	}
	body := &genclient.ListBodyBead{Items: &items, Total: int64(len(items))}

	got := beadsFromGenList(body)

	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].ID != "gc-1" || got[0].Title != "first" || got[0].Type != "task" || got[0].Status != "open" {
		t.Errorf("got[0] = %+v", got[0])
	}
	if got[0].Assignee != "builder-1" {
		t.Errorf("got[0].Assignee = %q, want builder-1", got[0].Assignee)
	}
	if got[0].Priority == nil || *got[0].Priority != 2 {
		t.Errorf("got[0].Priority = %v, want *int(2)", got[0].Priority)
	}
	if got[0].Description != "work here" {
		t.Errorf("got[0].Description = %q", got[0].Description)
	}
	if len(got[0].Labels) != 2 || got[0].Labels[0] != "ready-to-build" {
		t.Errorf("got[0].Labels = %v", got[0].Labels)
	}
	if got[1].ID != "gc-2" || got[1].Status != "closed" || got[1].Assignee != "" {
		t.Errorf("got[1] = %+v", got[1])
	}
}

func TestBeadsFromGenList_Empty(t *testing.T) {
	t.Run("nil body", func(t *testing.T) {
		got := beadsFromGenList(nil)
		if got == nil {
			t.Fatal("want non-nil slice, got nil")
		}
		if len(got) != 0 {
			t.Errorf("len = %d, want 0", len(got))
		}
	})
	t.Run("nil items", func(t *testing.T) {
		body := &genclient.ListBodyBead{}
		got := beadsFromGenList(body)
		if got == nil || len(got) != 0 {
			t.Errorf("got = %+v, want empty non-nil", got)
		}
	})
	t.Run("empty items slice", func(t *testing.T) {
		items := []genclient.Bead{}
		body := &genclient.ListBodyBead{Items: &items}
		got := beadsFromGenList(body)
		if got == nil || len(got) != 0 {
			t.Errorf("got = %+v, want empty non-nil", got)
		}
	})
}

func TestBeadsFromGenList_PartialMissingFields(t *testing.T) {
	// A bead with optional pointer fields absent must decode to zero values
	// without panicking — mirrors the convoy decoder's partial-field test.
	items := []genclient.Bead{
		{Id: "gc-sparse", Title: "minimal", IssueType: "task", Status: "open"},
	}
	body := &genclient.ListBodyBead{Items: &items, Total: 1}

	got := beadsFromGenList(body)

	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	b := got[0]
	if b.ID != "gc-sparse" || b.Title != "minimal" || b.Status != "open" {
		t.Errorf("core fields: %+v", b)
	}
	if b.Assignee != "" {
		t.Errorf("Assignee = %q, want empty", b.Assignee)
	}
	if b.Priority != nil {
		t.Errorf("Priority = %v, want nil", b.Priority)
	}
	if b.Description != "" {
		t.Errorf("Description = %q, want empty", b.Description)
	}
	if b.Labels != nil {
		t.Errorf("Labels = %v, want nil", b.Labels)
	}
	if b.Metadata != nil {
		t.Errorf("Metadata = %v, want nil", b.Metadata)
	}
	if b.Dependencies != nil {
		t.Errorf("Dependencies = %v, want nil", b.Dependencies)
	}
}

func TestBeadFromGenPtr_Nil(t *testing.T) {
	b := beadFromGenPtr(nil)
	if b.ID != "" || b.Title != "" {
		t.Errorf("beadFromGenPtr(nil) = %+v, want zero value", b)
	}
}

func TestBeadFromGenPtr_Valid(t *testing.T) {
	g := &genclient.Bead{Id: "gc-99", Title: "ok", IssueType: "task", Status: "open"}
	b := beadFromGenPtr(g)
	if b.ID != "gc-99" || b.Title != "ok" || b.Type != "task" {
		t.Errorf("beadFromGenPtr = %+v", b)
	}
}
