package beads_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/fsys"
)

type statRaceFS struct {
	fsys.FS
	path            string
	beforeFirstStat func()
	fired           bool
}

func (f *statRaceFS) Stat(name string) (os.FileInfo, error) {
	if name == f.path && !f.fired {
		f.fired = true
		if f.beforeFirstStat != nil {
			f.beforeFirstStat()
		}
	}
	return f.FS.Stat(name)
}

type toggledErrorFS struct {
	fsys.FS
	path    string
	statErr error
	readErr error
}

func (f *toggledErrorFS) Stat(name string) (os.FileInfo, error) {
	if name == f.path && f.statErr != nil {
		return nil, f.statErr
	}
	return f.FS.Stat(name)
}

func (f *toggledErrorFS) ReadFile(name string) ([]byte, error) {
	if name == f.path && f.readErr != nil {
		return nil, f.readErr
	}
	return f.FS.ReadFile(name)
}

type oneShotStatErrorFS struct {
	fsys.FS
	path  string
	err   error
	fired bool
}

func (f *oneShotStatErrorFS) Stat(name string) (os.FileInfo, error) {
	if name == f.path && !f.fired {
		f.fired = true
		return nil, f.err
	}
	return f.FS.Stat(name)
}

type errLocker struct {
	lockErr   error
	unlockErr error
}

func (l errLocker) Lock() error   { return l.lockErr }
func (l errLocker) Unlock() error { return l.unlockErr }

