package events

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// lockedBuffer is a goroutine-safe stderr sink: recorders write to stderr from
// both the caller's goroutine and their background rotation goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// seedEventLog writes n events with seqs 1..n straight to path, standing in for
// an active log some earlier process filled.
func seedEventLog(t *testing.T, path string, n int) {
	t.Helper()
	var data bytes.Buffer
	for i := 1; i <= n; i++ {
		line, err := json.Marshal(Event{Seq: uint64(i), Type: BeadCreated, Ts: time.Now(), Actor: "seed", Subject: fmt.Sprintf("seed-%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		data.Write(line)
		data.WriteByte('\n')
	}
	if err := os.WriteFile(path, data.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

// assertLogIntegrity reads the whole log (archives then active file) and checks
// that every subject in want appears exactly once, that seqs are unique and
// strictly increasing, and that every rotation anchor continues its prior
// archive (anchor seq == prior last seq + 1).
func assertLogIntegrity(t *testing.T, path string, want []string) []Event {
	t.Helper()
	all, err := ReadAll(path)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	counts := make(map[string]int)
	for i, e := range all {
		counts[e.Subject]++
		if i > 0 && e.Seq <= all[i-1].Seq {
			t.Errorf("seq not strictly increasing at %d: %d (%s) after %d (%s)", i, e.Seq, e.Subject, all[i-1].Seq, all[i-1].Subject)
		}
		if e.Type == EventsRotated {
			var p RotatedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatalf("anchor payload: %v", err)
			}
			if e.Seq != p.PriorLastSeq+1 {
				t.Errorf("anchor seq = %d, want prior last seq %d + 1", e.Seq, p.PriorLastSeq)
			}
		}
	}
	for _, s := range want {
		if counts[s] != 1 {
			t.Errorf("subject %q appears %d times in the log, want exactly 1", s, counts[s])
		}
	}
	return all
}

// TestStaleRecorderDoesNotRotateFreshLogOrOrphanOwner reproduces the
// mc-zndi7.58 loss: a recorder that opened its handle on the full log before
// the owner rotated must not, on its first write, rotate the owner's fresh
// active log (the path now names a different file than its handle), and the
// owner must keep writing to the live log rather than into a renamed file that
// gzip then deletes.
func TestStaleRecorderDoesNotRotateFreshLogOrOrphanOwner(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	seedEventLog(t, path, 20) // ~2 KiB: over the threshold below
	opts := []FileRecorderOption{WithMaxSize(1024), WithRotationCheckRecords(1)}

	var ownerErr, staleErr lockedBuffer
	owner, err := NewFileRecorder(path, &ownerErr, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close() //nolint:errcheck // test cleanup
	// Opened before the owner rotates: its handle names the full log.
	stale, err := NewFileRecorder(path, &staleErr, append(opts, WithoutStartupSweep())...)
	if err != nil {
		t.Fatal(err)
	}
	defer stale.Close() //nolint:errcheck // test cleanup

	if err := owner.RecordAck(Event{Type: BeadCreated, Actor: "owner", Subject: "owner-1"}); err != nil {
		t.Fatal(err)
	}
	if err := stale.RecordAck(Event{Type: BeadCreated, Actor: "cli", Subject: "stale-1"}); err != nil {
		t.Fatal(err)
	}
	// Let any rotation gzip finish and unlink its source, so a write through a
	// handle on that source would land in a deleted file.
	stale.WaitForRotations()
	owner.WaitForRotations()
	if err := owner.RecordAck(Event{Type: BeadCreated, Actor: "owner", Subject: "owner-2"}); err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}

	all := assertLogIntegrity(t, path, []string{"owner-1", "stale-1", "owner-2"})
	rotations := 0
	for _, e := range all {
		if e.Type == EventsRotated {
			rotations++
		}
	}
	if rotations != 1 {
		t.Errorf("rotations = %d, want exactly 1 (the stale recorder must not rotate the fresh log)", rotations)
	}
	if !strings.Contains(staleErr.String(), "replaced by another writer") {
		t.Errorf("stale recorder stderr = %q, want a replaced-handle warning", staleErr.String())
	}
}

// TestConcurrentRecordersKeepSeqsUniqueAcrossRotation drives concurrent
// writers, each on its own handle (as separate processes would be), across
// many rotations. Every acknowledged event must be in the log exactly once,
// seqs must stay unique and monotonic, and every anchor must continue its
// archive. It covers the intended shape (one rotating owner beside
// non-rotating writers) and the shape the incident ran (every writer rotating,
// as every CLI recorder did): sidecar serialization and the handle check must
// keep even that safe.
func TestConcurrentRecordersKeepSeqsUniqueAcrossRotation(t *testing.T) {
	const writers, perWriter = 4, 60
	rotating := []FileRecorderOption{WithMaxSize(2048), WithRotationCheckRecords(1)}
	for _, tc := range []struct {
		name      string
		secondary []FileRecorderOption
	}{
		{name: "owner with secondary writers", secondary: []FileRecorderOption{WithoutStartupSweep()}},
		{name: "every writer rotates", secondary: append([]FileRecorderOption{WithoutStartupSweep()}, rotating...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "events.jsonl")
			var stderr lockedBuffer
			owner, err := NewFileRecorder(path, &stderr, rotating...)
			if err != nil {
				t.Fatal(err)
			}
			recs := []*FileRecorder{owner}
			for i := 1; i < writers; i++ {
				rec, err := NewFileRecorder(path, &stderr, tc.secondary...)
				if err != nil {
					t.Fatal(err)
				}
				recs = append(recs, rec)
			}

			var (
				mu    sync.Mutex
				acked []string
				wg    sync.WaitGroup
				start = make(chan struct{})
			)
			for w, rec := range recs {
				wg.Add(1)
				go func(w int, rec *FileRecorder) {
					defer wg.Done()
					<-start
					for i := 0; i < perWriter; i++ {
						subject := fmt.Sprintf("w%d-%d", w, i)
						if err := rec.RecordAck(Event{Type: BeadCreated, Actor: "test", Subject: subject}); err == nil {
							mu.Lock()
							acked = append(acked, subject)
							mu.Unlock()
						}
					}
				}(w, rec)
			}
			close(start)
			wg.Wait()
			for _, rec := range recs {
				if err := rec.Close(); err != nil {
					t.Fatal(err)
				}
			}

			if len(acked) == 0 {
				t.Fatal("no event was acknowledged")
			}
			all := assertLogIntegrity(t, path, acked)
			rotations := 0
			for _, e := range all {
				if e.Type == EventsRotated {
					rotations++
				}
			}
			if rotations == 0 {
				t.Fatal("expected at least one rotation")
			}
		})
	}
}

// TestRecordReopensDeletedHandle covers a recorder whose handle was rotated away
// and deleted underneath it by another writer: the next write must land in the
// live log at the path and continue the sequence. Only a rotation owner warns,
// since for it another rotator is a misconfiguration; the warning is printed
// after the locks are released, so it never holds up other writers.
func TestRecordReopensDeletedHandle(t *testing.T) {
	for _, tc := range []struct {
		name     string
		opts     []FileRecorderOption
		wantWarn bool
	}{
		{name: "secondary stays quiet", opts: []FileRecorderOption{WithoutStartupSweep()}},
		{name: "owner warns", opts: []FileRecorderOption{WithMaxSize(1 << 30)}, wantWarn: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "events.jsonl")
			stderr := &sidecarProbeWriter{t: t, lockPath: path + ".lock"}
			rec, err := NewFileRecorder(path, stderr, tc.opts...)
			if err != nil {
				t.Fatal(err)
			}
			defer rec.Close() //nolint:errcheck // test cleanup
			if err := rec.RecordAck(Event{Type: BeadCreated, Actor: "test", Subject: "before"}); err != nil {
				t.Fatal(err)
			}

			// Another writer rotates the log and the archived source is deleted.
			moved := filepath.Join(dir, "moved")
			if err := os.Rename(path, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(moved); err != nil {
				t.Fatal(err)
			}

			if err := rec.RecordAck(Event{Type: BeadCreated, Actor: "test", Subject: "after"}); err != nil {
				t.Fatal(err)
			}
			all, err := ReadAll(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(all) != 1 || all[0].Subject != "after" || all[0].Seq != 2 {
				t.Fatalf("live log = %+v, want only the post-delete event at seq 2", all)
			}
			got := stderr.String()
			if warned := strings.Contains(got, "replaced by another writer"); warned != tc.wantWarn {
				t.Errorf("stderr = %q, want replaced-handle warning = %t", got, tc.wantWarn)
			}
		})
	}
}

// TestRecorderSerializesOnStableSidecarLock pins the cross-process lock to the
// events.jsonl.lock sidecar, which rotation never renames: a writer holding it
// excludes every recorder, before and after a rotation, and the recorder gives
// up after its bounded wait instead of deadlocking.
func TestRecorderSerializesOnStableSidecarLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	rec, err := NewFileRecorder(path, &lockedBuffer{})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup
	if err := rec.RecordAck(Event{Type: BeadCreated, Actor: "test", Subject: "first"}); err != nil {
		t.Fatal(err)
	}
	if _, err := rec.ForceRotate(); err != nil {
		t.Fatal(err)
	}
	rec.WaitForRotations()

	holder := flockExclusive(t, path+".lock")
	if err := rec.RecordAck(Event{Type: BeadCreated, Actor: "test", Subject: "blocked"}); err == nil {
		t.Fatal("RecordAck succeeded while another writer held the sidecar lock")
	}
	if err := holder.Close(); err != nil {
		t.Fatal(err)
	}
	if err := rec.RecordAck(Event{Type: BeadCreated, Actor: "test", Subject: "after"}); err != nil {
		t.Fatalf("RecordAck after release: %v", err)
	}
	assertLogIntegrity(t, path, []string{"first", "after"})
}

// TestRecorderStaysSafeBesideLegacyWriters covers a mixed-version city: an older
// binary locks the data file itself (not the sidecar) and appends with its own
// seqs. A new recorder must still honor that data-file lock (bounded, no
// deadlock) and continue the sequence past the legacy append.
func TestRecorderStaysSafeBesideLegacyWriters(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	rec, err := NewFileRecorder(path, &lockedBuffer{})
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup
	if err := rec.RecordAck(Event{Type: BeadCreated, Actor: "test", Subject: "new-1"}); err != nil {
		t.Fatal(err)
	}

	legacy := flockExclusive(t, path)
	if err := rec.RecordAck(Event{Type: BeadCreated, Actor: "test", Subject: "blocked"}); err == nil {
		t.Fatal("RecordAck succeeded while a legacy writer held the data-file lock")
	}
	line, err := json.Marshal(Event{Seq: 7, Type: BeadCreated, Ts: time.Now(), Actor: "legacy", Subject: "legacy-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	if err := rec.RecordAck(Event{Type: BeadCreated, Actor: "test", Subject: "new-2"}); err != nil {
		t.Fatal(err)
	}
	all := assertLogIntegrity(t, path, []string{"new-1", "legacy-1", "new-2"})
	if last := all[len(all)-1]; last.Seq != 8 {
		t.Errorf("seq after legacy append = %d, want 8", last.Seq)
	}
}

// TestRotationWarnsWhenAnchorSkipsSeqs covers the anchor alarm: when the
// recorder has issued (or observed) seqs past the last one in the file it is
// archiving, those seqs are not in that archive, so the anchor cannot be prior
// last + 1. The rotation must still continue past every seq it knows of, and
// report the gap.
func TestRotationWarnsWhenAnchorSkipsSeqs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	seedEventLog(t, path, 5)
	// An archive holding seqs beyond the active log's tail, as a lost or
	// colliding writer leaves behind.
	writeArchiveWithEvents(t, dir, "20261001T125436Z", 50, 100, Event{Seq: 100, Type: BeadCreated, Ts: time.Now()})

	var stderr lockedBuffer
	rec, err := NewFileRecorder(path, &stderr, WithoutStartupSweep())
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup
	res, err := rec.ForceRotate()
	if err != nil {
		t.Fatal(err)
	}
	rec.WaitForRotations()
	if res.LastSeq != 5 || res.AnchorSeq != 101 {
		t.Fatalf("rotation = last %d anchor %d, want last 5 anchor 101", res.LastSeq, res.AnchorSeq)
	}
	if want := "anchor seq 101 is not archived last seq 5 + 1"; !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr = %q, want it to contain %q", stderr.String(), want)
	}
}

// flockExclusive opens path and takes an exclusive flock on it, standing in for
// another process holding that lock. Closing the returned file releases it.
func flockExclusive(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			t.Fatalf("%s is already locked", path)
		}
		t.Fatal(err)
	}
	return f
}
