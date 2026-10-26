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
	"testing"
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

func fastMirror(path string) MirrorOptions {
	return MirrorOptions{Path: path, RetryInterval: 20 * time.Millisecond, LogInterval: time.Hour}
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

// waitForSeq waits until one of paths holds a line with seq want.
func waitForSeq(t *testing.T, want uint64, paths ...string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		for _, p := range paths {
			for _, s := range fileSeqs(t, p) {
				if s == want {
					return
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no line with seq %d appeared in %v", want, paths)
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
	owner, err := NewFileRecorder(canonical, &stderr, WithMirror(fastMirror(mirrorPath)))
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

	waitForSeq(t, 8, mirrorPath)
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
			opts := fastMirror(mirrorPath)
			opts.Mode = tc.mode
			r, err := NewFileRecorder(canonical, io.Discard, WithMirror(opts))
			if err != nil {
				t.Fatal(err)
			}
			recordMirrorN(t, r, 1, 1)
			waitForSeq(t, 1, mirrorPath)
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
	r, err := NewFileRecorder(canonical, &stderr, WithMirror(fastMirror(mirrorPath)))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close() //nolint:errcheck

	recordMirrorN(t, r, 1, 3)
	if got := fileSeqs(t, canonical); !equalSeqs(got, mirrorSeqRange(1, 3)) {
		t.Fatalf("canonical seqs = %v, want 1..3", got)
	}
	// Let the copier fail and retry many times.
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(missingDir); !os.IsNotExist(err) {
		t.Fatalf("mirror dir was created (stat err %v); the operator provisions it", err)
	}
	if n := strings.Count(stderr.String(), "events: mirror "); n != 1 {
		t.Fatalf("stderr reported the failure %d times, want once (rate-limited):\n%s", n, stderr.String())
	}

	// Once the operator provisions the directory, mirroring resumes.
	if err := os.Mkdir(missingDir, 0o750); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for !strings.Contains(stderr.String(), "recovered") {
		if time.Now().After(deadline) {
			t.Fatalf("stderr does not report the recovery:\n%s", stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	recordMirrorN(t, r, 4, 1)
	waitForSeq(t, 4, mirrorPath)
	// Nothing recorded since the recorder opened the log is lost, including
	// the events written while the directory was missing.
	if got := fileSeqs(t, mirrorPath); !equalSeqs(got, mirrorSeqRange(1, 4)) {
		t.Fatalf("mirror seqs = %v, want 1..4", got)
	}
}

func TestMirrorRotatesAtMaxSize(t *testing.T) {
	canonical, mirrorDir := mirrorTestDirs(t)
	mirrorPath := filepath.Join(mirrorDir, "events.jsonl")
	opts := fastMirror(mirrorPath)
	opts.MaxSize = 1024
	r, err := NewFileRecorder(canonical, io.Discard, WithMirror(opts))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close() //nolint:errcheck

	recordMirrorN(t, r, 1, 40)
	waitForSeq(t, 40, mirrorPath)
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
	r, err := NewFileRecorder(canonical, io.Discard, WithMirror(fastMirror(mirrorPath)))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close() //nolint:errcheck

	recordMirrorN(t, r, 1, 3)
	waitForSeq(t, 3, mirrorPath)

	// logrotate "create": rename the file away.
	rotated := filepath.Join(mirrorDir, "events.jsonl.old")
	if err := os.Rename(mirrorPath, rotated); err != nil {
		t.Fatal(err)
	}
	recordMirrorN(t, r, 4, 2)
	waitForSeq(t, 5, mirrorPath)
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
	waitForSeq(t, 6, mirrorPath)
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
	r, err := NewFileRecorder(canonical, io.Discard, WithMirror(fastMirror(mirrorPath)))
	if err != nil {
		t.Fatal(err)
	}
	recordMirrorN(t, r, 1, 3)
	waitForSeq(t, 3, mirrorPath)
	_ = r.Close()

	// Events written while no copier runs (controller down).
	plain, err := NewFileRecorder(canonical, io.Discard, WithMaxSize(0), WithoutStartupSweep())
	if err != nil {
		t.Fatal(err)
	}
	recordMirrorN(t, plain, 4, 2)
	_ = plain.Close()

	r, err = NewFileRecorder(canonical, io.Discard, WithMirror(fastMirror(mirrorPath)))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close() //nolint:errcheck
	recordMirrorN(t, r, 6, 1)
	waitForSeq(t, 6, mirrorPath)
	if got := fileSeqs(t, mirrorPath); !equalSeqs(got, mirrorSeqRange(1, 6)) {
		t.Fatalf("mirror seqs after restart = %v, want 1..6 once each", got)
	}
}

func TestMirrorSecondCopierDoesNotDuplicate(t *testing.T) {
	canonical, mirrorDir := mirrorTestDirs(t)
	mirrorPath := filepath.Join(mirrorDir, "events.jsonl")
	var stderrA, stderrB lockedBuffer
	a, err := NewFileRecorder(canonical, &stderrA, WithMirror(fastMirror(mirrorPath)))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close() //nolint:errcheck
	recordMirrorN(t, a, 1, 1)
	waitForSeq(t, 1, mirrorPath)
	b, err := NewFileRecorder(canonical, &stderrB, WithMaxSize(0), WithoutStartupSweep(), WithMirror(fastMirror(mirrorPath)))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close() //nolint:errcheck
	recordMirrorN(t, b, 2, 2)
	recordMirrorN(t, a, 4, 2)
	waitForSeq(t, 5, mirrorPath)
	time.Sleep(600 * time.Millisecond) // a duplicate copier would have written by now
	if got := fileSeqs(t, mirrorPath); !equalSeqs(got, mirrorSeqRange(1, 5)) {
		t.Fatalf("mirror seqs = %v, want 1..5 once each", got)
	}
	if !strings.Contains(stderrB.String(), "another process is mirroring") {
		t.Fatalf("second copier stderr = %q, want a note that it waits", stderrB.String())
	}
}

func TestMirrorRejectsUnsafePaths(t *testing.T) {
	canonical, _ := mirrorTestDirs(t)
	for name, path := range map[string]string{
		"relative":            "log/events.jsonl",
		"canonical directory": filepath.Join(filepath.Dir(canonical), "mirror.jsonl"),
		"canonical file":      canonical,
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
