package events

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
)

// mirrorTestDirs returns a canonical log path under a city-like .gc directory
// and a separate, already provisioned mirror directory.
func mirrorTestDirs(t *testing.T) (canonical, mirrorDir string) {
	t.Helper()
	root := t.TempDir()
	canonical = filepath.Join(root, "city", ".gc", "events.jsonl")
	mirrorDir = filepath.Join(root, "log")
	if err := os.MkdirAll(mirrorDir, 0o750); err != nil {
		t.Fatal(err)
	}
	return canonical, mirrorDir
}

// fastMirror returns mirror options that retry quickly. Its StopTimeout is a
// safety bound only: Close waits that long solely for a copier that catches
// up or exits (a test stalls only if one never does).
func fastMirror(path string) MirrorOptions {
	return MirrorOptions{Path: path, RetryInterval: 20 * time.Millisecond, LogInterval: time.Hour, StopTimeout: 15 * time.Second}
}

// plainRecordN records n events onto the log at canonical through a
// short-lived writer that never mirrors, as a CLI process does.
func plainRecordN(t *testing.T, canonical string, n int) {
	t.Helper()
	plain, err := NewFileRecorder(canonical, io.Discard, WithMaxSize(0), WithoutStartupSweep())
	if err != nil {
		t.Fatal(err)
	}
	recordMirrorN(t, plain, 1, n)
	if err := plain.Close(); err != nil {
		t.Fatal(err)
	}
}

// fileLines returns the lines of path, without their newlines.
func fileLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

func recordMirrorN(t *testing.T, r *FileRecorder, from, n int) {
	t.Helper()
	for i := from; i < from+n; i++ {
		err := r.RecordAck(Event{
			Type:    "test.mirror",
			Actor:   "tester",
			Subject: fmt.Sprintf("bead-%d", i),
			Message: "a <message> & more",
			Payload: json.RawMessage(fmt.Sprintf(`{"i": %d, "s": "<x>"}`, i)),
		})
		if err != nil {
			t.Fatalf("RecordAck(%d): %v", i, err)
		}
	}
}

// fileSeqs returns the seq of every line in path (none when it is absent).
func fileSeqs(t *testing.T, path string) []uint64 {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var seqs []uint64
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("%s: unparseable line %q: %v", path, sc.Text(), err)
		}
		seqs = append(seqs, e.Seq)
	}
	return seqs
}

// mirrorProbe records the copier's progress (MirrorOptions.Observe) so a test
// waits for the step it needs instead of for elapsed time.
type mirrorProbe struct {
	ch chan MirrorProgress
}

func newMirrorProbe() *mirrorProbe {
	return &mirrorProbe{ch: make(chan MirrorProgress, 4096)}
}

// opts returns fast-retrying mirror options that report to the probe.
func (p *mirrorProbe) opts(path string) MirrorOptions {
	o := fastMirror(path)
	o.Observe = func(step MirrorProgress) {
		select {
		case p.ch <- step:
		default: // never block the copier; the buffer outlasts any test here
		}
	}
	return o
}

// waitFor returns the first progress step that matches, failing with the last
// step seen if none arrives before the safety deadline.
func (p *mirrorProbe) waitFor(t *testing.T, desc string, match func(MirrorProgress) bool) MirrorProgress {
	t.Helper()
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	var last MirrorProgress
	for {
		select {
		case step := <-p.ch:
			if match(step) {
				return step
			}
			last = step
		case <-deadline.C:
			t.Fatalf("copier never reached %s; last step %+v", desc, last)
		}
	}
}

// waitWrote waits until the copier has appended the event with seq.
func (p *mirrorProbe) waitWrote(t *testing.T, seq uint64) {
	t.Helper()
	p.waitFor(t, fmt.Sprintf("writing seq %d", seq), func(step MirrorProgress) bool {
		return step.Kind == MirrorWrote && step.Seq == seq
	})
}

// waitKind waits for the next step of kind.
func (p *mirrorProbe) waitKind(t *testing.T, kind MirrorProgressKind) MirrorProgress {
	t.Helper()
	return p.waitFor(t, fmt.Sprintf("step kind %d", kind), func(step MirrorProgress) bool {
		return step.Kind == kind
	})
}

// drain returns the steps reported so far without waiting.
func (p *mirrorProbe) drain() []MirrorProgress {
	var steps []MirrorProgress
	for {
		select {
		case step := <-p.ch:
			steps = append(steps, step)
		default:
			return steps
		}
	}
}

func mirrorSeqRange(from, to uint64) []uint64 {
	var out []uint64
	for s := from; s <= to; s++ {
		out = append(out, s)
	}
	return out
}

