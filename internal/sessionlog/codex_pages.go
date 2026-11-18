package sessionlog

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// codexUsageCheckpointVersion is the schema version of CodexUsageCheckpoint.
// A resume carrying a different version is refused rather than guessed at.
const codexUsageCheckpointVersion = 1

// Default per-pass budgets for ReadCodexUsagePage. A pass reads at most
// codexUsagePageInputBudget bytes and collects at most codexUsagePageFactBudget
// candidate usage records before stopping with More set, so one sweep's work
// stays bounded no matter how large the backlog; the committed checkpoint lets
// the next pass resume at the boundary.
const (
	codexUsagePageInputBudget int64 = 16 << 20 // 16 MiB
	codexUsagePageFactBudget        = 1024
)

// CodexUsageCheckpoint is the committed forward-progress state of one codex
// rollout usage read: everything at or before Offset has been parsed, and the
// fields beside it are the parser state at that boundary, so a later pass
// resumes exactly where this one stopped.
type CodexUsageCheckpoint struct {
	// Version is the checkpoint schema version (codexUsageCheckpointVersion).
	Version int
	// SourcePath and SourceID bind the checkpoint to the rollout it describes:
	// the path the read resolved to and the session_meta payload.id observed at
	// the head of that file. A resume against a different source is refused.
	SourcePath string
	SourceID   string
	// Offset is a committed complete-line boundary: every byte before it has
	// been parsed. An unterminated trailing line is never committed.
	Offset int64
	// TurnModel is the model announced by the last turn_context at or before
	// Offset. It labels invocations the next pass finds before any newer
	// announcement, so model context that scrolled past every window still
	// prices later usage.
	TurnModel string
	// LastInvocationID, LastInvocationModel and LastInvocationAt describe the
	// last usage emitted at or before Offset: its cumulative-total identity,
	// first-observed model and first-observed time. A later pass uses them to
	// recognize the duplicate snapshot the CLI re-emits after the next
	// turn_context without counting it twice.
	LastInvocationID    string
	LastInvocationModel string
	LastInvocationAt    time.Time
}

// CodexUsagePageBudgets bounds one ReadCodexUsagePage pass. Zero values select
// the package defaults; tests inject smaller budgets to force page boundaries.
type CodexUsagePageBudgets struct {
	// InputBytes caps the bytes one pass commits. A record that would cross
	// the cap is deferred whole to the next pass; a record larger than the
	// whole cap can never fit and is reported incomplete instead.
	InputBytes int64
	// Facts caps the candidate usage records one pass collects. The candidate
	// that would exceed the cap is deferred, uncommitted.
	Facts int
}

// CodexUsagePage is one bounded forward pass over a codex rollout.
type CodexUsagePage struct {
	// Usages are the eligible per-invocation usages found this pass, in file
	// order, with duplicate cumulative totals collapsed to one entry.
	Usages []TailUsage
	// Checkpoints[i] is the committed continuation after Usages[i]'s last
	// observed line, so a consumer that emits facts one at a time can persist
	// exactly the progress its last successful emission covers.
	Checkpoints []CodexUsageCheckpoint
	// Final is the committed continuation after the last line this pass
	// consumed. Its Offset never crosses an unread line.
	Final CodexUsageCheckpoint
	// More reports that input remains beyond Final.Offset: a budget stop, or
	// an unterminated trailing line held pending until the writer completes it.
	More bool
	// Incomplete names why the pass's coverage has a hole: a record too large
	// for the input budget, or a complete line that did not parse. It is ""
	// when the pass committed every line it read cleanly.
	Incomplete string
}

