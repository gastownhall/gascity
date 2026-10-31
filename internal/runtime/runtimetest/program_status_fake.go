package runtimetest

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/programstatus"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest/osc7501emitter/emit"
)

// fakeProgramPaneID is the agent pane every fake fixture session has.
const fakeProgramPaneID = "%1"

// fakeProgramSession is the session name every fake fixture starts, each on
// its own fake.
const fakeProgramSession = "program-status-session"

// FakeProgramStatusFixtures returns [RunProgramStatusContract] fixtures over
// a [runtime.FullFake]. Each session's agent pane is a [runtime.ProgramPaneRow]
// the fake judges with [runtime.JudgeProgramStatus]; its program is the
// emitter (package osc7501emitter/emit) run in process, and its record is
// what the capture path makes of the program's bytes: a [programstatus.Scanner]
// reassembles them, [programstatus.ParseBody] validates each report, a
// [programstatus.Store] applies it, and the record round-trips through the
// pane-option codec before the fake reads it. The program reads no input, so
// a probe goes unanswered: there is no Relayed fixture.
func FakeProgramStatusFixtures() ProgramStatusFixtures {
	return ProgramStatusFixtures{
		Armed: func(t *testing.T, mode runtime.ProgramStatusMode, script ...string) ProgramStatusSession {
			p := newFakeProgramPane(t, mode)
			p.arm(t)
			p.emit(t, script...)
			return p.session()
		},
		Unarmed: func(t *testing.T, mode runtime.ProgramStatusMode, script ...string) ProgramStatusSession {
			p := newFakeProgramPane(t, mode)
			p.emit(t, script...)
			return p.session()
		},
		Inherited: func(t *testing.T, mode runtime.ProgramStatusMode) ProgramStatusSession {
			p := newFakeProgramPane(t, mode)
			const otherPane = "i1.%9"
			other := programstatus.NewStore(otherPane)
			other.Apply(programstatus.Report{State: runtime.ProgramWorking, Progress: -1}, time.Now())
			p.update(func(row *runtime.ProgramPaneRow) {
				row.Tap, row.PanePipe, row.TapLiveness = otherPane, true, runtime.ProgramTapAlive
			})
			p.publish(t, other)
			return p.session()
		},
	}
}

// fakeProgramPane is one fake fixture session's agent pane and the capture
// state of its incarnations.
type fakeProgramPane struct {
	fake         *runtime.Fake
	incarnations int
	store        *programstatus.Store // the current incarnation's capture; nil before arming
	stale        *programstatus.Store // the incarnation a respawn replaced
}

// newFakeProgramPane starts the fixture session on a new fake, its pane
// unarmed under mode with the capture gate passed.
func newFakeProgramPane(t *testing.T, mode runtime.ProgramStatusMode) *fakeProgramPane {
	t.Helper()
	f := runtime.NewFake()
	if err := f.Start(t.Context(), fakeProgramSession, runtime.Config{}); err != nil {
		t.Fatalf("starting fake session: %v", err)
	}
	f.SetProgramPane(fakeProgramSession, runtime.ProgramPaneRow{Mode: mode, CaptureGate: true, PaneID: fakeProgramPaneID})
	return &fakeProgramPane{fake: f}
}

// session is the pane's ProgramStatusSession.
func (p *fakeProgramPane) session() ProgramStatusSession {
	return ProgramStatusSession{
		Provider: runtime.FullFake{Fake: p.fake},
		Name:     fakeProgramSession,
		Emit:     p.emit,
		Respawn: func(t *testing.T, script ...string) {
			p.stale = p.store
			p.update(func(row *runtime.ProgramPaneRow) { row.PaneDead = false })
			p.arm(t)
			p.emit(t, script...)
		},
		PublishStale: func(t *testing.T) {
			if p.stale != nil {
				p.publish(t, p.stale)
			}
		},
		ClosePipe: func(*testing.T) {
			p.update(func(row *runtime.ProgramPaneRow) { row.PanePipe, row.TapLiveness = false, runtime.ProgramTapGone })
		},
		KillReader: func(*testing.T) {
			p.update(func(row *runtime.ProgramPaneRow) { row.TapLiveness = runtime.ProgramTapGone })
		},
		ForeignPipe: func(*testing.T) {
			// The foreign pipe-pane replaces ours: our tap reads EOF and
			// exits, and the pane's pipe stays open, now not ours.
			p.update(func(row *runtime.ProgramPaneRow) { row.PanePipe, row.TapLiveness = true, runtime.ProgramTapGone })
		},
		SetMode: func(_ *testing.T, mode runtime.ProgramStatusMode) {
			p.update(func(row *runtime.ProgramPaneRow) { row.Mode = mode })
		},
	}
}