func equalSeqs(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func TestMirrorUnsetLeavesRecorderUnchanged(t *testing.T) {
	for name, opts := range map[string][]FileRecorderOption{
		"no option":  nil,
		"empty path": {WithMirror(MirrorOptions{})},
	} {
		t.Run(name, func(t *testing.T) {
			canonical, mirrorDir := mirrorTestDirs(t)
			var stderr lockedBuffer
			r, err := NewFileRecorder(canonical, &stderr, opts...)
			if err != nil {
				t.Fatal(err)
			}
			if r.mirror != nil {
				t.Fatal("recorder started a mirror without a mirror path")
			}
			recordMirrorN(t, r, 1, 3)
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			if got, want := dirNames(t, filepath.Dir(canonical)), []string{"events.jsonl", "events.jsonl.lock"}; strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf(".gc holds %v, want %v", got, want)
			}
			if got := dirNames(t, mirrorDir); len(got) != 0 {
				t.Fatalf("mirror dir holds %v, want nothing", got)
			}
			if stderr.String() != "" {
				t.Fatalf("stderr = %q, want empty", stderr.String())
			}
		})
	}
}

func TestMirrorCopiesIdenticalLines(t *testing.T) {
	canonical, mirrorDir := mirrorTestDirs(t)
	mirrorPath := filepath.Join(mirrorDir, "events.jsonl")
	var stderr lockedBuffer
	probe := newMirrorProbe()
	owner, err := NewFileRecorder(canonical, &stderr, WithMirror(probe.opts(mirrorPath)))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close() //nolint:errcheck

	recordMirrorN(t, owner, 1, 4)
	// Another writer on the same log (a CLI process) is mirrored too.
	secondary, err := NewFileRecorder(canonical, io.Discard, WithMaxSize(0), WithoutStartupSweep())
	if err != nil {
		t.Fatal(err)
	}
	recordMirrorN(t, secondary, 5, 2)
	_ = secondary.Close()
	if err := owner.AppendBatch([]Event{{Type: "test.batch", Actor: "tester"}, {Type: "test.batch", Actor: "tester"}}); err != nil {
		t.Fatal(err)
	}

	probe.waitWrote(t, 8)
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(canonical)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(mirrorPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("mirror differs from the canonical log\nmirror:\n%s\ncanonical:\n%s", got, want)
	}
	if stderr.String() != "" {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestMirrorFileMode(t *testing.T) {
	cases := []struct {
		name     string
		mode     os.FileMode
		preexist os.FileMode
		want     os.FileMode
	}{
		{name: "default", want: 0o640},
		{name: "configured", mode: 0o600, want: 0o600},
		{name: "existing file is set to the mode", preexist: 0o666, want: 0o640},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			canonical, mirrorDir := mirrorTestDirs(t)
			mirrorPath := filepath.Join(mirrorDir, "events.jsonl")
			if tc.preexist != 0 {
				if err := os.WriteFile(mirrorPath, nil, tc.preexist); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(mirrorPath, tc.preexist); err != nil {
					t.Fatal(err)
				}
			}
			probe := newMirrorProbe()
			opts := probe.opts(mirrorPath)
			opts.Mode = tc.mode
			r, err := NewFileRecorder(canonical, io.Discard, WithMirror(opts))
			if err != nil {
				t.Fatal(err)
			}
			recordMirrorN(t, r, 1, 1)
			probe.waitWrote(t, 1)
			_ = r.Close()
			info, err := os.Stat(mirrorPath)
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != tc.want {
				t.Fatalf("mirror mode = %#o, want %#o", got, tc.want)
			}
			dirInfo, err := os.Stat(mirrorDir)
			if err != nil {
				t.Fatal(err)
			}
			if got := dirInfo.Mode().Perm(); got != 0o750 {
				t.Fatalf("mirror dir mode = %#o, want it untouched at 0750", got)
			}
		})
	}
}

