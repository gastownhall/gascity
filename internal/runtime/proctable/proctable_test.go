package proctable

import (
	"errors"
	"fmt"
	"strings"
	"testing"
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