// ReadCodexUsagePage reads one bounded forward pass over a codex rollout
// transcript, starting at byte zero or resuming from a committed checkpoint.
// It returns the eligible token_count usages found this pass with a
// continuation checkpoint per usage and a final checkpoint, replacing the fixed
// tail window that lost any growth beyond it between reads (ga-amrzyf case 2).
//
// The pass takes one size snapshot, commits only complete lines, and never
// commits an offset into a partially read record: an unterminated trailing
// line stays pending, and a record crossing the input or fact budget is
// deferred whole to the next pass. turn_context model announcements update the
// checkpoint's TurnModel as they are read, so a model announced any distance
// back still labels later invocations (case 3's mechanism). Duplicate
// cumulative totals collapse within a pass exactly as ExtractCodexTailUsage
// collapses them — first-observed model and time, last entry identity — and a
// re-emission of the resumed checkpoint's last invocation is skipped rather
// than counted again. A record too large for the input budget or a complete
// line that does not parse is consumed with an Incomplete reason rather than
// silently skipped.
func ReadCodexUsagePage(ctx context.Context, path string, resume *CodexUsageCheckpoint, budgets CodexUsagePageBudgets) (CodexUsagePage, error) {
	inputBudget, factBudget := budgets.defaults()

	info, err := os.Stat(path)
	if err != nil {
		return CodexUsagePage{}, fmt.Errorf("statting codex rollout %q: %w", path, err)
	}
	// One EOF snapshot feeds the whole pass: the budget math and the More
	// signal describe the same file view even while the writer appends.
	size := info.Size()

	cp := CodexUsageCheckpoint{Version: codexUsageCheckpointVersion, SourcePath: path}
	if resume != nil {
		if err := validateCodexUsageResume(resume, path, size); err != nil {
			return CodexUsagePage{}, err
		}
		cp = *resume
		cp.SourcePath = path
	}

	f, err := os.Open(path)
	if err != nil {
		return CodexUsagePage{}, fmt.Errorf("opening codex rollout %q: %w", path, err)
	}
	defer f.Close() //nolint:errcheck // best-effort close on read-only file
	if _, err := f.Seek(cp.Offset, io.SeekStart); err != nil {
		return CodexUsagePage{}, fmt.Errorf("seeking codex rollout %q to %d: %w", path, cp.Offset, err)
	}

	page := CodexUsagePage{}
	byMessageID := make(map[string]int)
	reader := bufio.NewReaderSize(f, 64*1024)
	passInput := int64(0)
	facts := 0
	var incomplete []string

	for {
		if err := ctx.Err(); err != nil {
			return CodexUsagePage{}, fmt.Errorf("reading codex usage page from %q: %w", path, err)
		}
		data, overflow, terminated, rerr := readCodexPageLine(reader, inputBudget)
		if rerr != nil {
			return CodexUsagePage{}, fmt.Errorf("reading codex rollout %q: %w", path, rerr)
		}
		if !terminated {
			// A final line the writer has not newline-terminated yet is
			// pending, not parseable: leave it uncommitted (zero-length data
			// here is a clean EOF exactly at a committed boundary).
			break
		}
		if overflow > 0 {
			// No pass with this budget could ever commit the record. Report it
			// rather than silently skipping past usage it may hold.
			incomplete = append(incomplete, fmt.Sprintf("record of %d bytes exceeds the %d-byte input budget", overflow, inputBudget))
			cp.Offset += overflow
			continue
		}
		lineLen := int64(len(data)) // data includes its terminating newline
		if passInput+lineLen > inputBudget {
			break // the record would cross the input budget: defer it whole
		}

		line := bytes.TrimSpace(data)
		var usage TailUsage
		hasUsage := false
		if len(line) > 0 {
			var entry codexRawEntry
			var payload codexUsagePayload
			switch {
			case json.Unmarshal(line, &entry) != nil:
				incomplete = append(incomplete, fmt.Sprintf("malformed record at offset %d", cp.Offset))
			case len(entry.Payload) == 0:
				// A record with no payload (compaction markers and the like)
				// is not malformed and holds no usage; it just advances.
			case json.Unmarshal(entry.Payload, &payload) != nil:
				incomplete = append(incomplete, fmt.Sprintf("malformed record at offset %d", cp.Offset))
			case entry.Type == "session_meta" && cp.SourceID == "":
				var meta struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal(entry.Payload, &meta); err == nil && meta.ID != "" {
					cp.SourceID = meta.ID
				}
			case entry.Type == "turn_context":
				if payload.Model != "" {
					cp.TurnModel = payload.Model
				}
			default:
				u, ok := codexTokenCountUsage(entry, payload, cp.TurnModel)
				if ok {
					usage, hasUsage = u, true
				}
			}
		}
		if hasUsage && facts >= factBudget {
			break // the candidate would cross the fact budget: defer it whole
		}

		// Commit the line, then apply its effect to the parser state.
		passInput += lineLen
		cp.Offset += lineLen
		if !hasUsage {
			continue
		}
		facts++
		if usage.MessageID == cp.LastInvocationID && !page.emitted(byMessageID, usage.MessageID) {
			// The CLI re-emits the prior turn's final cumulative snapshot
			// after the next turn_context; the resumed reader already emitted
			// that invocation, so this is a duplicate, not a new call.
			continue
		}
		if i, seen := byMessageID[usage.MessageID]; seen {
			// Duplicate collapse, mirroring ExtractCodexTailUsage: the
			// first-observed model and invocation time are kept, the entry
			// identity is refreshed to the last emission.
			if page.Usages[i].Model != "" {
				usage.Model = page.Usages[i].Model
			}
			if !page.Usages[i].Timestamp.IsZero() {
				usage.Timestamp = page.Usages[i].Timestamp
			}
			page.Usages[i] = usage
		} else {
			byMessageID[usage.MessageID] = len(page.Usages)
			page.Usages = append(page.Usages, usage)
		}
		cp.LastInvocationID = usage.MessageID
		cp.LastInvocationModel = usage.Model
		cp.LastInvocationAt = usage.Timestamp
		page.Checkpoints = append(page.Checkpoints, cp)
	}
	page.Final = cp
	page.More = page.Final.Offset < size
	page.Incomplete = joinIncomplete(incomplete)
	return page, nil
}

