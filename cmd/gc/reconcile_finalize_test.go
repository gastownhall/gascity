package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
)

// TestFinalizePatchGoldenVsLegacy (I19, DS F2; CONTRACT v5 D4). Kills a
// finalize that writes asleep where legacy writes drained, closes a seat
// that holds work or an operator's hold, or keeps restart_requested: each
// case's golden outcome holds for legacy's finalize, run on a stopped
// stop-pending row, and for finalizePatch on the same facts.
func TestFinalizePatchGoldenVsLegacy(t *testing.T) {
	for _, c := range []struct {
		name                        string
		mayClose, work, held, fresh bool
		restart                     bool
		wantClosed                  bool
		wantState, wantSleepReason  string
	}{
		{name: "a seat with no work closes", mayClose: true, wantClosed: true},
		{name: "a seat with work sleeps idle", mayClose: true, work: true, wantState: "asleep", wantSleepReason: "idle"},
		{name: "a held seat is kept, drained", mayClose: true, held: true, wantState: "drained"},
		{name: "a named row with no work is drained", wantState: "drained"},
		{name: "a named row with work sleeps idle", work: true, wantState: "asleep", wantSleepReason: "idle"},
		{name: "a fresh-wake row resets its conversation", fresh: true, wantState: "drained"},
		{name: "a restart request is consumed", restart: true, wantState: "drained"},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := newReconcilerTestEnv(t)
			env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
			b := env.createSessionBead("worker", "worker")
			meta := map[string]string{"session_key": "k-1"}
			if c.fresh {
				meta["wake_mode"] = "fresh"
			}
			if c.restart {
				meta["restart_requested"] = "true"
			}
			env.setSessionMetadata(&b, meta)
			if c.held {
				if _, err := sessionFrontDoor(env.store).OperatorSuspend(b.ID, env.clk.Now()); err != nil {
					t.Fatal(err)
				}
			}
			if c.work {
				if _, err := env.store.Create(beads.Bead{Title: "w", Type: "task", Status: "in_progress", Assignee: b.ID}); err != nil {
					t.Fatal(err)
				}
			}
			env.setSessionMetadata(&b, session.DrainAckStopPendingPatch(env.clk.Now().UTC()))
			info := env.sessionInfo(b.ID)
			finalizeDrainAckStoppedSession(env.cfg, env.store, testSeatWork(env.city, env.cfg, env.store, nil), info, "worker", c.mayClose, newFakeDrainOps(), env.dt, env.clk, events.Discard, io.Discard)
			got, err := env.store.Get(b.ID)
			if err != nil {
				t.Fatal(err)
			}
			m := got.Metadata
			closed := got.Status == "closed"
			if closed != c.wantClosed || (!c.wantClosed && (m["state"] != c.wantState || m["sleep_reason"] != c.wantSleepReason)) {
				t.Fatalf("legacy: closed=%v state=%q sleep_reason=%q, want closed=%v %q %q", closed, m["state"], m["sleep_reason"], c.wantClosed, c.wantState, c.wantSleepReason)
			}
			if c.restart && m["restart_requested"] != "" {
				t.Fatalf("legacy kept restart_requested=%q", m["restart_requested"])
			}
			if c.fresh && (m["session_key"] != "" || m["continuation_reset_pending"] != "true") {
				t.Fatalf("legacy: session_key=%q continuation_reset_pending=%q, want the fresh-wake reset", m["session_key"], m["continuation_reset_pending"])
			}
			choice := finalizePatch(finalizeFacts{
				Now: env.clk.Now().UTC(), CloseIfUnassigned: c.mayClose, HasWork: c.work, OperatorHeld: c.held,
				FreshWake: c.fresh, RestartRequested: c.restart,
			})
			if choice.CloseSeat != c.wantClosed || choice.AsSleep != c.work {
				t.Fatalf("finalizePatch: close=%v sleep=%v, want close=%v sleep=%v", choice.CloseSeat, choice.AsSleep, c.wantClosed, c.work)
			}
			if c.wantClosed {
				return
			}
			p := choice.Patch
			if p["state"] != c.wantState || (c.wantSleepReason != "" && p["sleep_reason"] != c.wantSleepReason) {
				t.Fatalf("finalizePatch: state=%q sleep_reason=%q, want %q %q", p["state"], p["sleep_reason"], c.wantState, c.wantSleepReason)
			}
			if r, ok := p["restart_requested"]; c.restart != ok || r != "" {
				t.Fatalf("finalizePatch: restart_requested %q (set %v), want cleared only on a request", r, ok)
			}
			if c.fresh && (p["continuation_reset_pending"] != "true" || p["session_key"] != "") {
				t.Fatalf("finalizePatch: %v, want the fresh-wake reset", p)
			}
		})
	}
}

// TestFinalizePatchHonoredWakeNeverCloses (D7 rule 4). Kills a close over
// a pending explicit wake AL1 honors: the finalize writes its non-close
// patch and leaves the request to the next pass's start.
func TestFinalizePatchHonoredWakeNeverCloses(t *testing.T) {
	if c := finalizePatch(finalizeFacts{CloseIfUnassigned: true, HonoredWake: true}); c.CloseSeat || c.Patch["state"] != "drained" {
		t.Fatalf("%+v, want drained and no close over an honored wake", c)
	}
}

// TestLegacyFinalizeUsesExtractedBuilder (D4: legacy calls the extracted
// builder too). Kills a legacy finalize that drifts from finalizePatch:
// finalizeDrainAckStoppedSession calls it, and builds neither drain patch
// itself.
func TestLegacyFinalizeUsesExtractedBuilder(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "session_reconciler.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	calls := map[string]int{}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "finalizeDrainAckStoppedSession" {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.Ident:
				if x.Name == "finalizePatch" {
					calls[x.Name]++
				}
			case *ast.SelectorExpr:
				calls[x.Sel.Name]++
			}
			return true
		})
	}
	if calls["finalizePatch"] == 0 || calls["AcknowledgeDrainPatch"] != 0 || calls["CompleteDrainPatch"] != 0 {
		t.Fatalf("finalizeDrainAckStoppedSession calls %v; want finalizePatch and no drain patch built itself", calls)
	}
}
