package fsyncprobe

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

var probeFileName = regexp.MustCompile(`^\.gc-fsync-probe-\d+-\d+$`)

func probeFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".gc-fsync-probe-") {
			names = append(names, e.Name())
		}
	}
	return names
}

// TestMeasureFinishedReportsMeasuredTimeAndRemovesFile covers the finished
// path: the sync is timed, the probe file is a 4 KiB regular dotfile in the
// data-dir root while the sync runs, and it is gone afterwards.
func TestMeasureFinishedReportsMeasuredTimeAndRemovesFile(t *testing.T) {
	dir := t.TempDir()
	var seen []string
	var seenSize int64
	var seenRegular bool
	base := time.Unix(1_700_000_000, 0)
	ticks := []time.Time{base, base.Add(40 * time.Millisecond)}
	prev := clock
	clock = func() time.Time {
		now := ticks[0]
		if len(ticks) > 1 {
			ticks = ticks[1:]
		}
		return now
	}
	t.Cleanup(func() { clock = prev })
	res := Measure(dir, func(f *os.File) error {
		seen = probeFiles(t, dir)
		if info, err := f.Stat(); err == nil {
			seenSize = info.Size()
			seenRegular = info.Mode().IsRegular()
		}
		return nil
	})
	if res.Status != StatusFinished {
		t.Fatalf("Status = %q (error %q), want %q", res.Status, res.Error, StatusFinished)
	}
	if res.FsyncMS != 40 {
		t.Fatalf("FsyncMS = %d, want 40 (the clock advanced 40ms across the sync)", res.FsyncMS)
	}
	if len(seen) != 1 || !probeFileName.MatchString(seen[0]) {
		t.Fatalf("probe files during sync = %v, want exactly one matching %s", seen, probeFileName)
	}
	if !seenRegular || seenSize != 4096 {
		t.Fatalf("probe file during sync: regular=%v size=%d, want regular 4096 bytes", seenRegular, seenSize)
	}
	if left := probeFiles(t, dir); len(left) != 0 {
		t.Fatalf("probe files left after a finished sync: %v", left)
	}
}

// TestMeasureErrorCases covers the status=error inputs: a missing path, a
// regular file instead of a directory, and a sync that fails.
func TestMeasureErrorCases(t *testing.T) {
	notDir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notDir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		dir  string
		sync func(*os.File) error
	}{
		"missing path": {dir: filepath.Join(t.TempDir(), "absent"), sync: func(f *os.File) error { return f.Sync() }},
		"not a dir":    {dir: notDir, sync: func(f *os.File) error { return f.Sync() }},
		"sync fails":   {dir: t.TempDir(), sync: func(*os.File) error { return errors.New("injected sync failure") }},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			res := Measure(tc.dir, tc.sync)
			if res.Status != StatusError {
				t.Fatalf("Status = %q, want %q", res.Status, StatusError)
			}
			if res.Error == "" {
				t.Fatal("Error is empty; status=error must say why")
			}
		})
	}
}

// answerWith returns a starter whose "worker" writes res to the result pipe and
// exits, the way a finished RunWorker does.
func answerWith(t *testing.T, res Result) func(*os.File) error {
	t.Helper()
	return func(result *os.File) error {
		defer result.Close() //nolint:errcheck // the encode below reports failures
		return json.NewEncoder(result).Encode(res)
	}
}

// TestProbeFinished expects the worker's measurement back through the pipe.
func TestProbeFinished(t *testing.T) {
	res := probe(10*time.Second, answerWith(t, Result{Status: StatusFinished, FsyncMS: 7}))
	if res.Status != StatusFinished || res.FsyncMS != 7 {
		t.Fatalf("got %+v, want finished with fsync_ms 7", res)
	}
}

// TestProbeBoundHoldsForCaller is the proof of the binding trap: a worker that
// never answers (a D-state fsync) still holds its copy of the result pipe, and
// must not hold the caller past the bound.
func TestProbeBoundHoldsForCaller(t *testing.T) {
	bound := 300 * time.Millisecond
	start := time.Now()
	res := probe(bound, func(result *os.File) error {
		fd, err := syscall.Dup(int(result.Fd()))
		if err != nil {
			return err
		}
		held := os.NewFile(uintptr(fd), "held-result")
		t.Cleanup(func() { _ = held.Close() })
		return nil
	})
	elapsed := time.Since(start)

	if res.Status != StatusUnfinished {
		t.Fatalf("Status = %q (error %q), want %q", res.Status, res.Error, StatusUnfinished)
	}
	if res.FsyncMS != bound.Milliseconds() {
		t.Fatalf("FsyncMS = %d, want the bound %d", res.FsyncMS, bound.Milliseconds())
	}
	if elapsed < bound || elapsed > bound+2*time.Second {
		t.Fatalf("returned after %v, want within [%v, %v]", elapsed, bound, bound+2*time.Second)
	}
}

