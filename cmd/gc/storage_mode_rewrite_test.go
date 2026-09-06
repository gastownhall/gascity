package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
)

// This file verifies that canonicalization preserves existing embedded Dolt
// scopes. New installs may select proxied-server mode, but lifecycle commands
// must not silently repoint an initialized workspace at another database.

// embeddedScopeWithBeads builds a scope whose .beads/ is an embedded-mode bd
// workspace with a Dolt repository under it — what `bd init -p <prefix>` leaves
// behind, and what the live proof's city had before gc touched it.
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

// emptyBdRunner answers every bd invocation with `[]` and exit 0.
func emptyBdRunner(_, _ string, _ ...string) ([]byte, error) { return []byte("[]"), nil }

// TestCanonicalizingAScopeAnnouncesAStorageModeChange ensures an existing
// embedded scope is left untouched and emits no misleading migration notice.
func TestCanonicalizingAScopeAnnouncesAStorageModeChange(t *testing.T) {
	scope := embeddedScopeWithBeads(t, "jc")
	notices := captureStorageModeChanges(t)

	if err := ensureCanonicalScopeMetadataForInit(fsys.OSFS{}, scope, "jc"); err != nil {
		t.Fatalf("ensureCanonicalScopeMetadataForInit: %v", err)
	}

	if mode := readScopeDoltMode(t, scope); mode != "embedded" {
		t.Fatalf("dolt_mode = %q after canonicalization, want embedded", mode)
	}
	if notices.Len() != 0 {
		t.Fatalf("preserved embedded scope emitted a storage-mode notice: %q", notices.String())
	}
}

// TestTheStorageModeAnnouncementNamesARecoveryThatSURVIVESTheNextBoot is the
// operator-guidance half of ga-qi9km, and it pins the two ways this message can
// be worse than useless.
//
// The first is advice gc itself undoes. ensureCanonicalScopeMetadata forces
// dolt_mode=server unconditionally, and `gc start`, `gc rig add`, `gc supervisor
// run` and the controller's rig-create handler all run it — so "point
// metadata.json back at the embedded database" works until the next boot and
// then silently stops, leaving the operator in a loop and, in between, on a
// mode internal/beads/contract's preflight checker FAILS the native store on.
// The message must not offer it, and must say the edit does not hold.
//
// The second is overstating what is on disk. `bd init` creates the embedded
// repository before a single bead exists, so "holds a Dolt bead database" is
// all that is knowable — a claim that it holds ROWS is one gc cannot make
// without opening it, and a message that overstates gets ignored the next time
// it is right.
//
// The remediation named is `gc doctor`'s own (splitStoreFixHint), word for
// word on the load-bearing clause, so the two do not send an operator in
// different directions about the same two directories.
func TestTheStorageModeAnnouncementNamesARecoveryThatSURVIVESTheNextBoot(t *testing.T) {
	scope := embeddedScopeWithBeads(t, "jc")
	notices := captureStorageModeChanges(t)
	if err := ensureCanonicalScopeMetadataForInit(fsys.OSFS{}, scope, "jc"); err != nil {
		t.Fatalf("ensureCanonicalScopeMetadataForInit: %v", err)
	}
	notice := notices.String()

	if notice != "" {
		t.Fatalf("preserved embedded scope emitted an announcement: %q", notice)
	}
	// Not a restatement: the check the announcement names is run against the
	// scope the announcement was printed for, so a drift in either text or a
	// regression that makes the check silent on this shape fails here.
	result := doctor.NewBDSplitStoreCheck(scope).Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("gc doctor's bd-split-store check reports %v (%q) for an unchanged embedded scope", result.Status, result.Message)
	}
	if result.FixHint != "" {
		t.Errorf("gc doctor emitted split-store guidance for unchanged scope: %q", result.FixHint)
	}
	for _, forbidden := range []string{
		// Naming this edit as a recovery sends the operator round a loop.
		`"dolt_mode": "embedded"`,
		"point .beads/metadata.json back",
		// Nothing on disk supports these.
		"lost", "deleted", "corrupt",
	} {
		if strings.Contains(strings.ToLower(notice), strings.ToLower(forbidden)) {
			t.Errorf("the announcement claims %q, which gc either cannot know or immediately reverts: %q", forbidden, notice)
		}
	}
}

