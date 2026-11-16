package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api/genclient"
)

func sessionListStrPtr(s string) *string { return &s }

// TestSessionListViewsFromGen_DecodesListContractFields pins the list decode:
// every field the CLI's unified `gc session list` row needs survives, closed is
// derived from the wire state, and timestamps parse to the same instant.
func TestSessionListViewsFromGen_DecodesListContractFields(t *testing.T) {
	progress := int64(40)
	since := time.Date(2026, 10, 28, 11, 0, 0, 0, time.UTC)
	items := []genclient.SessionResponse{
		{
			Id:                   "gc-1",
			Template:             "alpha/rigbot",
			State:                "active",
			Title:                "Rig bot",
			SessionName:          "alpha--rigbot",
			Provider:             "claude",
			CreatedAt:            "2026-10-28T10:00:00Z",
			Attached:             true,
			Running:              true,
			Alias:                sessionListStrPtr("bot"),
			AgentName:            sessionListStrPtr("alpha/rigbot"),
			Transport:            sessionListStrPtr("tmux"),
			WorkDir:              sessionListStrPtr("/w"),
			WorkerDir:            sessionListStrPtr("/w/tree"),
			Reason:               sessionListStrPtr("config"),
			LastActive:           sessionListStrPtr("2026-10-28T14:00:00+02:00"),
			LastNudgeDeliveredAt: sessionListStrPtr("2026-10-28T13:30:00+02:00"),
			LastOutput:           sessionListStrPtr("ready"),
			ProgramStatus: &genclient.SessionProgramStatus{
				State: "working", Since: since, WorkingChildren: 2, Progress: &progress,
			},
		},
		{
			Id:          "gc-2",
			Template:    "worker",
			State:       "closed",
			SessionName: "worker",
			CreatedAt:   "2026-10-27T10:00:00Z",
		},
	}
	views, err := sessionListViewsFromGen(&genclient.ListBodySessionResponse{Items: &items})
	if err != nil {
		t.Fatalf("sessionListViewsFromGen: %v", err)
	}
	if len(views) != 2 {
		t.Fatalf("len(views) = %d, want 2", len(views))
	}
	v := views[0]
	if v.ID != "gc-1" || v.Template != "alpha/rigbot" || v.State != "active" || v.Closed {
		t.Errorf("identity = %+v", v)
	}
	if v.Alias != "bot" || v.AgentName != "alpha/rigbot" || v.Transport != "tmux" || v.Provider != "claude" {
		t.Errorf("alias/agent/transport/provider = %q/%q/%q/%q", v.Alias, v.AgentName, v.Transport, v.Provider)
	}
	if v.WorkDir != "/w" || v.WorkerDir != "/w/tree" || v.SessionName != "alpha--rigbot" || v.Title != "Rig bot" {
		t.Errorf("dirs/name/title = %q/%q/%q/%q", v.WorkDir, v.WorkerDir, v.SessionName, v.Title)
	}
	if v.Reason != "config" || v.LastOutput != "ready" || !v.Attached || !v.Running {
		t.Errorf("reason/output/attached/running = %q/%q/%v/%v", v.Reason, v.LastOutput, v.Attached, v.Running)
	}
	if want := time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC); !v.CreatedAt.Equal(want) {
		t.Errorf("CreatedAt = %v, want %v", v.CreatedAt, want)
	}
	if want := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC); !v.LastActive.Equal(want) {
		t.Errorf("LastActive = %v, want %v", v.LastActive, want)
	}
	if want := time.Date(2026, 10, 28, 11, 30, 0, 0, time.UTC); !v.LastNudgeDeliveredAt.Equal(want) {
		t.Errorf("LastNudgeDeliveredAt = %v, want %v", v.LastNudgeDeliveredAt, want)
	}
	if v.ProgramStatus == nil || v.ProgramStatus.State != "working" || v.ProgramStatus.WorkingChildren != 2 {
		t.Errorf("ProgramStatus = %+v", v.ProgramStatus)
	}

	c := views[1]
	if !c.Closed || c.State != "closed" {
		t.Errorf("closed row: Closed=%v State=%q, want true/closed", c.Closed, c.State)
	}
	if !c.LastActive.IsZero() || !c.LastNudgeDeliveredAt.IsZero() {
		t.Errorf("absent timestamps must decode to zero: last_active=%v nudge=%v", c.LastActive, c.LastNudgeDeliveredAt)
	}
	if c.ProgramStatus != nil {
		t.Errorf("absent program_status = %+v, want nil", c.ProgramStatus)
	}
}