func TestMirrorMissingDirDoesNotBreakCanonicalLog(t *testing.T) {
	canonical, mirrorDir := mirrorTestDirs(t)
	missingDir := filepath.Join(mirrorDir, "not-provisioned")
	mirrorPath := filepath.Join(missingDir, "events.jsonl")
	var stderr lockedBuffer
	probe := newMirrorProbe()
	r, err := NewFileRecorder(canonical, &stderr, WithMirror(probe.opts(mirrorPath)))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close() //nolint:errcheck

	recordMirrorN(t, r, 1, 3)
	if got := fileSeqs(t, canonical); !equalSeqs(got, mirrorSeqRange(1, 3)) {
		t.Fatalf("canonical seqs = %v, want 1..3", got)
	}
	// The copier fails and retries; five failures are well past the first.
	for i := 0; i < 5; i++ {
		probe.waitKind(t, MirrorFailed)
	}
	if _, err := os.Stat(missingDir); !os.IsNotExist(err) {
		t.Fatalf("mirror dir was created (stat err %v); the operator provisions it", err)
	}
	if n := strings.Count(stderr.String(), "events: mirror "); n != 1 {
		t.Fatalf("stderr reported the failure %d times, want once (rate-limited):\n%s", n, stderr.String())
	}

	// Once the operator provisions the directory, mirroring resumes, and
	// nothing recorded since the recorder opened the log is lost, including
	// the events written while the directory was missing.
	if err := os.Mkdir(missingDir, 0o750); err != nil {
		t.Fatal(err)
	}
	probe.waitWrote(t, 3)
	recordMirrorN(t, r, 4, 1)
	probe.waitWrote(t, 4)
	if got := fileSeqs(t, mirrorPath); !equalSeqs(got, mirrorSeqRange(1, 4)) {
		t.Fatalf("mirror seqs = %v, want 1..4", got)
	}
	if !strings.Contains(stderr.String(), "recovered") {
		t.Fatalf("stderr does not report the recovery:\n%s", stderr.String())
	}
}

func TestMirrorRotatesAtMaxSize(t *testing.T) {
	canonical, mirrorDir := mirrorTestDirs(t)
	mirrorPath := filepath.Join(mirrorDir, "events.jsonl")
	probe := newMirrorProbe()
	opts := probe.opts(mirrorPath)
	opts.MaxSize = 1024
	r, err := NewFileRecorder(canonical, io.Discard, WithMirror(opts))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close() //nolint:errcheck

	recordMirrorN(t, r, 1, 40)
	probe.waitWrote(t, 40)
	_ = r.Close()

	for _, p := range []string{mirrorPath, mirrorPath + ".1"} {
		info, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if info.Size() > opts.MaxSize {
			t.Fatalf("%s is %d bytes, over MaxSize %d", p, info.Size(), opts.MaxSize)
		}
		if got := info.Mode().Perm(); got != DefaultMirrorMode {
			t.Fatalf("%s mode = %#o, want %#o", p, got, DefaultMirrorMode)
		}
	}
	// The two files are a contiguous run ending at the newest event.
	all := append(fileSeqs(t, mirrorPath+".1"), fileSeqs(t, mirrorPath)...)
	if len(all) == 0 || !equalSeqs(all, mirrorSeqRange(all[0], 40)) {
		t.Fatalf("mirror .1 + active seqs = %v, want a contiguous run ending at 40", all)
	}
}