// TestEveryDoorThatFlipsTheStorageModeAnnouncesIt closes the gap a
// per-command warning always has.
//
// `gc rig set-endpoint` and `gc beads city use-managed`/`use-external` reach
// their own canonicalizers (requireCanonicalizedScopeMetadata for the scope the
// command names, canonicalizeScopeMetadataIfPresent for the inherited rigs a
// city endpoint change sweeps along, both in cmd_rig_endpoint.go) rather than
// the init one, and they perform the identical embedded→server rewrite. A
// warning that depends on which command the operator happened to run — or on
// which of the two endpoint doors the scope arrived through — is a warning
// nobody can rely on.
func TestEveryDoorThatFlipsTheStorageModeAnnouncesIt(t *testing.T) {
	for name, canonicalize := range map[string]func(scope string) error{
		"init path": func(scope string) error {
			return ensureCanonicalScopeMetadataForInit(fsys.OSFS{}, scope, "jc")
		},
		"endpoint path, named scope": func(scope string) error {
			return requireCanonicalizedScopeMetadata(fsys.OSFS{}, scope)
		},
		"endpoint path, inherited rig": func(scope string) error {
			return canonicalizeScopeMetadataIfPresent(fsys.OSFS{}, scope)
		},
	} {
		t.Run(name, func(t *testing.T) {
			scope := embeddedScopeWithBeads(t, "jc")
			notices := captureStorageModeChanges(t)
			if err := canonicalize(scope); err != nil {
				t.Fatalf("canonicalize: %v", err)
			}
			if mode := readScopeDoltMode(t, scope); mode != "embedded" {
				t.Fatalf("dolt_mode = %q, want embedded", mode)
			}
			if notices.Len() != 0 {
				t.Fatalf("preserved embedded scope emitted notice: %q", notices.String())
			}
		})
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

// TestTheStorageModeRewriteNeverChangesWhatAReadAnswers is the preservation
// proof for an existing embedded scope. Canonicalization must not silently
// migrate it to proxied/direct server mode, and reads remain unchanged.
//
// A scope whose metadata already names embedded storage continues answering
// `[]` with nil on every read shape. Existing installs are never auto-converted.
func TestTheStorageModeRewriteNeverChangesWhatAReadAnswers(t *testing.T) {
	scope := embeddedScopeWithBeads(t, "jc")
	captureStorageModeChanges(t)
	if err := ensureCanonicalScopeMetadataForInit(fsys.OSFS{}, scope, "jc"); err != nil {
		t.Fatalf("ensureCanonicalScopeMetadataForInit: %v", err)
	}
	if mode := readScopeDoltMode(t, scope); mode != "embedded" {
		t.Fatalf("dolt_mode = %q after canonicalization, want embedded", mode)
	}

	var notices bytes.Buffer
	store := beads.NewBdStore(scope, emptyBdRunner, beads.WithBdStoreNoticeSink(&notices))
	for _, name := range []string{"List", "Ready", "Children"} {
		reads := map[string]func() ([]beads.Bead, error){
			"List":     func() ([]beads.Bead, error) { return store.List(beads.ListQuery{AllowScan: true}) },
			"Ready":    func() ([]beads.Bead, error) { return store.Ready() },
			"Children": func() ([]beads.Bead, error) { return store.Children("jc-1") },
		}
		t.Run(name, func(t *testing.T) {
			got, err := reads[name]()
			if err != nil || len(got) != 0 {
				t.Fatalf("%s = (%d beads, %v), want (0, nil)", name, len(got), err)
			}
		})
	}
	// No mode changed, so no storage-mode or read-time notice is emitted.
	if notices.Len() != 0 {
		t.Fatalf("unexpected read-time notice for preserved embedded scope: %q", notices.String())
	}
}

// TestAnEmptyReadIsNotEvidenceTheScopeIsReadingTheWrongDatabase is the first of
// the two cases that keep a read-time REFUSAL out of this change, and it is the
// one a populated city hits every minute.
//
// A refusal keyed on the presence of a second Dolt directory fires on the
// RESULT of one call, not on the store: `Ready()` returning zero rows is the
// steady state of an idle city and of every assignee-scoped probe, and the
// filtered reads below are answered by a store bd just handed rows for. A city
// that migrated deliberately and kept the old directory — which is the state
// `gc doctor`'s own fix hint tells operators to sit in — would have every one
// of these turn into an error, and `federateBeadLegs` aborts the whole
// federation on any leg error, so `gc ready` exits non-zero for the city and
// every worker's generated work query fails with it.
//
// It is also the acceptance case for the notice ga-clsfl ships: this store is
// demonstrably populated, so it must be answered AND left alone — no error, and
// nothing printed. The notice decides per STORE (has this store ever handed
// back a row?), which is why the first List below immunizes every read after
// it.
//
// Red before ga-clsfl's predecessor, on a scope with metadata pointing at the
// server store and a retained .beads/embeddeddolt/jc:
//
//	List  = (1 beads, <nil>)                    ← the active store is populated
//	Ready = (0 beads, bead store read returned empty while an unread bead database sits beside it…)
func TestAnEmptyReadIsNotEvidenceTheScopeIsReadingTheWrongDatabase(t *testing.T) {
	scope := embeddedScopeWithBeads(t, "jc")
	captureStorageModeChanges(t)
	if err := ensureCanonicalScopeMetadataForInit(fsys.OSFS{}, scope, "jc"); err != nil {
		t.Fatalf("ensureCanonicalScopeMetadataForInit: %v", err)
	}
	answering := func(_, _ string, args ...string) ([]byte, error) {
		if len(args) > 0 && args[0] == "ready" {
			return []byte(`[]`), nil
		}
		return []byte(`[{"id":"jc-1","title":"real row","status":"open","assignee":"alice"}]`), nil
	}
	var notices bytes.Buffer
	store := beads.NewBdStore(scope, answering, beads.WithBdStoreNoticeSink(&notices))

	got, err := store.List(beads.ListQuery{AllowScan: true})
	if err != nil || len(got) != 1 {
		t.Fatalf("List = (%d beads, %v), want (1, nil): this store is demonstrably the populated one", len(got), err)
	}
	t.Cleanup(func() {
		if notices.Len() != 0 {
			t.Errorf("a demonstrably populated store printed the unread-store notice: %q", notices.String())
		}
	})
	for name, read := range map[string]func() ([]beads.Bead, error){
		// The frontier is empty because nothing is claimable right now.
		"empty frontier on a populated store": func() ([]beads.Bead, error) { return store.Ready() },
		// bd answered with a row; the in-process assignee filter dropped it.
		"per-assignee frontier": func() ([]beads.Bead, error) {
			return store.Ready(beads.ReadyQuery{Assignee: "demo/worker"})
		},
		// bd answered with a row; the wisp-tier filter dropped it.
		"wisp tier over issue rows": func() ([]beads.Bead, error) {
			return store.Ready(beads.ReadyQuery{TierMode: beads.TierWisps})
		},
		// A leaf really has no children, and 26 non-test call sites walk them.
		"children of a leaf": func() ([]beads.Bead, error) { return store.Children("jc-9") },
		// An empty inbox is the normal state of a mail poll.
		"mail poll with no mail": func() ([]beads.Bead, error) {
			return store.List(beads.ListQuery{Type: "message", Status: "open", Assignee: "demo/worker"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := read()
			if err != nil {
				t.Fatalf("read returned %d beads and err = %v; an empty answer from a demonstrably populated store is a real answer, and refusing it fails `gc ready` for the whole city", len(got), err)
			}
		})
	}
}

// TestAdoptingAFreshlyInitializedWorkspaceStillReads verifies that an existing
// embedded workspace remains readable during adoption without migration noise.
func TestAdoptingAFreshlyInitializedWorkspaceStillReads(t *testing.T) {
	scope := embeddedScopeWithBeads(t, "jkq") // `bd init -p jkq`: empty repo, embedded mode
	notices := captureStorageModeChanges(t)
	if err := ensureCanonicalScopeMetadataForInit(fsys.OSFS{}, scope, "jkq"); err != nil {
		t.Fatalf("ensureCanonicalScopeMetadataForInit: %v", err)
	}
	if notices.Len() != 0 {
		t.Fatalf("adopting an embedded workspace emitted a storage-mode notice: %q", notices.String())
	}

	var readNotices bytes.Buffer
	store := beads.NewBdStore(scope, emptyBdRunner, beads.WithBdStoreNoticeSink(&readNotices))
	// The readiness gate `gc rig add` and `gc start` block adoption on.
	slept := 0
	if err := verifyCanonicalBdScopeStoreReady(store, func(time.Duration) { slept++ }); err != nil {
		t.Fatalf("verifyCanonicalBdScopeStoreReady = %v, want nil: a rig with no beads yet is not a broken rig", err)
	}
	if slept != 0 {
		t.Fatalf("adoption slept %d time(s) before succeeding; the gate must pass on the first attempt", slept)
	}
	if got, err := store.Ready(); err != nil || len(got) != 0 {
		t.Fatalf("Ready = (%d beads, %v), want (0, nil)", len(got), err)
	}
	if readNotices.Len() != 0 {
		t.Fatalf("unexpected read-time notice for preserved embedded scope: %q", readNotices.String())
	}
}

// TestTheStorageModeAnnouncementIsNotSplitStoreSpecific verifies that a
// single-store embedded city is also preserved without migration output.
func TestTheStorageModeAnnouncementIsNotSplitStoreSpecific(t *testing.T) {
	scope := embeddedScopeWithBeads(t, "hq")
	if _, present := beads.BeadDatabaseDirForDoltMode(scope, "server", "hq"); present {
		t.Fatal("the fixture has a server database; nothing has been rewritten yet")
	}

	notices := captureStorageModeChanges(t)
	if err := ensureCanonicalScopeMetadataForInit(fsys.OSFS{}, scope, "hq"); err != nil {
		t.Fatalf("ensureCanonicalScopeMetadataForInit: %v", err)
	}
	if notices.Len() != 0 {
		t.Fatalf("unexpected rewrite notice on a single-store city: %q", notices.String())
	}
	_, present := beads.BeadDatabaseDirForDoltMode(scope, "embedded", "hq")
	if !present {
		t.Fatal("the embedded database gc stopped reading is no longer resolvable")
	}
}

// TestTheThreeMessagesAboutOneUnreadDatabaseAgree verifies that unchanged
// embedded storage produces no contradictory split-store guidance.
func TestTheThreeMessagesAboutOneUnreadDatabaseAgree(t *testing.T) {
	scope := embeddedScopeWithBeads(t, "jc")
	announcement := captureStorageModeChanges(t)
	if err := ensureCanonicalScopeMetadataForInit(fsys.OSFS{}, scope, "jc"); err != nil {
		t.Fatalf("ensureCanonicalScopeMetadataForInit: %v", err)
	}
	var readNotice bytes.Buffer
	store := beads.NewBdStore(scope, emptyBdRunner, beads.WithBdStoreNoticeSink(&readNotice))
	if _, err := store.Ready(); err != nil {
		t.Fatalf("Ready() error = %v, want nil", err)
	}
	diagnostic := doctor.NewBDSplitStoreCheck(scope).Run(&doctor.CheckContext{})
	if diagnostic.Status != doctor.StatusOK {
		t.Fatalf("gc doctor reports %v (%q) for an unchanged embedded scope", diagnostic.Status, diagnostic.Message)
	}

	messages := map[string]string{
		"flip-time announcement": announcement.String(),
		"read-time notice":       readNotice.String(),
		"gc doctor fix hint":     diagnostic.FixHint,
	}
	for name, msg := range messages {
		if msg != "" && name != "gc doctor fix hint" {
			t.Errorf("%s unexpectedly emitted split-store guidance: %q", name, msg)
		}
	}
	// Doctor prescribes the state the notice fires in, so it has to name the
	// way to live in that state quietly.
	if diagnostic.FixHint != "" || readNotice.Len() != 0 {
		t.Fatalf("unchanged embedded scope emitted guidance: doctor=%q read=%q", diagnostic.FixHint, readNotice.String())
	}
	// And doctor has to promise the bound the guard actually holds. The guard
	// memoizes per SCOPE PATH inside one process, and cmd/gc builds a throwaway
	// bd store per request on the paths internal/api reads through — so "a
	// one-time notice" was false there, at status-rebuild rate, in a tree with
	// a documented log-flood history. Telling an operator to expect less noise
	// than they will get is the same class of false statement as telling them
	// rows are gone.
}
