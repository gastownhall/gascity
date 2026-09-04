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

// This file covers ga-qi9km: `gc rig add` and `gc supervisor run` silently
// rewrite a city's .beads/metadata.json from embedded to server, re-pointing
// the scope at a database that does not hold its beads, after which every
// work-store read answers `[]` with exit 0.
//
// What landed first is the half that is DECIDABLE: the rewrite becomes visible,
// with the path it stops reading and the durable remediation attached.
//
// ga-clsfl then closed the read-time half — as a NOTICE, never a refusal. The
// two experiment tests near the end of this file are why: they are the cases a
// refusal keyed on "a `.dolt` directory exists under the other mode's
// subdirectory" gets wrong, and that fact is the only evidence available
// without opening the second database. They now double as the acceptance
// tests for the notice, since a notice must leave every one of their answers
// exactly as it found them.
//
// Both fixtures are real directories with real files. The defect survived a
// suite that passes precisely because it lives in the disagreement between a
// JSON file and a directory listing, and a double for either one asserts the
// bug away.

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
	// These tests exercise the legacy embedded→direct-server reconciliation
	// warning. Pin an explicit external endpoint so the new fresh-scope
	// proxy opt-in is not the variable under test.
	t.Setenv("GC_DOLT_HOST", "db.example.test")
	t.Setenv("GC_DOLT_PORT", "3306")
	scope := t.TempDir()
	if err := os.MkdirAll(filepath.Join(scope, ".beads", "embeddeddolt", database, ".dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeScopeMetadata(t, scope, map[string]string{
		"database": "dolt",
		// Legacy metadata without an authoritative backend marker models the
		// pre-canonicalization workspace these tests exercise. Explicit modern
		// embedded metadata is preserved by the new default migration.
		"backend":       "legacy",
		"dolt_mode":     "embedded",
		"dolt_database": database,
	})
	// The cases below exercise the legacy embedded→direct-server rewrite.
	// Make that endpoint authority explicit in the scope itself: ambient
	// GC_DOLT_HOST/PORT values are intentionally not canonical state and must
	// not turn a fresh managed scope into direct-server mode.
	if err := os.WriteFile(filepath.Join(scope, ".beads", "config.yaml"), []byte(
		"issue_prefix: "+database+"\n"+
			"gc.endpoint_origin: managed_city\n"+
			"gc.endpoint_status: verified\n"+
			"dolt.mode: server\n"), 0o644); err != nil {
		t.Fatal(err)
	}
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
	left := filepath.Join(scope, ".beads", "embeddeddolt", "jc")
	for _, want := range []string{scope, "embedded", "server", left, "STOP reading"} {
		if !strings.Contains(notices.String(), want) {
			t.Errorf("the storage-mode change never names %q; notices=%q", want, notices.String())
		}
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

	for _, want := range []string{
		"gc doctor",
		"bd import --dry-run",
		// gc doctor's splitStoreFixHint prescribes exactly this state; the
		// announcement must not tell the operator to undo it.
		"keep both directories until reconciled",
		// The edit an operator reaches for first is the one gc reverts.
		"re-canonicalizes",
	} {
		if !strings.Contains(notice, want) {
			t.Errorf("the announcement does not name %q: %q", want, notice)
		}
	}
	// Not a restatement: the check the announcement names is run against the
	// scope the announcement was printed for, so a drift in either text or a
	// regression that makes the check silent on this shape fails here.
	result := doctor.NewBDSplitStoreCheck(scope).Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusWarning {
		t.Fatalf("gc doctor's bd-split-store check reports %v (%q) for the scope the announcement steers it at; a diagnostic that answers OK on the state an operator was just warned about is a false all-clear",
			result.Status, result.Message)
	}
	if !strings.Contains(result.FixHint, "keep both directories until reconciled") {
		t.Errorf("gc doctor's fix hint %q no longer carries the clause the announcement mirrors; the two have drifted", result.FixHint)
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
			if mode := readScopeDoltMode(t, scope); mode != "server" {
				t.Fatalf("dolt_mode = %q, want %q", mode, "server")
			}
			left := filepath.Join(scope, ".beads", "embeddeddolt", "jc")
			if !strings.Contains(notices.String(), left) {
				t.Fatalf("the rewrite did not name %q; notices=%q", left, notices.String())
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

// TestTheStorageModeRewriteNeverChangesWhatAReadAnswers is the mutation proof
// for both halves: the flip-time announcement and the read-time notice are
// WARNINGS, and a warning that changes the answer is not a warning.
//
// A scope whose metadata was just re-pointed away from the database holding its
// rows still answers `[]` with nil, exactly as it did before either change, on
// every read shape. Closing the fail-open by REFUSING needs evidence gc does
// not have (see the two experiment tests below), so what changed is that the
// flip says so when it happens and the empty read says so when it is paid for.
func TestTheStorageModeRewriteNeverChangesWhatAReadAnswers(t *testing.T) {
	scope := embeddedScopeWithBeads(t, "jc")
	captureStorageModeChanges(t)
	if err := ensureCanonicalScopeMetadataForInit(fsys.OSFS{}, scope, "jc"); err != nil {
		t.Fatalf("ensureCanonicalScopeMetadataForInit: %v", err)
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
	// And the empty answer is no longer confident: the whole-ledger reads carry
	// the notice naming the database this scope stopped reading.
	left := filepath.Join(scope, ".beads", "embeddeddolt", "jc")
	if !strings.Contains(notices.String(), left) {
		t.Fatalf("no read-time notice naming %q; notices=%q", left, notices.String())
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

// TestAdoptingAFreshlyInitializedWorkspaceStillReads is the second case, and it
// is why narrowing the refusal to "the active store is provably empty" does not
// rescue it either.
//
// `bd init` defaults to embedded mode and creates .beads/embeddeddolt/<db>/.dolt
// before a single bead exists (bd's own cmd/bd/init_embedded_test.go asserts
// that file). Adopting such a workspace with `gc rig add` canonicalizes it to
// server mode, and from that moment BOTH databases are empty — which is the
// same on-disk shape as a populated workspace that was re-pointed at an empty
// server store. Presence of a `.dolt` directory cannot tell them apart; only
// opening the second database can, and that is a store open, a lock and a
// possible schema migration on a directory the operator was told to preserve.
//
// So the fresh rig reads as a fresh rig: zero beads, nil error. Refusing here
// would break the adoption path this change's own announcement exists to keep
// usable, and would fail `gc rig add` itself —
// verifyCanonicalBdScopeStoreReady requires List(AllowScan, Limit 1) to return
// a nil error, retried 20 times at 500ms, so a guard that fired here would
// break adoption after a ten-second stall. That whole gate runs below, and it
// must complete without sleeping once.
//
// What the read-time notice does here is SAY so, once, and let the read
// through: a fresh adoption and a re-pointed workspace are the same shape on
// disk, and describing the shape is the most that is knowable.
//
// Red before ga-clsfl's predecessor:
//
//	List(AllowScan, Limit 1) = (0 beads, bead store read returned empty while an unread bead database sits beside it…)
//	Ready()                  = (0 beads, bead store read returned empty while an unread bead database sits beside it…)
func TestAdoptingAFreshlyInitializedWorkspaceStillReads(t *testing.T) {
	scope := embeddedScopeWithBeads(t, "jkq") // `bd init -p jkq`: empty repo, embedded mode
	notices := captureStorageModeChanges(t)
	if err := ensureCanonicalScopeMetadataForInit(fsys.OSFS{}, scope, "jkq"); err != nil {
		t.Fatalf("ensureCanonicalScopeMetadataForInit: %v", err)
	}
	if notices.Len() == 0 {
		t.Fatal("adopting a bd-initialized workspace changed its storage mode silently")
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
	// Said once, for the whole store, however many reads it answers.
	if n := strings.Count(readNotices.String(), "does not point at"); n != 1 {
		t.Fatalf("the read-time notice printed %d times across the adoption gate and a Ready, want exactly 1:\n%s", n, readNotices.String())
	}
}

// TestTheStorageModeAnnouncementIsNotSplitStoreSpecific states the scope of the
// fix out loud, because the program it lands in is a split-store program and
// the reflex is to assume this rides along with it.
//
// The fixture carries no [storage] section and no relocated coordination class.
// Everything about the defect — the metadata rewrite, the re-pointed workspace,
// the `[]` with exit 0 — happens on a city with exactly one store, and the
// announcement fires there.
func TestTheStorageModeAnnouncementIsNotSplitStoreSpecific(t *testing.T) {
	scope := embeddedScopeWithBeads(t, "hq")
	if _, present := beads.BeadDatabaseDirForDoltMode(scope, "server", "hq"); present {
		t.Fatal("the fixture has a server database; nothing has been rewritten yet")
	}

	notices := captureStorageModeChanges(t)
	if err := ensureCanonicalScopeMetadataForInit(fsys.OSFS{}, scope, "hq"); err != nil {
		t.Fatalf("ensureCanonicalScopeMetadataForInit: %v", err)
	}
	if notices.Len() == 0 {
		t.Fatal("the rewrite was silent on a single-store city")
	}
	left, present := beads.BeadDatabaseDirForDoltMode(scope, "embedded", "hq")
	if !present {
		t.Fatal("the embedded database gc stopped reading is no longer resolvable")
	}
	if !strings.Contains(notices.String(), left) {
		t.Errorf("the announcement does not name %q; notices=%q", left, notices.String())
	}
}

// TestTheThreeMessagesAboutOneUnreadDatabaseAgree is the reconciliation pin for
// ga-clsfl's fourth acceptance criterion.
//
// Three things now talk about the same two directories, at three different
// moments: the announcement when gc flips the mode, `gc doctor`'s
// bd-split-store check when an operator goes looking, and the read-time notice
// when an empty answer is actually paid for. They were written months apart and
// nothing but this test stops them drifting into three different stories about
// one scope — which is the failure mode that produced the bug: `gc doctor`
// telling an operator to "keep both directories until reconciled" while a guard
// turned that exact state into a hard error.
//
// So all three run against ONE scope here, and the claims that have to line up
// are asserted across them: the same remediation clause, the same directory,
// the same evidential ceiling (nothing claims rows were lost), and doctor
// naming the override the notice tells the operator to set — because doctor's
// advice is what parks them in the shape the notice describes.
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
	if diagnostic.Status != doctor.StatusWarning {
		t.Fatalf("gc doctor reports %v (%q) for the scope the other two messages are about; a diagnostic that answers OK on the state an operator was just warned about is a false all-clear",
			diagnostic.Status, diagnostic.Message)
	}

	messages := map[string]string{
		"flip-time announcement": announcement.String(),
		"read-time notice":       readNotice.String(),
		"gc doctor fix hint":     diagnostic.FixHint,
	}
	left := filepath.Join(scope, ".beads", "embeddeddolt", "jc")
	for name, msg := range messages {
		if !strings.Contains(msg, "keep both directories until reconciled") {
			t.Errorf("%s no longer carries the shared remediation clause: %q", name, msg)
		}
		if !strings.Contains(msg, "bd import --dry-run") {
			t.Errorf("%s no longer names the review step the other two do: %q", name, msg)
		}
		for _, forbidden := range []string{"lost", "deleted", "corrupt"} {
			if strings.Contains(strings.ToLower(msg), forbidden) {
				t.Errorf("%s claims %q, which no message here can know without opening the second database: %q", name, forbidden, msg)
			}
		}
	}
	for _, name := range []string{"flip-time announcement", "read-time notice"} {
		if !strings.Contains(messages[name], left) {
			t.Errorf("%s does not name %q, so the two point at different directories: %q", name, left, messages[name])
		}
		if !strings.Contains(messages[name], "gc doctor") {
			t.Errorf("%s does not steer at the diagnostic that enumerates both stores: %q", name, messages[name])
		}
	}
	// Doctor prescribes the state the notice fires in, so it has to name the
	// way to live in that state quietly.
	if !strings.Contains(diagnostic.FixHint, beads.AllowUnreadStoreReadEnvVar) {
		t.Errorf("gc doctor tells an operator to keep both directories but never names %s, the override that makes the read-time notice bearable while they do: %q",
			beads.AllowUnreadStoreReadEnvVar, diagnostic.FixHint)
	}
	if !strings.Contains(readNotice.String(), beads.AllowUnreadStoreReadEnvVar) {
		t.Errorf("the read-time notice does not name its own override: %q", readNotice.String())
	}
	// And doctor has to promise the bound the guard actually holds. The guard
	// memoizes per SCOPE PATH inside one process, and cmd/gc builds a throwaway
	// bd store per request on the paths internal/api reads through — so "a
	// one-time notice" was false there, at status-rebuild rate, in a tree with
	// a documented log-flood history. Telling an operator to expect less noise
	// than they will get is the same class of false statement as telling them
	// rows are gone.
	if strings.Contains(diagnostic.FixHint, "one-time notice") {
		t.Errorf("gc doctor promises a one-time notice; the guard bounds it per scope per process, and the API rebuilds its store per request: %q", diagnostic.FixHint)
	}
	if !strings.Contains(diagnostic.FixHint, "one notice per gc process") {
		t.Errorf("gc doctor does not state the bound the guard holds (one notice per gc process): %q", diagnostic.FixHint)
	}
}