func TestMirrorReopensAfterExternalRotation(t *testing.T) {
	canonical, mirrorDir := mirrorTestDirs(t)
	mirrorPath := filepath.Join(mirrorDir, "events.jsonl")
	probe := newMirrorProbe()
	r, err := NewFileRecorder(canonical, io.Discard, WithMirror(probe.opts(mirrorPath)))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close() //nolint:errcheck

	recordMirrorN(t, r, 1, 3)
	probe.waitWrote(t, 3)

	// logrotate "create": rename the file away.
	rotated := filepath.Join(mirrorDir, "events.jsonl.old")
	if err := os.Rename(mirrorPath, rotated); err != nil {
		t.Fatal(err)
	}
	recordMirrorN(t, r, 4, 2)
	probe.waitWrote(t, 5)
	if got := fileSeqs(t, rotated); !equalSeqs(got, mirrorSeqRange(1, 3)) {
		t.Fatalf("renamed mirror seqs = %v, want 1..3", got)
	}
	if got := fileSeqs(t, mirrorPath); !equalSeqs(got, mirrorSeqRange(4, 5)) {
		t.Fatalf("reopened mirror seqs = %v, want 4..5", got)
	}

	// logrotate "copytruncate": truncate in place.
	if err := os.Truncate(mirrorPath, 0); err != nil {
		t.Fatal(err)
	}
	recordMirrorN(t, r, 6, 1)
	probe.waitWrote(t, 6)
	data, err := os.ReadFile(mirrorPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.IndexByte(data, 0) >= 0 {
		t.Fatalf("truncated mirror has a NUL-filled hole: %q", data)
	}
	if got := fileSeqs(t, mirrorPath); !equalSeqs(got, []uint64{6}) {
		t.Fatalf("truncated mirror seqs = %v, want [6]", got)
	}
}

func TestMirrorResumesWithoutGapsOrDuplicates(t *testing.T) {
	canonical, mirrorDir := mirrorTestDirs(t)
	mirrorPath := filepath.Join(mirrorDir, "events.jsonl")
	probe := newMirrorProbe()
	r, err := NewFileRecorder(canonical, io.Discard, WithMirror(probe.opts(mirrorPath)))
	if err != nil {
		t.Fatal(err)
	}
	recordMirrorN(t, r, 1, 3)
	probe.waitWrote(t, 3)
	_ = r.Close()

	// Events written while no copier runs (controller down).
	plain, err := NewFileRecorder(canonical, io.Discard, WithMaxSize(0), WithoutStartupSweep())
	if err != nil {
		t.Fatal(err)
	}
	recordMirrorN(t, plain, 4, 2)
	_ = plain.Close()

	r, err = NewFileRecorder(canonical, io.Discard, WithMirror(probe.opts(mirrorPath)))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close() //nolint:errcheck
	if got := probe.waitKind(t, MirrorWatching); got.Seq != 3 {
		t.Fatalf("restarted copier resumed after seq %d, want 3 (the mirror's last)", got.Seq)
	}
	recordMirrorN(t, r, 6, 1)
	probe.waitWrote(t, 6)
	if got := fileSeqs(t, mirrorPath); !equalSeqs(got, mirrorSeqRange(1, 6)) {
		t.Fatalf("mirror seqs after restart = %v, want 1..6 once each", got)
	}
}

func TestMirrorSecondCopierDoesNotDuplicate(t *testing.T) {
	canonical, mirrorDir := mirrorTestDirs(t)
	mirrorPath := filepath.Join(mirrorDir, "events.jsonl")
	var stderrB lockedBuffer
	probeA, probeB := newMirrorProbe(), newMirrorProbe()
	a, err := NewFileRecorder(canonical, io.Discard, WithMirror(probeA.opts(mirrorPath)))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close() //nolint:errcheck
	recordMirrorN(t, a, 1, 1)
	probeA.waitWrote(t, 1)

	b, err := NewFileRecorder(canonical, &stderrB, WithMaxSize(0), WithoutStartupSweep(), WithMirror(probeB.opts(mirrorPath)))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close() //nolint:errcheck
	// B's copier is refused the lock while A holds it.
	if got := probeB.waitKind(t, MirrorFailed); !strings.Contains(got.Err.Error(), "another process is mirroring") {
		t.Fatalf("second copier failed with %v, want the lock refusal", got.Err)
	}
	recordMirrorN(t, b, 2, 2)
	recordMirrorN(t, a, 4, 2)
	probeA.waitWrote(t, 5)
	for _, step := range probeB.drain() {
		if step.Kind != MirrorFailed {
			t.Fatalf("second copier made progress while the first held the lock: %+v", step)
		}
	}

	// When A's copier stops, B's takes over from the mirror's last event.
	_ = a.Close()
	if got := probeB.waitKind(t, MirrorWatching); got.Seq != 5 {
		t.Fatalf("second copier resumed after seq %d, want 5", got.Seq)
	}
	recordMirrorN(t, b, 6, 1)
	probeB.waitWrote(t, 6)
	if got := fileSeqs(t, mirrorPath); !equalSeqs(got, mirrorSeqRange(1, 6)) {
		t.Fatalf("mirror seqs = %v, want 1..6 once each", got)
	}
	if !strings.Contains(stderrB.String(), "another process is mirroring") {
		t.Fatalf("second copier stderr = %q, want a note that it waits", stderrB.String())
	}
	// The wait's report is not left as the copier's last word.
	if !strings.Contains(stderrB.String(), "took the mirror lock") {
		t.Fatalf("second copier stderr = %q, want a note that it took over", stderrB.String())
	}
}

func TestMirrorRejectsUnsafePaths(t *testing.T) {
	canonical, _ := mirrorTestDirs(t)
	for name, path := range map[string]string{
		"relative":            "log/events.jsonl",
		"canonical directory": filepath.Join(filepath.Dir(canonical), "mirror.jsonl"),
		"canonical file":      canonical,
		// Deeper under .gc, which the shipper can never read.
		"under the canonical directory": filepath.Join(filepath.Dir(canonical), "export", "events.jsonl"),
		"the canonical directory":       filepath.Dir(canonical),
	} {
		t.Run(name, func(t *testing.T) {
			var stderr lockedBuffer
			r, err := NewFileRecorder(canonical, &stderr, WithMirror(fastMirror(path)))
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close() //nolint:errcheck
			if r.mirror != nil {
				t.Fatalf("mirror started for %q", path)
			}
			if !strings.Contains(stderr.String(), "events: mirror disabled") {
				t.Fatalf("stderr = %q, want a mirror-disabled warning", stderr.String())
			}
		})
	}
}

func TestLastSeqInFileSkipsTornTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.jsonl")
	if err := os.WriteFile(path, []byte("{\"seq\":7,\"type\":\"a\"}\n{\"seq\":8,\"ty"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := lastSeqInFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != 7 {
		t.Fatalf("lastSeqInFile = %d, want 7", got)
	}
	if got, err := lastSeqInFile(filepath.Join(t.TempDir(), "absent")); err != nil || got != 0 {
		t.Fatalf("lastSeqInFile(absent) = %d, %v; want 0, nil", got, err)
	}
}

// TestMirrorCloseCopiesTheTail pins that Close lets the copier catch up: the
// events recorded just before Close (a run's last ones, controller.stopped
// among them) reach the mirror in that run, whether the copier had not begun
// tailing yet or was waiting between two polls of the canonical log.
func TestMirrorCloseCopiesTheTail(t *testing.T) {
	for name, betweenPolls := range map[string]bool{
		"copier still starting": false,
		"copier between polls":  true,
	} {
		t.Run(name, func(t *testing.T) {
			canonical, mirrorDir := mirrorTestDirs(t)
			mirrorPath := filepath.Join(mirrorDir, "events.jsonl")
			probe := newMirrorProbe()
			r, err := NewFileRecorder(canonical, io.Discard, WithMirror(probe.opts(mirrorPath)))
			if err != nil {
				t.Fatal(err)
			}
			if betweenPolls {
				recordMirrorN(t, r, 1, 2)
				probe.waitWrote(t, 2)
				recordMirrorN(t, r, 3, 1)
			} else {
				recordMirrorN(t, r, 1, 3)
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			if got := fileSeqs(t, mirrorPath); !equalSeqs(got, mirrorSeqRange(1, 3)) || !equalSeqs(got, fileSeqs(t, canonical)) {
				t.Fatalf("mirror seqs right after Close = %v, want the canonical log's 1..3", got)
			}
		})
	}
}

// TestMirrorCloseCopiesAnUnanchoredRotation pins the stop drain across a
// rotation that renamed the canonical log and then failed to write its anchor:
// the events the copier lacks sit only in the rotating-* file and the active
// log is empty, and Close must still let the copier copy them. It runs in a
// synctest bubble so the copier lags deterministically: it sleeps until its
// next poll of the log, and the bubble's clock stands still while the test
// records, rotates and calls Close.
func TestMirrorCloseCopiesAnUnanchoredRotation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		canonical, mirrorDir := mirrorTestDirs(t)
		mirrorPath := filepath.Join(mirrorDir, "events.jsonl")
		probe := newMirrorProbe()
		r, err := NewFileRecorder(canonical, io.Discard, WithMirror(probe.opts(mirrorPath)))
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close() //nolint:errcheck // stops the copier if the test fails early
		recordMirrorN(t, r, 1, 1)
		probe.waitWrote(t, 1)
		synctest.Wait() // the copier sleeps until its next poll
		recordMirrorN(t, r, 2, 2)

		// The rotation renames the log and reopens it, but the fresh log's
		// handle cannot write, so the anchor fails as it would on a full disk.
		restore := openEventLog
		stubOpenEventLog(t, func(path string) (*os.File, error) {
			openEventLog = restore
			return os.OpenFile(path, os.O_CREATE|os.O_RDONLY, 0o644)
		})
		if _, err := r.ForceRotate(); err == nil || !strings.Contains(err.Error(), "anchor") {
			t.Fatalf("ForceRotate = %v, want its anchor write to fail", err)
		}

		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
		if got := fileSeqs(t, mirrorPath); !equalSeqs(got, mirrorSeqRange(1, 3)) {
			t.Fatalf("mirror seqs right after Close = %v, want 1..3, the last two copied from the rotating file", got)
		}
	})
}

