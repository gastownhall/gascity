package beads_test

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/fsys"
)

// TestFileStoreAtomicCloserConformance runs the shared atomic terminal-close
// suite against FileStore. Without the capability a session close on a
// file-backed city stamps its terminal metadata and closes the row as two
// flushes, and a writer landing between them strands a closed row that still
// looks live.
func TestFileStoreAtomicCloserConformance(t *testing.T) {
	beadstest.RunAtomicConditionalCloserConformance(t, "FileStore", func(t *testing.T) beads.Store {
		s, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}

// TestFileStoreAtomicCloseFencesAcrossInstances proves the fence holds between
// two FileStore instances on one file (two processes, in production): the
// close reloads from disk under the flock before checking the revision, so a
// write another instance flushed after this one's observation is seen and the
// close is refused. The winning close is durable for a fresh reopen.
func TestFileStoreAtomicCloseFencesAcrossInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "beads.json")
	closerSide, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	writerSide, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	created, err := closerSide.Create(beads.Bead{Title: "cross-instance close"})
	if err != nil {
		t.Fatal(err)
	}
	observed, err := closerSide.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := writerSide.SetMetadata(created.ID, "state", "awake"); err != nil {
		t.Fatalf("SetMetadata from the other instance: %v", err)
	}

	_, err = closerSide.CloseWithMetadataIfMatch(created.ID, observed.Revision, map[string]string{"state": "drained"})
	if !beads.IsPreconditionFailed(err) {
		t.Fatalf("close fenced on a revision another instance moved = %v, want *PreconditionFailedError", err)
	}

	current, err := closerSide.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != "open" || current.Metadata["state"] != "awake" {
		t.Fatalf("refused close left status %q state %q, want open/awake", current.Status, current.Metadata["state"])
	}
	if _, err := closerSide.CloseWithMetadataIfMatch(created.ID, current.Revision, map[string]string{"state": "drained"}); err != nil {
		t.Fatalf("close at the current revision: %v", err)
	}

	reopened, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "closed" || got.Metadata["state"] != "drained" {
		t.Fatalf("reopened row = status %q state %q, want the durable closed/drained", got.Status, got.Metadata["state"])
	}
}

// TestFileStoreAtomicCloseFlushFailureRollsBack proves both halves of the
// close are dropped together when the flush fails: neither memory nor disk may
// keep the metadata without the close, or the close without the metadata.
func TestFileStoreAtomicCloseFlushFailureRollsBack(t *testing.T) {
	f := fsys.NewFake()
	s, err := beads.OpenFileStore(f, "/city/.gc/beads.json")
	if err != nil {
		t.Fatal(err)
	}
	created, err := s.Create(beads.Bead{Title: "flush failure"})
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}

	f.Errors["/city/.gc/beads.json.tmp"] = fmt.Errorf("disk full")
	if _, err := s.CloseWithMetadataIfMatch(created.ID, before.Revision, map[string]string{"state": "drained"}); err == nil {
		t.Fatal("CloseWithMetadataIfMatch succeeded although the flush failed")
	}
	delete(f.Errors, "/city/.gc/beads.json.tmp")

	after, err := s.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != "open" || after.Metadata["state"] != "" || after.Revision != before.Revision {
		t.Fatalf("failed flush left status %q state %q revision %d, want the untouched open row at %d",
			after.Status, after.Metadata["state"], after.Revision, before.Revision)
	}
}

// TestFileStoreAtomicCloseHonorsDisableConditionalWrites pins the instance
// toggle: a disabled store refuses the fenced close as unsupported and writes
// nothing, like its four ConditionalWriter siblings.
func TestFileStoreAtomicCloseHonorsDisableConditionalWrites(t *testing.T) {
	s, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
	if err != nil {
		t.Fatal(err)
	}
	created, err := s.Create(beads.Bead{Title: "disabled"})
	if err != nil {
		t.Fatal(err)
	}
	s.DisableConditionalWrites = true
	if _, err := s.CloseWithMetadataIfMatch(created.ID, created.Revision, map[string]string{"state": "drained"}); !errors.Is(err, beads.ErrConditionalWriteUnsupported) {
		t.Fatalf("CloseWithMetadataIfMatch on a disabled store = %v, want ErrConditionalWriteUnsupported", err)
	}
	after, err := s.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != "open" || after.Metadata["state"] != "" {
		t.Fatalf("refused close mutated the row: status %q state %q", after.Status, after.Metadata["state"])
	}
}
