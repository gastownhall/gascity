package events

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// sidecarProbeWriter is a stderr sink that fails the test if anything is
// written while some recorder holds the sidecar lock at lockPath.
type sidecarProbeWriter struct {
	lockedBuffer
	t        *testing.T
	lockPath string
}

func (w *sidecarProbeWriter) Write(p []byte) (int, error) {
	f, err := os.Open(w.lockPath)
	if err != nil {
		w.t.Errorf("probe sidecar: %v", err)
	} else {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			w.t.Errorf("stderr written while the sidecar lock was held: %q", p)
		}
		_ = f.Close()
	}
	return w.lockedBuffer.Write(p)
}

// openFDsUnder lists this process's open file descriptors whose target lies in
// dir, as /proc reports them (a deleted file ends in " (deleted)"). It skips
// the test where /proc/self/fd is unavailable.
func openFDsUnder(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("no /proc/self/fd: %v", err)
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	var fds []string
	for _, e := range entries {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", e.Name()))
		if err == nil && strings.HasPrefix(target, resolved+string(filepath.Separator)) {
			fds = append(fds, target)
		}
	}
	return fds
}

// TestRecorderCloseReleasesEveryHandle opens and closes owner and secondary
// recorders repeatedly, rotating along the way, and requires that no file
// descriptor on the log directory outlives Close: not the data file, not a
// rotated-away handle, and not the sidecar lock.
func TestRecorderCloseReleasesEveryHandle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	roles := [][]FileRecorderOption{
		{WithoutStartupSweep()},
		{WithMaxSize(1), WithRotationCheckRecords(1)},
	}
	for cycle := 0; cycle < 4; cycle++ {
		for _, opts := range roles {
			rec, err := NewFileRecorder(path, io.Discard, opts...)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 3; i++ {
				if err := rec.RecordAck(Event{Type: BeadCreated, Actor: "test"}); err != nil {
					t.Fatal(err)
				}
			}
			if err := rec.Close(); err != nil {
				t.Fatal(err)
			}
			if fds := openFDsUnder(t, dir); len(fds) != 0 {
				t.Fatalf("cycle %d: descriptors still open after Close: %v", cycle, fds)
			}
		}
	}
}

// TestRecordClosesReplacedHandle covers the reopen itself: once a secondary
// finds its handle rotated away and deleted, it must close that handle rather
// than keep the deleted file pinned on disk.
func TestRecordClosesReplacedHandle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	owner, err := NewFileRecorder(path, io.Discard, WithMaxSize(1<<30))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close() //nolint:errcheck // test cleanup
	secondary, err := NewFileRecorder(path, io.Discard, WithoutStartupSweep())
	if err != nil {
		t.Fatal(err)
	}
	defer secondary.Close() //nolint:errcheck // test cleanup
	if err := secondary.RecordAck(Event{Type: BeadCreated, Actor: "test", Subject: "before"}); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.ForceRotate(); err != nil {
		t.Fatal(err)
	}
	owner.WaitForRotations()
	if err := secondary.RecordAck(Event{Type: BeadCreated, Actor: "test", Subject: "after"}); err != nil {
		t.Fatal(err)
	}

	fds := openFDsUnder(t, dir)
	for _, fd := range fds {
		if strings.HasSuffix(fd, " (deleted)") || strings.Contains(fd, ".rotating-") {
			t.Errorf("descriptor still open on a rotated-away log: %s", fd)
		}
	}
	if len(fds) != 4 {
		t.Errorf("open descriptors = %v, want 4 (each recorder's active log and sidecar)", fds)
	}
}

// TestAppendBatchFollowsReplacedHandle holds AppendBatch to the same rules as
// Record: it serializes on the sidecar and appends to the file the path names,
// not to a handle another writer rotated away.
func TestAppendBatchFollowsReplacedHandle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	owner, err := NewFileRecorder(path, io.Discard, WithMaxSize(1<<30))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close() //nolint:errcheck // test cleanup
	secondary, err := NewFileRecorder(path, io.Discard, WithoutStartupSweep())
	if err != nil {
		t.Fatal(err)
	}
	defer secondary.Close() //nolint:errcheck // test cleanup
	if err := secondary.RecordAck(Event{Type: BeadCreated, Actor: "test", Subject: "before"}); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.ForceRotate(); err != nil {
		t.Fatal(err)
	}
	owner.WaitForRotations()

	holder := flockExclusive(t, path+".lock")
	if err := secondary.AppendBatch([]Event{{Type: BeadCreated, Actor: "test", Subject: "blocked"}}); err == nil {
		t.Fatal("AppendBatch succeeded while another writer held the sidecar lock")
	}
	if err := holder.Close(); err != nil {
		t.Fatal(err)
	}
	batch := []Event{{Type: BeadCreated, Actor: "test", Subject: "batch-1"}, {Type: BeadCreated, Actor: "test", Subject: "batch-2"}}
	if err := secondary.AppendBatch(batch); err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	assertLogIntegrity(t, path, []string{"before", "batch-1", "batch-2"})
}