func TestFileStore(t *testing.T) {
	factory := func() beads.Store {
		path := filepath.Join(t.TempDir(), "beads.json")
		s, err := beads.OpenFileStore(fsys.OSFS{}, path)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	beadstest.RunStoreTests(t, factory)
	beadstest.RunSequentialIDTests(t, factory)
	beadstest.RunCreationOrderTests(t, factory)
	beadstest.RunDepTests(t, factory)
	beadstest.RunMetadataTests(t, factory)
}

func TestFileStoreConditionalWriterConformance(t *testing.T) {
	open := func(st *testing.T) beads.Store {
		path := filepath.Join(st.TempDir(), "beads.json")
		s, err := beads.OpenFileStore(fsys.OSFS{}, path)
		if err != nil {
			st.Fatal(err)
		}
		return s
	}
	beadstest.RunConditionalWriterConformanceWithOptions(t, "FileStore", open,
		beadstest.ConditionalWriterOptions{
			SuppliesCurrent: true,
			OpenDisabled: func(st *testing.T) beads.Store {
				s := open(st)
				s.(*beads.FileStore).DisableConditionalWrites = true
				return s
			},
		},
	)
}

// TestFileStoreRevisionSurvivesReopen proves the ConditionalWriter revision
// round-trips through disk — it is json:"-" on Bead, so it only survives via the
// out-of-band Revisions map. reloadFromDisk runs before every write, so a
// dropped revision here would reset to 0 mid-session in cross-process mode. Two
// beads (one bumped, one left at revision 1) catch per-bead persistence bugs a
// single-bead test cannot: a reload that resets bead N>0 to 0, or a persist that
// drops untouched rev-1 beads.
func TestFileStoreRevisionSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beads.json")
	s1, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	bumped, err := s1.Create(beads.Bead{Title: "bumped"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := s1.SetMetadata(bumped.ID, "k", fmt.Sprintf("v%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	untouched, err := s1.Create(beads.Bead{Title: "untouched"})
	if err != nil {
		t.Fatal(err)
	}
	beforeBumped, err := s1.Get(bumped.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeUntouched, err := s1.Get(untouched.ID)
	if err != nil {
		t.Fatal(err)
	}
	if beforeUntouched.Revision != 1 {
		t.Fatalf("freshly created bead revision = %d, want 1", beforeUntouched.Revision)
	}
	if beforeBumped.Revision <= beforeUntouched.Revision {
		t.Fatalf("bumped revision %d did not advance past a fresh bead %d", beforeBumped.Revision, beforeUntouched.Revision)
	}

	// Reopen from disk in a fresh handle (a second process).
	s2, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	afterBumped, err := s2.Get(bumped.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterUntouched, err := s2.Get(untouched.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterBumped.Revision != beforeBumped.Revision {
		t.Fatalf("bumped revision did not survive reopen: %d -> %d", beforeBumped.Revision, afterBumped.Revision)
	}
	if afterUntouched.Revision != 1 {
		t.Fatalf("untouched (rev 1) bead did not survive reopen: got %d, want 1", afterUntouched.Revision)
	}

	w, ok := beads.ConditionalWriterFor(s2)
	if !ok {
		t.Fatal("reopened FileStore lost ConditionalWriter")
	}
	// A CAS at the surviving revision succeeds and moves it; a second CAS at the
	// same (now stale) revision must fail — proving the persisted revision is a
	// live OCC token across the reopen, not a reset-to-zero.
	title := "v-fresh"
	if err := w.UpdateIfMatch(bumped.ID, afterBumped.Revision, beads.UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("UpdateIfMatch at surviving revision: %v", err)
	}
	staleTitle := "v-stale"
	if err := w.UpdateIfMatch(bumped.ID, afterBumped.Revision, beads.UpdateOpts{Title: &staleTitle}); !beads.IsPreconditionFailed(err) {
		t.Fatalf("UpdateIfMatch at now-stale revision: got %v, want PreconditionFailed", err)
	}
}

// TestFileStoreConditionalWriteCrossHandle is the load-bearing test for
// FileStore's reason to exist: two handles on one file (two processes). It kills
// mutations that delete the reloadFromDisk or the save from the conditional
// verbs — invisible to any single-handle test because in-memory state already
// equals disk.
func TestFileStoreConditionalWriteCrossHandle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beads.json")
	s1, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}

	b, err := s1.Create(beads.Bead{Title: "x"})
	if err != nil {
		t.Fatal(err)
	}
	// s2 reads the bead, caching its revision in memory.
	cached, err := s2.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}

	// s1 mutates through to disk, advancing the revision past s2's cached view.
	if err := s1.SetMetadata(b.ID, "k", "v"); err != nil {
		t.Fatal(err)
	}
	s1Cur, err := s1.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if s1Cur.Revision <= cached.Revision {
		t.Fatalf("s1 write did not advance revision: %d -> %d", cached.Revision, s1Cur.Revision)
	}

	w2, ok := beads.ConditionalWriterFor(s2)
	if !ok {
		t.Fatal("s2 lost ConditionalWriter")
	}

	// s2 CASes at its STALE cached revision. It must reload under the flock, see
	// s1's newer revision, and reject — otherwise it clobbers s1's write. A
	// missing reloadFromDisk makes s2's stale revision match and succeed.
	clobber := "clobber"
	err = w2.UpdateIfMatch(b.ID, cached.Revision, beads.UpdateOpts{Title: &clobber})
	var pfe *beads.PreconditionFailedError
	if !errors.As(err, &pfe) {
		t.Fatalf("cross-handle stale CAS: got %v, want *PreconditionFailedError", err)
	}
	if pfe.Current != s1Cur.Revision {
		t.Fatalf("PreconditionFailedError.Current = %d, want %d (s1's committed revision)", pfe.Current, s1Cur.Revision)
	}

	// s2 CASes at the CURRENT revision — must succeed and persist to disk.
	winTitle := "s2-win"
	if err := w2.UpdateIfMatch(b.ID, s1Cur.Revision, beads.UpdateOpts{Title: &winTitle}); err != nil {
		t.Fatalf("cross-handle current CAS: %v", err)
	}

	// A third fresh handle reads straight from disk — this is the durability
	// assertion that a missing save cannot fake.
	s3, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	s3Got, err := s3.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if s3Got.Title != winTitle {
		t.Fatalf("durability: fresh handle title = %q, want %q (s2's CAS was not saved)", s3Got.Title, winTitle)
	}
	if s3Got.Revision <= s1Cur.Revision {
		t.Fatalf("s2's CAS did not advance the persisted revision: %d -> %d", s1Cur.Revision, s3Got.Revision)
	}
}

// TestFileStoreConditionalWriteLegacyFileNoRevisions pins the downgrade-safe
// continuity contract: an UNSEALED store file with beads (a pre-revisions
// legacy file, or a file an older binary rewrote — dropping the revisions map
// and the seal) re-seeds every revision at a deterministic floor far above
// any token a prior writer could have issued, so tokens are never reused. A
// CAS at the observed (re-seeded) revision works normally.
func TestFileStoreConditionalWriteLegacyFileNoRevisions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beads.json")
	legacy := `{"seq":1,"beads":[{"id":"gc-1","title":"legacy","status":"open","issue_type":"task","created_at":"2026-01-01T00:00:00Z"}]}`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Get("gc-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision < 1<<40 {
		t.Fatalf("unsealed-file revision = %d, want the continuity floor re-seed (>= 2^40): fresh-from-zero tokens could reuse previously issued ones", got.Revision)
	}
	w, ok := beads.ConditionalWriterFor(s)
	if !ok {
		t.Fatal("FileStore lost ConditionalWriter")
	}
	// Any previously issued small counter token must fail against the seed.
	title := "v2"
	if err := w.UpdateIfMatch("gc-1", 5, beads.UpdateOpts{Title: &title}); !beads.IsPreconditionFailed(err) {
		t.Fatalf("legacy stale CAS: got %v, want PreconditionFailed", err)
	}
	// A CAS at the observed re-seeded revision succeeds and bumps.
	if err := w.UpdateIfMatch("gc-1", got.Revision, beads.UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("legacy CAS at re-seeded revision: %v", err)
	}
	// The bump now persists to a fresh handle.
	s2, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	got2, err := s2.Get("gc-1")
	if err != nil {
		t.Fatal(err)
	}
	if got2.Revision == 0 {
		t.Fatalf("bumped revision did not persist for a migrated legacy bead: got %d", got2.Revision)
	}
}

func TestFileStorePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beads.json")

	// First process: create two beads.
	s1, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	b1, err := s1.Create(beads.Bead{Title: "first"})
	if err != nil {
		t.Fatal(err)
	}
	b2, err := s1.Create(beads.Bead{Title: "second"})
	if err != nil {
		t.Fatal(err)
	}

	// Second process: open a new FileStore on the same path.
	s2, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}

	// Verify Get works for both beads.
	got1, err := s2.Get(b1.ID)
	if err != nil {
		t.Fatalf("Get(%q) after reopen: %v", b1.ID, err)
	}
	if got1.Title != "first" {
		t.Errorf("Title = %q, want %q", got1.Title, "first")
	}

	got2, err := s2.Get(b2.ID)
	if err != nil {
		t.Fatalf("Get(%q) after reopen: %v", b2.ID, err)
	}
	if got2.Title != "second" {
		t.Errorf("Title = %q, want %q", got2.Title, "second")
	}

	// Verify next Create continues the sequence.
	b3, err := s2.Create(beads.Bead{Title: "third"})
	if err != nil {
		t.Fatal(err)
	}
	if b3.ID != "gc-3" {
		t.Errorf("third bead ID = %q, want %q", b3.ID, "gc-3")
	}
}

func TestFileStoreDepPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beads.json")

	// First process: create deps.
	s1, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.DepAdd("a", "b", "blocks"); err != nil {
		t.Fatal(err)
	}

	// Second process: reopen and verify deps survived.
	s2, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	deps, err := s2.DepList("a", "down")
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 {
		t.Fatalf("DepList after reopen = %d deps, want 1", len(deps))
	}
	if deps[0].DependsOnID != "b" {
		t.Errorf("dep.DependsOnID = %q, want %q", deps[0].DependsOnID, "b")
	}
}

func TestFileStoreMetadataPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beads.json")

	// First process: create bead with metadata.
	s1, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s1.Create(beads.Bead{Title: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.SetMetadata(b.ID, "convoy.owner", "mayor"); err != nil {
		t.Fatal(err)
	}

	// Second process: verify metadata survived.
	s2, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s2.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata["convoy.owner"] != "mayor" {
		t.Errorf("Metadata[convoy.owner] = %q, want %q", got.Metadata["convoy.owner"], "mayor")
	}
}

func TestFileStoreRefreshesReadsAcrossOpenInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beads.json")

	s1, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}

	created, err := s1.Create(beads.Bead{
		Title:  "manual session",
		Type:   "session",
		Labels: []string{"gc:session"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.SetMetadata(created.ID, "state", "creating"); err != nil {
		t.Fatal(err)
	}

	got, err := s2.Get(created.ID)
	if err != nil {
		t.Fatalf("Get(%q) from second handle: %v", created.ID, err)
	}
	if got.Metadata["state"] != "creating" {
		t.Fatalf("Get(%q) metadata[state] = %q, want %q", created.ID, got.Metadata["state"], "creating")
	}

	sessions, err := s2.List(beads.ListQuery{Label: "gc:session"})
	if err != nil {
		t.Fatalf("List(session label) from second handle: %v", err)
	}
	if len(sessions) != 1 || sessions[0].ID != created.ID {
		t.Fatalf("List(session label) = %+v, want only %s", sessions, created.ID)
	}
}

func TestFileStoreReadyRefreshesAcrossOpenInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beads.json")

	s1, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}

	blocker, err := s1.Create(beads.Bead{Title: "blocker"})
	if err != nil {
		t.Fatal(err)
	}
	target, err := s1.Create(beads.Bead{Title: "target"})
	if err != nil {
		t.Fatal(err)
	}

	ready, err := s2.Ready()
	if err != nil {
		t.Fatalf("Ready() before dep add: %v", err)
	}
	if !hasBeadID(ready, blocker.ID) || !hasBeadID(ready, target.ID) {
		t.Fatalf("Ready() before dep add = %+v, want %s and %s", ready, blocker.ID, target.ID)
	}

	if err := s1.DepAdd(target.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd(%s, %s): %v", target.ID, blocker.ID, err)
	}

	ready, err = s2.Ready()
	if err != nil {
		t.Fatalf("Ready() after dep add: %v", err)
	}
	if !hasBeadID(ready, blocker.ID) {
		t.Fatalf("Ready() after dep add = %+v, want blocker %s", ready, blocker.ID)
	}
	if hasBeadID(ready, target.ID) {
		t.Fatalf("Ready() after dep add still contains blocked bead %s: %+v", target.ID, ready)
	}
}

