package proctable

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// entryErrors builds per-entry failures for pids the way a scan does: one
// errors.Join per entry, so the tree nests. reason renders one pid's failure.
func entryErrors(pids []int, reason func(pid int) error) error {
	var scanErr error
	for _, pid := range pids {
		scanErr = errors.Join(scanErr, &EntryError{PID: pid, Err: reason(pid)})
	}
	return scanErr
}

// firstPIDs returns n consecutive pids starting at 10.
func firstPIDs(n int) []int {
	pids := make([]int, 0, n)
	for pid := 10; pid < 10+n; pid++ {
		pids = append(pids, pid)
	}
	return pids
}

// A real environ failure is itself a multi-line join, whose numbers vary per
// pid.
func environDenied(pid int) error {
	return errors.Join(fmt.Errorf("proving age for pid %d: stat unreadable", pid),
		fmt.Errorf("reading environ for pid %d: open /proc/%d/environ: permission denied", pid, pid))
}

func processVanished(pid int) error {
	return fmt.Errorf("checking root for pid %d: open /proc/%d/stat: no such process", pid, pid)
}

// The orphan sweep logs this summary every patrol tick; on a host with dozens
// of unreadable same-uid entries the raw joined error was one line per entry.
func TestSummarizeScanErrorIsBounded(t *testing.T) {
	small := SummarizeScanError(fmt.Errorf("tmux backend: %w", entryErrors(firstPIDs(70), environDenied)))
	large := SummarizeScanError(fmt.Errorf("tmux backend: %w", entryErrors(firstPIDs(5000), environDenied)))
	if strings.Contains(large, "\n") {
		t.Fatalf("summary spans lines: %q", large)
	}
	if len(large) > len(small)+8 {
		t.Fatalf("summary grows with the entry count: %d bytes for 70, %d for 5000", len(small), len(large))
	}
	want := `5000 unreadable process entries in 1 classes: 5000 like "proving age for pid 10: stat unreadable; reading environ for pid 10: open /proc/10/environ: permission denied" (pids 10, 11, 12, ...)`
	if large != want {
		t.Fatalf("summary = %q, want %q", large, want)
	}

	// Classes are capped too: fifty kinds of failure name four and count the rest.
	var many error
	for i := 0; i < 50; i++ {
		kind := strings.Repeat("x", i+1)
		many = errors.Join(many, &EntryError{PID: 100 + i, Err: fmt.Errorf("reading %s for pid %d: denied", kind, 100+i)})
	}
	got := SummarizeScanError(many)
	if !strings.HasPrefix(got, "50 unreadable process entries in 50 classes: ") || !strings.HasSuffix(got, "; 46 more classes") {
		t.Fatalf("summary of fifty classes = %q, want four described and 46 counted", got)
	}
}

// Each class of entry failure gets its own count and example, so a new class
// shows up even when the total entry count is unchanged.
func TestSummarizeScanErrorShowsEachClass(t *testing.T) {
	before := SummarizeScanError(entryErrors(firstPIDs(70), environDenied))
	after := SummarizeScanError(errors.Join(
		entryErrors(firstPIDs(69), environDenied),
		entryErrors([]int{500}, processVanished),
	))
	if !strings.HasPrefix(before, "70 unreadable process entries in 1 classes: ") {
		t.Fatalf("summary before = %q", before)
	}
	for _, want := range []string{
		"70 unreadable process entries in 2 classes: ",
		`69 like "proving age for pid 10: stat unreadable; reading environ for pid 10: open /proc/10/environ: permission denied" (pids 10, 11, 12, ...)`,
		`1 like "checking root for pid 500: open /proc/500/stat: no such process" (pids 500)`,
	} {
		if !strings.Contains(after, want) {
			t.Errorf("summary after a new class = %q, want it to contain %q", after, want)
		}
	}
}

// A composite provider's backends scan the same /proc, so one unreadable pid
// arrives once per backend; it is still one entry.
func TestSummarizeScanErrorCountsEachPIDOnce(t *testing.T) {
	pids := firstPIDs(3)
	got := SummarizeScanError(errors.Join(
		fmt.Errorf("default backend: %w", entryErrors(pids, environDenied)),
		fmt.Errorf("acp backend: %w", entryErrors(pids, environDenied)),
	))
	if !strings.HasPrefix(got, "3 unreadable process entries in 1 classes: 3 like ") || !strings.HasSuffix(got, "(pids 10, 11, 12)") {
		t.Fatalf("summary = %q, want each pid counted once", got)
	}
}

// A failure that is not one entry's is the scan failing as a whole, so the
// summary keeps it verbatim.
func TestSummarizeScanErrorKeepsWholeScanFailures(t *testing.T) {
	listErr := errors.New("tmux list running: no tmux server running")
	enumErr := errors.New("enumerating /proc: permission denied")
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{name: "beside entry failures", err: errors.Join(entryErrors(firstPIDs(70), environDenied), listErr), want: listErr.Error()},
		{name: "alone", err: fmt.Errorf("tmux backend: %w", enumErr), want: "tmux backend: " + enumErr.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SummarizeScanError(tc.err); !strings.Contains(got, tc.want) {
				t.Errorf("summary = %q, want it to keep %q", got, tc.want)
			}
		})
	}
	if got := SummarizeScanError(nil); got != "" {
		t.Errorf("SummarizeScanError(nil) = %q, want \"\"", got)
	}
}

