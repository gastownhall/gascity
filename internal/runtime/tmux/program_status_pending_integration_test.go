//go:build integration && linux

package tmux

import (
	"encoding/hex"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/programstatus"
)

// pendingDialogScreen is a live Claude permission prompt as the program
// draws it, rows ended for a raw terminal.
var pendingDialogScreen = strings.Join([]string{
	"● Bash(date)",
	"",
	"────────────────────────────────────────",
	" Bash command",
	"",
	"   date",
	"   Print the date",
	"",
	" Do you want to proceed?",
	" ❯ 1. Yes",
	"   2. No",
	"",
	" Esc to cancel",
}, "\r\n")

// rawStep is an emitter step writing text verbatim.
func rawStep(text string) string { return "raw " + hex.EncodeToString([]byte(text)) }

// The record side of a pending read on the real tmux with the built companion
// and emitter, on a scratch server (osc7501x-pend-<rand>), under shadow: the
// program reports blocked and draws its dialog; the dialog closes while the
// record stays blocked (no report follows) for longer than the state cache's
// TTL, the cache primed with that blocked reading. The fresh pending read
// pairs the capture with the record and, once the parsers have seen the
// close, judges none; the shadow comparison says the same; a newer blocked
// report for another dialog, which keeps the root's Since, stands.
func TestPendingFreshReadSupersedesBlockedAfterParserClose(t *testing.T) {
	s := startProgramSession(t, captureCompanionBin(t), captureEmitterBin(t), runtime.ProgramStatusShadow,
		runtime.ProgramStatusArm{Capture: true}, []string{"report state=blocked:kind=permission"})
	var mu sync.Mutex
	var got []runtime.ProgramStatusComparison
	s.p.tm.cfg.ComparisonSink = func(c runtime.ProgramStatusComparison) {
		mu.Lock()
		got = append(got, c)
		mu.Unlock()
	}

	verdict := func(label string) (runtime.ProgramPendingReading, *runtime.PendingInteraction) {
		t.Helper()
		pending, r, ok, err := s.p.pendingWithRecord(s.name)
		if err != nil || !ok {
			t.Fatalf("%s: pendingWithRecord = %v, %+v, %t, %v; want a fresh reading", label, pending, r, ok, err)
		}
		return r, pending
	}

	s.emit(t, rawStep("\x1b[2J\x1b[H"+pendingDialogScreen))
	eventually(t, "the parsers to see the dialog", func() bool {
		_, pending := verdict("dialog")
		return pending != nil
	})
	r, _ := verdict("dialog open")
	if !r.Status.Blocked() || runtime.ProgramFreshPendingVerdict(r, time.Time{}, time.Now()) != runtime.ProgramVerdictPending {
		t.Fatalf("dialog open: reading %+v, want a blocked record judged pending", r)
	}

	// The dialog closes; the program reports nothing, so the record stays
	// blocked, and the state cache is primed with that reading.
	s.emit(t, rawStep("\x1b[2J\x1b[H❯ "))
	if cached, err := s.p.ProgramStatus(s.name); err != nil || !cached.Blocked() {
		t.Fatalf("cached reading = %+v, %v; want the stale blocked record", cached, err)
	}
	eventually(t, "the parsers to see the close", func() bool {
		_, pending := verdict("closing")
		return pending == nil
	})
	// Every fresh read while the record stays blocked, past the state
	// cache's TTL, judges none.
	closed := time.Now()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	for reads := 0; reads == 0 || time.Since(closed) < 2500*time.Millisecond; reads++ {
		r, _ = verdict("dialog closed")
		if !r.Status.Blocked() || !r.CloseSupersedes() || runtime.ProgramFreshPendingVerdict(r, time.Time{}, time.Now()) != runtime.ProgramVerdictNone {
			t.Fatalf("dialog closed %v ago, record still blocked: reading %+v, want the parser's close to supersede it", time.Since(closed), r)
		}
		<-tick.C
	}
	if rec, err := programstatus.Decode(paneOption(t, s.p, s.name, programStatusOption)); err != nil || rec.State != runtime.ProgramBlocked {
		t.Fatalf("published record = %+v, %v; want it still blocked", rec, err)
	}

	mu.Lock()
	got = nil
	mu.Unlock()
	if pending, err := s.p.Pending(s.name); pending != nil || err != nil {
		t.Fatalf("Pending = %v, %v; want the parsers' nil, nil", pending, err)
	}
	eventually(t, "the pending comparison", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) > 0
	})
	mu.Lock()
	c := got[0]
	mu.Unlock()
	if c.Point != runtime.ProgramPointPending || c.Scrape != runtime.ProgramVerdictNone || c.Program != runtime.ProgramVerdictNone || c.Status.State != runtime.ProgramBlocked {
		t.Fatalf("comparison %+v, want pending none/none on the blocked reading", c)
	}

	// A newer report for another dialog the parsers cannot see: the program
	// reports blocked again without leaving it, so the root keeps its Since
	// while the record's Seq moves. That record stands.
	before := r.Status
	s.emit(t, "report state=blocked:kind=question")
	r, _ = verdict("newer report")
	if !r.Status.Blocked() || !r.Status.Since.Equal(before.Since) || r.Status.Seq == before.Seq ||
		r.CloseSupersedes() || runtime.ProgramFreshPendingVerdict(r, time.Time{}, time.Now()) != runtime.ProgramVerdictPending {
		t.Fatalf("newer blocked report: reading %+v after %+v, want Since kept, Seq moved and the record judged pending", r, before)
	}
}