// TestMirrorCloseIsBoundedWhileTheCopierIsBlocked pins that Close does not
// wait without bound for a copier stuck in a system call (a write to a hung
// network filesystem): it gives up on the catch-up and then on the exit after
// StopTimeout each, says so, and returns.
func TestMirrorCloseIsBoundedWhileTheCopierIsBlocked(t *testing.T) {
	canonical, mirrorDir := mirrorTestDirs(t)
	var stderr lockedBuffer
	blocked, release, stopped := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var block sync.Once
	opts := fastMirror(filepath.Join(mirrorDir, "events.jsonl"))
	opts.StopTimeout = 50 * time.Millisecond
	opts.Observe = func(step MirrorProgress) {
		switch step.Kind {
		case MirrorWatching:
			// Stands in for a system call that does not return: the copier
			// holds the mirror lock and makes no progress until released.
			block.Do(func() {
				close(blocked)
				<-release
			})
		case MirrorStopped:
			close(stopped)
		}
	}
	r, err := NewFileRecorder(canonical, &stderr, WithMirror(opts))
	if err != nil {
		t.Fatal(err)
	}
	waitClosed(t, blocked, "the tailing copier's signal")
	recordMirrorN(t, r, 1, 1)

	closed := make(chan error, 1)
	go func() { closed <- r.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Close is still waiting on a blocked copier")
	}
	for _, want := range []string{"stopping before it copied seq 1", "still blocked"} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr = %q, want %q", stderr.String(), want)
		}
	}
	// The copier exits on its own once its system call returns.
	close(release)
	waitClosed(t, stopped, "the released copier's exit signal")
}

