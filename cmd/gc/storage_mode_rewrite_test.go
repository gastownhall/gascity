package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
)

// This file covers ga-qi9km: `gc rig add` and `gc supervisor run` silently
// rewrite a city's .beads/metadata.json from embedded to server, orphaning the
// database that holds the beads, after which every work-store read answers `[]`
// with exit 0.
//
// It is a fail-open on the WORK leg and it defeats a federated reader from the
// inside. `gc ready` fails loudly on a dead rig or an unreadable binding, but
// if the city work leg answers empty because its metadata was rewritten
// underneath it, the federation reports a confident short answer with every
// leg "healthy". Nothing here is split-store specific: the fixtures below carry
// no [storage] section, because a single-store city is bitten identically.
//
// Both fixtures are real directories with real files. The defect survived a
// suite that passes precisely because it lives in the disagreement between a
// JSON file and a directory listing, and a double for either one asserts the
// bug away.

// embeddedScopeWithBeads builds a scope whose .beads/ is an embedded-mode bd
// workspace with a populated Dolt repository — what `bd init -p <prefix>`
// leaves behind, and what the live proof's city had before gc touched it.
//
// The Dolt repository is represented by the directory shape gc itself uses to
// recognize one (a `.dolt` subdirectory, the same test gc doctor's
// doltReposUnder applies). Standing up a real Dolt server to prove a
// path-and-JSON disagreement would test Dolt, not this.
func embeddedScopeWithBeads(t *testing.T, database string) string {
	t.Helper()
	scope := t.TempDir()
	if err := os.MkdirAll(filepath.Join(scope, ".beads", "embeddeddolt", database, ".dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeScopeMetadata(t, scope, map[string]string{
		"database":      "dolt",
		"backend":       "dolt",
		"dolt_mode":     "embedded",
		"dolt_database": database,
	})
	return scope
}

