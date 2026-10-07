package proctable

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// joinedEntryErrors builds n per-entry failures the way a scan does: one
// errors.Join per entry, so the tree nests, then a backend wrap on top.
func joinedEntryErrors(n int) error {
	var scanErr error
	for pid := n + 1; pid > 1; pid-- {
		// A real entry failure can itself be a multi-line join.
		readErr := errors.Join(fmt.Errorf("proving age for pid %d: stat unreadable", pid),
			fmt.Errorf("reading environ for pid %d: permission denied", pid))
		scanErr = errors.Join(scanErr, &EntryError{PID: pid, Err: readErr})
	}
	return fmt.Errorf("tmux backend: %w", scanErr)
}

// The orphan sweep logs this summary every patrol tick; on a host with dozens
// of unreadable same-uid entries the raw joined error was one line per entry.
func TestSummarizeScanErrorIsBounded(t *testing.T) {
	small, _ := SummarizeScanError(joinedEntryErrors(70))
	large, entriesOnly := SummarizeScanError(joinedEntryErrors(5000))
	if !entriesOnly {
		t.Fatalf("entriesOnly = false for per-entry failures only: %q", large)
	}
	if strings.Contains(large, "\n") {
		t.Fatalf("summary spans lines: %q", large)
	}
	if len(large) > len(small)+8 {
		t.Fatalf("summary grows with the entry count: %d bytes for 70, %d for 5000", len(small), len(large))
	}
	want := "5000 unreadable process entries (pids 2, 3, 4, ...; first: proving age for pid 2: stat unreadable; reading environ for pid 2: permission denied)"
	if large != want {
		t.Fatalf("summary = %q, want %q", large, want)
	}
}

// A failure that is not one entry's is the scan failing as a whole, so the
// summary keeps it verbatim and reports it as more than entry noise.
func TestSummarizeScanErrorKeepsWholeScanFailures(t *testing.T) {
	listErr := errors.New("tmux list running: no tmux server running")
	enumErr := errors.New("enumerating /proc: permission denied")
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{name: "beside entry failures", err: errors.Join(joinedEntryErrors(70), listErr), want: listErr.Error()},
		{name: "alone", err: fmt.Errorf("tmux backend: %w", enumErr), want: "tmux backend: " + enumErr.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, entriesOnly := SummarizeScanError(tc.err)
			if entriesOnly {
				t.Errorf("entriesOnly = true; a whole-scan failure would be logged as entry noise")
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("summary = %q, want it to keep %q", got, tc.want)
			}
		})
	}
	if got, entriesOnly := SummarizeScanError(nil); got != "" || !entriesOnly {
		t.Errorf("SummarizeScanError(nil) = %q, %v; want \"\", true", got, entriesOnly)
	}
}