// TestMirrorRefusesUnsafeFiles pins that the copier writes only a regular
// file with no other name. Neither a symbolic link nor a hard link at the
// mirror path lets whoever can write the mirror's directory have gc chmod and
// append to another file, the canonical log included; a FIFO blocks neither
// the copier nor Close. Each is a failure the copier reports and retries, and
// nothing is removed.
func TestMirrorRefusesUnsafeFiles(t *testing.T) {
	for _, tc := range []struct {
		name    string
		place   func(canonical, victim, mirrorPath string) error
		wantErr string
	}{
		{
			name:    "symlink to the canonical log",
			place:   func(canonical, _, mirrorPath string) error { return os.Symlink(canonical, mirrorPath) },
			wantErr: "symbolic link",
		},
		{
			name:    "symlink to another file",
			place:   func(_, victim, mirrorPath string) error { return os.Symlink(victim, mirrorPath) },
			wantErr: "symbolic link",
		},
		{
			name:    "hard link to the canonical log",
			place:   func(canonical, _, mirrorPath string) error { return os.Link(canonical, mirrorPath) },
			wantErr: "canonical event log",
		},
		{
			name:    "hard link to another file",
			place:   func(_, victim, mirrorPath string) error { return os.Link(victim, mirrorPath) },
			wantErr: "hard links",
		},
		{
			name:    "FIFO",
			place:   func(_, _, mirrorPath string) error { return syscall.Mkfifo(mirrorPath, 0o600) },
			wantErr: "not a regular file",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			canonical, mirrorDir := mirrorTestDirs(t)
			mirrorPath := filepath.Join(mirrorDir, "events.jsonl")
			plainRecordN(t, canonical, 2)
			// A file of the city's user that the mirror must never touch.
			victim := filepath.Join(t.TempDir(), "secret")
			if err := os.WriteFile(victim, []byte("do not touch\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := tc.place(canonical, victim, mirrorPath); err != nil {
				t.Fatal(err)
			}
			canonBefore, err := os.Stat(canonical)
			if err != nil {
				t.Fatal(err)
			}

			probe := newMirrorProbe()
			r, err := NewFileRecorder(canonical, io.Discard, WithMirror(probe.opts(mirrorPath)))
			if err != nil {
				t.Fatal(err)
			}
			recordMirrorN(t, r, 3, 1)
			if got := probe.waitKind(t, MirrorFailed); !strings.Contains(got.Err.Error(), tc.wantErr) {
				t.Fatalf("copier failed with %v, want an error naming %q", got.Err, tc.wantErr)
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}

			if got := fileSeqs(t, canonical); !equalSeqs(got, mirrorSeqRange(1, 3)) {
				t.Fatalf("canonical seqs = %v, want 1..3 once each", got)
			}
			canonAfter, err := os.Stat(canonical)
			if err != nil {
				t.Fatal(err)
			}
			if canonAfter.Mode() != canonBefore.Mode() {
				t.Fatalf("canonical log mode = %v, want it unchanged at %v", canonAfter.Mode(), canonBefore.Mode())
			}
			if data, err := os.ReadFile(victim); err != nil || string(data) != "do not touch\n" {
				t.Fatalf("victim file = %q (err %v), want it untouched", data, err)
			}
			victimInfo, err := os.Stat(victim)
			if err != nil {
				t.Fatal(err)
			}
			if victimInfo.Mode().Perm() != 0o600 {
				t.Fatalf("victim mode = %v, want 0600", victimInfo.Mode())
			}
			if _, err := os.Lstat(mirrorPath); err != nil {
				t.Fatalf("what was at the mirror path is gone: %v", err)
			}
		})
	}
}

