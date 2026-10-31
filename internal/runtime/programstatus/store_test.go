package programstatus

import (
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

const testNonce = "r4nd.%3"

var t0 = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

// at returns t0 plus n seconds.
func at(n int) time.Time { return t0.Add(time.Duration(n) * time.Second) }

// has reports whether the store holds a record for id.
func (s *Store) has(id string) bool {
	_, ok := s.records[id]
	return ok
}

func apply(t *testing.T, s *Store, body string, now time.Time) {
	t.Helper()
	s.Apply(mustParse(t, body), now)
}

func TestStoreReportReplacesRecord(t *testing.T) {
	s := NewStore(testNonce)
	apply(t, s, "state=blocked:kind=permission:app=a:progress=40:msg="+b64("approve"), at(1))
	apply(t, s, "state=blocked:app=a", at(2))
	got := s.Status()
	if got.Kind != runtime.ProgramBlockUnspecified || got.Progress != -1 || s.Text().Msg != "" {
		t.Fatalf("report did not replace the record: %+v text %+v", got, s.Text())
	}
	if !got.Since.Equal(at(1)) {
		t.Errorf("Since moved within one state: %v", got.Since)
	}
	if got.Nonce != testNonce || got.App != "a" || got.State != runtime.ProgramBlocked {
		t.Errorf("status %+v", got)
	}
}

func TestStoreZeroStatus(t *testing.T) {
	got := NewStore(testNonce).Status()
	want := runtime.ProgramStatus{Nonce: testNonce, Progress: -1}
	if got != want {
		t.Fatalf("fresh store status %+v, want %+v", got, want)
	}
}

func TestStoreTurnCounters(t *testing.T) {
	s := NewStore(testNonce)
	steps := []struct {
		body         string
		starts, ends uint32
		quiet        int // seconds; -1 = zero
		since        int
	}{
		{"state=idle", 0, 0, -1, 1},
		{"state=working", 1, 0, -1, 2},
		{"state=working:progress=10", 1, 0, -1, 2},
		{"state=done", 1, 1, 4, 4},
		{"state=working", 2, 1, 4, 5},
		{"state=idle", 2, 2, 6, 6}, // Esc interrupt reports idle, not done
		{"state=working", 3, 2, 6, 7},
		{"state=error", 3, 3, 8, 8},
		{"state=working", 4, 3, 8, 9},
	}
	for i, step := range steps {
		apply(t, s, step.body, at(i+1))
		got := s.Status()
		if got.TurnStarts != step.starts || got.TurnEnds != step.ends {
			t.Fatalf("step %d %s: starts/ends %d/%d, want %d/%d", i, step.body, got.TurnStarts, got.TurnEnds, step.starts, step.ends)
		}
		wantQuiet := time.Time{}
		if step.quiet >= 0 {
			wantQuiet = at(step.quiet)
		}
		if !got.QuietSince.Equal(wantQuiet) || !got.Since.Equal(at(step.since)) {
			t.Fatalf("step %d %s: since %v quiet %v", i, step.body, got.Since, got.QuietSince)
		}
	}
}

func TestStoreBlockedPausesTurnWithoutEndingIt(t *testing.T) {
	s := NewStore(testNonce)
	apply(t, s, "state=idle", at(1))
	apply(t, s, "state=working", at(2))
	apply(t, s, "state=blocked:kind=permission", at(3))
	got := s.Status()
	if got.TurnStarts != 1 || got.TurnEnds != 0 || !got.QuietSince.IsZero() {
		t.Fatalf("blocked ended the turn: %+v", got)
	}
	if got.State != runtime.ProgramBlocked || got.Kind != runtime.ProgramBlockPermission || !got.Since.Equal(at(3)) {
		t.Fatalf("blocked state: %+v", got)
	}
	// blocked -> working resumes the same turn: not a start.
	apply(t, s, "state=working", at(4))
	if got := s.Status(); got.TurnStarts != 1 || got.TurnEnds != 0 {
		t.Fatalf("resume counted a start: %+v", got)
	}
	// blocked -> boundary ends the turn.
	apply(t, s, "state=blocked:kind=question", at(5))
	apply(t, s, "state=done", at(6))
	if got := s.Status(); got.TurnStarts != 1 || got.TurnEnds != 1 || !got.QuietSince.Equal(at(6)) {
		t.Fatalf("blocked->done: %+v", got)
	}
}

func TestPromptStartDropsWorkingWithoutCountingEnd(t *testing.T) {
	s := NewStore(testNonce)
	apply(t, s, "state=working:app=a", at(1))
	apply(t, s, "state=blocked:id=c1", at(2))
	apply(t, s, "state=working:id=c2", at(2))
	apply(t, s, "state=done:id=c3", at(2))
	apply(t, s, "state=idle:id=c4", at(2))
	seq := s.Status().Seq
	s.PromptStart()
	got := s.Status()
	if got.State != "" || got.TurnStarts != 1 || got.TurnEnds != 0 || !got.QuietSince.IsZero() || !got.Since.IsZero() {
		t.Fatalf("prompt start: %+v", got)
	}
	if got.Children != 2 || got.WorkingChildren != 0 {
		t.Fatalf("children after prompt start: %d (%d working), want done and idle kept", got.Children, got.WorkingChildren)
	}
	if got.Seq <= seq {
		t.Errorf("Seq did not advance: %d -> %d", seq, got.Seq)
	}
	// A boundary root survives a prompt start.
	apply(t, s, "state=done", at(3))
	s.PromptStart()
	if got := s.Status(); got.State != runtime.ProgramDone {
		t.Fatalf("done dropped by prompt start: %+v", got)
	}
	// Working after a no-root gap is a new turn.
	apply(t, s, "state=working", at(4))
	apply(t, s, "state=blocked", at(5))
	s.PromptStart()
	apply(t, s, "state=working", at(6))
	if got := s.Status(); got.TurnStarts != 3 || got.TurnEnds != 0 {
		t.Fatalf("working after dropped blocked: %+v", got)
	}
}

func TestClearIDRemovesDescendants(t *testing.T) {
	s := NewStore(testNonce)
	apply(t, s, "state=working:app=deploy", at(1))
	for _, id := range []string{"build", "build/test", "build/test/unit", "builder", "us-east"} {
		apply(t, s, "state=working:id="+id, at(2))
	}
	if got := s.Status(); got.Children != 5 || got.WorkingChildren != 5 {
		t.Fatalf("children %+v", got)
	}
	apply(t, s, "state=clear:id=build", at(3))
	got := s.Status()
	if got.Children != 2 || got.State != runtime.ProgramWorking {
		t.Fatalf("after clear build: %+v", got)
	}
	if !s.has("builder") || !s.has("us-east") || s.has("build/test") {
		t.Fatalf("clear removed the wrong records")
	}
	// Clearing an id with no record still clears its descendants.
	apply(t, s, "state=idle:id=x/y", at(4))
	apply(t, s, "state=clear:id=x", at(5))
	if s.has("x/y") {
		t.Fatal("descendant of an absent record survived")
	}
	// Clear with no id drops everything.
	apply(t, s, "state=clear", at(6))
	got = s.Status()
	if got.Children != 0 || got.State != "" || !got.Since.IsZero() || got.TurnEnds != 0 {
		t.Fatalf("after clear: %+v", got)
	}
}

func TestStoreChildrenNeverTouchRootTurn(t *testing.T) {
	s := NewStore(testNonce)
	apply(t, s, "state=idle", at(1))
	apply(t, s, "state=working:id=sub", at(2))
	apply(t, s, "state=done:id=sub", at(3))
	apply(t, s, "state=error:id=sub2", at(3))
	got := s.Status()
	if got.TurnStarts != 0 || got.TurnEnds != 0 || got.State != runtime.ProgramIdle || !got.Since.Equal(at(1)) {
		t.Fatalf("child moved the root: %+v", got)
	}
	if got.LastError {
		t.Fatal("a child error set the root's sticky last error")
	}
	if got.Children != 2 || got.WorkingChildren != 0 {
		t.Fatalf("children %+v", got)
	}
}

func TestErrorThenClearKeepsStickyLastError(t *testing.T) {
	s := NewStore(testNonce)
	apply(t, s, "state=working:app=claude-code", at(1))
	apply(t, s, "state=error:app=claude-code:msg="+b64("API Error: 400"), at(2))
	if got := s.Status(); !got.LastError || got.TurnEnds != 1 {
		t.Fatalf("error: %+v", got)
	}
	apply(t, s, "state=clear", at(3))
	apply(t, s, "state=clear", at(3))
	got := s.Status()
	if !got.LastError || got.State != "" {
		t.Fatalf("clear dropped the sticky last error: %+v", got)
	}
	if s.Text().LastErrorMsg != "API Error: 400" || s.Text().Msg != "" {
		t.Fatalf("text %+v", s.Text())
	}
	// A later healthy turn does not erase it either.
	apply(t, s, "state=working", at(4))
	apply(t, s, "state=done", at(5))
	if !s.Status().LastError {
		t.Fatal("last error not sticky across a later turn")
	}
}

func TestStoreProbeAndSeq(t *testing.T) {
	s := NewStore(testNonce)
	s.Probe()
	got := s.Status()
	if !got.Probed || got.Seq != 1 {
		t.Fatalf("probe: %+v", got)
	}
	s.Probe()
	if got := s.Status(); got.Seq != 1 {
		t.Fatalf("a repeated probe changed nothing but bumped Seq: %+v", got)
	}
	apply(t, s, "state=idle:app=a", at(1))
	apply(t, s, "state=idle:app=a", at(2)) // identical record: no change
	if got := s.Status(); got.Seq != 2 {
		t.Fatalf("Seq %d, want 2", got.Seq)
	}
	// Text-only changes do not change @gc_ps.
	apply(t, s, "state=idle:app=a:msg="+b64("hello"), at(3))
	if got := s.Status(); got.Seq != 2 {
		t.Fatalf("text-only change bumped Seq: %d", got.Seq)
	}
	// Clearing nothing changes nothing.
	s2 := NewStore(testNonce)
	apply(t, s2, "state=clear", at(1))
	s2.PromptStart()
	if got := s2.Status(); got.Seq != 0 {
		t.Fatalf("no-op clear bumped Seq: %d", got.Seq)
	}
}

func TestStoreEvictsLeastRecentlyUpdatedChild(t *testing.T) {
	s := NewStore(testNonce)
	for i := range MaxChildren {
		apply(t, s, fmt.Sprintf("state=working:id=c%d", i), at(1))
	}
	apply(t, s, "state=working:id=c0", at(2)) // c0 is now the most recent
	apply(t, s, "state=working:id=new", at(3))
	got := s.Status()
	if got.Children != MaxChildren {
		t.Fatalf("children %d, want %d", got.Children, MaxChildren)
	}
	if !s.has("c0") || s.has("c1") || !s.has("new") {
		t.Fatal("evicted the wrong child")
	}
}

func TestStoreEffectiveAppInherits(t *testing.T) {
	s := NewStore(testNonce)
	apply(t, s, "state=working:app=deploy", at(1))
	apply(t, s, "state=working:id=us/east:app=edge", at(1))
	cases := map[string]string{
		"state=working:id=eu":         "deploy",
		"state=working:id=us/east/a":  "edge",
		"state=working:id=us/west":    "deploy",
		"state=working:id=eu:app=own": "own",
		"state=working":               "",
		"state=working:app=x":         "x",
	}
	for body, want := range cases {
		if got := s.EffectiveApp(mustParse(t, body)); got != want {
			t.Errorf("%s: effective app %q, want %q", body, got, want)
		}
	}
	empty := NewStore(testNonce)
	if got := empty.EffectiveApp(mustParse(t, "state=idle:id=a")); got != "" {
		t.Errorf("no app anywhere: %q", got)
	}
}
