package api

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api/genclient"
)

func TestStatusViewFromGen_NilReturnsZero(t *testing.T) {
	got := statusViewFromGen(nil)
	if got.CityName != "" || got.CityPath != "" {
		t.Fatalf("statusViewFromGen(nil) = %+v, want zero value", got)
	}
}

func TestStatusViewFromGen_ValidResponse(t *testing.T) {
	version := "v0.1.0"
	sessionName := "city--mayor"
	groupName := "mayor"
	scaleLabel := "scaled (min=0, max=3)"
	expanded := true
	preflightGate := "metadata_backend"
	preflightReason := "metadata backend=file; native store requires dolt"
	body := &genclient.StatusBody{
		Name:      "bright-lights",
		Path:      "/home/u/bright-lights",
		Version:   &version,
		UptimeSec: 42,
		Suspended: false,
		Agents:    genclient.StatusAgentCounts{Total: 2, Running: 1},
		Rigs:      genclient.StatusRigCounts{Total: 1, Suspended: 0},
		Work:      genclient.StatusWorkCounts{},
		Mail:      genclient.StatusMailCounts{},
		AgentDetails: &[]genclient.StatusAgentDetail{
			{
				Name:          "mayor",
				QualifiedName: "mayor",
				Scope:         "city",
				Running:       true,
				Suspended:     false,
				SessionName:   &sessionName,
				GroupName:     &groupName,
				ScaleLabel:    &scaleLabel,
				Expanded:      &expanded,
			},
		},
		RigDetails: &[]genclient.StatusRigDetail{
			{Name: "myrig", Path: "/home/u/myrig", Suspended: false},
		},
		NamedSessionDetails: &[]genclient.StatusNamedSessionDetail{
			{Identity: "myrig/worker", Status: "materialized", Mode: "always"},
		},
		SessionCountsDetail: &genclient.StatusSessionCountsDetail{Active: 3, Suspended: 1},
		Beads: &genclient.BeadsDiagnostic{
			BeadsStore:          "BdStore",
			NativeStoreEligible: false,
			PreflightGate:       &preflightGate,
			PreflightReason:     &preflightReason,
		},
	}

	got := statusViewFromGen(body)

	if got.CityName != "bright-lights" {
		t.Errorf("CityName = %q, want %q", got.CityName, "bright-lights")
	}
	if got.CityPath != "/home/u/bright-lights" {
		t.Errorf("CityPath = %q", got.CityPath)
	}
	if got.Version != "v0.1.0" {
		t.Errorf("Version = %q", got.Version)
	}
	if got.UptimeSec != 42 {
		t.Errorf("UptimeSec = %d", got.UptimeSec)
	}
	if got.Summary.TotalAgents != 2 || got.Summary.RunningAgents != 1 {
		t.Errorf("Summary = %+v", got.Summary)
	}
	if len(got.Agents) != 1 || got.Agents[0].QualifiedName != "mayor" {
		t.Errorf("Agents = %+v", got.Agents)
	}
	if got.Agents[0].SessionName != "city--mayor" {
		t.Errorf("agent[0].SessionName = %q", got.Agents[0].SessionName)
	}
	if got.Agents[0].ScaleLabel != "scaled (min=0, max=3)" {
		t.Errorf("agent[0].ScaleLabel = %q", got.Agents[0].ScaleLabel)
	}
	if !got.Agents[0].Expanded {
		t.Errorf("agent[0].Expanded should be true")
	}
	if len(got.Rigs) != 1 || got.Rigs[0].Name != "myrig" {
		t.Errorf("Rigs = %+v", got.Rigs)
	}
	if len(got.NamedSessions) != 1 || got.NamedSessions[0].Status != "materialized" {
		t.Errorf("NamedSessions = %+v", got.NamedSessions)
	}
	if got.SessionCounts.Active != 3 || got.SessionCounts.Suspended != 1 {
		t.Errorf("SessionCounts = %+v", got.SessionCounts)
	}
	if got.Beads == nil {
		t.Fatal("Beads = nil, want diagnostic")
	}
	if got.Beads.Store != "BdStore" {
		t.Errorf("beads_store = %q, want BdStore", got.Beads.Store)
	}
	if got.Beads.PreflightGate != "metadata_backend" || got.Beads.PreflightReason == "" {
		t.Errorf("Beads diagnostic = %+v, want preflight metadata diagnostic", got.Beads)
	}
}

func TestStatusViewFromGen_EmptyLists(t *testing.T) {
	// Body without any detail slices: views should be non-nil empty slices.
	body := &genclient.StatusBody{
		Name:   "empty-city",
		Path:   "/tmp/empty",
		Agents: genclient.StatusAgentCounts{},
		Rigs:   genclient.StatusRigCounts{},
		Work:   genclient.StatusWorkCounts{},
		Mail:   genclient.StatusMailCounts{},
	}
	got := statusViewFromGen(body)
	if got.Agents == nil {
		t.Error("Agents should be non-nil empty slice, got nil")
	}
	if len(got.Agents) != 0 {
		t.Errorf("Agents length = %d, want 0", len(got.Agents))
	}
	if got.Rigs == nil {
		t.Error("Rigs should be non-nil empty slice, got nil")
	}
	if got.NamedSessions == nil {
		t.Error("NamedSessions should be non-nil empty slice, got nil")
	}
}