// TestMirrorResumesFromTheRotatedFile pins the resume fallback: a restart that
// finds the mirror empty, because it was just rotated, continues after the
// last event in <path>.1 instead of at the canonical log's tail.
func TestMirrorResumesFromTheRotatedFile(t *testing.T) {
	canonical, mirrorDir := mirrorTestDirs(t)
	mirrorPath := filepath.Join(mirrorDir, "events.jsonl")
	plainRecordN(t, canonical, 5)
	// The previous run mirrored 1..3, then the mirror was rotated.
	if err := os.WriteFile(mirrorPath+".1", []byte(strings.Join(fileLines(t, canonical)[:3], "\n")+"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mirrorPath, nil, 0o640); err != nil {
		t.Fatal(err)
	}

	probe := newMirrorProbe()
	r, err := NewFileRecorder(canonical, io.Discard, WithMirror(probe.opts(mirrorPath)))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close() //nolint:errcheck
	if got := probe.waitKind(t, MirrorWatching); got.Seq != 3 {
		t.Fatalf("copier resumed after seq %d, want 3 (the last in %s.1)", got.Seq, mirrorPath)
	}
	probe.waitWrote(t, 5)
	if got := fileSeqs(t, mirrorPath); !equalSeqs(got, mirrorSeqRange(4, 5)) {
		t.Fatalf("mirror seqs = %v, want 4..5", got)
	}
}

// TestMirrorFollowsARestartedCanonicalLog pins the resume cap: when the
// canonical log restarted its seqs below the mirror's last (it was removed and
// recreated), the copier follows it from its tail instead of waiting for a seq
// past the mirror's.
func TestMirrorFollowsARestartedCanonicalLog(t *testing.T) {
	canonical, mirrorDir := mirrorTestDirs(t)
	mirrorPath := filepath.Join(mirrorDir, "events.jsonl")
	plainRecordN(t, canonical, 5)
	data, err := os.ReadFile(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mirrorPath, data, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(canonical); err != nil {
		t.Fatal(err)
	}
	plainRecordN(t, canonical, 2)
	if got := fileSeqs(t, canonical); !equalSeqs(got, mirrorSeqRange(1, 2)) {
		t.Fatalf("recreated canonical seqs = %v, want them restarted at 1..2", got)
	}

	probe := newMirrorProbe()
	r, err := NewFileRecorder(canonical, io.Discard, WithMirror(probe.opts(mirrorPath)))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close() //nolint:errcheck
	if got := probe.waitKind(t, MirrorWatching); got.Seq != 2 {
		t.Fatalf("copier resumed after seq %d, want 2 (the canonical tail)", got.Seq)
	}
	recordMirrorN(t, r, 3, 1)
	probe.waitWrote(t, 3)
	if got := fileSeqs(t, mirrorPath); !equalSeqs(got, append(mirrorSeqRange(1, 5), 3)) {
		t.Fatalf("mirror seqs = %v, want 1..5 then the restarted log's 3", got)
	}
}

// TestMirrorResumesInsideAnUnanchoredRotation pins the resume point while a
// rotation's window sits only in its rotating-* file, the canonical log
// renamed and its anchor never written. A copier that starts then, as one
// retrying after a failure does, continues after the mirror's last event: it
// neither copies the window again nor skips the part of it the mirror lacks.
func TestMirrorResumesInsideAnUnanchoredRotation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		holds int // the mirror already holds seqs 1..holds
	}{
		{name: "mirror holds the window", holds: 3},
		{name: "mirror lags the window", holds: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			canonical, mirrorDir := mirrorTestDirs(t)
			mirrorPath := filepath.Join(mirrorDir, "events.jsonl")
			plainRecordN(t, canonical, 3)
			lines := fileLines(t, canonical)
			if err := os.WriteFile(mirrorPath, []byte(strings.Join(lines[:tc.holds], "\n")+"\n"), 0o640); err != nil {
				t.Fatal(err)
			}
			renameWithoutAnchor(t, canonical, 1, 3, true)

			// No startup sweep: it would archive the rotating file, as the
			// owner's restart does, and so close the window.
			probe := newMirrorProbe()
			r, err := NewFileRecorder(canonical, io.Discard, WithMaxSize(0), WithoutStartupSweep(), WithMirror(probe.opts(mirrorPath)))
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close() //nolint:errcheck
			if got := probe.waitKind(t, MirrorWatching); got.Seq != uint64(tc.holds) {
				t.Fatalf("copier resumed after seq %d, want %d (the mirror's last)", got.Seq, tc.holds)
			}
			recordMirrorN(t, r, 4, 1)
			probe.waitWrote(t, 4)
			if got := fileSeqs(t, mirrorPath); !equalSeqs(got, mirrorSeqRange(1, 4)) {
				t.Fatalf("mirror seqs = %v, want 1..4 once each", got)
			}
		})
	}
}