// TestSessionListViewsFromGen_RejectsMalformedTimestamp pins that a timestamp
// the CLI cannot normalize is an error naming the session and the field, not a
// value echoed through.
func TestSessionListViewsFromGen_RejectsMalformedTimestamp(t *testing.T) {
	for _, tc := range []struct {
		field string
		item  genclient.SessionResponse
	}{
		{"created_at", genclient.SessionResponse{Id: "gc-bad", CreatedAt: "yesterday"}},
		{"last_active", genclient.SessionResponse{Id: "gc-bad", CreatedAt: "2026-10-28T10:00:00Z", LastActive: sessionListStrPtr("nope")}},
		{"last_nudge_delivered_at", genclient.SessionResponse{Id: "gc-bad", CreatedAt: "2026-10-28T10:00:00Z", LastNudgeDeliveredAt: sessionListStrPtr("nope")}},
	} {
		t.Run(tc.field, func(t *testing.T) {
			items := []genclient.SessionResponse{tc.item}
			_, err := sessionListViewsFromGen(&genclient.ListBodySessionResponse{Items: &items})
			if err == nil {
				t.Fatal("want an error for a malformed timestamp")
			}
			if !strings.Contains(err.Error(), "gc-bad") || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("error %q must name the session id and %s", err, tc.field)
			}
		})
	}
}

// TestSessionListViewsFromGen_Empty pins that a missing body or item list
// decodes to an empty, non-nil slice so the CLI renders [] rather than null.
func TestSessionListViewsFromGen_Empty(t *testing.T) {
	empty := []genclient.SessionResponse{}
	for name, body := range map[string]*genclient.ListBodySessionResponse{
		"nil-body":    nil,
		"nil-items":   {},
		"empty-items": {Items: &empty},
	} {
		views, err := sessionListViewsFromGen(body)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if views == nil || len(views) != 0 {
			t.Fatalf("%s: views = %#v, want an empty non-nil slice", name, views)
		}
	}
}

