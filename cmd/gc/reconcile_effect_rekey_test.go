package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// Arm A3 and the rekey effect (CONTRACT v5 S4, O2; I12; scenario R44).

// rekeyFixture is one row (generation 3, instance_token tok-new) on a fenced
// store, its runtime on a hardened leaf with readable identity, and the
// pass that decided from the inventory's read of that runtime.
type rekeyFixture struct {
	t     *testing.T
	store *beads.MemStore
	leaf  *fenceLeaf
	id    string
	name  string
}

func newRekeyFixture(t *testing.T, rowMeta ...string) *rekeyFixture {
	t.Helper()
	store, _ := stampedMem(t, gate.Require)
	meta := append([]string{"template", "worker", "session_name", "s-rekey", "state", "active", "generation", "3", "instance_token", "tok-new"}, rowMeta...)
	b, err := store.Create(sessionRow("rekey", meta...))
	if err != nil {
		t.Fatal(err)
	}
	return &rekeyFixture{t: t, store: store, leaf: newFenceLeaf(), id: b.ID, name: "s-rekey"}
}

// residue starts the row's runtime as a warm-reuse residue: the row's own
// session ID at epoch, carrying token.
func (f *rekeyFixture) residue(epoch, token string) runtimeIdentity {
	f.t.Helper()
	meta := map[string]string{"GC_SESSION_ID": f.id, "GC_RUNTIME_EPOCH": epoch}
	if token != "" {
		meta["GC_INSTANCE_TOKEN"] = token
	}
	startRuntime(f.t, f.leaf.Fake, f.name, meta)
	return runtimeIdentity{Known: true, SessionID: f.id, Epoch: epoch, Token: token}
}

// pass decides the row from the store as it is now, with the inventory
// reading rt, and returns the pass's effect inputs and the row's intent.
func (f *rekeyFixture) pass(rt runtimeIdentity) (*effectPass, intent) {
	f.t.Helper()
	c := readCensus(f.t, gatherNow, censusLegs(rowLeg, f.store))
	k := rowKey{Leg: rowLeg, ID: f.id}
	w := &World{
		Now: gatherNow, Census: c, Mislabelled: map[rowKey]bool{}, CityPath: f.t.Name(),
		Observed:  map[rowKey]rowObservation{k: {Identity: rt}},
		Env:       &reconcileEnv{SP: f.leaf},
		LegStores: map[string]beads.Store{rowLeg: f.store},
	}
	a := &allocDecision{Snapshot: &selectionSnapshot{Entries: map[rowKey]*selectionEntry{k: {Key: k, Liveness: livenessAlive}}}}
	it, _ := decideRow(w, a, k)
	it.Key = k
	return newEffectPass(w, a), it
}

// run runs it as its kind's transaction, as the executor does.
func (f *rekeyFixture) run(p *effectPass, it intent) settlement {
	f.t.Helper()
	if !effectSpecs[it.Kind].runs() {
		f.t.Fatalf("no effect registered for %q", it.Kind)
	}
	return runTx(context.Background(), p, it, effectSpecs[it.Kind], nil)
}

func (f *rekeyFixture) row() map[string]string {
	f.t.Helper()
	b, err := f.store.Get(f.id)
	if err != nil {
		f.t.Fatal(err)
	}
	return b.Metadata
}

// stop runs the merged stop fence on the row as stored now.
func (f *rekeyFixture) stop() fenceVerdict {
	f.t.Helper()
	row, err := sessionFrontDoor(f.store).Get(f.id)
	if err != nil {
		f.t.Fatal(err)
	}
	v, _ := fenceRun(f.t, f.leaf, fenceRequest{Row: row, Legs: legsFull}, fenceOpts{stop: true})
	return v
}

// Kills the stop of a warm-reuse residue left unreachable (OI F4, SC F5;
// scenario R44) and a rekey registered nowhere: L2 refuses the residue as a
// mismatch, A3 proposes the rekey, the effect lands the runtime's token on
// the row, and the stop's L2 then passes and the runtime is stopped.
func TestWarmReuseResidueRekeyedThenDrainable(t *testing.T) {
	f := newRekeyFixture(t)
	rt := f.residue("2", "tok-old")
	if v := f.stop(); v.Proceed || v.Reason != fenceTokenMismatch {
		t.Fatalf("stop before the rekey: %+v, want L2's mismatch", v)
	}
	p, it := f.pass(rt)
	if it.Kind != intentRekey || it.Patch["instance_token"] != "tok-old" {
		t.Fatalf("A3 = %+v, want a rekey to the runtime's token", it)
	}
	if s := f.run(p, it); s.Outcome != settledLanded {
		t.Fatalf("rekey settled %+v, want landed", s)
	}
	if got := f.row()["instance_token"]; got != "tok-old" {
		t.Fatalf("row instance_token = %q, want the runtime's tok-old", got)
	}
	if v := f.stop(); !v.Proceed || f.leaf.CountCalls("Stop", f.name) != 1 {
		t.Fatalf("stop after the rekey: %+v, want L2 Current and the runtime stopped", v)
	}
}