// stubLockDataFile replaces lockDataFile for one test.
func stubLockDataFile(t *testing.T, fn func(fd int, path string) error) {
	t.Helper()
	previous := lockDataFile
	t.Cleanup(func() { lockDataFile = previous })
	lockDataFile = fn
}

// stubOpenEventLog replaces openEventLog for one test.
func stubOpenEventLog(t *testing.T, fn func(path string) (*os.File, error)) {
	t.Helper()
	previous := openEventLog
	t.Cleanup(func() { openEventLog = previous })
	openEventLog = fn
}

// TestRotationLocksNewLogBeforeAnchor pins the data-file flock on the fresh
// active log: older binaries lock only the data file, so the anchor must be
// written under that file's lock or one of their appends could land first.
func TestRotationLocksNewLogBeforeAnchor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	seedEventLog(t, path, 3)
	var locked []syscall.Stat_t
	next := lockDataFile
	stubLockDataFile(t, func(fd int, path string) error {
		if err := next(fd, path); err != nil {
			return err
		}
		var st syscall.Stat_t
		if err := syscall.Fstat(fd, &st); err != nil {
			t.Fatal(err)
		}
		locked = append(locked, st)
		return nil
	})
	rec, err := NewFileRecorder(path, io.Discard, WithoutStartupSweep())
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup
	if _, err := rec.ForceRotate(); err != nil {
		t.Fatal(err)
	}
	rec.WaitForRotations()

	var active syscall.Stat_t
	if err := syscall.Stat(path, &active); err != nil {
		t.Fatal(err)
	}
	for _, st := range locked {
		if st.Dev == active.Dev && st.Ino == active.Ino && st.Size == 0 {
			return
		}
	}
	t.Fatal("rotation never locked the new active log before writing its anchor")
}

// TestRecorderRechecksHandleAfterLegacyRotation covers an older binary, which
// rotates under the data-file lock alone, renaming the log while this recorder
// has passed its handle check and waits for that lock. Once the lock is
// granted the handle names the renamed file, so the recorder must check again
// and append to the new active log rather than into the rotated one.
func TestRecorderRechecksHandleAfterLegacyRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	seedEventLog(t, path, 20)
	rec, err := NewFileRecorder(path, io.Discard, WithoutStartupSweep())
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	rotatingPath := filepath.Join(dir, formatRotatingBasename(time.Now().UTC(), 1, 20))
	rotated := false
	next := lockDataFile
	stubLockDataFile(t, func(fd int, path string) error {
		if !rotated {
			rotated = true
			legacyRotate(t, path, rotatingPath, 20)
		}
		return next(fd, path)
	})
	if err := rec.RecordAck(Event{Type: BeadCreated, Actor: "test", Subject: "after"}); err != nil {
		t.Fatal(err)
	}

	active, err := ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 2 || active[1].Subject != "after" || active[1].Seq != 22 {
		t.Fatalf("active log = %+v, want the legacy anchor then the event at seq 22", active)
	}
	if last := lastSeqIn(t, rotatingPath); last != 20 {
		t.Fatalf("rotated file ends at seq %d, want 20: the event was appended to the rotated file", last)
	}
}