// emitted reports whether an identity was already emitted within this page, so
// the resumed-duplicate skip cannot suppress a fresh in-page observation.
func (p CodexUsagePage) emitted(byMessageID map[string]int, id string) bool {
	_, ok := byMessageID[id]
	return ok
}

// defaults fills zero budget fields with the package defaults.
func (b CodexUsagePageBudgets) defaults() (int64, int) {
	input, facts := b.InputBytes, b.Facts
	if input <= 0 {
		input = codexUsagePageInputBudget
	}
	if facts <= 0 {
		facts = codexUsagePageFactBudget
	}
	return input, facts
}

// validateCodexUsageResume refuses a checkpoint that cannot describe this
// source: a foreign schema version, a path binding mismatch, an offset beyond
// the file (replaced or truncated), or an offset that does not sit on a
// committed line boundary.
func validateCodexUsageResume(resume *CodexUsageCheckpoint, path string, size int64) error {
	if resume.Version != codexUsageCheckpointVersion {
		return fmt.Errorf("resuming codex usage read of %q: checkpoint version %d is not %d", path, resume.Version, codexUsageCheckpointVersion)
	}
	if resume.SourcePath != "" && !sameFilePath(resume.SourcePath, path) {
		return fmt.Errorf("resuming codex usage read of %q: checkpoint belongs to %q", path, resume.SourcePath)
	}
	if resume.Offset < 0 || resume.Offset > size {
		return fmt.Errorf("resuming codex usage read of %q: checkpoint offset %d is outside the %d-byte source (replaced or truncated)", path, resume.Offset, size)
	}
	if resume.Offset == 0 {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("resuming codex usage read of %q: %w", path, err)
	}
	defer f.Close() //nolint:errcheck // best-effort close on read-only file
	if _, err := f.Seek(resume.Offset-1, io.SeekStart); err != nil {
		return fmt.Errorf("resuming codex usage read of %q: %w", path, err)
	}
	var prev [1]byte
	if _, err := io.ReadFull(f, prev[:]); err != nil {
		return fmt.Errorf("resuming codex usage read of %q: %w", path, err)
	}
	if prev[0] != '\n' {
		return fmt.Errorf("resuming codex usage read of %q: checkpoint offset %d is not on a committed line boundary", path, resume.Offset)
	}
	return nil
}

// sameFilePath compares two paths lexically first and falls back to a symlink
// evaluation, tolerating the cleaned forms different callers may pass.
func sameFilePath(a, b string) bool {
	if a == b {
		return true
	}
	ea, errA := filepath.EvalSymlinks(a)
	eb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ea == eb
}

// readCodexPageLine reads one newline-terminated line. Bytes are retained only
// up to maxKeep: a longer line keeps draining without retention and returns
// overflow set to its full length (including the terminator), letting the
// caller consume-and-report it as oversized without buffering it. terminated
// is false only when no newline was reached — a clean EOF returns zero-length
// pending data — and such bytes belong to an incomplete record the caller must
// not commit.
func readCodexPageLine(r *bufio.Reader, maxKeep int64) (data []byte, overflow int64, terminated bool, err error) {
	for {
		chunk, rerr := r.ReadSlice('\n')
		switch {
		case rerr == nil:
			if overflow > 0 {
				return nil, overflow + int64(len(chunk)), true, nil
			}
			return append(data, chunk...), 0, true, nil
		case errors.Is(rerr, bufio.ErrBufferFull):
			if overflow > 0 {
				overflow += int64(len(chunk))
				continue
			}
			if int64(len(data))+int64(len(chunk)) > maxKeep {
				overflow = int64(len(data)) + int64(len(chunk))
				data = nil
				continue
			}
			data = append(data, chunk...)
		case errors.Is(rerr, io.EOF):
			if overflow > 0 {
				return nil, overflow + int64(len(chunk)), false, nil
			}
			return append(data, chunk...), 0, false, nil
		default:
			return data, overflow, false, rerr
		}
	}
}

// joinIncomplete renders the pass's coverage holes as one reason string.
func joinIncomplete(reasons []string) string {
	switch len(reasons) {
	case 0:
		return ""
	case 1:
		return reasons[0]
	default:
		out := reasons[0]
		for _, r := range reasons[1:] {
			out += "; " + r
		}
		return out
	}
}

// ReadCodexUsagePageFromSearchPaths is ReadCodexUsagePage with the adapter's
// search-path validation: the codex default roots merge on top of the
// configured paths, and the rollout must resolve inside one of them.
func ReadCodexUsagePageFromSearchPaths(searchPaths []string, path string, resume *CodexUsageCheckpoint, budgets CodexUsagePageBudgets) (CodexUsagePage, error) {
	safePath, err := validateSearchPathFile(mergeCodexSearchPaths(searchPaths), path)
	if err != nil {
		return CodexUsagePage{}, err
	}
	return ReadCodexUsagePage(context.Background(), safePath, resume, budgets)
}
