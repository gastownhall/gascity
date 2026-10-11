package session

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// The census is the guard: this package's tests, like cmd/gc's, internal/api's
// and internal/worker's, fail on any write of an unregistered session key, and
// on a hand-built write of a disposition class key.
func init() {
	GuardSessionKeys(func(msg string) { panic(msg) })
	GuardDispositionWrites(func(msg string) { panic(msg) })
}

// TestMain fails the run on a violation a recover() swallowed, and on a start
// or stop refused for want of a city that no test expected.
func TestMain(m *testing.M) {
	os.Exit(FailOnNoCityRefusals(func() int { return FailOnKeyViolations(m.Run(), os.Stderr) }, os.Stderr))
}

// TestGuardSessionKeys pins the guard: any write of an unregistered key to a
// session row fails, a fixture's as much as production code's; a registered
// key, a test_* fixture key, and a key on another bead pass.
func TestGuardSessionKeys(t *testing.T) {
	var failed []string
	GuardSessionKeys(func(msg string) { failed = append(failed, msg) })
	defer GuardSessionKeys(func(msg string) { panic(msg) })
	defer func() { keyViolations.Lock(); keyViolations.msgs = nil; keyViolations.Unlock() }() // its own, deliberate
	m := beads.NewMemStore()
	b, err := m.Create(beads.Bead{Type: BeadType, Metadata: map[string]string{"state": "asleep", "test_fixture": "x"}})
	if err != nil {
		t.Fatal(err)
	}
	work, err := m.Create(beads.Bead{Type: "task", Metadata: map[string]string{"anything": "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetMetadata(b.ID, "fixture_junk", "x"); err != nil { // a fixture's own write
		t.Fatal(err)
	}
	if _, err := NewStore(beads.SessionStore{Store: m}).UpdateMetadataFenced(b.ID, 1, func(Info, PersistedResponse) MetadataPatch {
		return MetadataPatch{"unregistered_key": "v"} // production code's write
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.SetMetadata(work.ID, "work_key", "x"); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(failed, "\n")
	if keyViolations.Lock(); len(keyViolations.msgs) != 2 {
		t.Errorf("recorded %d violations, want 2", len(keyViolations.msgs))
	}
	keyViolations.Unlock()
	if len(failed) != 2 || !strings.Contains(got, `"fixture_junk"`) || !strings.Contains(got, `"unregistered_key"`) || !strings.Contains(got, "UpdateMetadataFenced") {
		t.Fatalf("guard reports %q, want failures for fixture_junk and unregistered_key, with their stacks", failed)
	}
}

// TestGuardDispositionWrites pins the disposition write guard's verdicts: a
// write carrying class keys passes only as exactly a minted DispositionPatch
// for its row (spent once), a fixture's, or a listed bypass writer's within
// its bound; a hand-built one fails, as does a key beside a mint, a mint for
// another row, and a bypass writer's write outside its bound.
func TestGuardDispositionWrites(t *testing.T) {
	prod := func() (string, []string, bool) { return "cmd/gc.someWriter", nil, false }
	if msg := checkDispositionWrite("r1", map[string]string{"state": "asleep"}, prod); msg != "" {
		t.Errorf("a write with no class key failed: %s", msg)
	}
	if msg := checkDispositionWrite("r1", map[string]string{"held_until": "x"}, prod); !strings.Contains(msg, "written by hand at cmd/gc.someWriter") {
		t.Errorf("a hand-built class write = %q, want a violation naming its writer", msg)
	}
	minted := DecodeDisposition(nil)
	minted.Wait = true
	write := minted.Changed().Into("r1", MetadataPatch{"state": "asleep"})
	if msg := checkDispositionWrite("r2", write, prod); msg == "" {
		t.Error("a mint for r1 passed a write to r2")
	}
	if msg := checkDispositionWrite("r1", write, prod); msg != "" {
		t.Errorf("a minted write failed: %s", msg)
	}
	if msg := checkDispositionWrite("r1", write, prod); msg == "" {
		t.Error("a mint was spent twice")
	}
	minted.Changed().Into("r1", nil)
	if msg := checkDispositionWrite("r1", map[string]string{"wait_hold": "true", "held_until": "x"}, prod); msg == "" {
		t.Error("a mint with a hand-built key beside it passed")
	}
	if msg := checkDispositionWrite("r1", map[string]string{"wait_hold": "true"}, prod); msg != "" {
		t.Errorf("the unspent mint failed its exact write: %s", msg)
	}
	if msg := checkDispositionWrite("r1", map[string]string{"held_until": "x"}, func() (string, []string, bool) { return "cmd/gc.TestX", nil, true }); msg != "" {
		t.Errorf("a fixture write failed: %s", msg)
	}
	if msg := checkDispositionWrite("r1", minted.Encode().Fold(), prod); msg == "" {
		t.Error("a Fold, which mints nothing, passed as a write")
	}
	stamp := dispT0.Format(time.RFC3339)
	for _, tc := range []struct {
		writer string
		class  map[string]string
		admit  bool
	}{
		{"cmd/gc.fencedWriter.casRow", map[string]string{"quarantined_until": stamp, "sleep_intent": ""}, true},
		{"cmd/gc.fencedWriter.casRow", map[string]string{"sleep_intent": "user-hold"}, false},
		{"cmd/gc.fencedWriter.casRow", map[string]string{"held_until": stamp}, false},
		{"cmd/gc.fencedWriter.casRow", map[string]string{"suspended_at": stamp}, false},
	} {
		on := func() (string, []string, bool) { return "cmd/gc.inner", []string{tc.writer}, false }
		if msg := checkDispositionWrite("r3", tc.class, on); (msg == "") != tc.admit {
			t.Errorf("%s writing %v: admitted %v, want %v", tc.writer, tc.class, msg == "", tc.admit)
		}
	}
	unlisted := func() (string, []string, bool) { return "x", []string{"cmd/gc.healExpiredTimersInfo"}, false }
	if msg := checkDispositionWrite("r3", map[string]string{"held_until": ""}, unlisted); msg == "" {
		t.Error("a writer moved onto its cell (no longer listed) passed a hand-built clear")
	}
}

// TestDispositionGuardReadsTheStack: the plumbing is found under go test's
// absolute paths and bazel's workspace-relative ones, closures fold into
// their function, and a write from a test through the generic verbs is a
// fixture.
func TestDispositionGuardReadsTheStack(t *testing.T) {
	for _, file := range []string{"/src/gascity/internal/beads/memstore.go", "internal/beads/memstore.go", "/x/cmd/gc/class_store_emit.go", "cmd/gc/bead_policy_store.go"} {
		if !plumbing(file) {
			t.Errorf("plumbing(%q) = false", file)
		}
	}
	for _, file := range []string{"internal/session/manager.go", "internal/session/store.go", "cmd/gc/session_beads.go", "internal/beads/memstore_test.go"} {
		if plumbing(file) {
			t.Errorf("plumbing(%q) = true", file)
		}
	}
	for full, want := range map[string]string{
		"github.com/gastownhall/gascity/cmd/gc.reopenNamed.func1.2":          "cmd/gc.reopenNamed",
		"github.com/gastownhall/gascity/internal/session.(*Manager).Suspend": "internal/session.(*Manager).Suspend",
		"github.com/gastownhall/gascity/cmd/gc.(*sessionKillFence).rollback": "cmd/gc.(*sessionKillFence).rollback",
	} {
		if got := dispositionFuncName(full); got != want {
			t.Errorf("dispositionFuncName(%q) = %q, want %q", full, got, want)
		}
	}
	if !dispositionVerbs["internal/session.(*Store).ApplyPatch"] || dispositionVerbs["internal/session.(*Store).OperatorSuspend"] {
		t.Error("the session store's generic verbs, and only they, are plumbing")
	}
	if at, bypass, fixture := dispositionWriterFromHere(); !fixture || bypass != nil {
		t.Errorf("a test's write: at %q bypass %v fixture %v, want a fixture", at, bypass, fixture)
	}
}

// dispositionWriterFromHere reads the stack as the hook does, from a frame
// standing for the hook (hookFrame).
func dispositionWriterFromHere() (string, []string, bool) { return hookFrame() }

func hookFrame() (string, []string, bool) { return dispositionWriter() }

// TestLostDispositionWritesLeaveNoMint (L1, L1b): a move whose every CAS
// attempt loses leaves no open mint, so a hand-built write of the same
// content fails the guard, on its row and on the same ID in another store
// (every MemStore's first row is gc-1).
func TestLostDispositionWritesLeaveNoMint(t *testing.T) {
	stamped := stampedMemStore(t, gate.Auto, beads.NewMemStore())
	b, err := stamped.Create(beads.Bead{Title: "w", Type: BeadType, Labels: []string{LabelSession}, Metadata: map[string]string{"state": string(StateActive), "sleep_intent": "idle-stop-pending"}})
	if err != nil {
		t.Fatal(err)
	}
	before := OpenDispositionMints(b.ID)
	loser := &wakeCASLoser{rowWriteRecorder: &rowWriteRecorder{Store: stamped}, backing: stamped}
	if w, err := NewStore(beads.SessionStore{Store: loser}).ApplyDispositionFresh(b.ID, ActorController, DispEvent{Kind: EvIdleStopEnd}, nil, dispT0); err != nil || w.Result != CommitContended {
		t.Fatalf("IdleStopEnd under a CAS that always loses = %+v, %v; want contended", w, err)
	}
	if n := OpenDispositionMints(b.ID) - before; n != 0 {
		t.Errorf("the lost move left %d open mints for %s", n, b.ID)
	}
	other := beads.NewMemStore()
	b2, err := other.Create(beads.Bead{Title: "x", Type: BeadType, Labels: []string{LabelSession}, Metadata: map[string]string{"state": string(StateActive)}})
	if err != nil {
		t.Fatal(err)
	}
	prod := func() (string, []string, bool) { return "cmd/gc.someHandWriter", nil, false }
	if b2.ID == b.ID && checkDispositionWrite(b2.ID, map[string]string{"sleep_intent": ""}, prod) == "" {
		t.Errorf("a hand-built {sleep_intent: \"\"} to %s in another store passed the guard", b2.ID)
	}
}

// TestUpdateMetadataFencedUnmintsLostAttempts: a fenced decider that mints
// (the wait clear, the undos) and loses every CAS attempt leaves no open mint.
func TestUpdateMetadataFencedUnmintsLostAttempts(t *testing.T) {
	stamped := stampedMemStore(t, gate.Auto, beads.NewMemStore())
	b, err := stamped.Create(beads.Bead{Title: "w", Type: BeadType, Labels: []string{LabelSession}, Metadata: map[string]string{"state": string(StateActive), "wait_hold": "true"}})
	if err != nil {
		t.Fatal(err)
	}
	before := OpenDispositionMints(b.ID)
	loser := &wakeCASLoser{rowWriteRecorder: &rowWriteRecorder{Store: stamped}, backing: stamped}
	ok, err := NewStore(beads.SessionStore{Store: loser}).UpdateMetadataFenced(b.ID, 3, func(info Info, _ PersistedResponse) MetadataPatch {
		to, err := DispositionOfInfo(info).Apply(ActorController, DispEvent{Kind: EvWaitEnd}, State(info.MetadataState), dispT0)
		if err != nil {
			t.Fatal(err)
		}
		return to.Changed().Into(info.ID, nil)
	})
	if ok || err != nil {
		t.Fatalf("UpdateMetadataFenced under a CAS that always loses = %v, %v; want not written", ok, err)
	}
	if n := OpenDispositionMints(b.ID) - before; n != 0 {
		t.Errorf("the lost attempts left %d open mints", n)
	}
}