func readScopeDoltMode(t *testing.T, scope string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(scope, ".beads", "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	var meta struct {
		DoltMode string `json:"dolt_mode"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	return meta.DoltMode
}

// captureStorageModeChanges redirects the sink the canonicalization announces
// storage-mode changes on and returns the buffer holding them.
func captureStorageModeChanges(t *testing.T) *bytes.Buffer {
	t.Helper()
	orig := storageModeChangeSink
	buf := &bytes.Buffer{}
	storageModeChangeSink = buf
	t.Cleanup(func() { storageModeChangeSink = orig })
	return buf
}

// emptyBdRunner answers every bd invocation with `[]` and exit 0 — the exact
// thing bd does after the rewrite, because bd is not broken: it connects to the
// database its metadata names, runs the query, and matches nothing.
func emptyBdRunner(_, _ string, _ ...string) ([]byte, error) { return []byte("[]"), nil }

// TestCanonicalizingAScopeAnnouncesAStorageModeChange is priority (1) of
// ga-qi9km: a command that changes a city's storage mode must say so.
//
// The rewrite itself is kept — see announceStorageModeChange for why gc's
// managed store cannot run a scope in embedded mode — so what this asserts is
// the change becoming VISIBLE, and visible with the consequence attached: an
// operator who reads "changed embedded to server" and an operator who reads
// "…and .beads/embeddeddolt/jc will stop being read" are in two different
// positions when their beads disappear.
//
// Red before the fix: the canonicalization emitted nothing at all. It flipped
// dolt_mode, wrote the file, and returned nil, which is how the live proof
// discovered the rewrite by diffing metadata.json rather than by being told.
func TestCanonicalizingAScopeAnnouncesAStorageModeChange(t *testing.T) {
	scope := embeddedScopeWithBeads(t, "jc")
	notices := captureStorageModeChanges(t)

	if err := ensureCanonicalScopeMetadataForInit(fsys.OSFS{}, scope, "jc"); err != nil {
		t.Fatalf("ensureCanonicalScopeMetadataForInit: %v", err)
	}

	// The rewrite still happens: this fix makes it loud, it does not make the
	// managed store unusable.
	if mode := readScopeDoltMode(t, scope); mode != "server" {
		t.Fatalf("dolt_mode = %q after canonicalization, want %q", mode, "server")
	}
	orphan := filepath.Join(scope, ".beads", "embeddeddolt", "jc")
	for _, want := range []string{scope, "embedded", "server", orphan, "STOP reading"} {
		if !strings.Contains(notices.String(), want) {
			t.Errorf("the storage-mode change never names %q; notices=%q", want, notices.String())
		}
	}
}

// TestCanonicalizingAnAlreadyCanonicalScopeIsSilent keeps the signal worth
// something. Every boot re-canonicalizes every scope; a line per scope per boot
// is a line nobody reads, and the one that matters would arrive inside it.
func TestCanonicalizingAnAlreadyCanonicalScopeIsSilent(t *testing.T) {
	for name, meta := range map[string]map[string]string{
		"already server":   {"database": "dolt", "backend": "dolt", "dolt_mode": "server", "dolt_database": "jc"},
		"no mode recorded": {"database": "dolt", "backend": "dolt", "dolt_database": "jc"},
	} {
		t.Run(name, func(t *testing.T) {
			scope := t.TempDir()
			writeScopeMetadata(t, scope, meta)
			notices := captureStorageModeChanges(t)

			if err := ensureCanonicalScopeMetadataForInit(fsys.OSFS{}, scope, "jc"); err != nil {
				t.Fatalf("ensureCanonicalScopeMetadataForInit: %v", err)
			}
			if notices.Len() != 0 {
				t.Fatalf("a canonical scope announced a storage-mode change: %q", notices.String())
			}
		})
	}
}

// TestWorkStoreReadRefusesTheEmptyAnswerAfterAStorageModeRewrite is priority
// (2), and it is the whole defect end to end: the real rewrite, then the real
// read, with only bd's subprocess faked — and faked to do exactly what it does
// on a healthy server, which is answer `[]` and exit 0.
//
// Red before the fix, for both reads:
//
//	List:  got 0 beads, err = <nil>
//	Ready: got 0 beads, err = <nil>
//
// That nil is the fail-open. `gc ready`'s city leg returns it, the federation
// merges it with the rig legs, every leg reports healthy, and the caller is
// told the city has no work while its entire ledger sits unread one directory
// over. A dead rig fails loudly and an unreadable binding fails loudly; this
// did not, which made the loud legs worth less than they looked.
func TestWorkStoreReadRefusesTheEmptyAnswerAfterAStorageModeRewrite(t *testing.T) {
	scope := embeddedScopeWithBeads(t, "jc")
	captureStorageModeChanges(t)
	if err := ensureCanonicalScopeMetadataForInit(fsys.OSFS{}, scope, "jc"); err != nil {
		t.Fatalf("ensureCanonicalScopeMetadataForInit: %v", err)
	}

	store := beads.NewBdStore(scope, emptyBdRunner)
	for name, read := range map[string]func() ([]beads.Bead, error){
		"List":  func() ([]beads.Bead, error) { return store.List(beads.ListQuery{AllowScan: true}) },
		"Ready": func() ([]beads.Bead, error) { return store.Ready() },
	} {
		t.Run(name, func(t *testing.T) {
			got, err := read()
			if err == nil {
				t.Fatalf("%s returned %d beads and err = <nil> from a store whose metadata was just re-pointed away from the database holding its rows; that empty answer is the fail-open on the work leg", name, len(got))
			}
			if !errors.Is(err, beads.ErrOrphanedBeadStore) {
				t.Fatalf("%s error = %v, want beads.ErrOrphanedBeadStore", name, err)
			}
			orphan := filepath.Join(scope, ".beads", "embeddeddolt", "jc")
			for _, want := range []string{scope, orphan, "gc doctor"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("%s refusal does not name %q: %v", name, want, err)
				}
			}
		})
	}
}

// TestWorkStoreReadIsUnchangedWhenNothingIsOrphaned is the mutation proof, and
// it carries the compatibility claim this fix owes every city in the field.
//
// A scope gc created has no .beads/embeddeddolt at all, so an empty ledger is
// an empty ledger and answers as one. A scope that answers with ROWS is not
// checked even when a stale directory is present: the store bd opened is
// demonstrably the populated one, and refusing there would brick a city that
// migrated deliberately and left the old directory behind.
func TestWorkStoreReadIsUnchangedWhenNothingIsOrphaned(t *testing.T) {
	t.Run("no stale database", func(t *testing.T) {
		scope := t.TempDir()
		writeScopeMetadata(t, scope, map[string]string{
			"database": "dolt", "backend": "dolt", "dolt_mode": "server", "dolt_database": "jc",
		})
		store := beads.NewBdStore(scope, emptyBdRunner)
		got, err := store.List(beads.ListQuery{AllowScan: true})
		if err != nil || len(got) != 0 {
			t.Fatalf("List = (%d beads, %v), want (0, nil) on a scope with one database", len(got), err)
		}
		if got, err := store.Ready(); err != nil || len(got) != 0 {
			t.Fatalf("Ready = (%d beads, %v), want (0, nil)", len(got), err)
		}
	})

	t.Run("stale database but the read answers", func(t *testing.T) {
		scope := embeddedScopeWithBeads(t, "jc")
		captureStorageModeChanges(t)
		if err := ensureCanonicalScopeMetadataForInit(fsys.OSFS{}, scope, "jc"); err != nil {
			t.Fatalf("ensureCanonicalScopeMetadataForInit: %v", err)
		}
		answering := func(_, _ string, _ ...string) ([]byte, error) {
			return []byte(`[{"id":"jc-1","title":"real row","status":"open"}]`), nil
		}
		store := beads.NewBdStore(scope, answering)
		got, err := store.List(beads.ListQuery{AllowScan: true})
		if err != nil || len(got) != 1 {
			t.Fatalf("List = (%d beads, %v), want (1, nil): a store that answers is the one being read", len(got), err)
		}
	})

	t.Run("an empty directory is not a database", func(t *testing.T) {
		scope := t.TempDir()
		if err := os.MkdirAll(filepath.Join(scope, ".beads", "embeddeddolt", "jc"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeScopeMetadata(t, scope, map[string]string{
			"database": "dolt", "backend": "dolt", "dolt_mode": "server", "dolt_database": "jc",
		})
		store := beads.NewBdStore(scope, emptyBdRunner)
		if got, err := store.List(beads.ListQuery{AllowScan: true}); err != nil || len(got) != 0 {
			t.Fatalf("List = (%d beads, %v), want (0, nil): a directory with no .dolt repository holds no beads", len(got), err)
		}
	})
}

// TestOrphanedStoreGuardIsNotSplitStoreSpecific states the scope of the fix
// out loud, because the program it lands in is a split-store program and the
// reflex is to assume this rides along with it.
//
// The fixture carries no [storage] section and no relocated coordination class.
// Everything about the defect — the metadata rewrite, the re-pointed workspace,
// the `[]` with exit 0 — happens on a city with exactly one store, and the
// guard fires there. What changes for a single-store city is precisely this: an
// empty answer from a scope holding an unread bead database becomes an error
// instead of an empty slice. A single-store city with one database is untouched
// (TestWorkStoreReadIsUnchangedWhenNothingIsOrphaned).
func TestOrphanedStoreGuardIsNotSplitStoreSpecific(t *testing.T) {
	scope := embeddedScopeWithBeads(t, "hq")
	if orphan, active, ok := beads.OrphanedBeadDatabase(scope); ok {
		t.Fatalf("an embedded scope pointing at its own database reported %q orphaned (active %q); nothing has been rewritten yet", orphan, active)
	}

	notices := captureStorageModeChanges(t)
	if err := ensureCanonicalScopeMetadataForInit(fsys.OSFS{}, scope, "hq"); err != nil {
		t.Fatalf("ensureCanonicalScopeMetadataForInit: %v", err)
	}
	if notices.Len() == 0 {
		t.Fatal("the rewrite was silent on a single-store city")
	}
	orphan, active, ok := beads.OrphanedBeadDatabase(scope)
	if !ok {
		t.Fatal("no orphaned database reported after the rewrite on a single-store city")
	}
	if active != "dolt" {
		t.Errorf("active store = %q, want %q", active, "dolt")
	}
	if want := filepath.Join(scope, ".beads", "embeddeddolt", "hq"); orphan != want {
		t.Errorf("orphaned database = %q, want %q", orphan, want)
	}
}
