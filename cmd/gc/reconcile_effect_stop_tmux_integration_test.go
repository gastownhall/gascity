//go:build integration

package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestStopRefusesARuntimeReplacedAtTheCallOnRealTmux (mc-rq4d0; the #7788
// review). Kills a re-read before the name-based Stop that tmux answers
// from the snapshot it took before the Call: on the seam-backed provider gc
// runs, the fenced session is killed at the Call and a new one carrying the
// row's own identity takes the name; the stop refuses runtime_moved, and
// the new session stands.
func TestStopRefusesARuntimeReplacedAtTheCallOnRealTmux(t *testing.T) {
	p, it, store, _ := admittedBeginOn(t, signaled(drainOrphaned), undesired(drainOrphaned))
	if it.Kind != intentStop {
		t.Fatalf("decided %+v, want the stop", it)
	}
	p.Writers[rowLeg] = fencedWriter{store: atomicClosing{store.(*beads.MemStore)}}
	sp, tm := realTmux(t, "sfi")
	env := map[string]string{"GC_SESSION_ID": it.Key.ID, "GC_INSTANCE_TOKEN": "tok-3", "GC_RUNTIME_EPOCH": "3"}
	if err := sp.Start(context.Background(), "s-gc-1", runtime.Config{Command: "sleep 300", Env: env}); err != nil {
		t.Fatalf("start: %v", err)
	}
	p.Runtime = sp
	object := func() string {
		return tm("display-message", "-p", "-t", "=s-gc-1:", "#{session_id}/#{session_created}")
	}
	before, replaced := object(), ""
	p.seam = func(_ context.Context, at txSeam, _ intent, section, _ int) error {
		if at != seamBeforeCall || section != 0 {
			return nil
		}
		tm("kill-session", "-t", "=s-gc-1")
		second := time.Now().Unix()
		waitTmux(t, "a new second", func() bool { return time.Now().Unix() > second }) // a distinct session_created
		tm("new-session", "-d", "-s", "s-gc-1", "sleep 300")
		for k, v := range env {
			tm("set-environment", "-t", "=s-gc-1", k, v)
		}
		replaced = object()
		return nil
	}
	s := runTx(context.Background(), p, it, effectSpecs[intentStop], nil)
	got, err := store.Get(it.Key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after := object(); s.Cause != fenceRuntimeMoved || replaced == before || after != replaced || got.Status == "closed" {
		t.Fatalf("object %q replaced by %q at the Call: settlement %+v, object after %q, row %s %v; want refused runtime_moved, the new object standing", before, replaced, s, after, got.Status, got.Metadata)
	}
}