// Kills a stop request decided before the rekey (v5.1 B3; scenario R44):
// a stop-pending row whose runtime is StaleSelf is re-keyed first, and the
// next pass, reading the runtime Current, proposes no second rekey while
// the stop's L2 passes. C6b2's A4 lands below A3 in rowArms.
func TestStaleSelfStopPendingRowRekeyedThenStopped(t *testing.T) {
	f := newRekeyFixture(t, "state", "draining", "state_reason", "drain-ack-stop-pending")
	rt := f.residue("3", "tok-old")
	p, it := f.pass(rt)
	if it.Kind != intentRekey {
		t.Fatalf("stop-pending StaleSelf row decided %+v, want A3's rekey first", it)
	}
	if s := f.run(p, it); s.Outcome != settledLanded {
		t.Fatalf("rekey settled %+v, want landed", s)
	}
	if _, next := f.pass(rt); next.Kind == intentRekey {
		t.Fatalf("the re-keyed row decided %+v, want no second rekey", next)
	}
	if v := f.stop(); !v.Proceed {
		t.Fatalf("stop after the rekey: %+v, want it to proceed", v)
	}
}

// Kills a rekey to an empty runtime token (v5.1 B6): the comparator's
// same-ID empty-token row reads Unknown, so A3 proposes nothing; and an
// effect whose fresh read finds the token cleared refuses, writing nothing.
func TestRekeyRequiresNonEmptyRuntimeToken(t *testing.T) {
	f := newRekeyFixture(t)
	if _, it := f.pass(f.residue("2", "")); it.Kind == intentRekey {
		t.Fatalf("A3 = %+v on an empty runtime token, want no rekey", it)
	}

	f = newRekeyFixture(t)
	p, it := f.pass(f.residue("2", "tok-old"))
	if err := f.leaf.SetMeta(f.name, "GC_INSTANCE_TOKEN", ""); err != nil {
		t.Fatal(err)
	}
	if s := f.run(p, it); s.Outcome != settledRefused || s.Cause != causeIdentityChanged {
		t.Fatalf("settlement %+v, want refused %s", s, causeIdentityChanged)
	}
	if got := f.row()["instance_token"]; got != "tok-new" {
		t.Fatalf("row instance_token = %q, want it unwritten", got)
	}
}

// Kills a rekey that trusts the pass's read (S4): the runtime's token moved
// on after the pass, the runtime went, or another holder has its name
// lock; each refuses, writing nothing.
func TestRekeyRefusesOnTokenChange(t *testing.T) {
	for _, tc := range []struct {
		name  string
		after func(f *rekeyFixture)
		cause string
	}{
		{"token changed", func(f *rekeyFixture) { _ = f.leaf.SetMeta(f.name, "GC_INSTANCE_TOKEN", "tok-other") }, causeIdentityChanged},
		{"runtime gone", func(f *rekeyFixture) { _ = f.leaf.Stop(f.name) }, causeNotPresent},
		{"presence unreadable", func(f *rekeyFixture) { f.leaf.LivenessErrors[f.name] = errors.New("tmux: server busy") }, causeLivenessUnknown},
		{"name busy", func(f *rekeyFixture) { f.t.Cleanup(runtimeNames.tryLock(f.t.Name(), f.name)) }, causeNameBusy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRekeyFixture(t)
			p, it := f.pass(f.residue("2", "tok-old"))
			tc.after(f)
			if s := f.run(p, it); s.Outcome != settledRefused || s.Cause != tc.cause {
				t.Fatalf("settlement %+v, want refused %s", s, tc.cause)
			}
			if got := f.row()["instance_token"]; got != "tok-new" {
				t.Fatalf("row instance_token = %q, want it unwritten", got)
			}
		})
	}
}

// Kills a rekey that writes generation, or lands across a moved one (I12):
// a landed rekey leaves generation as it was; a NewerSelf runtime is held,
// never re-keyed; and a generation raised after the pass refuses the CAS.
func TestRekeyNeverLowersGeneration(t *testing.T) {
	f := newRekeyFixture(t)
	p, it := f.pass(f.residue("2", "tok-old"))
	if s := f.run(p, it); s.Outcome != settledLanded || f.row()["generation"] != "3" {
		t.Fatalf("settlement %+v, generation %q; want landed at generation 3", s, f.row()["generation"])
	}

	f = newRekeyFixture(t)
	if _, it := f.pass(f.residue("4", "tok-next")); it.Kind != "" || it.Reason != decideNewerSelf {
		t.Fatalf("A3 = %+v on a NewerSelf runtime, want a hold", it)
	}

	f = newRekeyFixture(t)
	p, it = f.pass(f.residue("2", "tok-old"))
	if err := f.store.SetMetadata(f.id, "generation", "5"); err != nil {
		t.Fatal(err)
	}
	if s := f.run(p, it); s.Outcome != settledRefused || f.row()["generation"] != "5" || f.row()["instance_token"] != "tok-new" {
		t.Fatalf("settlement %+v, row %v; want refused with the raised generation and token kept", s, f.row())
	}
}

