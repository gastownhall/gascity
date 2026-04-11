package events

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"sync"
	"testing"
)

// TestFileRecorderTwoInstancesUniqueSeq is the regression test for the
// events.jsonl duplicate seq bug. Two FileRecorder instances pointing at
// the same file record N events each concurrently. After both finish,
// every seq value in the file must be unique and the set must be
// contiguous from 1..2N with no gaps.
//
// Before the sidecar counter fix, each FileRecorder maintained its own
// in-memory seq counter seeded from a scan, so two instances produced
// duplicate seq numbers under concurrent writes.
func TestFileRecorderTwoInstancesUniqueSeq(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var stderr bytes.Buffer

	rec1, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rec1.Close() })

	rec2, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rec2.Close() })

	const n = 100
	var wg sync.WaitGroup
	wg.Add(2)

	record := func(rec *FileRecorder, actor string) {
		defer wg.Done()
		for i := 0; i < n; i++ {
			rec.Record(Event{Type: BeadUpdated, Actor: actor})
		}
	}
	go record(rec1, "rec1")
	go record(rec2, "rec2")
	wg.Wait()

	if err := rec1.Close(); err != nil {
		t.Fatalf("rec1 close: %v", err)
	}
	if err := rec2.Close(); err != nil {
		t.Fatalf("rec2 close: %v", err)
	}

	if stderr.Len() > 0 {
		t.Fatalf("unexpected stderr output: %s", stderr.String())
	}

	events, err := ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(events), 2*n; got != want {
		t.Fatalf("event count = %d, want %d", got, want)
	}

	seqs := make([]uint64, len(events))
	for i, e := range events {
		seqs[i] = e.Seq
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })

	// Uniqueness + contiguity check: must be 1..2N.
	for i, s := range seqs {
		want := uint64(i + 1)
		if s != want {
			t.Fatalf("seq[%d] = %d, want %d (full sorted seqs: %v)", i, s, want, seqs)
		}
	}
}

// TestFileRecorderSeedsFromExistingFile verifies backwards compatibility:
// when the sidecar counter file does not yet exist but events.jsonl
// already contains records, the sidecar is seeded from the max seq
// scanned from the existing file.
func TestFileRecorderSeedsFromExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	// Simulate a legacy events.jsonl with no sidecar: write three
	// pre-existing events by hand.
	preExisting := []byte(
		`{"seq":7,"type":"bead.updated","ts":"2026-04-10T00:00:00Z","actor":"legacy"}` + "\n" +
			`{"seq":8,"type":"bead.updated","ts":"2026-04-10T00:00:01Z","actor":"legacy"}` + "\n" +
			`{"seq":9,"type":"bead.updated","ts":"2026-04-10T00:00:02Z","actor":"legacy"}` + "\n",
	)
	if err := os.WriteFile(path, preExisting, 0o644); err != nil {
		t.Fatal(err)
	}
	// Sanity: sidecar must not exist yet.
	if _, err := os.Stat(seqCounterPath(path)); !os.IsNotExist(err) {
		t.Fatalf("sidecar unexpectedly exists before NewFileRecorder: err=%v", err)
	}

	var stderr bytes.Buffer
	rec, err := NewFileRecorder(path, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rec.Close() })

	// Sidecar should now exist and be seeded from max seq (9).
	if _, err := os.Stat(seqCounterPath(path)); err != nil {
		t.Fatalf("sidecar was not created: %v", err)
	}

	rec.Record(Event{Type: BeadUpdated, Actor: "new"})
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}

	events, err := ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 {
		t.Fatalf("got %d events, want 4", len(events))
	}
	if events[3].Seq != 10 {
		t.Errorf("new event Seq = %d, want 10", events[3].Seq)
	}
}

// TestFileRecorderCrossProcessUniqueSeq exercises the cross-process path
// by spawning child processes that each append N events to the same
// events.jsonl concurrently. This is the real-world scenario that
// motivated the fix: two Gas City processes (e.g. the daemon and a CLI
// subcommand) sharing an events log.
//
// On Linux/Unix this uses flock; on Windows it uses LockFileEx. On
// platforms where lockFile is a no-op this test is skipped because
// cross-process coordination cannot be guaranteed.
func TestFileRecorderCrossProcessUniqueSeq(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" && runtime.GOOS != "freebsd" &&
		runtime.GOOS != "netbsd" && runtime.GOOS != "openbsd" && runtime.GOOS != "windows" {
		t.Skipf("cross-process flock not supported on %s", runtime.GOOS)
	}
	// The child invocation runs this same test binary with a sentinel
	// env var set; when that env var is present, main switches to a
	// helper mode that appends events and exits. We implement the
	// helper inline using TestMain-style hijacking via a sentinel env
	// var evaluated in an init-time helper below.
	if os.Getenv("GC_EVENTS_SEQ_CHILD") != "" {
		runSeqChild()
		return
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")

	const procs = 2
	const perProc = 100

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, procs)
	for i := 0; i < procs; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd := exec.Command(exe, "-test.run", "TestFileRecorderCrossProcessUniqueSeq", "-test.v=false")
			cmd.Env = append(os.Environ(),
				"GC_EVENTS_SEQ_CHILD=1",
				"GC_EVENTS_SEQ_PATH="+path,
				"GC_EVENTS_SEQ_N="+strconv.Itoa(perProc),
				"GC_EVENTS_SEQ_ACTOR=proc"+strconv.Itoa(i),
			)
			out, err := cmd.CombinedOutput()
			if err != nil {
				errs <- &childError{out: string(out), err: err}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatalf("child process failed: %v", e)
	}

	events, err := ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(events), procs*perProc; got != want {
		t.Fatalf("event count = %d, want %d", got, want)
	}
	seqs := make([]uint64, len(events))
	for i, e := range events {
		seqs[i] = e.Seq
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	for i, s := range seqs {
		want := uint64(i + 1)
		if s != want {
			t.Fatalf("seq[%d] = %d, want %d (full sorted seqs: %v)", i, s, want, seqs)
		}
	}
}

type childError struct {
	out string
	err error
}

func (c *childError) Error() string {
	return c.err.Error() + ": " + c.out
}

// runSeqChild is the helper entry point invoked when GC_EVENTS_SEQ_CHILD
// is set. It opens a FileRecorder at GC_EVENTS_SEQ_PATH and records
// GC_EVENTS_SEQ_N events as actor GC_EVENTS_SEQ_ACTOR, then exits cleanly.
func runSeqChild() {
	path := os.Getenv("GC_EVENTS_SEQ_PATH")
	nStr := os.Getenv("GC_EVENTS_SEQ_N")
	actor := os.Getenv("GC_EVENTS_SEQ_ACTOR")
	n, err := strconv.Atoi(nStr)
	if err != nil {
		os.Stderr.WriteString("bad GC_EVENTS_SEQ_N: " + err.Error() + "\n")
		os.Exit(2)
	}
	rec, err := NewFileRecorder(path, os.Stderr)
	if err != nil {
		os.Stderr.WriteString("NewFileRecorder: " + err.Error() + "\n")
		os.Exit(2)
	}
	for i := 0; i < n; i++ {
		rec.Record(Event{Type: BeadUpdated, Actor: actor})
	}
	if err := rec.Close(); err != nil {
		os.Stderr.WriteString("Close: " + err.Error() + "\n")
		os.Exit(2)
	}
	os.Exit(0)
}