// TestScanErrorShapeIgnoresCountsAndPIDs (mc-ygved): scans whose failures
// differ only in counts and PIDs share a shape; a new kind, or a whole-scan
// failure, changes it.
func TestScanErrorShapeIgnoresCountsAndPIDs(t *testing.T) {
	a := errors.Join(&EntryError{PID: 11, Err: errors.New("reading environ for pid 11: permission denied")})
	b := errors.Join(&EntryError{PID: 13, Err: errors.New("reading environ for pid 13: permission denied")}, &EntryError{PID: 14, Err: errors.New("reading environ for pid 14: permission denied")})
	c := errors.Join(b, &EntryError{PID: 15, Err: errors.New("reading owner for pid 15: no such device")})
	d := errors.Join(b, errors.New("enumerating /proc: permission denied"))
	if ScanErrorShape(a) != ScanErrorShape(b) {
		t.Fatalf("shapes differ on counts and pids: %q vs %q", ScanErrorShape(a), ScanErrorShape(b))
	}
	if ScanErrorShape(b) == ScanErrorShape(c) || ScanErrorShape(b) == ScanErrorShape(d) {
		t.Fatal("a new kind, or a whole-scan failure, left the shape unchanged")
	}
}

// entryScanError is a scan error of one unreadable entry per pid, each
// failing as format (with %d for its pid) describes.
func entryScanError(format string, pids ...int) error {
	return entryErrors(pids, func(pid int) error { return fmt.Errorf(format, pid) })
}

// TestScanErrorLogPacesUnreadableEntries (mc-ygved, ga-zpcphq): a zero-value
// ScanErrorLog reports a scan of unreadable entries once per shape, again
// after ScanErrorLogRepeat or when the shape changes, per key; a failure of
// the whole scan is reported every time.
func TestScanErrorLogPacesUnreadableEntries(t *testing.T) {
	var l ScanErrorLog
	t0 := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	perm := entryScanError("reading environ for pid %d: permission denied", 11, 12)
	owner := "reading owner for pid %d: no such device"
	steps := []struct {
		name string
		key  string
		err  error
		at   time.Time
		want bool
	}{
		{"first", "/c", perm, t0, true},
		{"same shape, other pids and count", "/c", entryScanError("reading environ for pid %d: permission denied", 13, 14, 15), t0.Add(time.Minute), false},
		{"another key", "/d", perm, t0.Add(time.Minute), true},
		{"a new kind", "/c", entryScanError(owner, 16), t0.Add(2 * time.Minute), true},
		{"the new kind again", "/c", entryScanError(owner, 17), t0.Add(3 * time.Minute), false},
		{"just inside the repeat window", "/c", entryScanError(owner, 18), t0.Add(2*time.Minute + ScanErrorLogRepeat - time.Nanosecond), false},
		{"after the repeat window", "/c", entryScanError(owner, 18), t0.Add(2*time.Minute + ScanErrorLogRepeat), true},
		{"a whole-scan failure", "/c", errors.New("enumerating /proc: permission denied"), t0.Add(13 * time.Minute), true},
		{"the whole-scan failure again", "/c", errors.New("enumerating /proc: permission denied"), t0.Add(13*time.Minute + time.Second), true},
		{"a whole-scan failure joined with entries", "/c", errors.Join(errors.New("listing sessions: server gone"), perm), t0.Add(13*time.Minute + 2*time.Second), true},
	}
	for _, s := range steps {
		if got := l.Report(s.key, s.err, s.at); got != s.want {
			t.Fatalf("%s: Report = %t, want %t", s.name, got, s.want)
		}
	}
	if ScanErrorLogRepeat != 10*time.Minute {
		t.Fatalf("ScanErrorLogRepeat = %v, want ten minutes", ScanErrorLogRepeat)
	}
}

// TestScanErrorLogInstancesAreIndependent (ga-zpcphq): the orphan sweep and
// the pre-start scan each keep their own ScanErrorLog, so one's report for a
// city never suppresses the other's.
func TestScanErrorLogInstancesAreIndependent(t *testing.T) {
	var sweep, start ScanErrorLog
	t0 := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	perm := entryScanError("reading environ for pid %d: permission denied", 11)
	if !sweep.Report("/c", perm, t0) {
		t.Fatal("sweep: first Report = false, want true")
	}
	if !start.Report("/c", perm, t0.Add(time.Second)) {
		t.Fatal("start: first Report after the sweep's = false, want true")
	}
	if sweep.Report("/c", perm, t0.Add(2*time.Second)) || start.Report("/c", perm, t0.Add(2*time.Second)) {
		t.Fatal("a repeat within the window was reported, want each log to suppress its own repeat")
	}
}

// TestScanErrorLogConcurrentReports (ga-zpcphq): concurrent first reports of
// one shape on a zero-value ScanErrorLog report it exactly once.
func TestScanErrorLogConcurrentReports(t *testing.T) {
	var l ScanErrorLog
	t0 := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	perm := entryScanError("reading environ for pid %d: permission denied", 11)
	const callers = 16
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		reported int
	)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l.Report("/c", perm, t0) {
				mu.Lock()
				reported++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if reported != 1 {
		t.Fatalf("%d concurrent callers reported %d times, want once", callers, reported)
	}
}