// TestClientListSessionsDecodesListContract drives Client.ListSessions over
// two pages and checks both decode into SessionListView with the list fields,
// and that a malformed timestamp is a hard error, not a fallback trigger.
func TestClientListSessionsDecodesListContract(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("cursor") {
		case "":
			w.Header().Set("X-GC-Cache-Age-S", "1.5")
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"items": []map[string]any{{
					"id": "gc-1", "template": "alpha/rigbot", "state": "active", "title": "",
					"session_name": "alpha--rigbot", "provider": "claude", "created_at": "2026-10-28T10:00:00Z",
					"attached": false, "running": true, "agent_name": "alpha/rigbot", "transport": "tmux",
					"worker_dir": "/w", "last_nudge_delivered_at": "2026-10-28T14:00:00+02:00",
				}},
				"next_cursor": "page2",
			})
		case "page2":
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"items": []map[string]any{{
					"id": "gc-2", "template": "worker", "state": "closed", "title": "",
					"session_name": "worker", "provider": "", "created_at": "2026-10-27T10:00:00Z",
					"attached": false, "running": false, "agent_name": "",
				}},
			})
		default:
			t.Errorf("unexpected cursor %q", r.URL.Query().Get("cursor"))
		}
	}))
	defer ts.Close()

	cr, err := NewCityScopedClient(ts.URL, "alpha").ListSessions(ListSessionsOpts{State: "all"})
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if cr.AgeSeconds != 1.5 {
		t.Errorf("AgeSeconds = %v, want 1.5", cr.AgeSeconds)
	}
	if len(cr.Body.Sessions) != 2 {
		t.Fatalf("len(Body) = %d, want 2", len(cr.Body.Sessions))
	}
	first, second := cr.Body.Sessions[0], cr.Body.Sessions[1]
	if first.AgentName != "alpha/rigbot" || first.Transport != "tmux" || first.WorkerDir != "/w" || first.Provider != "claude" || !first.Running {
		t.Errorf("first = %+v", first)
	}
	if want := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC); !first.LastNudgeDeliveredAt.Equal(want) {
		t.Errorf("first.LastNudgeDeliveredAt = %v, want %v", first.LastNudgeDeliveredAt, want)
	}
	if !second.Closed || second.State != "closed" {
		t.Errorf("second = %+v, want a closed row", second)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"items": []map[string]any{{
				"id": "gc-bad", "template": "worker", "state": "active", "title": "",
				"session_name": "worker", "provider": "", "created_at": "not-a-time",
				"attached": false, "running": false,
			}},
		})
	}))
	defer bad.Close()
	badClient := NewCityScopedClient(bad.URL, "alpha")
	_, err = badClient.ListSessions(ListSessionsOpts{})
	if err == nil {
		t.Fatal("want a decode error for a malformed created_at")
	}
	if !strings.Contains(err.Error(), "gc-bad") {
		t.Errorf("error %q must name the session", err)
	}
	if ShouldFallbackForRead(badClient, err) {
		t.Errorf("a decode error must not trigger the local fallback: %v", err)
	}
}

// sessionListTestRow is one minimal GET /sessions item for client tests.
func sessionListTestRow(id string) map[string]any {
	return map[string]any{
		"id": id, "template": "worker", "state": "active", "title": "",
		"session_name": id, "provider": "claude", "created_at": "2026-10-28T10:00:00Z",
		"attached": false, "running": true, "agent_name": "worker",
	}
}

// TestClientListSessionsReportsPartialAcrossPages pins that Client.ListSessions
// keeps the server's partial report: Partial is ORed over every page, a page
// that names errors without the flag still counts, and each error message is
// kept once in first-seen order.
func TestClientListSessionsReportsPartialAcrossPages(t *testing.T) {
	cases := []struct {
		name        string
		pages       []map[string]any
		wantPartial bool
		wantErrs    []string
	}{
		{
			name: "flagged-pages",
			pages: []map[string]any{
				{"items": []map[string]any{sessionListTestRow("gc-1")}, "partial": true, "partial_errors": []string{"bd list: boom"}},
				{"items": []map[string]any{sessionListTestRow("gc-2")}, "partial": true, "partial_errors": []string{"bd list: boom", "rig alpha: gone"}},
			},
			wantPartial: true,
			wantErrs:    []string{"bd list: boom", "rig alpha: gone"},
		},
		{
			name: "errors-without-flag",
			pages: []map[string]any{
				{"items": []map[string]any{sessionListTestRow("gc-1")}},
				{"items": []map[string]any{sessionListTestRow("gc-2")}, "partial_errors": []string{"bd list: boom"}},
			},
			wantPartial: true,
			wantErrs:    []string{"bd list: boom"},
		},
		{
			name: "complete",
			pages: []map[string]any{
				{"items": []map[string]any{sessionListTestRow("gc-1")}},
				{"items": []map[string]any{sessionListTestRow("gc-2")}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				page := tc.pages[0]
				if r.URL.Query().Get("cursor") == "page2" {
					page = tc.pages[1]
					w.Header().Set("X-GC-Cache-Refresh", "failing")
				} else {
					page["next_cursor"] = "page2"
					w.Header().Set("X-GC-Cache-Refresh-Interval-S", "60")
					w.Header().Set("X-GC-Cache-Refresh", "scheduled")
				}
				json.NewEncoder(w).Encode(page) //nolint:errcheck
			}))
			defer ts.Close()
			cr, err := NewCityScopedClient(ts.URL, "alpha").ListSessions(ListSessionsOpts{State: "all"})
			if err != nil {
				t.Fatalf("ListSessions: %v", err)
			}
			if len(cr.Body.Sessions) != 2 {
				t.Errorf("sessions = %d, want 2", len(cr.Body.Sessions))
			}
			if cr.Body.Partial != tc.wantPartial {
				t.Errorf("Partial = %v, want %v", cr.Body.Partial, tc.wantPartial)
			}
			if strings.Join(cr.Body.PartialErrors, "|") != strings.Join(tc.wantErrs, "|") {
				t.Errorf("PartialErrors = %q, want %q", cr.Body.PartialErrors, tc.wantErrs)
			}
			if want := (CacheRefresh{IntervalS: 60, State: CacheRefreshScheduled}); cr.Refresh != want {
				t.Errorf("Refresh = %+v, want page 1's %+v", cr.Refresh, want)
			}
		})
	}
}

