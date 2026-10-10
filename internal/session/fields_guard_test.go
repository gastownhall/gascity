package session

import (
	"os"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// The census is the guard: this package's tests, like cmd/gc's, internal/api's
// and internal/worker's, fail on any write of an unregistered session key.
func init() { GuardSessionKeys(func(msg string) { panic(msg) }) }

// TestMain fails the run on a violation a recover() swallowed.
func TestMain(m *testing.M) { os.Exit(FailOnKeyViolations(m.Run(), os.Stderr)) }

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
