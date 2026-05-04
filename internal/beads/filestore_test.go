package beads_test

import (
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

	created, err := s1.Create(beads.Bead{Title: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Get(created.ID); err != nil {
		t.Fatalf("initial Get(%q): %v", created.ID, err)
	}

	beforeLen := len(f.Files[path])
	if err := s1.Update(created.ID, beads.UpdateOpts{Title: ptr("bravo")}); err != nil {
		t.Fatal(err)
	}
	afterLen := len(f.Files[path])
	if beforeLen != afterLen {
		t.Fatalf("expected same-size rewrite, got %d -> %d bytes", beforeLen, afterLen)
	}

	f.Calls = nil
	got, err := s2.Get(created.ID)
	if err != nil {
		t.Fatalf("Get(%q) after same-size update: %v", created.ID, err)
	}
	if got.Title != "bravo" {
		t.Fatalf("Title after same-size update = %q, want bravo", got.Title)
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
	writer, err := beads.OpenFileStore(f, path)
	if err != nil {
		t.Fatal(err)
	}

	created, err := stale.Create(beads.Bead{Title: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	originalModTime := f.ModTimes[path]
	originalLen := len(f.Files[path])

	if err := writer.Update(created.ID, beads.UpdateOpts{Title: ptr("bravo")}); err != nil {
		t.Fatalf("Update(%q) from second handle: %v", created.ID, err)
	}
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
	if got.Title != "bravo" {
		t.Fatalf("Title after stale-handle mutator = %q, want bravo", got.Title)
	}
	if got.Metadata["owner"] != "controller" {
		t.Fatalf("metadata[owner] after stale-handle mutator = %q, want controller", got.Metadata["owner"])
	}
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
