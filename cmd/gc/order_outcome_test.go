package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
)

// runExecOrderWithOutcome dispatches one exec order whose fake runner writes
// declaration (when non-empty) to the file the controller projects in
// GC_ORDER_OUTCOME_FILE, and returns the recorded events, the dispatcher's
// stderr, and the outcome-file path the runner saw.
func runExecOrderWithOutcome(t *testing.T, declaration string, execErr error) (*memRecorder, string, string) {
	t.Helper()
	store := beads.NewMemStore()
	var rec memRecorder
	var stderr bytes.Buffer
	tracking, err := store.Create(beads.Bead{
		Title:  "order:reaper",
		Labels: []string{"order-run:reaper", labelOrderTracking},
	})
	if err != nil {
		t.Fatal(err)
	}

	seenPath := ""
	fakeExec := func(_ context.Context, _, _ string, env []string) ([]byte, error) {
		for _, entry := range env {
			if value, ok := strings.CutPrefix(entry, orders.ExecOutcomeFileEnv+"="); ok {
				seenPath = value
			}
		}
		if seenPath != "" && declaration != "" {
			if err := os.WriteFile(seenPath, []byte(declaration), 0o600); err != nil {
				t.Errorf("writing outcome file: %v", err)
			}
		}
		return []byte("reaper: done\n"), execErr
	}

	aa := []orders.Order{{Name: "reaper", Trigger: "cooldown", Interval: "30m", Exec: "scripts/reaper.sh"}}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, fakeExec, &rec)
	mad := ad.(*memoryOrderDispatcher)
	mad.stderr = &stderr
	mad.dispatchExec(context.Background(), orders.NewStore(beads.OrdersStore{Store: store}), execStoreTarget{ScopeRoot: t.TempDir()}, aa[0], t.TempDir(), tracking.ID, nil)
	return &rec, stderr.String(), seenPath
}

func recordedEvent(rec *memRecorder, typ string) (events.Event, bool) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, e := range rec.events {
		if e.Type == typ {
			return e, true
		}
	}
	return events.Event{}, false
}

func TestOrderDispatchExecPartialOutcomeEmitsTypedOrderSkipped(t *testing.T) {
	rec, _, path := runExecOrderWithOutcome(t,
		`{"outcome":"partial","reason":"some scopes or steps did not run","scopes":[{"scope":"rig:api","reason":"bead store unreachable"}]}`, nil)

	if path == "" {
		t.Fatalf("exec env did not carry %s", orders.ExecOutcomeFileEnv)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("outcome file %s was not removed after the run (stat err %v)", path, err)
	}
	if !rec.hasType(events.OrderCompleted) {
		t.Fatal("a partial run still completed; order.completed is missing")
	}
	ev, ok := recordedEvent(rec, events.OrderSkipped)
	if !ok {
		t.Fatal("missing order.skipped event")
	}
	if ev.Subject != "reaper" {
		t.Fatalf("order.skipped subject = %q, want reaper", ev.Subject)
	}
	decoded, typed, err := events.DecodePayload(events.OrderSkipped, ev.Payload)
	if err != nil || !typed {
		t.Fatalf("DecodePayload(order.skipped) typed=%v err=%v", typed, err)
	}
	payload := decoded.(events.OrderSkippedPayload)
	if payload.OrderName != "reaper" || payload.Outcome != "partial" || len(payload.Scopes) != 1 || payload.Scopes[0].Scope != "rig:api" {
		t.Fatalf("order.skipped payload = %+v", payload)
	}
	if !strings.Contains(ev.Message, "rig:api") {
		t.Fatalf("order.skipped message = %q, want the skipped scope named", ev.Message)
	}
}

func TestOrderDispatchExecWithoutDeclarationEmitsNoOrderSkipped(t *testing.T) {
	rec, _, _ := runExecOrderWithOutcome(t, "", nil)
	if !rec.hasType(events.OrderCompleted) {
		t.Fatal("missing order.completed event")
	}
	if rec.hasType(events.OrderSkipped) {
		t.Fatal("order.skipped recorded for a run that declared nothing")
	}
}

func TestOrderDispatchExecMalformedDeclarationIsReportedNotSwallowed(t *testing.T) {
	rec, stderr, _ := runExecOrderWithOutcome(t, `{"outcome":"mostly"}`, nil)
	ev, ok := recordedEvent(rec, events.OrderSkipped)
	if !ok {
		t.Fatal("a malformed declaration must still surface as order.skipped")
	}
	decoded, _, err := events.DecodePayload(events.OrderSkipped, ev.Payload)
	if err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if got := decoded.(events.OrderSkippedPayload).Outcome; got != "unknown" {
		t.Fatalf("malformed declaration outcome = %q, want unknown", got)
	}
	if !strings.Contains(stderr, "outcome") {
		t.Fatalf("dispatcher stderr = %q, want the decode failure logged", stderr)
	}
}

func TestOrderDispatchExecFailureIgnoresDeclaration(t *testing.T) {
	rec, _, path := runExecOrderWithOutcome(t, `{"outcome":"skipped","reason":"x","scopes":[]}`, os.ErrPermission)
	if !rec.hasType(events.OrderFailed) {
		t.Fatal("missing order.failed event")
	}
	if rec.hasType(events.OrderSkipped) {
		t.Fatal("a failed run must not also report order.skipped")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("outcome file %s was not removed after a failed run (stat err %v)", path, err)
	}
}

func TestPrintOrderRunOutcomeShowsDeclaration(t *testing.T) {
	f, err := newOrderOutcomeFile()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.remove(); err != nil {
			t.Error(err)
		}
	})
	if err := os.WriteFile(f.path, []byte(`{"outcome":"skipped","reason":"no bead scope was reachable","scopes":[{"scope":"city","reason":"bead store unreachable"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	printOrderRunOutcome(f, "reaper", &stdout, &stderr)
	if got := stdout.String(); !strings.Contains(got, `Order "reaper" skipped: no bead scope was reachable`) ||
		!strings.Contains(got, "not run: city (bead store unreachable)") {
		t.Fatalf("stdout = %q", got)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestPrintOrderRunOutcomeReportsUnreadableDeclaration(t *testing.T) {
	f, err := newOrderOutcomeFile()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.remove(); err != nil {
			t.Error(err)
		}
	})
	if err := os.WriteFile(f.path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	printOrderRunOutcome(f, "reaper", &stdout, &stderr)
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "unreadable outcome declaration") {
		t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
}
