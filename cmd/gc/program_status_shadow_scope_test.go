package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/worker"
)

// shadowProcessUnder runs the test as a short-lived process (or a long-lived
// one) that decides for no session yet, restoring the process state after.
func shadowProcessUnder(t *testing.T, longLived bool) {
	t.Helper()
	prevLong := programShadowLongLived.Load()
	programShadowScope.mu.Lock()
	prevScope := programShadowScope.sessions
	programShadowScope.sessions = nil
	programShadowScope.mu.Unlock()
	programShadowLongLived.Store(longLived)
	t.Cleanup(func() {
		programShadowLongLived.Store(prevLong)
		programShadowScope.mu.Lock()
		programShadowScope.sessions = prevScope
		programShadowScope.mu.Unlock()
	})
}

// scopedShadowCity is a city under mode whose sessions (each a live working
// program, its screen idle an hour) sit on a fake leaf wired to the city's
// tmux comparison sink, recording into the returned collector.
func scopedShadowCity(t *testing.T, mode runtime.ProgramStatusMode, sessions []string) (runtime.FullFake, config.SessionConfig, *eventCollector, *programShadowRecorder) {
	t.Helper()
	c := &eventCollector{}
	prev := programShadowOpen
	programShadowOpen = func(string) events.Recorder { return c }
	t.Cleanup(func() { programShadowOpen = prev })
	city := t.TempDir()
	sc := config.SessionConfig{ProgramStatus: string(mode)}
	cfg := tmuxConfigFromSession(sc, "city", city)
	r, _ := programShadowRecorders.Load(city)

	f := runtime.NewFake()
	f.Activity = map[string]time.Time{}
	for i, name := range sessions {
		if err := f.Start(context.Background(), name, runtime.Config{}); err != nil {
			t.Fatal(err)
		}
		f.Activity[name] = time.Now().Add(-time.Hour)
		nonce := fmt.Sprintf("r1.%%%d", i)
		f.SetProgramPane(name, runtime.ProgramPaneRow{
			Mode: mode, CaptureGate: true, PaneID: fmt.Sprintf("%%%d", i), PanePipe: true, Tap: nonce,
			TapLiveness: runtime.ProgramTapAlive, RecordForm: runtime.ProgramRecordDecoded,
			Record: runtime.ProgramStatus{State: runtime.ProgramWorking, Nonce: nonce, Seq: 2, TurnStarts: 1, Progress: -1, Reported: true, Since: time.Now()},
		})
	}
	f.ProgramShadowSink = cfg.ComparisonSink
	return runtime.FullFake{Fake: f}, sc, c, r.(*programShadowRecorder)
}

// observeAll is a gc session list-style read: every session's observation,
// judged on the reading the list shows, and pending reading, the reads a
// list takes and decides nothing with.
func observeAll(sp runtime.Provider, sessions []string) {
	for _, name := range sessions {
		runtime.ShadowObserveIdle(sp, name, "", "claude", runtime.ProgramVerdictIdle, worker.ProgramStatusOf(sp, name))
		runtime.ShadowCompare(sp, name, "", "claude", runtime.ProgramPointPending, runtime.ProgramVerdictNone,
			func(s runtime.ProgramStatus) runtime.ProgramVerdict {
				return runtime.ProgramPendingVerdict(s, time.Time{}, time.Now())
			})
	}
}

func sessionNames(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("s%d", i)
	}
	return out
}

// Kills a short-lived process that records the sessions it only reads: one
// gc session list in the first shadow wrote ~114 pending records, every
// reading a change to a change memory that starts empty in each process.
func TestShadowReadOnlyCLIRecordsNothing(t *testing.T) {
	shadowProcessUnder(t, false)
	sessions := sessionNames(114)
	sp, sc, c, r := scopedShadowCity(t, runtime.ProgramStatusShadow, sessions)
	if tmuxConfigFromSession(sc, "city", t.TempDir()).ComparisonEnabled() {
		t.Fatal("a process deciding for no session enables the leaf's comparisons")
	}
	observeAll(sp, sessions)
	if !r.flush(5 * time.Second) {
		t.Fatal("flush timed out")
	}
	if got := c.payloads(t); len(got) != 0 {
		t.Fatalf("a read-only CLI process recorded %d comparisons, want 0", len(got))
	}
}