// arm starts a new capture incarnation on the pane: a new nonce naming it,
// its pipe open, its tap alive, nothing published.
func (p *fakeProgramPane) arm(t *testing.T) {
	t.Helper()
	p.incarnations++
	nonce := fmt.Sprintf("a%d.%s", p.incarnations, fakeProgramPaneID)
	if !programstatus.ValidNonce(nonce) {
		t.Fatalf("fixture nonce %q is not valid", nonce)
	}
	p.store = programstatus.NewStore(nonce)
	p.update(func(row *runtime.ProgramPaneRow) {
		row.Tap, row.PanePipe, row.TapLiveness = nonce, true, runtime.ProgramTapAlive
		row.RecordForm, row.Record = runtime.ProgramRecordAbsent, runtime.ProgramStatus{}
	})
}

// update applies mutate to the pane's row.
func (p *fakeProgramPane) update(mutate func(*runtime.ProgramPaneRow)) {
	row, _ := p.fake.ProgramPane(fakeProgramSession)
	mutate(&row)
	p.fake.SetProgramPane(fakeProgramSession, row)
}

// emit runs steps as the pane's program and captures what it writes. Only
// a live reader of ours captures: a pane healed since the last emit starts
// a new incarnation's store, and bytes written with the reader gone are
// lost. An exit or signal step leaves the pane dead.
func (p *fakeProgramPane) emit(t *testing.T, steps ...string) {
	t.Helper()
	if len(steps) == 0 {
		return
	}
	prog, err := emit.ParseLines(steps...)
	if err != nil {
		t.Fatalf("emitter script: %v", err)
	}
	var out bytes.Buffer
	outcome, err := emit.Run(t.Context(), prog, emit.Terminal{Out: &out})
	if err != nil {
		t.Fatalf("running emitter script: %v", err)
	}
	row, _ := p.fake.ProgramPane(fakeProgramSession)
	armed := strings.HasSuffix(row.Tap, "."+row.PaneID)
	if armed && (p.store == nil || p.store.Status().Nonce != row.Tap) {
		p.store = programstatus.NewStore(row.Tap)
	}
	if armed && row.PanePipe && row.TapLiveness != runtime.ProgramTapGone {
		p.capture(out.Bytes())
		p.publish(t, p.store)
	}
	if outcome.Ended {
		p.update(func(row *runtime.ProgramPaneRow) { row.PaneDead = true })
	}
}

// capture applies the program's bytes to the current store as the tap does.
func (p *fakeProgramPane) capture(b []byte) {
	var sc programstatus.Scanner
	sc.Feed(b, func(ev programstatus.Event) {
		switch ev.Kind {
		case programstatus.EventProbe:
			p.store.Probe()
		case programstatus.EventPromptStart:
			p.store.PromptStart()
		case programstatus.EventReport:
			// A report that breaks a rule is dropped whole (spec rev 0.4),
			// as the tap drops it.
			if r, err := programstatus.ParseBody(ev.Body); err == nil {
				p.store.Apply(r, time.Now())
			}
		}
	})
}

// publish writes store's record to the pane through the codec, as the tap's
// pane-option write and the provider's read do.
func (p *fakeProgramPane) publish(t *testing.T, store *programstatus.Store) {
	t.Helper()
	v, err := programstatus.Encode(store.Status())
	if err != nil {
		t.Fatalf("encoding the captured record: %v", err)
	}
	rec, err := programstatus.Decode(v)
	p.update(func(row *runtime.ProgramPaneRow) {
		row.RecordForm, row.Record = programstatus.RecordForm(err), rec
	})
}