// TestClientListSessionsReportsAServerWithoutAgentName pins the capability
// signal: a row that comes without the agent_name key, on any page, marks
// the answer as one from a server that predates it; an empty agent_name is a
// current server's answer for a session that records none.
func TestClientListSessionsReportsAServerWithoutAgentName(t *testing.T) {
	withoutKey := sessionListTestRow("gc-2")
	delete(withoutKey, "agent_name")
	emptyName := sessionListTestRow("gc-2")
	emptyName["agent_name"] = ""
	for name, tc := range map[string]struct {
		second      map[string]any
		wantMissing bool
	}{
		"key-missing-on-a-later-page": {second: withoutKey, wantMissing: true},
		"empty-agent-name":            {second: emptyName},
	} {
		t.Run(name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Query().Get("cursor") == "page2" {
					json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{tc.second}}) //nolint:errcheck
					return
				}
				json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
					"items": []map[string]any{sessionListTestRow("gc-1")}, "next_cursor": "page2",
				})
			}))
			defer ts.Close()
			cr, err := NewCityScopedClient(ts.URL, "alpha").ListSessions(ListSessionsOpts{})
			if err != nil {
				t.Fatalf("ListSessions: %v", err)
			}
			if cr.Body.AgentNameMissing != tc.wantMissing {
				t.Errorf("AgentNameMissing = %v, want %v", cr.Body.AgentNameMissing, tc.wantMissing)
			}
		})
	}
}

// TestClientListSessionsAsksForWakeReasonsOnlyWhenTold pins that
// ListSessions sends wake_reasons=true only for WakeReasons and decodes the
// rows' wake_reasons.
func TestClientListSessionsAsksForWakeReasonsOnlyWhenTold(t *testing.T) {
	var asked []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Query().Get("wake_reasons"))
		row := sessionListTestRow("gc-1")
		if r.URL.Query().Get("wake_reasons") == "true" {
			row["wake_reasons"] = []string{"session", "config"}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{row}}) //nolint:errcheck
	}))
	defer ts.Close()
	c := NewCityScopedClient(ts.URL, "alpha")

	cr, err := c.ListSessions(ListSessionsOpts{WakeReasons: true})
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if got := strings.Join(cr.Body.Sessions[0].WakeReasons, ","); got != "session,config" {
		t.Errorf("WakeReasons = %q, want session,config", got)
	}
	if _, err := c.ListSessions(ListSessionsOpts{}); err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if strings.Join(asked, "|") != "true|" {
		t.Errorf("wake_reasons query per request = %q, want true then absent", asked)
	}
}