// TestProbeWorkerThatExitsWithoutAnswerIsError reports a worker that dies
// before writing as status=error, not as a stall.
func TestProbeWorkerThatExitsWithoutAnswerIsError(t *testing.T) {
	res := probe(10*time.Second, func(*os.File) error { return nil })
	if res.Status != StatusError || res.Error == "" {
		t.Fatalf("got %+v, want status=error with a message", res)
	}
}

// TestProbeSpawnFailureIsError reports a worker that cannot start as
// status=error rather than a silent success.
func TestProbeSpawnFailureIsError(t *testing.T) {
	res := probe(time.Second, func(*os.File) error { return errors.New("boom") })
	if res.Status != StatusError || res.Error == "" {
		t.Fatalf("got %+v, want status=error with a message", res)
	}
}

// TestProbeDoesNotTouchTargetDir checks that the caller leaves directory
// validation to the worker: a stat on stalled storage would block the caller
// before the bound starts, so even a missing directory goes to the worker.
func TestProbeDoesNotTouchTargetDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent")
	started := false
	answer := answerWith(t, Result{Status: StatusError, Error: "probe directory: stat " + dir + ": no such file or directory"})
	res := probe(10*time.Second, func(result *os.File) error {
		started = true
		return answer(result)
	})
	if !started {
		t.Fatal("probe did not start a worker for a missing directory; the caller must not check it")
	}
	if res.Status != StatusError || res.Error == "" {
		t.Fatalf("got %+v, want the worker's status=error", res)
	}
}

// TestProbeRejectsUnknownWorkerStatus keeps a worker from reporting a status
// of its own choosing, such as unfinished.
func TestProbeRejectsUnknownWorkerStatus(t *testing.T) {
	res := probe(10*time.Second, answerWith(t, Result{Status: StatusUnfinished, FsyncMS: 1}))
	if res.Status != StatusError {
		t.Fatalf("got %+v, want status=error for a worker-reported unfinished", res)
	}
}

// TestWorkerCommandIsDetached pins the properties the bound relies on: a new
// session, null stdio, and the result pipe as ResultFD.
func TestWorkerCommandIsDetached(t *testing.T) {
	result, err := os.CreateTemp(t.TempDir(), "result")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = result.Close() })
	cmd := workerCommand("/path/to/gc", "/data/dir", result)

	// exec numbers ExtraFiles from fd 3, and the worker opens ResultFD by
	// number: the two must be the same descriptor.
	if ResultFD != 3 {
		t.Fatalf("ResultFD = %d, want 3 (the first ExtraFiles slot)", ResultFD)
	}

	want := []string{"/path/to/gc", "dolt-state", "fsync-probe-worker", "--dir", "/data/dir"}
	if strings.Join(cmd.Args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("Args = %v, want %v", cmd.Args, want)
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Fatalf("SysProcAttr = %+v, want Setsid", cmd.SysProcAttr)
	}
	if cmd.Stdin != nil || cmd.Stdout != nil || cmd.Stderr != nil {
		t.Fatal("worker stdio must be null so no inherited pipe outlives the call")
	}
	if len(cmd.ExtraFiles) != 1 || cmd.ExtraFiles[0] != result {
		t.Fatalf("ExtraFiles = %v, want the result pipe as fd %d", cmd.ExtraFiles, ResultFD)
	}
}

// TestRunWorkerWritesJSONResult covers the worker body: it measures, writes
// one JSON Result to its descriptor and closes it.
func TestRunWorkerWritesJSONResult(t *testing.T) {
	dir := t.TempDir()
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rd.Close() })
	if err := RunWorker(dir, wr); err != nil {
		t.Fatalf("RunWorker: %v", err)
	}
	var res Result
	if err := json.NewDecoder(rd).Decode(&res); err != nil {
		t.Fatalf("decode worker output: %v", err)
	}
	if res.Status != StatusFinished || res.FsyncMS < 0 {
		t.Fatalf("got %+v, want a finished measurement", res)
	}
	if left := probeFiles(t, dir); len(left) != 0 {
		t.Fatalf("probe files left by the worker: %v", left)
	}
}