// Kills a rekey of a pending create's runtime (v5 X2): A3 proposes none
// while the row holds pending_create_claim, and a claim stamped after the
// pass refuses the CAS, which re-decides on the fresh row.
func TestRekeyRefusedWhilePendingCreateClaim(t *testing.T) {
	f := newRekeyFixture(t, "pending_create_claim", "true")
	if _, it := f.pass(f.residue("2", "tok-old")); it.Kind == intentRekey {
		t.Fatalf("A3 = %+v under a pending create claim, want no rekey", it)
	}

	f = newRekeyFixture(t)
	p, it := f.pass(f.residue("2", "tok-old"))
	if err := f.store.SetMetadata(f.id, "pending_create_claim", "true"); err != nil {
		t.Fatal(err)
	}
	if s := f.run(p, it); s.Outcome != settledRefused || s.Cause != causePremise || f.row()["instance_token"] != "tok-new" {
		t.Fatalf("settlement %+v, row token %q; want refused %s, unwritten", s, f.row()["instance_token"], causePremise)
	}
}

// hookLeaf is the fixture's leaf with a session object behind the name, and
// a hook run once, on the effect's goroutine, when the second presence read
// begins: after the rekey's identity read has completed (the sidecar read
// compares two reads, so a hook inside it would only fail that read).
type hookLeaf struct {
	*fenceLeaf
	object, created string
	reads           int
	after           func()
}

func (l *hookLeaf) ObserveLivenessWithError(name string, pn []string) (runtime.Liveness, error) {
	if l.reads++; l.reads == 2 && l.after != nil {
		l.after()
	}
	live, err := l.fenceLeaf.ObserveLivenessWithError(name, pn)
	live.ObjectID, live.ObjectCreated = l.object, l.created
	return live, err
}

// Kills an identity read not bracketed by presence (v5 O2), and a bracket on
// the object ID alone: a runtime replaced under the name right after the
// identity read, by another session object or by one reusing the ID with
// another creation time (a server restart), refuses as not present, writing
// nothing.
func TestRekeyBracketsIdentityReadWithPresence(t *testing.T) {
	for _, tc := range []struct {
		name            string
		object, created string
	}{{"another object", "$2", "100"}, {"reused id, another creation time", "$1", "200"}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRekeyFixture(t)
			rt := f.residue("2", "tok-old")
			hook := &hookLeaf{fenceLeaf: f.leaf, object: "$1", created: "100"}
			p, it := f.pass(rt)
			p.Runtime = hook
			hook.after = func() { hook.object, hook.created = tc.object, tc.created }
			if s := f.run(p, it); s.Outcome != settledRefused || s.Cause != causeNotPresent {
				t.Fatalf("settlement %+v, want refused %s", s, causeNotPresent)
			}
			if got := f.row()["instance_token"]; got != "tok-new" {
				t.Fatalf("row instance_token = %q, want it unwritten", got)
			}
		})
	}
}

// Kills the rekey's reads outside the row's session mutation lock (the C4c2
// review's race): an in-process restart (Manager.ensureRunning, which takes
// that lock) that gives the runtime the row's token right after the
// identity read must not be overwritten by a CAS of the stale token. Under
// the lock the restart waits for the rekey and then stamps the row's new
// token, so the row and the runtime end on one token.
func TestRekeyHoldsTheMutationLockFromReadToCAS(t *testing.T) {
	f := newRekeyFixture(t)
	rt := f.residue("2", "tok-old")
	hook := &hookLeaf{fenceLeaf: f.leaf, object: "$1"}
	p, it := f.pass(rt)
	p.Runtime = hook
	restarted := make(chan struct{})
	hook.after = func() {
		go func() {
			defer close(restarted)
			_ = session.WithSessionMutationLock(f.id, func() error {
				return f.leaf.SetMeta(f.name, "GC_INSTANCE_TOKEN", f.row()["instance_token"])
			})
		}()
		select { // an open window lets the restart land before the CAS
		case <-restarted:
		case <-time.After(50 * time.Millisecond):
		}
	}
	s := f.run(p, it)
	select {
	case <-restarted:
	case <-time.After(10 * time.Second):
		t.Fatal("the restart never ran")
	}
	token, err := f.leaf.GetMeta(f.name, "GC_INSTANCE_TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	if got := f.row()["instance_token"]; got != token {
		t.Fatalf("settlement %+v: row token %q, runtime token %q; want one token", s, got, token)
	}
}
