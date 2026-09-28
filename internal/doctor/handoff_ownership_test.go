package doctor

import (
	"os"
	"path/filepath"
	"testing"
)

// A city handed to bd by the journaled ownership handoff has neither of the two
// signals doctor used to classify on: the transfer writes no
// .gc/scope-ownership.json record (bd's journal is the record), and the
// replacement store is bd-owned *direct*, so metadata.json still says
// dolt_mode: server. Before this arm existed the whole city read as
// legacy-GC-managed to every doctor check — most visibly as a rig dolt-backup
// warning prescribing a `dolt backup` against a server gc no longer runs.
//
// The read here is deliberately shallow, exactly like scopeJournalStateIs next
// to it: cmd/gc owns the journal's schema and its full validation, because
// cmd/gc is where the answer gates a lifecycle action (whether to start a
// second sql-server). Doctor's answer only chooses which lens to report
// through, so an unreadable, malformed, or not-yet-committed journal is simply
// "not provider-owned" — the ordinary lens, which is what doctor did before.

func writeHandoffJournal(t *testing.T, scopeRoot, body string) {
	t.Helper()
	beadsDir := filepath.Join(scopeRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "ownership-handoff.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestScopeIsProviderOwnedReadsACommittedOwnershipHandoff(t *testing.T) {
	city := t.TempDir()
	writeHandoffJournal(t, city, `{"phase":"committed","owner":"bd","request":{"owner":"legacy-gc"}}`)

	if !scopeIsProviderOwned(city, city) {
		t.Fatal("a city with a committed ownership handoff is bd's, but doctor still classifies it as gc-managed")
	}
}

func TestScopeIsProviderOwnedIgnoresAnUnsettledOwnershipHandoff(t *testing.T) {
	for name, body := range map[string]string{
		"pending":        `{"phase":"old_owner_stopped","owner":"legacy-gc"}`,
		"rolled back":    `{"phase":"rolled_back","owner":"legacy-gc"}`,
		"wrong owner":    `{"phase":"committed","owner":"legacy-gc"}`,
		"malformed":      `{"phase":`,
		"empty":          ``,
		"unknown phase":  `{"phase":"something_else","owner":"bd"}`,
		"absent journal": "",
	} {
		t.Run(name, func(t *testing.T) {
			city := t.TempDir()
			if name != "absent journal" {
				writeHandoffJournal(t, city, body)
			}
			if scopeIsProviderOwned(city, city) {
				t.Fatalf("a %s handoff journal made doctor hand the scope to bd", name)
			}
		})
	}
}

// The handoff is city-root only, so a rig inherits the city's transfer rather
// than carrying one of its own. Doctor asks the question per scope, so a rig
// under a handed-off city has to answer yes without a journal in its own
// .beads — otherwise every rig in a handed-off city keeps the legacy lens.
func TestScopeIsProviderOwnedCoversARigUnderAHandedOffCity(t *testing.T) {
	city := t.TempDir()
	rig := filepath.Join(city, "rig")
	if err := os.MkdirAll(filepath.Join(rig, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeHandoffJournal(t, city, `{"phase":"committed","owner":"bd","request":{"owner":"legacy-gc"}}`)

	if !scopeIsProviderOwned(city, rig) {
		t.Fatal("a rig under a handed-off city is bd's too; the handoff is city-root only")
	}
}