func TestFileStoreChildrenRefreshesAcrossOpenInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beads.json")

	s1, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}

	parent, err := s1.Create(beads.Bead{Title: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	children, err := s2.Children(parent.ID)
	if err != nil {
		t.Fatalf("Children(%q) before child create: %v", parent.ID, err)
	}
	if len(children) != 0 {
		t.Fatalf("Children(%q) before child create = %+v, want empty", parent.ID, children)
	}

	child, err := s1.Create(beads.Bead{Title: "child", ParentID: parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	children, err = s2.Children(parent.ID)
	if err != nil {
		t.Fatalf("Children(%q) after child create: %v", parent.ID, err)
	}
	if len(children) != 1 || children[0].ID != child.ID {
		t.Fatalf("Children(%q) after child create = %+v, want only %s", parent.ID, children, child.ID)
	}
}

func TestFileStoreDepListRefreshesAcrossOpenInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beads.json")

	s1, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}

	a, err := s1.Create(beads.Bead{Title: "a"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s1.Create(beads.Bead{Title: "b"})
	if err != nil {
		t.Fatal(err)
	}

	deps, err := s2.DepList(a.ID, "down")
	if err != nil {
		t.Fatalf("DepList(%q, down) before dep add: %v", a.ID, err)
	}
	if len(deps) != 0 {
		t.Fatalf("DepList(%q, down) before dep add = %+v, want empty", a.ID, deps)
	}

	if err := s1.DepAdd(a.ID, b.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd(%s, %s): %v", a.ID, b.ID, err)
	}

	deps, err = s2.DepList(a.ID, "down")
	if err != nil {
		t.Fatalf("DepList(%q, down) after dep add: %v", a.ID, err)
	}
	if len(deps) != 1 || deps[0].DependsOnID != b.ID {
		t.Fatalf("DepList(%q, down) after dep add = %+v, want one dep on %s", a.ID, deps, b.ID)
	}
}

func TestFileStoreListByAssigneeRefreshesAcrossOpenInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beads.json")

	s1, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}

	assigned, err := s2.ListByAssignee("mayor", "open", 0)
	if err != nil {
		t.Fatalf("ListByAssignee before create: %v", err)
	}
	if len(assigned) != 0 {
		t.Fatalf("ListByAssignee before create = %+v, want empty", assigned)
	}

	created, err := s1.Create(beads.Bead{Title: "owned", Assignee: "mayor"})
	if err != nil {
		t.Fatal(err)
	}

	assigned, err = s2.ListByAssignee("mayor", "open", 0)
	if err != nil {
		t.Fatalf("ListByAssignee after create: %v", err)
	}
	if len(assigned) != 1 || assigned[0].ID != created.ID {
		t.Fatalf("ListByAssignee after create = %+v, want only %s", assigned, created.ID)
	}
}

func TestFileStoreRefreshesAfterOpenRace(t *testing.T) {
	path := "/city/.gc/beads.json"
	base := fsys.NewFake()

	s1, err := beads.OpenFileStore(base, path)
	if err != nil {
		t.Fatal(err)
	}
	created, err := s1.Create(beads.Bead{Title: "alpha"})
	if err != nil {
		t.Fatal(err)
	}

	racyFS := &statRaceFS{
		FS:   base,
		path: path,
		beforeFirstStat: func() {
			if err := s1.Update(created.ID, beads.UpdateOpts{Title: ptr("bravo")}); err != nil {
				t.Fatalf("Update(%q) during open race: %v", created.ID, err)
			}
		},
	}

	s2, err := beads.OpenFileStore(racyFS, path)
	if err != nil {
		t.Fatal(err)
	}

	got, err := s2.Get(created.ID)
	if err != nil {
		t.Fatalf("Get(%q) after open race: %v", created.ID, err)
	}
	if got.Title != "bravo" {
		t.Fatalf("Title after open race = %q, want bravo", got.Title)
	}
}

func TestFileStoreSkipsReadReloadWhenFileIsUnchanged(t *testing.T) {
	f := fsys.NewFake()
	path := "/city/.gc/beads.json"

	s1, err := beads.OpenFileStore(f, path)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := beads.OpenFileStore(f, path)
	if err != nil {
		t.Fatal(err)
	}

	created, err := s1.Create(beads.Bead{Title: "cached bead"})
	if err != nil {
		t.Fatal(err)
	}

	f.Calls = nil
	for i := 0; i < 2; i++ {
		if _, err := s2.Get(created.ID); err != nil {
			t.Fatalf("Get(%q) #%d: %v", created.ID, i+1, err)
		}
	}

	var statCalls, readCalls int
	for _, call := range f.Calls {
		if call.Path != path {
			continue
		}
		switch call.Method {
		case "Stat":
			statCalls++
		case "ReadFile":
			readCalls++
		}
	}
	if statCalls != 2 {
		t.Fatalf("Stat(%s) calls = %d, want 2", path, statCalls)
	}
	if readCalls != 1 {
		t.Fatalf("ReadFile(%s) calls = %d, want 1 after cache warmup", path, readCalls)
	}
}

func TestFileStoreRefreshesSameSizeExternalRewrite(t *testing.T) {
	f := fsys.NewFake()
	path := "/city/.gc/beads.json"

	s1, err := beads.OpenFileStore(f, path)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := beads.OpenFileStore(f, path)
	if err != nil {
		t.Fatal(err)
	}

	created, err := s1.Create(beads.Bead{Title: strings.Repeat("a", 32)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Get(created.ID); err != nil {
		t.Fatalf("initial Get(%q): %v", created.ID, err)
	}

	beforeLen := len(f.Files[path])
	updatedTitle := rewriteTitleKeepingFileSize(t, f, path, created.ID, beforeLen)
	afterLen := len(f.Files[path])
	if beforeLen != afterLen {
		t.Fatalf("expected same-size rewrite, got %d -> %d bytes", beforeLen, afterLen)
	}

	f.Calls = nil
	got, err := s2.Get(created.ID)
	if err != nil {
		t.Fatalf("Get(%q) after same-size update: %v", created.ID, err)
	}
	if got.Title != updatedTitle {
		t.Fatalf("Title after same-size update = %q, want %q", got.Title, updatedTitle)
	}

	var readCalls int
	for _, call := range f.Calls {
		if call.Method == "ReadFile" && call.Path == path {
			readCalls++
		}
	}
	if readCalls != 1 {
		t.Fatalf("ReadFile(%s) calls = %d, want 1 after same-size rewrite", path, readCalls)
	}
}

func TestFileStoreMutatorReloadsSameSizeExternalRewriteWithUnchangedFreshness(t *testing.T) {
	f := fsys.NewFake()
	path := "/city/.gc/beads.json"

	stale, err := beads.OpenFileStore(f, path)
	if err != nil {
		t.Fatal(err)
	}
	created, err := stale.Create(beads.Bead{Title: strings.Repeat("a", 32)})
	if err != nil {
		t.Fatal(err)
	}
	originalModTime := f.ModTimes[path]
	originalLen := len(f.Files[path])

	updatedTitle := rewriteTitleKeepingFileSize(t, f, path, created.ID, originalLen)
	if gotLen := len(f.Files[path]); gotLen != originalLen {
		t.Fatalf("expected same-size external rewrite, got %d -> %d bytes", originalLen, gotLen)
	}
	f.ModTimes[path] = originalModTime

	if err := stale.SetMetadata(created.ID, "owner", "controller"); err != nil {
		t.Fatalf("SetMetadata(%q) from stale handle: %v", created.ID, err)
	}

	fresh, err := beads.OpenFileStore(f, path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := fresh.Get(created.ID)
	if err != nil {
		t.Fatalf("Get(%q) after stale-handle mutator: %v", created.ID, err)
	}
	if got.Title != updatedTitle {
		t.Fatalf("Title after stale-handle mutator = %q, want %q", got.Title, updatedTitle)
	}
	if got.Metadata["owner"] != "controller" {
		t.Fatalf("metadata[owner] after stale-handle mutator = %q, want controller", got.Metadata["owner"])
	}
}

func rewriteTitleKeepingFileSize(t *testing.T, f *fsys.Fake, path, id string, targetLen int) string {
	t.Helper()

	var fd struct {
		Seq   int          `json:"seq"`
		Beads []beads.Bead `json:"beads"`
		Deps  []beads.Dep  `json:"deps,omitempty"`
	}
	if err := json.Unmarshal(f.Files[path], &fd); err != nil {
		t.Fatalf("unmarshal file store data: %v", err)
	}

	for titleLen := 1; titleLen <= targetLen; titleLen++ {
		title := "b" + strings.Repeat("x", titleLen-1)
		for descLen := 0; descLen <= targetLen; descLen++ {
			candidate := fd
			candidate.Beads = append([]beads.Bead(nil), fd.Beads...)
			found := false
			for i := range candidate.Beads {
				if candidate.Beads[i].ID != id {
					continue
				}
				found = true
				candidate.Beads[i].Title = title
				if descLen == 0 {
					candidate.Beads[i].Description = ""
				} else {
					candidate.Beads[i].Description = strings.Repeat("d", descLen)
				}
				break
			}
			if !found {
				t.Fatalf("bead %q missing from file store data", id)
			}
			data, err := json.MarshalIndent(candidate, "", "  ")
			if err != nil {
				t.Fatalf("marshal same-size file store data: %v", err)
			}
			if len(data) != targetLen {
				continue
			}
			if err := f.WriteFile(path, data, 0o644); err != nil {
				t.Fatalf("write same-size file store data: %v", err)
			}
			return title
		}
	}
	t.Fatalf("could not produce same-size rewrite for %s: target=%d last=%d", path, targetLen, len(f.Files[path]))
	return ""
}

func TestFileStoreRefreshFallbackReloadsWhenStatFails(t *testing.T) {
	base := fsys.NewFake()
	path := "/city/.gc/beads.json"

	writer, err := beads.OpenFileStore(base, path)
	if err != nil {
		t.Fatal(err)
	}
	created, err := writer.Create(beads.Bead{Title: "alpha"})
	if err != nil {
		t.Fatal(err)
	}

	readerFS := &oneShotStatErrorFS{
		FS:   base,
		path: path,
		err:  fmt.Errorf("stat unavailable"),
	}
	reader, err := beads.OpenFileStore(readerFS, path)
	if err != nil {
		t.Fatal(err)
	}

	got, err := reader.Get(created.ID)
	if err != nil {
		t.Fatalf("Get(%q) after Stat failure fallback: %v", created.ID, err)
	}
	if got.Title != "alpha" {
		t.Fatalf("Get(%q) title = %q, want alpha", created.ID, got.Title)
	}
}

func TestFileStoreRefreshPropagatesReloadErrorAfterExternalRewrite(t *testing.T) {
	base := fsys.NewFake()
	path := "/city/.gc/beads.json"

	writer, err := beads.OpenFileStore(base, path)
	if err != nil {
		t.Fatal(err)
	}
	created, err := writer.Create(beads.Bead{Title: "alpha"})
	if err != nil {
		t.Fatal(err)
	}

	readerFS := &toggledErrorFS{FS: base, path: path}
	reader, err := beads.OpenFileStore(readerFS, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Get(created.ID); err != nil {
		t.Fatalf("initial Get(%q): %v", created.ID, err)
	}

	if err := writer.Update(created.ID, beads.UpdateOpts{Title: ptr("bravo")}); err != nil {
		t.Fatalf("Update(%q): %v", created.ID, err)
	}
	readerFS.readErr = fmt.Errorf("read boom")

	if _, err := reader.Get(created.ID); err == nil {
		t.Fatalf("Get(%q) after external rewrite err = nil, want read boom", created.ID)
	} else if !strings.Contains(err.Error(), "read boom") {
		t.Fatalf("Get(%q) after external rewrite err = %v, want read boom", created.ID, err)
	}
}

func TestFileStoreCreateRewarmsAfterFreshnessStatFailure(t *testing.T) {
	base := fsys.NewFake()
	path := "/city/.gc/beads.json"
	fs := &toggledErrorFS{
		FS:      base,
		path:    path,
		statErr: fmt.Errorf("stat unavailable"),
	}

	s, err := beads.OpenFileStore(fs, path)
	if err != nil {
		t.Fatal(err)
	}
	created, err := s.Create(beads.Bead{Title: "alpha"})
	if err != nil {
		t.Fatalf("Create() with post-save Stat failure: %v", err)
	}

	fs.statErr = nil
	base.Calls = nil

	got, err := s.Get(created.ID)
	if err != nil {
		t.Fatalf("Get(%q) after clearing Stat failure: %v", created.ID, err)
	}
	if got.Title != "alpha" {
		t.Fatalf("Get(%q) title = %q, want alpha", created.ID, got.Title)
	}

	var readCalls int
	for _, call := range base.Calls {
		if call.Method == "ReadFile" && call.Path == path {
			readCalls++
		}
	}
	if readCalls == 0 {
		t.Fatalf("expected Get(%q) to re-read %s after freshness cache was cleared", created.ID, path)
	}
}

func TestFileStoreReadWrappersPropagateRefreshErrors(t *testing.T) {
	base := fsys.NewFake()
	path := "/city/.gc/beads.json"
	fs := &toggledErrorFS{FS: base, path: path}

	s, err := beads.OpenFileStore(fs, path)
	if err != nil {
		t.Fatal(err)
	}
	fs.statErr = fmt.Errorf("stat boom")
	fs.readErr = fmt.Errorf("read boom")

	tests := []struct {
		name string
		call func() error
	}{
		{
			name: "Get",
			call: func() error {
				_, err := s.Get("gc-1")
				return err
			},
		},
		{
			name: "List",
			call: func() error {
				_, err := s.List(beads.ListQuery{})
				return err
			},
		},
		{
			name: "ListOpen",
			call: func() error {
				_, err := s.ListOpen()
				return err
			},
		},
		{
			name: "Ready",
			call: func() error {
				_, err := s.Ready()
				return err
			},
		},
		{
			name: "Children",
			call: func() error {
				_, err := s.Children("gc-1")
				return err
			},
		},
		{
			name: "ListByLabel",
			call: func() error {
				_, err := s.ListByLabel("x", 0)
				return err
			},
		},
		{
			name: "ListByAssignee",
			call: func() error {
				_, err := s.ListByAssignee("mayor", "open", 0)
				return err
			},
		},
		{
			name: "ListByMetadata",
			call: func() error {
				_, err := s.ListByMetadata(map[string]string{"k": "v"}, 0)
				return err
			},
		},
		{
			name: "DepList",
			call: func() error {
				_, err := s.DepList("gc-1", "down")
				return err
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatalf("%s() err = nil, want refresh error", tc.name)
			}
			if !strings.Contains(err.Error(), "read boom") {
				t.Fatalf("%s() err = %v, want read boom", tc.name, err)
			}
		})
	}
}

func TestFileStoreMutatorsPropagateRefreshErrors(t *testing.T) {
	base := fsys.NewFake()
	path := "/city/.gc/beads.json"
	fs := &toggledErrorFS{FS: base, path: path}

	s, err := beads.OpenFileStore(fs, path)
	if err != nil {
		t.Fatal(err)
	}
	fs.statErr = fmt.Errorf("stat boom")
	fs.readErr = fmt.Errorf("read boom")

	tests := []struct {
		name string
		call func() error
	}{
		{
			name: "Create",
			call: func() error {
				_, err := s.Create(beads.Bead{Title: "x"})
				return err
			},
		},
		{
			name: "Update",
			call: func() error {
				return s.Update("gc-1", beads.UpdateOpts{Title: ptr("updated")})
			},
		},
		{
			name: "Close",
			call: func() error {
				return s.Close("gc-1")
			},
		},
		{
			name: "Delete",
			call: func() error {
				return s.Delete("gc-1")
			},
		},
		{
			name: "CloseAll",
			call: func() error {
				_, err := s.CloseAll([]string{"gc-1"}, map[string]string{"phase": "done"})
				return err
			},
		},
		{
			name: "SetMetadata",
			call: func() error {
				return s.SetMetadata("gc-1", "k", "v")
			},
		},
		{
			name: "SetMetadataBatch",
			call: func() error {
				return s.SetMetadataBatch("gc-1", map[string]string{"k": "v"})
			},
		},
		{
			name: "DepAdd",
			call: func() error {
				return s.DepAdd("gc-1", "gc-2", "blocks")
			},
		},
		{
			name: "DepRemove",
			call: func() error {
				return s.DepRemove("gc-1", "gc-2")
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatalf("%s() err = nil, want refresh error", tc.name)
			}
			if !strings.Contains(err.Error(), "read boom") {
				t.Fatalf("%s() err = %v, want read boom", tc.name, err)
			}
		})
	}
}

func TestFileStoreCloseAllRefreshesAcrossOpenInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beads.json")

	s1, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}

	first, err := s1.Create(beads.Bead{Title: "first", Labels: []string{"x"}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s1.Create(beads.Bead{Title: "second", Labels: []string{"x"}})
	if err != nil {
		t.Fatal(err)
	}

	closed, err := s1.CloseAll([]string{first.ID, second.ID}, map[string]string{"gc.batch": "done"})
	if err != nil {
		t.Fatalf("CloseAll(): %v", err)
	}
	if closed != 2 {
		t.Fatalf("CloseAll() closed = %d, want 2", closed)
	}

	open, err := s2.ListOpen()
	if err != nil {
		t.Fatalf("ListOpen() after CloseAll: %v", err)
	}
	if len(open) != 0 {
		t.Fatalf("ListOpen() after CloseAll = %+v, want empty", open)
	}

	got, err := s2.ListByMetadata(map[string]string{"gc.batch": "done"}, 0, beads.IncludeClosed)
	if err != nil {
		t.Fatalf("ListByMetadata() after CloseAll: %v", err)
	}
	if len(got) != 2 || !hasBeadID(got, first.ID) || !hasBeadID(got, second.ID) {
		t.Fatalf("ListByMetadata() after CloseAll = %+v, want %s and %s", got, first.ID, second.ID)
	}
}

func TestFileStoreClearsCacheWhenBackingFileDisappears(t *testing.T) {
	f := fsys.NewFake()
	path := "/city/.gc/beads.json"

	s1, err := beads.OpenFileStore(f, path)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := beads.OpenFileStore(f, path)
	if err != nil {
		t.Fatal(err)
	}

	created, err := s1.Create(beads.Bead{Title: "ephemeral"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Get(created.ID); err != nil {
		t.Fatalf("initial Get(%q): %v", created.ID, err)
	}

	if err := f.Remove(path); err != nil {
		t.Fatalf("Remove(%s): %v", path, err)
	}

	if _, err := s2.Get(created.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Get(%q) after external delete err = %v, want ErrNotFound", created.ID, err)
	}

	got, err := s2.ListOpen()
	if err != nil {
		t.Fatalf("ListOpen() after external delete: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ListOpen() after external delete = %+v, want empty", got)
	}
}

func TestFileStoreDeletePersistsAcrossOpenInstances(t *testing.T) {
	f := fsys.NewFake()
	path := "/city/.gc/beads.json"

	s1, err := beads.OpenFileStore(f, path)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := beads.OpenFileStore(f, path)
	if err != nil {
		t.Fatal(err)
	}

	created, err := s1.Create(beads.Bead{Title: "ephemeral"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Get(created.ID); err != nil {
		t.Fatalf("initial Get(%q): %v", created.ID, err)
	}

	if err := s1.Delete(created.ID); err != nil {
		t.Fatalf("Delete(%q): %v", created.ID, err)
	}

	if _, err := s2.Get(created.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Get(%q) after persisted delete err = %v, want ErrNotFound", created.ID, err)
	}

	got, err := s2.ListOpen()
	if err != nil {
		t.Fatalf("ListOpen() after persisted delete: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ListOpen() after persisted delete = %+v, want empty", got)
	}
}

func TestFileStoreDeletePropagatesLockError(t *testing.T) {
	f := fsys.NewFake()
	s, err := beads.OpenFileStore(f, "/city/.gc/beads.json")
	if err != nil {
		t.Fatal(err)
	}
	s.SetLocker(errLocker{lockErr: fmt.Errorf("lock boom")})

	if err := s.Delete("gc-1"); err == nil {
		t.Fatal("Delete(gc-1) err = nil, want lock boom")
	} else if !strings.Contains(err.Error(), "lock boom") {
		t.Fatalf("Delete(gc-1) err = %v, want lock boom", err)
	}
}

func TestFileStoreDeletePropagatesMemStoreError(t *testing.T) {
	f := fsys.NewFake()
	s, err := beads.OpenFileStore(f, "/city/.gc/beads.json")
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Delete("gc-404"); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Delete(gc-404) err = %v, want ErrNotFound", err)
	}
}

func TestFileStoreDeleteRollsBackWhenSaveFails(t *testing.T) {
	f := fsys.NewFake()
	path := "/city/.gc/beads.json"

	s1, err := beads.OpenFileStore(f, path)
	if err != nil {
		t.Fatal(err)
	}
	created, err := s1.Create(beads.Bead{Title: "keep me"})
	if err != nil {
		t.Fatal(err)
	}

	f.Errors[path+".tmp"] = fmt.Errorf("disk full")

	err = s1.Delete(created.ID)
	if err == nil {
		t.Fatalf("Delete(%q) err = nil, want disk full", created.ID)
	}
	if !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("Delete(%q) err = %v, want disk full", created.ID, err)
	}

	delete(f.Errors, path+".tmp")

	if _, err := s1.Get(created.ID); err != nil {
		t.Fatalf("Get(%q) after rollback: %v", created.ID, err)
	}

	s2, err := beads.OpenFileStore(f, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Get(created.ID); err != nil {
		t.Fatalf("Get(%q) after reopen: %v", created.ID, err)
	}
}

func TestFileStoreDeletePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beads.json")

	s1, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s1.Create(beads.Bead{Title: "delete-me"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.Delete(b.ID); err != nil {
		t.Fatal(err)
	}

	s2, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Get(b.ID); err == nil {
		t.Fatalf("Get(%q) after reopen should fail", b.ID)
	} else if !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Get(%q) after reopen = %v, want ErrNotFound", b.ID, err)
	}
}

func ptr[T any](v T) *T {
	return &v
}

func hasBeadID(beadsList []beads.Bead, id string) bool {
	for _, b := range beadsList {
		if b.ID == id {
			return true
		}
	}
	return false
}

func TestFileStoreChildrenExcludeClosedByDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beads.json")
	s, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}

	parent, err := s.Create(beads.Bead{Title: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	openChild, err := s.Create(beads.Bead{Title: "open", ParentID: parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	closedChild, err := s.Create(beads.Bead{Title: "closed", ParentID: parent.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(closedChild.ID); err != nil {
		t.Fatal(err)
	}

	got, err := s.Children(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != openChild.ID {
		t.Fatalf("Children() = %+v, want only %s", got, openChild.ID)
	}

	got, err = s.Children(parent.ID, beads.IncludeClosed)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("Children(IncludeClosed) = %d items, want 2", len(got))
	}
}

func TestFileStoreListByLabelRequiresIncludeClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beads.json")
	s, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}

	open, err := s.Create(beads.Bead{Title: "open", Labels: []string{"x"}})
	if err != nil {
		t.Fatal(err)
	}
	closed, err := s.Create(beads.Bead{Title: "closed", Labels: []string{"x"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(closed.ID); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListByLabel("x", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != open.ID {
		t.Fatalf("ListByLabel() = %+v, want only %s", got, open.ID)
	}

	got, err = s.ListByLabel("x", 0, beads.IncludeClosed)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("ListByLabel(IncludeClosed) = %d items, want 2", len(got))
	}
}

func TestFileStoreListByMetadataRequiresIncludeClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beads.json")
	s, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}

	open, err := s.Create(beads.Bead{Title: "open"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetMetadata(open.ID, "gc.root_bead_id", "root-1"); err != nil {
		t.Fatal(err)
	}
	closed, err := s.Create(beads.Bead{Title: "closed"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetMetadata(closed.ID, "gc.root_bead_id", "root-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(closed.ID); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListByMetadata(map[string]string{"gc.root_bead_id": "root-1"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != open.ID {
		t.Fatalf("ListByMetadata() = %+v, want only %s", got, open.ID)
	}

	got, err = s.ListByMetadata(map[string]string{"gc.root_bead_id": "root-1"}, 0, beads.IncludeClosed)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("ListByMetadata(IncludeClosed) = %d items, want 2", len(got))
	}
}

func TestFileStoreOpenEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "subdir", "beads.json")

	// Opening a non-existent file should succeed (creates parent dirs).
	s, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}

	// First bead should be gc-1.
	b, err := s.Create(beads.Bead{Title: "first"})
	if err != nil {
		t.Fatal(err)
	}
	if b.ID != "gc-1" {
		t.Errorf("ID = %q, want %q", b.ID, "gc-1")
	}
}

func TestFileStorePingDetectsReadFailures(t *testing.T) {
	path := "/city/beads.json"
	f := fsys.NewFake()
	f.Dirs["/city"] = true
	f.Files[path] = []byte(`{}`)

	s, err := beads.OpenFileStore(f, path)
	if err != nil {
		t.Fatal(err)
	}

	f.Errors[path] = fmt.Errorf("permission denied")
	if err := s.Ping(); err == nil {
		t.Fatal("expected ping error")
	} else if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("Ping error = %v, want permission denied", err)
	}
}

func TestFileStoreOpenCorruptedJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beads.json")
	if err := os.WriteFile(path, []byte("{not json!!!"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err == nil {
		t.Fatal("expected error for corrupted JSON")
	}
	if !strings.Contains(err.Error(), "opening file store") {
		t.Errorf("error = %q, want 'opening file store' prefix", err)
	}
}

func TestFileStoreOpenUnreadable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("chmod 0 does not prevent reading on Windows")
	}
	if os.Getuid() == 0 {
		t.Skip("root can read any file")
	}

	path := filepath.Join(t.TempDir(), "beads.json")
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, 0o644) }) //nolint:errcheck // best-effort cleanup

	_, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err == nil {
		t.Fatal("expected error for unreadable file")
	}
	if !strings.Contains(err.Error(), "opening file store") {
		t.Errorf("error = %q, want 'opening file store' prefix", err)
	}
}

// --- failure-path tests with fsys.Fake ---

func TestFileStoreOpenMkdirFails(t *testing.T) {
	f := fsys.NewFake()
	f.Errors["/city/.gc"] = fmt.Errorf("permission denied")

	_, err := beads.OpenFileStore(f, "/city/.gc/beads.json")
	if err == nil {
		t.Fatal("expected error when MkdirAll fails")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("error = %q, want 'permission denied'", err)
	}
}

func TestFileStoreOpenReadFileFails(t *testing.T) {
	f := fsys.NewFake()
	f.Errors["/city/.gc/beads.json"] = fmt.Errorf("disk error")

	_, err := beads.OpenFileStore(f, "/city/.gc/beads.json")
	if err == nil {
		t.Fatal("expected error when ReadFile fails")
	}
	if !strings.Contains(err.Error(), "disk error") {
		t.Errorf("error = %q, want 'disk error'", err)
	}
}

func TestFileStoreOpenCorruptedJSONFake(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/.gc/beads.json"] = []byte("{not json!!!")

	_, err := beads.OpenFileStore(f, "/city/.gc/beads.json")
	if err == nil {
		t.Fatal("expected error for corrupted JSON")
	}
	if !strings.Contains(err.Error(), "opening file store") {
		t.Errorf("error = %q, want 'opening file store' prefix", err)
	}
}

func TestFileStoreSaveWriteFails(t *testing.T) {
	f := fsys.NewFake()
	s, err := beads.OpenFileStore(f, "/city/.gc/beads.json")
	if err != nil {
		t.Fatal(err)
	}

	// Inject error on the temp file write.
	f.Errors["/city/.gc/beads.json.tmp"] = fmt.Errorf("disk full")

	_, err = s.Create(beads.Bead{Title: "test"})
	if err == nil {
		t.Fatal("expected error when WriteFile fails")
	}
	if !strings.Contains(err.Error(), "disk full") {
		t.Errorf("error = %q, want 'disk full'", err)
	}
}

func TestFileStoreSaveRenameFails(t *testing.T) {
	f := fsys.NewFake()
	s, err := beads.OpenFileStore(f, "/city/.gc/beads.json")
	if err != nil {
		t.Fatal(err)
	}

	// Inject error on the rename (atomic commit step).
	f.Errors["/city/.gc/beads.json.tmp"] = fmt.Errorf("rename failed")

	_, err = s.Create(beads.Bead{Title: "test"})
	if err == nil {
		t.Fatal("expected error when Rename fails")
	}
	if !strings.Contains(err.Error(), "rename failed") {
		t.Errorf("error = %q, want 'rename failed'", err)
	}
}

// TestFileStoreConcurrentCreateWithFlock verifies that two FileStore instances
// backed by flock on the same file produce unique IDs (no collisions).
func TestFileStoreConcurrentCreateWithFlock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("flock not available on Windows")
	}

	dir := t.TempDir()
	beadsPath := filepath.Join(dir, "beads.json")
	lockPath := beadsPath + ".lock"

	const perStore = 20

	// Open two stores on the same file, each with its own flock.
	open := func() *beads.FileStore {
		s, err := beads.OpenFileStore(fsys.OSFS{}, beadsPath)
		if err != nil {
			t.Fatal(err)
		}
		s.SetLocker(beads.NewFileFlock(lockPath))
		return s
	}

	s1 := open()
	s2 := open()

	// Run creates concurrently from both stores.
	var wg sync.WaitGroup
	ids := make(chan string, perStore*2)

	createN := func(s *beads.FileStore, prefix string) {
		defer wg.Done()
		for i := 0; i < perStore; i++ {
			b, err := s.Create(beads.Bead{Title: fmt.Sprintf("%s-%d", prefix, i)})
			if err != nil {
				t.Errorf("Create failed: %v", err)
				return
			}
			ids <- b.ID
		}
	}

	wg.Add(2)
	go createN(s1, "s1")
	go createN(s2, "s2")
	wg.Wait()
	close(ids)

	// All IDs must be unique.
	seen := make(map[string]bool)
	for id := range ids {
		if seen[id] {
			t.Errorf("duplicate ID: %s", id)
		}
		seen[id] = true
	}
	if len(seen) != perStore*2 {
		t.Errorf("got %d unique IDs, want %d", len(seen), perStore*2)
	}

	// Reopen and verify all beads survived.
	s3 := open()
	all, err := s3.ListOpen()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != perStore*2 {
		t.Errorf("after reopen: %d beads, want %d", len(all), perStore*2)
	}
}

// This regression covers the default locker path for OS-backed file stores.
// It fails on branches where callers must inject locking manually.
func TestFileStoreConcurrentCreateUsesDefaultLock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("flock not available on Windows")
	}

	dir := t.TempDir()
	beadsPath := filepath.Join(dir, "beads.json")

	const perStore = 20

	open := func() *beads.FileStore {
		s, err := beads.OpenFileStore(fsys.OSFS{}, beadsPath)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	s1 := open()
	s2 := open()

	var wg sync.WaitGroup
	ids := make(chan string, perStore*2)

	createN := func(s *beads.FileStore, prefix string) {
		defer wg.Done()
		for i := 0; i < perStore; i++ {
			b, err := s.Create(beads.Bead{Title: fmt.Sprintf("%s-%d", prefix, i)})
			if err != nil {
				t.Errorf("Create failed: %v", err)
				return
			}
			ids <- b.ID
		}
	}

	wg.Add(2)
	go createN(s1, "s1")
	go createN(s2, "s2")
	wg.Wait()
	close(ids)

	seen := make(map[string]bool)
	for id := range ids {
		if seen[id] {
			t.Errorf("duplicate ID: %s", id)
		}
		seen[id] = true
	}
	if len(seen) != perStore*2 {
		t.Errorf("got %d unique IDs, want %d", len(seen), perStore*2)
	}

	s3 := open()
	all, err := s3.ListOpen()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != perStore*2 {
		t.Errorf("after reopen: %d beads, want %d", len(all), perStore*2)
	}
}

func TestFileStoreCloseWriteFails(t *testing.T) {
	f := fsys.NewFake()
	s, err := beads.OpenFileStore(f, "/city/.gc/beads.json")
	if err != nil {
		t.Fatal(err)
	}

	// Create a bead successfully first.
	b, err := s.Create(beads.Bead{Title: "test"})
	if err != nil {
		t.Fatal(err)
	}

	// Now inject error on the next save (Close flushes).
	f.Errors["/city/.gc/beads.json.tmp"] = fmt.Errorf("disk full")

	err = s.Close(b.ID)
	if err == nil {
		t.Fatal("expected error when save fails during Close")
	}
	if !strings.Contains(err.Error(), "disk full") {
		t.Errorf("error = %q, want 'disk full'", err)
	}
}

// BUG: PR #215 -- this test fails because FileStore has no cross-process
// flock. Two FileStore instances opened on the same empty file get
// independent seq counters (both starting at 0). Each produces "gc-1" for
// its first bead, and the second writer silently overwrites the first.
func TestFileStoreConcurrentInstances_DuplicateIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beads.json")

	// Simulate two processes opening the same file before either writes.
	s1, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}

	// Both stores start with seq=0 and will independently assign gc-1.
	b1, err := s1.Create(beads.Bead{Title: "from-process-1"})
	if err != nil {
		t.Fatal(err)
	}
	b2, err := s2.Create(beads.Bead{Title: "from-process-2"})
	if err != nil {
		t.Fatal(err)
	}

	// With a cross-process flock, the second store would reload the file
	// after the first write and assign gc-2. Without the flock, both get gc-1.
	if b1.ID == b2.ID {
		t.Errorf("two concurrent FileStore instances produced the same bead ID %q; cross-process flock is missing", b1.ID)
	}
}

