package fsys

import (
	"errors"
	"fmt"
	"os"
	"testing"
)

func TestFakeStatDir(t *testing.T) {
	f := NewFake()
	f.Dirs["/city/.gc"] = true

	fi, err := f.Stat("/city/.gc")
	if err != nil {
		t.Fatalf("Stat existing dir: %v", err)
	}
	if !fi.IsDir() {
		t.Error("expected IsDir() = true")
	}
	if fi.Name() != ".gc" {
		t.Errorf("Name() = %q, want %q", fi.Name(), ".gc")
	}
}

func TestFakeStatDirModeIncludesDirBit(t *testing.T) {
	f := NewFake()
	f.Dirs["/city/.gc"] = true

	fi, err := f.Stat("/city/.gc")
	if err != nil {
		t.Fatalf("Stat existing dir: %v", err)
	}
	if fi.Mode().IsRegular() {
		t.Fatalf("directory mode reports regular file: %v", fi.Mode())
	}
	if fi.Mode()&os.ModeDir == 0 {
		t.Fatalf("directory mode missing ModeDir bit: %v", fi.Mode())
	}
}

func TestFakeStatFile(t *testing.T) {
	f := NewFake()
	f.Dirs["/city"] = true
	if err := f.WriteFile("/city/city.toml", []byte("hello"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	fi, err := f.Stat("/city/city.toml")
	if err != nil {
		t.Fatalf("Stat existing file: %v", err)
	}
	if fi.IsDir() {
		t.Error("expected IsDir() = false for file")
	}
	if fi.Size() != 5 {
		t.Errorf("Size() = %d, want 5", fi.Size())
	}
	if fi.ModTime().IsZero() {
		t.Error("expected synthetic mod time for written file")
	}
}

func TestFakeStatSynthesizesModTimeForPrepopulatedFile(t *testing.T) {
	f := &Fake{
		Files: map[string][]byte{
			"/city/city.toml": []byte("hello"),
		},
	}

	fi, err := f.Stat("/city/city.toml")
	if err != nil {
		t.Fatalf("Stat existing file: %v", err)
	}
	if fi.ModTime().IsZero() {
		t.Fatal("expected synthetic mod time for prepopulated file")
	}
	if got := f.ModTimes["/city/city.toml"]; !got.Equal(fi.ModTime()) {
		t.Fatalf("stored mod time = %v, want %v", got, fi.ModTime())
	}

	fi2, err := f.Stat("/city/city.toml")
	if err != nil {
		t.Fatalf("second Stat existing file: %v", err)
	}
	if !fi2.ModTime().Equal(fi.ModTime()) {
		t.Fatalf("second Stat mod time = %v, want %v", fi2.ModTime(), fi.ModTime())
	}
}

func TestFakeStatFollowsSymlinkTargets(t *testing.T) {
	t.Run("file", func(t *testing.T) {
		f := NewFake()
		f.Files["/city/target.toml"] = []byte("hello")
		f.Symlinks["/city/link.toml"] = "/city/target.toml"

		fi, err := f.Stat("/city/link.toml")
		if err != nil {
			t.Fatalf("Stat symlink to file: %v", err)
		}
		if fi.Name() != "link.toml" {
			t.Fatalf("Name() = %q, want link.toml", fi.Name())
		}
		if fi.Size() != 5 {
			t.Fatalf("Size() = %d, want 5", fi.Size())
		}
		wantModTime := f.ModTimes["/city/target.toml"]
		if wantModTime.IsZero() {
			t.Fatal("expected Stat to synthesize and store target mod time")
		}
		if !fi.ModTime().Equal(wantModTime) {
			t.Fatalf("ModTime() = %v, want %v", fi.ModTime(), wantModTime)
		}
	})

	t.Run("dir", func(t *testing.T) {
		f := NewFake()
		f.Dirs["/city/rigs"] = true
		f.Symlinks["/city/rig-link"] = "/city/rigs"

		fi, err := f.Stat("/city/rig-link")
		if err != nil {
			t.Fatalf("Stat symlink to dir: %v", err)
		}
		if !fi.IsDir() {
			t.Fatal("expected symlink to dir to report directory")
		}
	})

	t.Run("missing target", func(t *testing.T) {
		f := NewFake()
		f.Symlinks["/city/missing-link"] = "/city/missing"

		_, err := f.Stat("/city/missing-link")
		if err == nil {
			t.Fatal("expected error for missing symlink target")
		}
		if !os.IsNotExist(err) {
			t.Fatalf("expected os.IsNotExist, got %v", err)
		}
	})
}

func TestFakeStatMissing(t *testing.T) {
	f := NewFake()

	_, err := f.Stat("/no/such/path")
	if err == nil {
		t.Fatal("expected error for missing path")
	}
	if !os.IsNotExist(err) {
		t.Errorf("expected os.IsNotExist, got: %v", err)
	}
}

func TestFakeStatErrorInjection(t *testing.T) {
	f := NewFake()
	injected := fmt.Errorf("disk on fire")
	f.Errors["/city/.gc"] = injected

	_, err := f.Stat("/city/.gc")
	if !errors.Is(err, injected) {
		t.Errorf("Stat error = %v, want %v", err, injected)
	}
}

func TestFakeMkdirAll(t *testing.T) {
	f := NewFake()

	if err := f.MkdirAll("/city/.gc/agents", 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// Should record the directory and parents.
	for _, d := range []string{"/city/.gc/agents", "/city/.gc", "/city"} {
		if !f.Dirs[d] {
			t.Errorf("Dirs[%q] = false, want true", d)
		}
	}

	// Should record the call.
	if len(f.Calls) != 1 || f.Calls[0].Method != "MkdirAll" {
		t.Errorf("Calls = %+v, want single MkdirAll", f.Calls)
	}
}

func TestFakeMkdirAllError(t *testing.T) {
	f := NewFake()
	injected := fmt.Errorf("permission denied")
	f.Errors["/city/.gc"] = injected

	err := f.MkdirAll("/city/.gc", 0o755)
	if !errors.Is(err, injected) {
		t.Errorf("MkdirAll error = %v, want %v", err, injected)
	}
}

func TestFakeWriteFile(t *testing.T) {
	f := NewFake()
	f.Dirs["/city"] = true

	data := []byte("# city.toml\n")
	if err := f.WriteFile("/city/city.toml", data, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, ok := f.Files["/city/city.toml"]
	if !ok {
		t.Fatal("file not recorded")
	}
	if string(got) != string(data) {
		t.Errorf("Files content = %q, want %q", got, data)
	}

	if len(f.Calls) != 1 || f.Calls[0].Method != "WriteFile" {
		t.Errorf("Calls = %+v, want single WriteFile", f.Calls)
	}
	if f.ModTimes["/city/city.toml"].IsZero() {
		t.Error("expected WriteFile to set a synthetic mod time")
	}
}

func TestFakeWriteFileInitializesNilMaps(t *testing.T) {
	f := &Fake{}

	if err := f.WriteFile("/city.toml", []byte("hello"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if got := string(f.Files["/city.toml"]); got != "hello" {
		t.Fatalf("Files content = %q, want %q", got, "hello")
	}
	if f.ModTimes["/city.toml"].IsZero() {
		t.Fatal("expected WriteFile to initialize synthetic mod time")
	}
}

func TestFakeWriteFileInitializesModes(t *testing.T) {
	f := &Fake{Files: map[string][]byte{}}

	if err := f.WriteFile("/run.sh", []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if f.Modes["/run.sh"] != 0o755 {
		t.Fatalf("mode = %v, want 0755", f.Modes["/run.sh"])
	}
}

func TestFakeWriteFileError(t *testing.T) {
	f := NewFake()
	injected := fmt.Errorf("read-only fs")
	f.Errors["/city/city.toml"] = injected

	err := f.WriteFile("/city/city.toml", []byte("x"), 0o644)
	if !errors.Is(err, injected) {
		t.Errorf("WriteFile error = %v, want %v", err, injected)
	}
}

func TestFakeReadDir(t *testing.T) {
	f := NewFake()
	f.Dirs["/city/rigs/alpha"] = true
	f.Dirs["/city/rigs/beta"] = true
	f.Files["/city/rigs/config.toml"] = []byte("x")

	entries, err := f.ReadDir("/city/rigs")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	// Should have 3 entries: alpha (dir), beta (dir), config.toml (file) — sorted.
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3: %+v", len(entries), entries)
	}

	want := []struct {
		name  string
		isDir bool
	}{
		{"alpha", true},
		{"beta", true},
		{"config.toml", false},
	}
	for i, w := range want {
		if entries[i].Name() != w.name {
			t.Errorf("entry[%d].Name() = %q, want %q", i, entries[i].Name(), w.name)
		}
		if entries[i].IsDir() != w.isDir {
			t.Errorf("entry[%d].IsDir() = %v, want %v", i, entries[i].IsDir(), w.isDir)
		}
	}
}

func TestFakeDirectSeededChildrenImplyOnlyComponentParents(t *testing.T) {
	f := NewFake()
	f.Files["/city/rigs/alpha/config.toml"] = []byte("alpha")

	info, err := f.Stat("/city/rigs")
	if err != nil {
		t.Fatalf("Stat implied parent: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("implied parent mode = %v, want directory", info.Mode())
	}
	entries, err := f.ReadDir("/city/rigs")
	if err != nil {
		t.Fatalf("ReadDir implied parent: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "alpha" || !entries[0].IsDir() {
		t.Fatalf("ReadDir implied entries = %+v, want alpha directory", entries)
	}
	if err := f.WriteFile("/city/rigs/new.toml", []byte("new"), 0o600); err != nil {
		t.Fatalf("WriteFile under implied parent: %v", err)
	}

	prefixOnly := NewFake()
	prefixOnly.Files["/city/rigs-old/config.toml"] = []byte("old")
	if _, err := prefixOnly.Stat("/city/rigs"); !os.IsNotExist(err) {
		t.Fatalf("Stat prefix-only path error = %v, want os.ErrNotExist", err)
	}
}

func TestFakeInferredParentsPersistAfterLastChildRemoval(t *testing.T) {
	f := NewFake()
	file := "/city/rig/file"
	f.Files[file] = []byte("content")

	if err := f.Remove(file); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	for _, path := range []string{"/city", "/city/rig"} {
		info, err := f.Stat(path)
		if err != nil {
			t.Fatalf("Stat(%q): %v", path, err)
		}
		if !info.IsDir() {
			t.Errorf("Stat(%q).Mode() = %v, want directory", path, info.Mode())
		}
	}
}

func TestFakeInferredSourceParentPersistsAfterDirectoryRename(t *testing.T) {
	f := NewFake()
	f.Files["/city/source/file"] = []byte("content")
	f.Dirs["/destination"] = true

	if err := f.Rename("/city/source", "/destination/source"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	info, err := f.Stat("/city")
	if err != nil {
		t.Fatalf("Stat source parent: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("source parent mode = %v, want directory", info.Mode())
	}
	if got, err := f.ReadFile("/destination/source/file"); err != nil || string(got) != "content" {
		t.Fatalf("renamed child = %q, %v; want %q, nil", got, err, "content")
	}
}

func TestFakeChmodMaterializesInferredDirectory(t *testing.T) {
	f := NewFake()
	f.Files["/city/rig/file"] = []byte("content")

	if err := f.Chmod("/city/rig", 0o700); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	delete(f.Files, "/city/rig/file")
	info, err := f.Stat("/city/rig")
	if err != nil {
		t.Fatalf("Stat after removing seeded child: %v", err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("materialized directory mode = %v, want directory 0700", info.Mode())
	}
}

func TestFakeReadDirInfoReportsTrackedMode(t *testing.T) {
	f := NewFake()
	f.Dirs["/city/rigs"] = true
	if err := f.WriteFile("/city/rigs/run.sh", []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	entries, err := f.ReadDir("/city/rigs")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	info, err := entries[0].Info()
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("ReadDir entry mode = %v, want 0755", info.Mode().Perm())
	}
}

func TestFakeReadDirError(t *testing.T) {
	f := NewFake()
	injected := fmt.Errorf("no such directory")
	f.Errors["/city/rigs"] = injected

	_, err := f.ReadDir("/city/rigs")
	if !errors.Is(err, injected) {
		t.Errorf("ReadDir error = %v, want %v", err, injected)
	}
}

func TestFakeReadDirEmpty(t *testing.T) {
	f := NewFake()
	f.Dirs["/city/rigs"] = true

	entries, err := f.ReadDir("/city/rigs")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("got %d entries, want 0", len(entries))
	}
}

func TestFakeRename(t *testing.T) {
	f := NewFake()
	f.Dirs["/city"] = true
	if err := f.WriteFile("/city/beads.json.tmp", []byte(`{"seq":1}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	oldModTime := f.ModTimes["/city/beads.json.tmp"]

	if err := f.Rename("/city/beads.json.tmp", "/city/beads.json"); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	// Old path gone, new path has the data.
	if _, ok := f.Files["/city/beads.json.tmp"]; ok {
		t.Error("old path still exists after Rename")
	}
	if string(f.Files["/city/beads.json"]) != `{"seq":1}` {
		t.Errorf("new path content = %q, want %q", f.Files["/city/beads.json"], `{"seq":1}`)
	}
	if got := f.ModTimes["/city/beads.json"]; !got.Equal(oldModTime) {
		t.Errorf("renamed file mod time = %v, want %v", got, oldModTime)
	}

	if len(f.Calls) != 2 || f.Calls[1].Method != "Rename" {
		t.Errorf("Calls = %+v, want WriteFile then Rename", f.Calls)
	}
}

func TestFakeRenameClearsStaleDestinationMode(t *testing.T) {
	f := NewFake()
	f.Files["/city/generated.tmp"] = []byte("new")
	f.Files["/city/generated"] = []byte("old")
	f.Modes["/city/generated.tmp"] = 0o600
	f.Modes["/city/generated"] = 0o644

	if err := f.Rename("/city/generated.tmp", "/city/generated"); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	info, err := f.Stat("/city/generated")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("renamed file mode = %v, want source mode 0600", info.Mode().Perm())
	}
}

func TestFakeRenameWithNilModTimes(t *testing.T) {
	f := &Fake{Files: map[string][]byte{"/source": []byte("content")}}

	if err := f.Rename("/source", "/destination"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if got := string(f.Files["/destination"]); got != "content" {
		t.Fatalf("destination content = %q, want %q", got, "content")
	}
	if f.ModTimes["/destination"].IsZero() {
		t.Fatal("destination mod time was not initialized")
	}
}

func TestFakeChmodInitializesModes(t *testing.T) {
	f := &Fake{Files: map[string][]byte{"/city/run.sh": []byte("#!/bin/sh\n")}}

	if err := f.Chmod("/city/run.sh", 0o755); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	if f.Modes["/city/run.sh"] != 0o755 {
		t.Fatalf("mode = %v, want 0755", f.Modes["/city/run.sh"])
	}
}

func TestFakeRenameSymlink(t *testing.T) {
	f := NewFake()
	f.Symlinks["/city/beads-link"] = "/city/beads.json"

	if err := f.Rename("/city/beads-link", "/city/beads-renamed"); err != nil {
		t.Fatalf("Rename symlink: %v", err)
	}
	if _, ok := f.Symlinks["/city/beads-link"]; ok {
		t.Fatal("old symlink path still exists after Rename")
	}
	if got := f.Symlinks["/city/beads-renamed"]; got != "/city/beads.json" {
		t.Fatalf("renamed symlink target = %q, want /city/beads.json", got)
	}
}

func TestFakeRenameSynthesizesModTimeWhenMissing(t *testing.T) {
	f := NewFake()
	f.Files["/city/beads.json.tmp"] = []byte(`{"seq":1}`)

	if err := f.Rename("/city/beads.json.tmp", "/city/beads.json"); err != nil {
		t.Fatalf("Rename without source modtime: %v", err)
	}
	if f.ModTimes["/city/beads.json"].IsZero() {
		t.Fatal("expected Rename to synthesize a mod time when source mod time is missing")
	}
}

func TestFakeRenameError(t *testing.T) {
	f := NewFake()
	injected := fmt.Errorf("cross-device link")
	f.Errors["/city/beads.json.tmp"] = injected

	err := f.Rename("/city/beads.json.tmp", "/city/beads.json")
	if !errors.Is(err, injected) {
		t.Errorf("Rename error = %v, want %v", err, injected)
	}
}

func TestFakeRenameMissing(t *testing.T) {
	f := NewFake()

	err := f.Rename("/no/such/file", "/city/beads.json")
	if err == nil {
		t.Fatal("expected error for missing source path")
	}
	if !os.IsNotExist(err) {
		t.Errorf("expected os.IsNotExist, got: %v", err)
	}
}

func TestFakeRemoveVariants(t *testing.T) {
	t.Run("file removes modtime", func(t *testing.T) {
		f := NewFake()
		f.Dirs["/city"] = true
		if err := f.WriteFile("/city/city.toml", []byte("hello"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		if err := f.Remove("/city/city.toml"); err != nil {
			t.Fatalf("Remove file: %v", err)
		}
		if _, ok := f.Files["/city/city.toml"]; ok {
			t.Fatal("file still exists after Remove")
		}
		if _, ok := f.ModTimes["/city/city.toml"]; ok {
			t.Fatal("mod time still exists after Remove")
		}
	})

	t.Run("dir", func(t *testing.T) {
		f := NewFake()
		f.Dirs["/city/.gc"] = true

		if err := f.Remove("/city/.gc"); err != nil {
			t.Fatalf("Remove dir: %v", err)
		}
		if f.Dirs["/city/.gc"] {
			t.Fatal("dir still exists after Remove")
		}
	})

	t.Run("symlink", func(t *testing.T) {
		f := NewFake()
		f.Symlinks["/city/link"] = "/city/target"

		if err := f.Remove("/city/link"); err != nil {
			t.Fatalf("Remove symlink: %v", err)
		}
		if _, ok := f.Symlinks["/city/link"]; ok {
			t.Fatal("symlink still exists after Remove")
		}
	})
}

func TestFakeChmodVariants(t *testing.T) {
	f := NewFake()
	f.Dirs["/city"] = true
	if err := f.WriteFile("/city/city.toml", []byte("hello"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	f.Dirs["/city/.gc"] = true
	f.Symlinks["/city/link"] = "/city/city.toml"

	for _, path := range []string{"/city/city.toml", "/city/.gc", "/city/link"} {
		if err := f.Chmod(path, 0o600); err != nil {
			t.Fatalf("Chmod(%s): %v", path, err)
		}
	}

	if err := f.Chmod("/city/missing", 0o600); err == nil {
		t.Fatal("expected error for missing path")
	} else if !os.IsNotExist(err) {
		t.Fatalf("expected os.IsNotExist, got %v", err)
	}
}
