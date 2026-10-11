package proctable

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/runtime"
)

// ErrIncarnationBoundUnsupported reports that this platform has no process
// inventory to bound to a session incarnation, so an empty scan result is the
// absence of evidence rather than evidence of absence. ScanBySessionIDSince
// returns it for a non-zero incarnationStartedAt on such platforms; a caller
// must treat the scan as incomplete, never as a certified absence.
var ErrIncarnationBoundUnsupported = errors.New("proctable: incarnation start-time bound unsupported on this platform")

// ScanAll returns all live agent root processes with a non-empty
// GC_SESSION_ID.
func ScanAll() ([]runtime.LiveRuntime, error) {
	return ScanBySessionID("")
}

// EntryError is a scan's failure to inspect one process entry. Its message is
// the underlying error's, unchanged. A scan joins one per unreadable entry, so
// on a busy host the joined error can name dozens of processes; loggers
// summarize it with [SummarizeScanError].
type EntryError struct {
	PID int
	Err error
}

func (e *EntryError) Error() string { return e.Err.Error() }

func (e *EntryError) Unwrap() error { return e.Err }

// Bounds on a scan error summary: how many entry error classes it describes,
// and how many PIDs it names per class.
const (
	maxSummarizedClasses = 4
	maxSummarizedPIDs    = 3
)

// digitRun matches the PIDs, inodes and other numbers that make two entry
// failures of the same kind read differently.
var digitRun = regexp.MustCompile(`[0-9]+`)

// SummarizeScanError renders a scan error as one bounded line. Every failure
// in the error tree that is not one entry's, such as an unenumerable /proc or
// a failed session listing, is kept verbatim, because a summary must never
// hide a scan that failed as a whole. Per-entry failures ([EntryError]) are
// counted once per PID, since a composite provider's backends scan the same
// /proc, and grouped into classes by their message with digit runs
// normalized. Each class shows its count, its lowest PIDs and one example
// message, so a new kind of failure is visible even when the total count does
// not change. A nil error summarizes to "".
func SummarizeScanError(err error) string {
	var entries []*EntryError
	var other []string
	collectScanErrors(err, &entries, &other)
	parts := other
	if len(entries) == 0 {
		return strings.Join(parts, "; ")
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].PID < entries[j].PID })
	type entryClass struct {
		example string
		pids    []int
	}
	classes := make(map[string]*entryClass)
	var order []string
	count := 0
	for i, entry := range entries {
		if i > 0 && entries[i-1].PID == entry.PID {
			continue
		}
		count++
		msg := oneLine(entry.Error())
		key := digitRun.ReplaceAllString(msg, "N")
		class, ok := classes[key]
		if !ok {
			class = &entryClass{example: msg}
			classes[key] = class
			order = append(order, key)
		}
		class.pids = append(class.pids, entry.PID)
	}
	sort.SliceStable(order, func(i, j int) bool { return len(classes[order[i]].pids) > len(classes[order[j]].pids) })
	described := make([]string, 0, maxSummarizedClasses+1)
	for i, key := range order {
		if i == maxSummarizedClasses {
			described = append(described, fmt.Sprintf("%d more classes", len(order)-maxSummarizedClasses))
			break
		}
		class := classes[key]
		pids := make([]string, 0, maxSummarizedPIDs+1)
		for j, pid := range class.pids {
			if j == maxSummarizedPIDs {
				pids = append(pids, "...")
				break
			}
			pids = append(pids, fmt.Sprint(pid))
		}
		described = append(described, fmt.Sprintf("%d like %q (pids %s)", len(class.pids), class.example, strings.Join(pids, ", ")))
	}
	parts = append(parts, fmt.Sprintf("%d unreadable process entries in %d classes: %s",
		count, len(order), strings.Join(described, "; ")))
	return strings.Join(parts, "; ")
}

// HasWholeScanFailure reports whether err holds any failure that is not one
// process entry's ([EntryError]): an unenumerable /proc, or a failed session
// listing or tracking read. Such a scan cannot vouch for IsTracked, while
// unreadable entries only drop those processes from the results.
func HasWholeScanFailure(err error) bool {
	var entries []*EntryError
	var other []string
	collectScanErrors(err, &entries, &other)
	return len(other) > 0
}

// collectScanErrors splits err's tree into per-entry failures and the
// messages of every subtree that holds none.
func collectScanErrors(err error, entries *[]*EntryError, other *[]string) {
	if err == nil {
		return
	}
	var entry *EntryError
	if !errors.As(err, &entry) {
		*other = append(*other, oneLine(err.Error()))
		return
	}
	switch wrapped := err.(type) { //nolint:errorlint // walks the error tree by hand to count every entry failure, which errors.As cannot express
	case *EntryError:
		*entries = append(*entries, wrapped)
	case interface{ Unwrap() []error }:
		for _, child := range wrapped.Unwrap() {
			collectScanErrors(child, entries, other)
		}
	case interface{ Unwrap() error }:
		collectScanErrors(wrapped.Unwrap(), entries, other)
	default:
		*other = append(*other, oneLine(err.Error()))
	}
}

func oneLine(msg string) string {
	return strings.ReplaceAll(msg, "\n", "; ")
}
