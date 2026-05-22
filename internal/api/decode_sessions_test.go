package api

import (
	"testing"

	"github.com/gastownhall/gascity/internal/api/genclient"
)

func TestSessionsFromGenList_Valid(t *testing.T) {
	alias := "mayor"
	lastActive := "2026-04-23T12:00:00Z"
	reason := "config"
	items := []genclient.SessionResponse{
		{
			Id:          "gc-abc",
			Template:    "mayor",
			State:       "active",
			Title:       "Overseer",
			SessionName: "mayor",
			CreatedAt:   "2026-04-23T10:00:00Z",
			Attached:    true,
			Running:     true,
			Alias:       &alias,
			LastActive:  &lastActive,
			Reason:      &reason,
		},
		{
			Id:          "gc-xyz",
			Template:    "polecat",
			State:       "closed",
			Title:       "Done",
			SessionName: "polecat-1",
			CreatedAt:   "2026-04-22T10:00:00Z",
		},
	}
	body := &genclient.ListBodySessionResponse{Items: &items, Total: int64(len(items))}

	got := sessionsFromGenList(body)

	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	want0 := SessionView{
		ID:          "gc-abc",
		Template:    "mayor",
		State:       "active",
		Reason:      "config",
		Title:       "Overseer",
		Alias:       "mayor",
		SessionName: "mayor",
		CreatedAt:   "2026-04-23T10:00:00Z",
		LastActive:  "2026-04-23T12:00:00Z",
		Attached:    true,
		Running:     true,
	}
	if got[0] != want0 {
		t.Errorf("got[0] = %+v\nwant    = %+v", got[0], want0)
	}
	want1 := SessionView{
		ID:          "gc-xyz",
		Template:    "polecat",
		State:       "closed",
		Title:       "Done",
		SessionName: "polecat-1",
		CreatedAt:   "2026-04-22T10:00:00Z",
	}
	if got[1] != want1 {
		t.Errorf("got[1] = %+v\nwant    = %+v", got[1], want1)
	}
}

func TestSessionsFromGenList_Empty(t *testing.T) {
	t.Run("nil body", func(t *testing.T) {
		got := sessionsFromGenList(nil)
		if got == nil {
			t.Fatal("want non-nil slice, got nil")
		}
		if len(got) != 0 {
			t.Errorf("len = %d, want 0", len(got))
		}
	})
	t.Run("nil items", func(t *testing.T) {
		body := &genclient.ListBodySessionResponse{}
		got := sessionsFromGenList(body)
		if got == nil || len(got) != 0 {
			t.Errorf("got = %+v, want empty non-nil", got)
		}
	})
	t.Run("empty items slice", func(t *testing.T) {
		items := []genclient.SessionResponse{}
		body := &genclient.ListBodySessionResponse{Items: &items}
		got := sessionsFromGenList(body)
		if got == nil || len(got) != 0 {
			t.Errorf("got = %+v, want empty non-nil", got)
		}
	})
}

func TestSessionsFromGenList_PartialMissingFields(t *testing.T) {
	items := []genclient.SessionResponse{
		{
			Id:          "gc-noopts",
			Template:    "agent",
			State:       "active",
			Title:       "",
			SessionName: "",
			CreatedAt:   "2026-04-23T00:00:00Z",
		},
	}
	body := &genclient.ListBodySessionResponse{Items: &items, Total: 1}

	got := sessionsFromGenList(body)

	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	v := got[0]
	if v.Alias != "" || v.LastActive != "" || v.Reason != "" || v.LastOutput != "" {
		t.Errorf("optional pointer fields must default to empty, got %+v", v)
	}
	if v.ID != "gc-noopts" || v.Template != "agent" {
		t.Errorf("required fields mismatch: %+v", v)
	}
}
