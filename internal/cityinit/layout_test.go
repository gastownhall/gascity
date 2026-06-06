package cityinit

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestEnsureCityScaffoldFSReturnsEventLogWriteError(t *testing.T) {
	eventsPath := filepath.Join("/city", citylayout.RuntimeRoot, "events.jsonl")
	writeErr := errors.New("read-only filesystem")
	f := writeErrorFS{
		Fake: fsys.NewFake(),
		path: eventsPath,
		err:  writeErr,
	}

	err := EnsureCityScaffoldFS(f, "/city")
	if !errors.Is(err, writeErr) {
		t.Fatalf("EnsureCityScaffoldFS error = %v, want %v", err, writeErr)
	}
}

func TestEnsureCityScaffoldFSReturnsEventLogStatError(t *testing.T) {
	eventsPath := filepath.Join("/city", citylayout.RuntimeRoot, "events.jsonl")
	statErr := errors.New("permission denied")
	f := statErrorFS{
		Fake: fsys.NewFake(),
		path: eventsPath,
		err:  statErr,
	}

	err := EnsureCityScaffoldFS(f, "/city")
	if !errors.Is(err, statErr) {
		t.Fatalf("EnsureCityScaffoldFS error = %v, want %v", err, statErr)
	}
}

type writeErrorFS struct {
	*fsys.Fake
	path string
	err  error
}

func (f writeErrorFS) WriteFile(name string, data []byte, perm os.FileMode) error {
	if name == f.path {
		return f.err
	}
	return f.Fake.WriteFile(name, data, perm)
}

type statErrorFS struct {
	*fsys.Fake
	path string
	err  error
}

func (f statErrorFS) Stat(name string) (os.FileInfo, error) {
	if name == f.path {
		return nil, f.err
	}
	return f.Fake.Stat(name)
}
