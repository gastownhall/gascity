package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeOwnershipJournalFile(t *testing.T, city, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(city, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(city, ".gc", scopeOwnershipFile), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A scope relocated behind a symlink still resolves to the same directory, so
// its record is not drift. Rejecting the whole journal for it bricked the
// city: every start and every stop reads the journal first, so one relocated
// rig stranded the proxies of every other scope.
func TestOwnershipJournalTolerantOfSymlinkedScopePath(t *testing.T) {
	city := t.TempDir()
	physical := filepath.Join(city, "moved", "r1")
	if err := os.MkdirAll(physical, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(city, "rigs"), 0o755); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(city, "rigs", "r1")
	if err := os.Symlink(physical, linked); err != nil {
		t.Fatal(err)
	}
	writeOwnershipJournalFile(t, city, `{"version":1,"scopes":{`+
		`"city":{"scope_path":"`+normalizePathForCompare(city)+`","lifecycle_owner":"provider","state":"ready"},`+
		`"rig:r1":{"scope_path":"`+linked+`","lifecycle_owner":"provider","state":"ready"}}}`)

	journal, exists, err := loadProviderScopeOwnershipJournal(city)
	if err != nil {
		t.Fatalf("loadProviderScopeOwnershipJournal: %v", err)
	}
	if !exists || len(journal.Scopes) != 2 {
		t.Fatalf("journal = (%t, %+v), want both scopes", exists, journal.Scopes)
	}
	if _, owned, err := providerScopeOwnership(city, physical); err != nil || !owned {
		t.Fatalf("relocated rig ownership = (%t, %v), want owned", owned, err)
	}
	if _, owned, err := providerScopeOwnership(city, city); err != nil || !owned {
		t.Fatalf("city ownership = (%t, %v), want owned", owned, err)
	}
}

// A genuinely malformed record is still refused: a relative or uncleaned path
// is not a relocation, it is a file gc cannot act on.
func TestOwnershipJournalStillRefusesMalformedScopePath(t *testing.T) {
	for _, tt := range []struct {
		name string
		path string
	}{
		{name: "relative", path: "rigs/r1"},
		{name: "uncleaned", path: "/tmp/rigs/../rigs/r1"},
		{name: "trailing separator", path: "/tmp/rigs/r1/"},
		{name: "empty", path: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			city := t.TempDir()
			writeOwnershipJournalFile(t, city, `{"version":1,"scopes":{"rig:r1":{"scope_path":"`+tt.path+
				`","lifecycle_owner":"provider","state":"ready"}}}`)
			if _, _, err := loadProviderScopeOwnershipJournal(city); err == nil {
				t.Fatalf("journal with a %s scope path loaded", tt.name)
			}
		})
	}
}

// Two records that name the same directory through different spellings are
// still a duplicate: whichever one a lookup found first would decide the
// scope's state.
func TestOwnershipJournalRefusesDuplicateScopePathsThroughSymlink(t *testing.T) {
	city := t.TempDir()
	physical := filepath.Join(city, "moved", "r1")
	if err := os.MkdirAll(physical, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(city, "rigs"), 0o755); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(city, "rigs", "r1")
	if err := os.Symlink(physical, linked); err != nil {
		t.Fatal(err)
	}
	writeOwnershipJournalFile(t, city, `{"version":1,"scopes":{`+
		`"rig:r1":{"scope_path":"`+linked+`","lifecycle_owner":"provider","state":"ready"},`+
		`"rig:r2":{"scope_path":"`+normalizePathForCompare(physical)+`","lifecycle_owner":"provider","state":"ready"}}}`)
	if _, _, err := loadProviderScopeOwnershipJournal(city); err == nil ||
		!strings.Contains(err.Error(), "duplicate scope ownership paths") {
		t.Fatalf("loadProviderScopeOwnershipJournal = %v, want a duplicate-path refusal", err)
	}
}

// A city.toml that no longer parses declares no rigs, so cfg.Rigs is empty and
// the journal is the only remaining record of which scopes this city owns. A
// retiring op has to read every entry in it, not only the detached `path:`
// ones, or a normally-added rig's proxy outlives the city.
func TestRetiringScopeRootsCoverRigKeysWhenCityConfigIsUnparseable(t *testing.T) {
	city := t.TempDir()
	rig := filepath.Join(city, "rigs", "r1")
	if err := os.MkdirAll(rig, 0o755); err != nil {
		t.Fatal(err)
	}
	detached := filepath.Join(city, "rigs", "gone")
	if err := os.MkdirAll(detached, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("this is not toml = = =\n"), 0o644); err != nil { //nolint:gosec // fixture config
		t.Fatal(err)
	}
	writeOwnershipJournalFile(t, city, `{"version":1,"scopes":{`+
		`"city":{"scope_path":"`+normalizePathForCompare(city)+`","lifecycle_owner":"provider","state":"ready"},`+
		`"rig:r1":{"scope_path":"`+normalizePathForCompare(rig)+`","lifecycle_owner":"provider","state":"ready"},`+
		`"path:`+normalizePathForCompare(detached)+`":{"scope_path":"`+normalizePathForCompare(detached)+
		`","lifecycle_owner":"provider","state":"ready"}}}`)

	roots, err := providerOwnedLifecycleScopeRoots(city, "stop")
	if err != nil {
		t.Fatalf("providerOwnedLifecycleScopeRoots(stop): %v", err)
	}
	want := []string{
		normalizePathForCompare(city),
		normalizePathForCompare(detached),
		normalizePathForCompare(rig),
	}
	if !reflect.DeepEqual(roots, want) {
		t.Fatalf("stop roots = %#v, want %#v", roots, want)
	}
}
