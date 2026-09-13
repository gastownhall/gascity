package api

import (
	"testing"

	"github.com/gastownhall/gascity/internal/api/genclient"
)

func TestOrderHistoryFromGenList_Valid(t *testing.T) {
	rig := "frontend"
	items := []genclient.OrderHistoryEntry{
		{BeadId: "fe-1", Name: "dolt-health", ScopedName: "dolt-health:rig:frontend", Rig: &rig, CreatedAt: "2026-04-22T12:00:00Z"},
		{BeadId: "ca-2", Name: "dolt-health", ScopedName: "dolt-health", CreatedAt: "2026-04-22T13:00:00Z"},
	}
	body := &genclient.OrderHistoryListBody{Entries: &items}

	got := orderHistoryFromGenList(body)

	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	want0 := OrderHistoryView{
		BeadID:     "fe-1",
		Name:       "dolt-health",
		ScopedName: "dolt-health:rig:frontend",
		Rig:        "frontend",
		CreatedAt:  "2026-04-22T12:00:00Z",
	}
	if got[0] != want0 {
		t.Errorf("got[0] = %+v, want %+v", got[0], want0)
	}
	want1 := OrderHistoryView{
		BeadID:     "ca-2",
		Name:       "dolt-health",
		ScopedName: "dolt-health",
		CreatedAt:  "2026-04-22T13:00:00Z",
	}
	if got[1] != want1 {
		t.Errorf("got[1] = %+v, want %+v", got[1], want1)
	}
}

func TestOrderHistoryFromGenList_Empty(t *testing.T) {
	t.Run("nil body", func(t *testing.T) {
		got := orderHistoryFromGenList(nil)
		if got == nil {
			t.Fatal("want non-nil slice, got nil")
		}
		if len(got) != 0 {
			t.Errorf("len = %d, want 0", len(got))
		}
	})
	t.Run("nil entries", func(t *testing.T) {
		body := &genclient.OrderHistoryListBody{}
		got := orderHistoryFromGenList(body)
		if got == nil || len(got) != 0 {
			t.Errorf("got = %+v, want empty non-nil", got)
		}
	})
	t.Run("empty entries slice", func(t *testing.T) {
		items := []genclient.OrderHistoryEntry{}
		body := &genclient.OrderHistoryListBody{Entries: &items}
		got := orderHistoryFromGenList(body)
		if got == nil || len(got) != 0 {
			t.Errorf("got = %+v, want empty non-nil", got)
		}
	})
}

func TestOrderHistoryFromGenList_PartialMissingFields(t *testing.T) {
	// Rig is an optional pointer on the wire; missing rig must decode to
	// an empty string rather than panicking.
	items := []genclient.OrderHistoryEntry{
		{BeadId: "ca-7", Name: "cron-sweep", ScopedName: "cron-sweep", CreatedAt: "2026-04-22T14:00:00Z"},
	}
	body := &genclient.OrderHistoryListBody{Entries: &items}

	got := orderHistoryFromGenList(body)

	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].Rig != "" {
		t.Errorf("Rig = %q, want empty", got[0].Rig)
	}
	if got[0].BeadID != "ca-7" {
		t.Errorf("BeadID = %q, want ca-7", got[0].BeadID)
	}
}
