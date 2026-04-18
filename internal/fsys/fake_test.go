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

func TestFakeStatFile(t *testing.T) {
	f := NewFake()
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

	if err := f.WriteFile("/city/city.toml", []byte("hello"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if got := string(f.Files["/city/city.toml"]); got != "hello" {
		t.Fatalf("Files content = %q, want %q", got, "hello")
	}
	if f.ModTimes["/city/city.toml"].IsZero() {
		t.Fatal("expected WriteFile to initialize synthetic mod time")
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