// TestMirrorTerminatesATornLine pins that a mirror whose last line was cut
// short (a write interrupted by a crash) gets a newline before the next event,
// so that event lands on its own parseable line.
func TestMirrorTerminatesATornLine(t *testing.T) {
	canonical, mirrorDir := mirrorTestDirs(t)
	mirrorPath := filepath.Join(mirrorDir, "events.jsonl")
	plainRecordN(t, canonical, 3)
	lines := fileLines(t, canonical)
	torn := lines[1][:len(lines[1])/2]
	if err := os.WriteFile(mirrorPath, []byte(lines[0]+"\n"+torn), 0o640); err != nil {
		t.Fatal(err)
	}

	probe := newMirrorProbe()
	r, err := NewFileRecorder(canonical, io.Discard, WithMirror(probe.opts(mirrorPath)))
	if err != nil {
		t.Fatal(err)
	}
	if got := probe.waitKind(t, MirrorWatching); got.Seq != 1 {
		t.Fatalf("copier resumed after seq %d, want 1 (the last complete line)", got.Seq)
	}
	probe.waitWrote(t, 3)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	want := []string{lines[0], torn, lines[1], lines[2]}
	if got := fileLines(t, mirrorPath); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("mirror lines =\n%s\nwant the torn line closed and each event on its own line:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestMirrorWriteFailureIsReportedOnce pins the failure report for a mirror
// that opens but cannot be written (here its rotation fails, because a
// directory stands at <path>.1; a full disk behaves the same): it is reported
// once per LogInterval with no false "recovered" at each retry's open, and
// "recovered" comes once a write succeeds.
func TestMirrorWriteFailureIsReportedOnce(t *testing.T) {
	canonical, mirrorDir := mirrorTestDirs(t)
	mirrorPath := filepath.Join(mirrorDir, "events.jsonl")
	plainRecordN(t, canonical, 2)
	// The mirror holds seq 1, so the copier resumes from it and its next
	// write, of seq 2, must rotate the mirror first.
	if err := os.WriteFile(mirrorPath, []byte(fileLines(t, canonical)[0]+"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(mirrorPath+".1", 0o750); err != nil {
		t.Fatal(err)
	}
	var stderr lockedBuffer
	probe := newMirrorProbe()
	opts := probe.opts(mirrorPath)
	opts.MaxSize = 1
	r, err := NewFileRecorder(canonical, &stderr, WithMirror(opts))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close() //nolint:errcheck

	// Each retry opens the mirror and watches the log, then fails to write.
	for i := 0; i < 5; i++ {
		if got := probe.waitKind(t, MirrorFailed); !strings.Contains(got.Err.Error(), "rotating") {
			t.Fatalf("copier failed with %v, want the rotation's failure", got.Err)
		}
	}
	if n := strings.Count(stderr.String(), "retrying every"); n != 1 {
		t.Fatalf("stderr reported the failure %d times, want once (rate-limited):\n%s", n, stderr.String())
	}
	if strings.Contains(stderr.String(), "recovered") {
		t.Fatalf("stderr reports a recovery while every write fails:\n%s", stderr.String())
	}

	if err := os.Remove(mirrorPath + ".1"); err != nil {
		t.Fatal(err)
	}
	probe.waitWrote(t, 2)
	if n := strings.Count(stderr.String(), "recovered"); n != 1 {
		t.Fatalf("stderr reported the recovery %d times, want once:\n%s", n, stderr.String())
	}
}

// TestMirrorChmodWarningIsRateLimited pins that a chmod the copier cannot make
// (a pre-created file another user owns) is warned about once per
// LogInterval, not at every reopen.
func TestMirrorChmodWarningIsRateLimited(t *testing.T) {
	var stderr lockedBuffer
	m := &eventMirror{opts: MirrorOptions{Path: "/var/log/gascity/hq/events.jsonl", Mode: 0o640, LogInterval: time.Hour}, stderr: &stderr}
	for i := 0; i < 3; i++ {
		m.warnChmod(os.ErrPermission)
	}
	if n := strings.Count(stderr.String(), "chmod 0640"); n != 1 {
		t.Fatalf("stderr warned about the chmod %d times, want once:\n%s", n, stderr.String())
	}
}