// legacyRotate does what an older binary's rotation does: rename the log and
// start a fresh one with an anchor continuing at last + 1.
func legacyRotate(t *testing.T, path, rotatingPath string, last uint64) {
	t.Helper()
	if err := os.Rename(path, rotatingPath); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(RotatedPayload{PriorArchive: "legacy", PriorFirstSeq: 1, PriorLastSeq: last})
	if err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(Event{Seq: last + 1, Type: EventsRotated, Ts: time.Now(), Actor: "events", Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(line, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func lastSeqIn(t *testing.T, path string) uint64 {
	t.Helper()
	_, last, err := readSeqWindow(path)
	if err != nil {
		t.Fatal(err)
	}
	return last
}

// TestRecorderLocksReopenedLogBesideLegacyWriter covers the lock order after a
// reopen: the data-file flock must be taken on the file the path names now. A
// legacy writer holding the fresh log's lock must block a recorder whose
// handle was rotated away, not be bypassed by a lock on the stale handle.
func TestRecorderLocksReopenedLogBesideLegacyWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	owner, err := NewFileRecorder(path, io.Discard, WithMaxSize(1<<30))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close() //nolint:errcheck // test cleanup
	secondary, err := NewFileRecorder(path, io.Discard, WithoutStartupSweep())
	if err != nil {
		t.Fatal(err)
	}
	defer secondary.Close() //nolint:errcheck // test cleanup
	if err := secondary.RecordAck(Event{Type: BeadCreated, Actor: "test", Subject: "before"}); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.ForceRotate(); err != nil {
		t.Fatal(err)
	}
	owner.WaitForRotations()

	legacy := flockExclusive(t, path)
	if err := secondary.RecordAck(Event{Type: BeadCreated, Actor: "test", Subject: "blocked"}); err == nil {
		t.Fatal("RecordAck succeeded while a legacy writer held the fresh log's lock")
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	if err := secondary.RecordAck(Event{Type: BeadCreated, Actor: "test", Subject: "after"}); err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	assertLogIntegrity(t, path, []string{"before", "after"})
}

// TestRotationFailureBetweenRenameAndAnchorKeepsSeqs covers a rotation that
// renamed the log and then could not reopen it, so no anchor was written and
// the active log starts empty. Whichever writer appends next, the owner or a
// secondary whose own counter is far behind, must continue past the rotated
// window: reissuing its seqs is the collision mc-zndi7.58 found.
func TestRotationFailureBetweenRenameAndAnchorKeepsSeqs(t *testing.T) {
	for _, tc := range []struct {
		name      string
		secondary bool
	}{
		{name: "owner writes next"},
		{name: "secondary writes next", secondary: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "events.jsonl")
			// Opened on an empty log, so its counter is 0 until it reads the log.
			secondary, err := NewFileRecorder(path, io.Discard, WithoutStartupSweep())
			if err != nil {
				t.Fatal(err)
			}
			defer secondary.Close() //nolint:errcheck // test cleanup
			seedEventLog(t, path, 20)
			owner, err := NewFileRecorder(path, io.Discard, WithMaxSize(1), WithRotationCheckRecords(1))
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Close() //nolint:errcheck // test cleanup

			restore := openEventLog
			stubOpenEventLog(t, func(string) (*os.File, error) {
				openEventLog = restore
				return nil, errors.New("injected open failure")
			})
			err = owner.RecordAck(Event{Type: BeadCreated, Actor: "owner", Subject: "dropped"})
			if err == nil || !strings.Contains(err.Error(), "unavailable after a failed rotation") {
				t.Fatalf("RecordAck after the failed reopen = %v, want the unavailable error", err)
			}

			next := owner
			if tc.secondary {
				next = secondary
			}
			if err := next.RecordAck(Event{Type: BeadCreated, Actor: "test", Subject: "next"}); err != nil {
				t.Fatal(err)
			}
			active, err := ReadAll(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(active) != 1 || active[0].Seq != 21 {
				t.Fatalf("active log = %+v, want the next event alone at seq 21", active)
			}
		})
	}
}

// TestCrashBetweenRenameAndAnchorKeepsSeqs is the crash form of the case
// above: a rotator died after renaming the log, leaving a rotating file and no
// active log. A recorder that was already open and one opened afterwards must
// both continue past the stranded window, and the owner's next open recovers
// it into an archive with every event intact.
func TestCrashBetweenRenameAndAnchorKeepsSeqs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	stale, err := NewFileRecorder(path, io.Discard, WithoutStartupSweep())
	if err != nil {
		t.Fatal(err)
	}
	defer stale.Close() //nolint:errcheck // test cleanup
	seedEventLog(t, path, 20)
	if err := os.Rename(path, filepath.Join(dir, formatRotatingBasename(time.Now().UTC(), 1, 20))); err != nil {
		t.Fatal(err)
	}

	fresh, err := NewFileRecorder(path, io.Discard, WithoutStartupSweep())
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close() //nolint:errcheck // test cleanup
	if err := fresh.RecordAck(Event{Type: BeadCreated, Actor: "test", Subject: "fresh"}); err != nil {
		t.Fatal(err)
	}
	if err := stale.RecordAck(Event{Type: BeadCreated, Actor: "test", Subject: "stale"}); err != nil {
		t.Fatal(err)
	}
	owner, err := NewFileRecorder(path, io.Discard, WithMaxSize(1<<30))
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.RecordAck(Event{Type: BeadCreated, Actor: "test", Subject: "owner"}); err != nil {
		t.Fatal(err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}

	want := []string{"fresh", "stale", "owner"}
	for i := 1; i <= 20; i++ {
		want = append(want, fmt.Sprintf("seed-%d", i))
	}
	all := assertLogIntegrity(t, path, want)
	if last := all[len(all)-1]; last.Seq != 23 {
		t.Errorf("last seq = %d, want 23", last.Seq)
	}
}

// TestSidecarLockOpensReadWriteWithReadOnlyFallback pins how the sidecar is
// opened: read-write, which byte-range flock emulation (NFS) needs, falling
// back to read-only for a sidecar this user may not write.
func TestSidecarLockOpensReadWriteWithReadOnlyFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	rec, err := NewFileRecorder(path, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if got := accessMode(t, rec.lock); got != syscall.O_RDWR {
		t.Errorf("sidecar access mode = %#o, want O_RDWR", got)
	}
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}

	if os.Geteuid() == 0 {
		t.Skip("root ignores file modes")
	}
	if err := os.Chmod(path+".lock", 0o444); err != nil {
		t.Fatal(err)
	}
	rec, err = NewFileRecorder(path, io.Discard)
	if err != nil {
		t.Fatalf("NewFileRecorder with a read-only sidecar: %v", err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup
	if got := accessMode(t, rec.lock); got != syscall.O_RDONLY {
		t.Errorf("sidecar access mode = %#o, want the O_RDONLY fallback", got)
	}
	if err := rec.RecordAck(Event{Type: BeadCreated, Actor: "test", Subject: "locked"}); err != nil {
		t.Fatal(err)
	}
}

func accessMode(t *testing.T, f *os.File) int {
	t.Helper()
	flags, _, errno := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), syscall.F_GETFL, 0)
	if errno != 0 {
		t.Fatal(errno)
	}
	return int(flags) & syscall.O_ACCMODE
}

// TestRotationNeverReplacesExistingRotatingFile covers finding 2's hardening:
// if the rotating name a rotation picks already exists (an older binary
// rotated the same window in the same second), the rotation must fail and
// leave both files alone rather than replace the other file and lose it.
func TestRotationNeverReplacesExistingRotatingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	seedEventLog(t, path, 5)
	now := time.Now().UTC()
	var planted []string
	for d := -1; d <= 2; d++ {
		name := filepath.Join(dir, formatRotatingBasename(now.Add(time.Duration(d)*time.Second), 1, 5))
		if err := os.WriteFile(name, []byte("planted\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		planted = append(planted, name)
	}
	rec, err := NewFileRecorder(path, io.Discard, WithoutStartupSweep())
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close() //nolint:errcheck // test cleanup

	if _, err := rec.ForceRotate(); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("ForceRotate onto an existing rotating name = %v, want fs.ErrExist", err)
	}
	for _, name := range planted {
		if data, err := os.ReadFile(name); err != nil || string(data) != "planted\n" {
			t.Errorf("%s = %q, %v; want it untouched", filepath.Base(name), data, err)
		}
	}
	if err := rec.RecordAck(Event{Type: BeadCreated, Actor: "test", Subject: "after"}); err != nil {
		t.Fatal(err)
	}
	active, err := ReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 6 || active[5].Seq != 6 {
		t.Fatalf("active log holds %d events, want the 5 seeds and the next at seq 6", len(active))
	}
}

// TestRenameIfAbsentRefusesExistingTarget covers the portable fallback.
func TestRenameIfAbsentRefusesExistingTarget(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	for _, f := range []string{src, dst} {
		if err := os.WriteFile(f, []byte(filepath.Base(f)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := renameIfAbsent(src, dst); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("renameIfAbsent onto an existing file = %v, want fs.ErrExist", err)
	}
	if data, _ := os.ReadFile(dst); string(data) != "dst" {
		t.Fatalf("dst = %q, want it untouched", data)
	}
	if err := os.Remove(dst); err != nil {
		t.Fatal(err)
	}
	if err := renameIfAbsent(src, dst); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(dst); string(data) != "src" {
		t.Fatalf("dst = %q, want the renamed source", data)
	}
}
