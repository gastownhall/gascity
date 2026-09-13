package api

import (
	"testing"

	"github.com/gastownhall/gascity/internal/api/genclient"
)

func TestRigsFromGenList_Valid(t *testing.T) {
	prefix := "fe"
	items := []genclient.RigResponse{
		{Name: "frontend", Path: "/abs/frontend", Prefix: &prefix, Suspended: false},
		{Name: "backend", Path: "/abs/backend", Suspended: true},
	}
	body := &genclient.ListBodyRigResponse{Items: &items, Total: int64(len(items))}

	got := rigsFromGenList(body)

	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0] != (RigView{Name: "frontend", Path: "/abs/frontend", Prefix: "fe"}) {
		t.Errorf("got[0] = %+v", got[0])
	}
	if got[1] != (RigView{Name: "backend", Path: "/abs/backend", Suspended: true}) {
		t.Errorf("got[1] = %+v", got[1])
	}
}

func TestRigsFromGenList_Empty(t *testing.T) {
	t.Run("nil body", func(t *testing.T) {
		got := rigsFromGenList(nil)
		if got == nil {
			t.Fatal("want non-nil slice, got nil")
		}
		if len(got) != 0 {
			t.Errorf("len = %d, want 0", len(got))
		}
	})
	t.Run("nil items", func(t *testing.T) {
		body := &genclient.ListBodyRigResponse{}
		got := rigsFromGenList(body)
		if got == nil || len(got) != 0 {
			t.Errorf("got = %+v, want empty non-nil", got)
		}
	})
	t.Run("empty items slice", func(t *testing.T) {
		items := []genclient.RigResponse{}
		body := &genclient.ListBodyRigResponse{Items: &items}
		got := rigsFromGenList(body)
		if got == nil || len(got) != 0 {
			t.Errorf("got = %+v, want empty non-nil", got)
		}
	})
}

func TestRigsFromGenList_PartialMissingFields(t *testing.T) {
	// A rig with Prefix nil must decode to RigView with empty Prefix, not
	// panic. Mirrors the wire shape where omitempty=optional pointers.
	items := []genclient.RigResponse{
		{Name: "noprefix", Path: "/abs/noprefix"},
	}
	body := &genclient.ListBodyRigResponse{Items: &items, Total: 1}

	got := rigsFromGenList(body)

	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].Prefix != "" {
		t.Errorf("Prefix = %q, want empty", got[0].Prefix)
	}
	if got[0].Name != "noprefix" {
		t.Errorf("Name = %q, want noprefix", got[0].Name)
	}
}