// TestFileStoreRevisionContinuityAcrossDowngradeRewrite simulates the mixed-
// version hazard the review flagged: a revisions-aware binary issues tokens,
// an OLDER binary then fully rewrites the file (dropping the revisions map
// and the seal), and the new binary reloads. Revisions must come back ABOVE
// every previously issued token — never reused — and a stale pre-rewrite
// token must precondition-fail.
func TestFileStoreRevisionContinuityAcrossDowngradeRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beads.json")
	s, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	created, err := s.Create(beads.Bead{Title: "target"})
	if err != nil {
		t.Fatal(err)
	}
	title := "mutated"
	for range 3 {
		got, err := s.Get(created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Update(created.ID, beads.UpdateOpts{Title: &title}); err != nil {
			t.Fatal(err)
		}
		_ = got
	}
	preToken, err := s.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate the N-1 binary's full rewrite: same beads, no revisions map,
	// no seal.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk map[string]json.RawMessage
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}
	delete(onDisk, "revisions")
	delete(onDisk, "revisions_sealed")
	rewritten, err := json.Marshal(onDisk)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, rewritten, 0o644); err != nil {
		t.Fatal(err)
	}

	s2, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := s2.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Revision <= preToken.Revision {
		t.Fatalf("post-rewrite revision %d <= previously issued %d: token reuse — the monotonic-never-reused contract is broken",
			reloaded.Revision, preToken.Revision)
	}
	w, _ := beads.ConditionalWriterFor(s2)
	if err := w.UpdateIfMatch(created.ID, preToken.Revision, beads.UpdateOpts{Title: &title}); !beads.IsPreconditionFailed(err) {
		t.Fatalf("stale pre-rewrite token: got %v, want PreconditionFailed", err)
	}
	// And determinism: a second reload of the same unsealed bytes seeds the
	// same value (no revision churn on the reload-before-write path).
	s3, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s3.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.Revision != reloaded.Revision {
		t.Fatalf("re-seed not deterministic: %d then %d", reloaded.Revision, again.Revision)
	}
}