// Kills a deciding CLI process that records a sweep over the sessions it
// reads beside its own decision: a nudge's delivery decision records its
// target's comparison, and no other session's.
func TestShadowCLINudgeRecordsOnlyItsDecision(t *testing.T) {
	shadowProcessUnder(t, false)
	sessions := sessionNames(5)
	sp, sc, c, r := scopedShadowCity(t, runtime.ProgramStatusShadow, sessions)
	stale := time.Now().Add(-time.Hour)
	target := nudgeTarget{sessionName: "s1", sessionID: "gc-1"}
	pollerSessionIdleEnough(target, sp, time.Second, worker.LiveObservation{LastActivity: &stale})
	if !tmuxConfigFromSession(sc, "city", t.TempDir()).ComparisonEnabled() {
		t.Fatal("a process deciding for a session leaves the leaf's comparisons disabled")
	}
	observeAll(sp, []string{"s0", "s2", "s3", "s4"})
	if !r.flush(5 * time.Second) {
		t.Fatal("flush timed out")
	}
	got := c.payloads(t)
	if len(got) != 1 || got[0].Point != string(runtime.ProgramPointQueuedDelivery) || got[0].SessionID != "gc-1" {
		t.Fatalf("a CLI nudge recorded %+v, want its one queued_delivery decision", got)
	}
	c.mu.Lock()
	subject := c.got[0].Subject
	c.mu.Unlock()
	if subject != "s1" {
		t.Fatalf("recorded subject %q, want the nudge target s1", subject)
	}
}

// Kills a deciding CLI process that records under the switch off.
func TestShadowCLINudgeOffRecordsNothing(t *testing.T) {
	shadowProcessUnder(t, false)
	sessions := sessionNames(3)
	sp, _, c, r := scopedShadowCity(t, runtime.ProgramStatusOff, sessions)
	stale := time.Now().Add(-time.Hour)
	pollerSessionIdleEnough(nudgeTarget{sessionName: "s1", sessionID: "gc-1"}, sp, time.Second, worker.LiveObservation{LastActivity: &stale})
	observeAll(sp, sessions)
	if !r.flush(5 * time.Second) {
		t.Fatal("flush timed out")
	}
	if got := c.payloads(t); len(got) != 0 {
		t.Fatalf("the switch off recorded %d comparisons, want 0", len(got))
	}
}

// Kills a scope that reaches the controller or supervisor: a long-lived
// process records every session it reads, as before, and keeps no scope.
func TestShadowLongLivedRecordsEverySession(t *testing.T) {
	shadowProcessUnder(t, true)
	sessions := sessionNames(4)
	sp, sc, c, r := scopedShadowCity(t, runtime.ProgramStatusShadow, sessions)
	if !tmuxConfigFromSession(sc, "city", t.TempDir()).ComparisonEnabled() {
		t.Fatal("a long-lived process leaves the leaf's comparisons disabled")
	}
	programShadowDecideFor("s0")
	programShadowScope.mu.Lock()
	scoped := len(programShadowScope.sessions)
	programShadowScope.mu.Unlock()
	if scoped != 0 {
		t.Fatalf("a long-lived process kept a scope of %d sessions", scoped)
	}
	observeAll(sp, sessions)
	if !r.flush(5 * time.Second) {
		t.Fatal("flush timed out")
	}
	if got := c.payloads(t); len(got) != 2*len(sessions) {
		t.Fatalf("a long-lived process recorded %d comparisons, want %d", len(got), 2*len(sessions))
	}
}

// Kills a scope that matches only the runtime name: a submit names its
// session by bead id, which the comparison carries.
func TestShadowScopeMatchesSessionID(t *testing.T) {
	shadowProcessUnder(t, false)
	programShadowDecideFor("gc-7", "  ")
	if !programShadowRecords(runtime.ProgramStatusComparison{Session: "x", SessionID: "gc-7"}) {
		t.Fatal("a comparison carrying the declared bead id is not recorded")
	}
	if programShadowRecords(runtime.ProgramStatusComparison{Session: "x", SessionID: "gc-8"}) {
		t.Fatal("a comparison of another session is recorded")
	}
	if programShadowRecords(runtime.ProgramStatusComparison{}) {
		t.Fatal("a comparison naming no session is recorded")
	}
}
