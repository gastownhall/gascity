package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
)

// Kills a session list that shows a STATUS column with nothing to show,
// renders an unknown status as idle, drops the blocked kind, or leaves
// program_status out of its JSON.
func TestCLISessionListStatusColumn(t *testing.T) {
	row := func(id string, ps *api.SessionProgramStatus) SessionView {
		return SessionView{ID: id, Template: "worker", State: "active", CreatedAt: "2026-10-31T09:00:00Z", ProgramStatus: ps}
	}

	var plain bytes.Buffer
	if code := renderSessionListFromAPI(api.CachedRead[[]SessionView]{Body: []SessionView{row("gc-1", nil)}}, false, &plain); code != 0 {
		t.Fatalf("render = %d", code)
	}
	header, _, _ := strings.Cut(plain.String(), "\n")
	if strings.Contains(header, "STATUS") {
		t.Fatalf("no session reports a status, header still shows STATUS: %q", header)
	}

	since := time.Now().Add(-3*time.Minute - 5*time.Second)
	blocked := &api.SessionProgramStatus{State: "blocked", Kind: "permission", App: "codex", Since: since}
	var out bytes.Buffer
	if code := renderSessionListFromAPI(api.CachedRead[[]SessionView]{Body: []SessionView{row("gc-1", blocked), row("gc-2", nil)}}, false, &out); code != 0 {
		t.Fatalf("render = %d", code)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if fields := strings.Fields(lines[0]); len(fields) < 4 || fields[2] != "STATE" || fields[3] != "STATUS" {
		t.Fatalf("header = %q, want STATUS right after STATE", lines[0])
	}
	if !strings.Contains(lines[1], "blocked:permission 3m") {
		t.Fatalf("reporting row = %q, want blocked:permission 3m", lines[1])
	}
	if fields := strings.Fields(lines[2]); len(fields) < 4 || fields[3] != "-" || strings.Contains(lines[2], "idle") {
		t.Fatalf("unknown row = %q, want - in STATUS and never idle", lines[2])
	}

	var js bytes.Buffer
	if code := renderSessionListFromAPI(api.CachedRead[[]SessionView]{Body: []SessionView{row("gc-1", blocked), row("gc-2", nil)}}, true, &js); code != 0 {
		t.Fatalf("render --json = %d", code)
	}
	validateJSONAgainstBuiltinSchema(t, []string{"session", "list"}, js.Bytes())
	var got struct {
		Sessions []map[string]json.RawMessage `json:"sessions"`
	}
	if err := json.Unmarshal(js.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	var ps api.SessionProgramStatus
	if err := json.Unmarshal(got.Sessions[0]["program_status"], &ps); err != nil || ps.State != "blocked" || ps.Kind != "permission" {
		t.Fatalf("program_status JSON = %s (%v), want blocked:permission", got.Sessions[0]["program_status"], err)
	}
	if _, present := got.Sessions[1]["program_status"]; present {
		t.Fatalf("unknown status rendered program_status: %s", js.String())
	}
}

// Kills a STATUS cell that reads a status without a since as an error or
// prints a negative age for a clock-skewed since.
func TestSessionViewProgramStatusFormatsEdges(t *testing.T) {
	now := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		ps   *api.SessionProgramStatus
		want string
	}{
		{nil, "-"},
		{&api.SessionProgramStatus{State: "done"}, "done"},
		{&api.SessionProgramStatus{State: "working", Since: now.Add(time.Minute)}, "working 0s"},
		{&api.SessionProgramStatus{State: "error", Since: now.Add(-2 * time.Hour)}, "error " + formatDuration(2*time.Hour)},
	}
	for _, tc := range cases {
		if got := sessionViewProgramStatus(tc.ps, now); got != tc.want {
			t.Errorf("sessionViewProgramStatus(%+v) = %q, want %q", tc.ps, got, tc.want)
		}
	}
}
