package api

import (
	"testing"

	"github.com/gastownhall/gascity/internal/api/genclient"
)

func TestConvoysFromGenList_Valid(t *testing.T) {
	owner := "mayor"
	items := []genclient.Bead{
		{Id: "gc-1", Title: "deploy v1", IssueType: "convoy", Status: "open", Assignee: &owner},
		{Id: "gc-2", Title: "deploy v2", IssueType: "convoy", Status: "open"},
	}
	body := &genclient.ListBodyBead{Items: &items, Total: int64(len(items))}

	got := convoysFromGenList(body)

	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].ID != "gc-1" || got[0].Title != "deploy v1" || got[0].Type != "convoy" || got[0].Assignee != "mayor" {
		t.Errorf("got[0] = %+v", got[0])
	}
	if got[1].ID != "gc-2" || got[1].Assignee != "" {
		t.Errorf("got[1] = %+v", got[1])
	}
}

func TestConvoysFromGenList_Empty(t *testing.T) {
	t.Run("nil body", func(t *testing.T) {
		got := convoysFromGenList(nil)
		if got == nil {
			t.Fatal("want non-nil slice, got nil")
		}
		if len(got) != 0 {
			t.Errorf("len = %d, want 0", len(got))
		}
	})
	t.Run("nil items", func(t *testing.T) {
		body := &genclient.ListBodyBead{}
		got := convoysFromGenList(body)
		if got == nil || len(got) != 0 {
			t.Errorf("got = %+v, want empty non-nil", got)
		}
	})
	t.Run("empty items slice", func(t *testing.T) {
		items := []genclient.Bead{}
		body := &genclient.ListBodyBead{Items: &items}
		got := convoysFromGenList(body)
		if got == nil || len(got) != 0 {
			t.Errorf("got = %+v, want empty non-nil", got)
		}
	})
}

func TestConvoysFromGenList_PartialMissingFields(t *testing.T) {
	// A convoy bead with optional pointer fields absent must decode
	// without panic to the zero values on the destination struct.
	items := []genclient.Bead{
		{Id: "gc-1", Title: "bare", IssueType: "convoy", Status: "open"},
	}
	body := &genclient.ListBodyBead{Items: &items, Total: 1}

	got := convoysFromGenList(body)

	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].Assignee != "" || got[0].From != "" || got[0].ParentID != "" || got[0].Priority != nil {
		t.Errorf("got[0] = %+v, want zero-value optionals", got[0])
	}
}

func TestConvoyStatusFromGen_Valid(t *testing.T) {
	convoy := genclient.Bead{Id: "gc-1", Title: "deploy", IssueType: "convoy", Status: "open"}
	children := []genclient.Bead{
		{Id: "gc-2", Title: "child a", Status: "closed"},
		{Id: "gc-3", Title: "child b", Status: "open"},
	}
	prog := genclient.ConvoyProgress{Total: 2, Closed: 1}
	resp := &genclient.ConvoyGetResponse{Convoy: &convoy, Children: &children, Progress: &prog}

	got := convoyStatusFromGen(resp)

	if got.Convoy.ID != "gc-1" || got.Convoy.Title != "deploy" {
		t.Errorf("Convoy = %+v, want gc-1/deploy", got.Convoy)
	}
	if len(got.Children) != 2 {
		t.Fatalf("len(Children) = %d, want 2", len(got.Children))
	}
	if got.Children[0].ID != "gc-2" || got.Children[1].ID != "gc-3" {
		t.Errorf("Children = %+v", got.Children)
	}
	if got.Progress.Total != 2 || got.Progress.Closed != 1 {
		t.Errorf("Progress = %+v, want {2, 1}", got.Progress)
	}
}

func TestConvoyStatusFromGen_Empty(t *testing.T) {
	t.Run("nil response", func(t *testing.T) {
		got := convoyStatusFromGen(nil)
		if got.Convoy.ID != "" || len(got.Children) != 0 || got.Progress.Total != 0 {
			t.Errorf("got = %+v, want zero-value view with empty children slice", got)
		}
		if got.Children == nil {
			t.Error("Children must be non-nil empty slice")
		}
	})
	t.Run("empty fields", func(t *testing.T) {
		got := convoyStatusFromGen(&genclient.ConvoyGetResponse{})
		if got.Convoy.ID != "" {
			t.Errorf("Convoy.ID = %q, want empty", got.Convoy.ID)
		}
		if len(got.Children) != 0 {
			t.Errorf("Children len = %d, want 0", len(got.Children))
		}
		if got.Progress.Total != 0 || got.Progress.Closed != 0 {
			t.Errorf("Progress = %+v, want zero", got.Progress)
		}
	})
}

func TestConvoyStatusFromGen_PartialMissingFields(t *testing.T) {
	// Workflow-branch response has no convoy body; simple-convoy branch has
	// no workflow fields. Translator must tolerate either.
	convoy := genclient.Bead{Id: "gc-7", Title: "solo", IssueType: "convoy", Status: "open"}
	resp := &genclient.ConvoyGetResponse{Convoy: &convoy}

	got := convoyStatusFromGen(resp)

	if got.Convoy.ID != "gc-7" {
		t.Errorf("Convoy.ID = %q, want gc-7", got.Convoy.ID)
	}
	if got.Children == nil {
		t.Error("Children must be non-nil empty slice")
	}
	if len(got.Children) != 0 {
		t.Errorf("Children len = %d, want 0", len(got.Children))
	}
	if got.Progress.Total != 0 {
		t.Errorf("Progress.Total = %d, want 0", got.Progress.Total)
	}
}

func TestConvoyCheckFromGen_Valid(t *testing.T) {
	resp := &genclient.ConvoyCheckResponse{
		ConvoyId: "gc-1",
		Total:    4,
		Closed:   4,
		Complete: true,
	}
	got := convoyCheckFromGen(resp)
	if got.ConvoyID != "gc-1" || got.Total != 4 || got.Closed != 4 || !got.Complete {
		t.Errorf("got = %+v, want {gc-1, 4, 4, true}", got)
	}
}

func TestConvoyCheckFromGen_Empty(t *testing.T) {
	got := convoyCheckFromGen(nil)
	if got != (ConvoyCheckView{}) {
		t.Errorf("got = %+v, want zero-value", got)
	}
}

func TestConvoyCheckFromGen_PartialMissingFields(t *testing.T) {
	// Incomplete convoy: Complete=false, Closed < Total.
	resp := &genclient.ConvoyCheckResponse{
		ConvoyId: "gc-2",
		Total:    3,
		Closed:   1,
		Complete: false,
	}
	got := convoyCheckFromGen(resp)
	if got.Complete {
		t.Errorf("Complete = true, want false")
	}
	if got.Total != 3 || got.Closed != 1 {
		t.Errorf("got = %+v, want {Total:3, Closed:1}", got)
	}
}
