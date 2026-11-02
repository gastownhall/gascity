package main

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// A close of a manual session that wins after the pass's census was read is
// terminal under v2, as #7466 makes it under legacy (SESS-711, the create
// arm, and staging #7465): the pass reads the row open on its stale census,
// yet nothing creates a successor under its alias or runtime name, and
// nothing starts it again. A manual row is never create demand, and a start
// the stale census admits refuses on the fresh read of the closed row.
func TestV2ClosedManualRowOnStaleCensusIsNeitherCreatedNorStarted(t *testing.T) {
	at := func(d time.Duration) string { return parityNow.Add(d).UTC().Format(time.RFC3339) }
	f := parityFixture{
		Name: "a manual row closed under a stale census",
		Rows: []parityRow{sessionRow("gc-1", "template", "worker", "agent_name", "worker", "alias", "conversation",
			"session_name", "s-gc-1", "session_origin", "manual", "state", "awake", "command", "true",
			"generation", "1", "continuation_epoch", "1", "instance_token", "tok-gc-1", "last_woke_at", at(-time.Hour))},
		Runtimes: []simRuntime{{id: "gc-1", epoch: "1", token: "tok-gc-1"}},
	}
	cityPath := t.TempDir()
	w := newV2World(t, f, cityPath, f.converged(t, cityPath), nil, "", nil)
	leg := w.s.legs[0]
	w.settle(t, f.Name)
	if !w.sp.IsRunning("s-gc-1") {
		t.Fatal("premise: the manual session runs before the close")
	}

	// The operator's close lands on the store; v2's cache has not seen it.
	mgr := session.NewManagerWithOptions(leg.backing, w.sp, session.WithCityPath(cityPath), session.WithClock(w.s.clk))
	if _, err := mgr.CloseDetailed(context.Background(), testActorIn(t, session.ActorOperator, cityPath), "gc-1"); err != nil {
		t.Fatalf("close: %v", err)
	}
	w.s.audit("close")
	if cached, err := leg.cache.Get("gc-1"); err != nil || cached.Status == "closed" {
		t.Fatalf("premise: the census must still read the row open (got %q, %v)", cached.Status, err)
	}
	starts := countCalls(w.sp.Fake, "Start")

	for step := 0; step < 4; step++ {
		w.settle(t, f.Name)
		w.s.advance(30 * time.Second)
	}

	// The stale census admitted a start for the closed row (the case this
	// pins); its effect refused on the fresh read.
	if !slices.ContainsFunc(w.s.schedule, func(line string) bool { return strings.Contains(line, "admitted [start:gc-1]") }) {
		t.Fatalf("premise: no pass admitted a start on the stale census\n%s", strings.Join(w.s.schedule, "\n"))
	}
	rows, err := leg.backing.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range rows {
		if b.ID == "gc-1" {
			if b.Status != "closed" {
				t.Errorf("the closed manual row is %q again: %v", b.Status, b.Metadata)
			}
			continue
		}
		if b.Metadata["alias"] == "conversation" || b.Metadata["session_name"] == "s-gc-1" {
			t.Errorf("a successor row %s took the closed conversation's alias or runtime name: %v", b.ID, b.Metadata)
		}
	}
	if got := countCalls(w.sp.Fake, "Start"); got != starts {
		t.Errorf("provider Starts after the close = %d, want none", got-starts)
	}
	if w.sp.IsRunning("s-gc-1") {
		t.Error("the closed manual session's runtime runs again")
	}
}

// countCalls counts the provider calls to method.
func countCalls(sp *runtime.Fake, method string) int {
	n := 0
	for _, c := range sp.SnapshotCalls() {
		if c.Method == method {
			n++
		}
	}
	return n
}
