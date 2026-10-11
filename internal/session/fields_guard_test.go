package session

import (
	"os"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
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
// (spent once), a fixture's or a listed bypass writer's; a hand-built one
// fails, as does a mint with a key added beside it.
func TestGuardDispositionWrites(t *testing.T) {
	prod := func() (string, bool, bool) { return "cmd/gc.someWriter", false, false }
	if msg := checkDispositionWrite(map[string]string{"state": "asleep"}, prod); msg != "" {
		t.Errorf("a write with no class key failed: %s", msg)
	}
	if msg := checkDispositionWrite(map[string]string{"held_until": "x"}, prod); !strings.Contains(msg, "written by hand at cmd/gc.someWriter") {
		t.Errorf("a hand-built class write = %q, want a violation naming its writer", msg)
	}
	minted := DecodeDisposition(nil)
	minted.Wait = true
	write := minted.Changed().Into(MetadataPatch{"state": "asleep"})
	if msg := checkDispositionWrite(write, prod); msg != "" {
		t.Errorf("a minted write failed: %s", msg)
	}
	if msg := checkDispositionWrite(write, prod); msg == "" {
		t.Error("a mint was spent twice")
	}
	minted.Changed().Into(nil)
	if msg := checkDispositionWrite(map[string]string{"wait_hold": "true", "held_until": "x"}, prod); msg == "" {
		t.Error("a mint with a hand-built key beside it passed")
	}
	if msg := checkDispositionWrite(map[string]string{"wait_hold": "true"}, prod); msg != "" {
		t.Errorf("the unspent mint failed its exact write: %s", msg)
	}
	if msg := checkDispositionWrite(map[string]string{"held_until": "x"}, func() (string, bool, bool) { return "cmd/gc.TestX", false, true }); msg != "" {
		t.Errorf("a fixture write failed: %s", msg)
	}
	if msg := checkDispositionWrite(map[string]string{"held_until": "x"}, func() (string, bool, bool) { return "cmd/gc.healExpiredTimersInfo", true, false }); msg != "" {
		t.Errorf("a bypass writer's write failed: %s", msg)
	}
	if msg := checkDispositionWrite(minted.Encode().Fold(), prod); msg == "" {
		t.Error("a Fold, which mints nothing, passed as a write")
	}
}

// TestDispositionGuardReadsTheStack: the plumbing is found under go test's
// absolute paths and bazel's workspace-relative ones, closures fold into
// their function, and a write from a test through the generic verbs is a
// fixture.
func TestDispositionGuardReadsTheStack(t *testing.T) {
	for _, file := range []string{"/src/gascity/internal/beads/memstore.go", "internal/beads/memstore.go", "internal/session/store.go", "/x/cmd/gc/class_store_emit.go"} {
		if !plumbing(file) {
			t.Errorf("plumbing(%q) = false", file)
		}
	}
	for _, file := range []string{"internal/session/manager.go", "cmd/gc/session_beads.go", "internal/beads/memstore_test.go"} {
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
	if at, bypass, fixture := dispositionWriterFromHere(); !fixture || bypass {
		t.Errorf("a test's write: at %q bypass %v fixture %v, want a fixture", at, bypass, fixture)
	}
}

// dispositionWriterFromHere reads the stack as the hook does, from a frame
// standing for the hook (hookFrame).
func dispositionWriterFromHere() (string, bool, bool) { return hookFrame() }

func hookFrame() (string, bool, bool) { return dispositionWriter() }
