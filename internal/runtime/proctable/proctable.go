package proctable

import (
	"errors"
	"fmt"
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

// maxSummarizedPIDs bounds how many unreadable PIDs a scan error summary names.
const maxSummarizedPIDs = 3

// SummarizeScanError renders a scan error as one bounded line. Per-entry
// failures ([EntryError]) collapse to a count, the lowest few PIDs and the
// first failure's message. Every other failure in the error tree, such as an
// unenumerable /proc or a failed session listing, is kept verbatim, because a
// summary must never hide a scan that failed as a whole. entriesOnly reports
// that the error held nothing but per-entry failures. A nil error summarizes
// to "", true.
func SummarizeScanError(err error) (summary string, entriesOnly bool) {
	var entries []*EntryError
	var other []string
	collectScanErrors(err, &entries, &other)
	parts := other
	if len(entries) > 0 {
		sort.Slice(entries, func(i, j int) bool { return entries[i].PID < entries[j].PID })
		pids := make([]string, 0, maxSummarizedPIDs+1)
		for i, entry := range entries {
			if i == maxSummarizedPIDs {
				pids = append(pids, "...")
				break
			}
			pids = append(pids, fmt.Sprint(entry.PID))
		}
		parts = append(parts, fmt.Sprintf("%d unreadable process entries (pids %s; first: %s)",
			len(entries), strings.Join(pids, ", "), oneLine(entries[0].Error())))
	}
	return strings.Join(parts, "; "), len(other) == 0
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