func TestStatusViewFromGen_PartialMissingField(t *testing.T) {
	// AgentDetails with minimal fields — optional strings unset.
	body := &genclient.StatusBody{
		Name: "city",
		Path: "/tmp",
		AgentDetails: &[]genclient.StatusAgentDetail{
			{
				Name:          "worker",
				QualifiedName: "myrig/worker",
				Scope:         "rig",
				Running:       false,
				Suspended:     true,
			},
		},
	}
	got := statusViewFromGen(body)
	if len(got.Agents) != 1 {
		t.Fatalf("Agents length = %d, want 1", len(got.Agents))
	}
	a := got.Agents[0]
	if a.QualifiedName != "myrig/worker" {
		t.Errorf("QualifiedName = %q", a.QualifiedName)
	}
	if !a.Suspended {
		t.Error("Suspended should be true")
	}
	if a.SessionName != "" || a.GroupName != "" || a.ScaleLabel != "" {
		t.Errorf("optional fields should be empty: SessionName=%q GroupName=%q ScaleLabel=%q", a.SessionName, a.GroupName, a.ScaleLabel)
	}
	if a.Expanded {
		t.Error("Expanded should default to false")
	}
}

// TestStatusViewFromGenMapsSessionID pins that the optional session_id on
// /status agent rows reaches the CLI view, and that an absent one decodes to
// the empty string (#8453).
func TestStatusViewFromGenMapsSessionID(t *testing.T) {
	id := "gc-42"
	details := []genclient.StatusAgentDetail{
		{Name: "worker", QualifiedName: "frontend/worker", Scope: "rig", SessionId: &id},
		{Name: "helper", QualifiedName: "frontend/helper", Scope: "rig"},
	}
	got := statusViewFromGen(&genclient.StatusBody{AgentDetails: &details})
	if len(got.Agents) != 2 {
		t.Fatalf("Agents = %+v, want 2 rows", got.Agents)
	}
	if got.Agents[0].SessionID != id {
		t.Fatalf("Agents[0].SessionID = %q, want %q", got.Agents[0].SessionID, id)
	}
	if got.Agents[1].SessionID != "" {
		t.Fatalf("Agents[1].SessionID = %q, want empty", got.Agents[1].SessionID)
	}
}

// TestStatusViewFromGenStuckCreating pins the three measured states of the
// abandoned-create block across the decode: an absent block is nil (unknown,
// never an empty measured block), a present empty complete block is a
// measured zero, and a present block keeps its rows, its anchor times and its
// complete flag.
func TestStatusViewFromGenStuckCreating(t *testing.T) {
	t.Run("absent decodes to nil", func(t *testing.T) {
		got := statusViewFromGen(&genclient.StatusBody{Name: "city"})
		if got.StuckCreating != nil {
			t.Fatalf("StuckCreating = %+v, want nil (not measured)", got.StuckCreating)
		}
	})
	t.Run("present empty complete is a measured zero", func(t *testing.T) {
		empty := []genclient.StatusStuckCreatingSession{}
		got := statusViewFromGen(&genclient.StatusBody{StuckCreating: &genclient.StatusStuckCreating{Sessions: &empty, Complete: true}})
		if got.StuckCreating == nil {
			t.Fatal("StuckCreating = nil, want a measured block")
		}
		if !got.StuckCreating.Complete || len(got.StuckCreating.Sessions) != 0 {
			t.Fatalf("StuckCreating = %+v, want complete and empty", *got.StuckCreating)
		}
	})
	t.Run("null sessions list still decodes as a present block", func(t *testing.T) {
		got := statusViewFromGen(&genclient.StatusBody{StuckCreating: &genclient.StatusStuckCreating{Complete: true}})
		if got.StuckCreating == nil || len(got.StuckCreating.Sessions) != 0 {
			t.Fatalf("StuckCreating = %+v, want a present, empty block", got.StuckCreating)
		}
	})
	t.Run("rows keep their fields and a partial read stays a lower bound", func(t *testing.T) {
		since := "2026-11-15T09:00:00Z"
		badSince := "not-a-time"
		tmpl := "myrig/worker"
		rows := []genclient.StatusStuckCreatingSession{
			{Id: "gc-1", Template: &tmpl, State: "creating", Since: &since},
			{Id: "gc-2", State: "start-pending", Since: &badSince},
			{Id: "gc-3", State: "creating"},
		}
		got := statusViewFromGen(&genclient.StatusBody{StuckCreating: &genclient.StatusStuckCreating{Sessions: &rows, Complete: false}})
		sc := got.StuckCreating
		if sc == nil || len(sc.Sessions) != 3 {
			t.Fatalf("StuckCreating = %+v, want 3 rows", sc)
		}
		if sc.Complete {
			t.Error("Complete = true, want false")
		}
		first := sc.Sessions[0]
		wantSince := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
		if first.ID != "gc-1" || first.Template != tmpl || first.State != "creating" || !first.Since.Equal(wantSince) {
			t.Errorf("row 0 = %+v, want gc-1 / %s / creating / %s", first, tmpl, wantSince)
		}
		if sc.Sessions[1].State != "start-pending" || !sc.Sessions[1].Since.IsZero() {
			t.Errorf("row 1 = %+v, want start-pending with a zero since (unparseable)", sc.Sessions[1])
		}
		if !sc.Sessions[2].Since.IsZero() || sc.Sessions[2].Template != "" {
			t.Errorf("row 2 = %+v, want no since and no template", sc.Sessions[2])
		}
	})
}
