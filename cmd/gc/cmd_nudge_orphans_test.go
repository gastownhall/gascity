package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// runNudgeStatusCmd runs `gc nudge status` with args and returns its exit
// code (0 or 1), stdout and stderr.
func runNudgeStatusCmd(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := newNudgeStatusCmd(&stdout, &stderr)
	cmd.SetArgs(args)
	cmd.SetOut(&stderr)
	cmd.SetErr(&stderr)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	code := 0
	if err := cmd.Execute(); err != nil {
		code = 1
		if !errors.Is(err, errExit) {
			stderr.WriteString(err.Error())
		}
	}
	return code, stdout.String(), stderr.String()
}

// `gc nudge status --orphaned` lists, city-wide, every queued item whose key
// no open session matches, with its resolution; an item a live session
// matches is not listed; --json carries the same items; a listed pending ID
// is accepted by `gc nudge drop` and then shows dead.
func TestNudgeStatusOrphanedListsAndDropsOrphans(t *testing.T) {
	f := newOrphanSeatFixture(t)
	writeNamedSessionCityTOML(t, f.dir)
	t.Setenv("GC_CITY", f.dir)
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_SESSION_ID", "")
	created := time.Now().Add(-time.Minute)

	// Seat worker-2/2: predecessor closed, one running holder.
	p := f.startIdle(t, f.createSeat(t, "worker-2", "2").ID)
	adoptable := f.enqueueFencedTo(t, p, "adopt me", created)
	f.closeSession(t, p.ID)
	s := f.startIdle(t, f.createSeat(t, "worker-2", "2").ID)
	// Seat worker-3/3: predecessor closed, no holder.
	v := f.startIdle(t, f.createSeat(t, "worker-3", "3").ID)
	vacant := f.enqueueFencedTo(t, v, "nobody holds my seat", created)
	f.closeSession(t, v.ID)
	// An unfenced item for a key no session ever had.
	unfenced := newQueuedNudge("gc-gone", "no fence at all", created)
	if err := enqueueQueuedNudge(f.dir, unfenced); err != nil {
		t.Fatal(err)
	}
	// An item the live successor's own key matches.
	matched := f.enqueueFencedTo(t, s, "for the live session", created)

	code, out, errOut := runNudgeStatusCmd(t, "--orphaned")
	if code != 0 {
		t.Fatalf("status --orphaned exit %d; stderr=%s", code, errOut)
	}
	for _, want := range []struct{ id, resolution string }{
		{adoptable.ID, "successor " + s.SessionName},
		{vacant.ID, "seat-vacant"},
		{unfenced.ID, "unfenced"},
	} {
		line := orphanStatusLine(out, want.id)
		if line == "" || !strings.Contains(line, want.resolution) || !strings.Contains(line, "pending") {
			t.Fatalf("text view line for %s = %q, want it pending with resolution %q; full output:\n%s", want.id, line, want.resolution, out)
		}
	}
	if strings.Contains(out, matched.ID) {
		t.Fatalf("text view lists %s, which the live session matches:\n%s", matched.ID, out)
	}

	code, jsonOut, errOut := runNudgeStatusCmd(t, "--orphaned", "--json")
	if code != 0 {
		t.Fatalf("status --orphaned --json exit %d; stderr=%s", code, errOut)
	}
	contentOpValidate(t, "schemas/nudge/status/result.schema.json", jsonOut)
	var doc struct {
		OK       bool   `json:"ok"`
		Command  string `json:"command"`
		CityPath string `json:"city_path"`
		Orphaned []struct {
			ID                string    `json:"id"`
			BeadID            string    `json:"bead_id"`
			Agent             string    `json:"agent"`
			Bucket            string    `json:"bucket"`
			DeliverAfter      time.Time `json:"deliver_after"`
			ExpiresAt         time.Time `json:"expires_at"`
			FenceSessionID    string    `json:"fence_session_id"`
			FenceSessionState string    `json:"fence_session_state"`
			Resolution        string    `json:"resolution"`
			Successor         string    `json:"successor"`
		} `json:"orphaned"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &doc); err != nil {
		t.Fatalf("decoding %q: %v", jsonOut, err)
	}
	if !doc.OK || doc.Command != "nudge status" || doc.CityPath == "" || len(doc.Orphaned) != 3 {
		t.Fatalf("--json document = %+v, want ok, command nudge status and three orphans", doc)
	}
	byID := make(map[string]int, len(doc.Orphaned))
	for i, item := range doc.Orphaned {
		byID[item.ID] = i
		if item.Bucket != "pending" || item.DeliverAfter.IsZero() || item.ExpiresAt.IsZero() {
			t.Fatalf("orphan %+v, want pending with due and expiry times", item)
		}
	}
	a, okA := byID[adoptable.ID]
	vi, okV := byID[vacant.ID]
	u, okU := byID[unfenced.ID]
	if !okA || !okV || !okU {
		t.Fatalf("--json orphans = %+v, want %s, %s and %s", doc.Orphaned, adoptable.ID, vacant.ID, unfenced.ID)
	}
	if got := doc.Orphaned[a]; got.Resolution != "successor" || got.Successor != s.SessionName || got.Agent != p.ID || got.FenceSessionID != p.ID || got.FenceSessionState != "closed" {
		t.Fatalf("adoptable orphan = %+v, want successor %s fenced to closed %s", got, s.SessionName, p.ID)
	}
	if got := doc.Orphaned[vi]; got.Resolution != "seat-vacant" || got.Successor != "" || got.FenceSessionID != v.ID {
		t.Fatalf("vacant orphan = %+v, want seat-vacant fenced to %s", got, v.ID)
	}
	if got := doc.Orphaned[u]; got.Resolution != "unfenced" || got.FenceSessionID != "" || got.Agent != "gc-gone" {
		t.Fatalf("unfenced orphan = %+v, want unfenced under gc-gone", got)
	}

	var dropOut, dropErr bytes.Buffer
	if code := cmdNudgeDrop([]string{vacant.ID}, false, &dropOut, &dropErr); code != 0 {
		t.Fatalf("gc nudge drop %s exit %d; stderr=%s", vacant.ID, code, dropErr.String())
	}
	code, out, errOut = runNudgeStatusCmd(t, "--orphaned")
	if code != 0 {
		t.Fatalf("status --orphaned after drop exit %d; stderr=%s", code, errOut)
	}
	if line := orphanStatusLine(out, vacant.ID); !strings.Contains(line, "dead") {
		t.Fatalf("after drop, line for %s = %q, want it dead; full output:\n%s", vacant.ID, line, out)
	}

	code, _, errOut = runNudgeStatusCmd(t, "--orphaned", s.SessionName)
	if code != 1 || !strings.Contains(errOut, "--orphaned") {
		t.Fatalf("status --orphaned <session> exit %d stderr %q, want a usage error naming --orphaned", code, errOut)
	}
}

// orphanStatusLine returns the line of out that names id, or "".
func orphanStatusLine(out, id string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, id) {
			return line
		}
	}
	return ""
}
