package sessionlog

import (
	"bytes"
	"encoding/json"
	"io"
	"slices"
	"time"
)

// Codex rollouts (X11 §2.2). An event_msg task_started opens a turn under
// its turn_id; task_complete (failed when it carries an error) and
// turn_aborted close it by the same id. A turn's start time is the
// task_started line's envelope timestamp, which has milliseconds, never the
// payload's whole-second started_at. turn_context lines name the running
// turn too, which identifies a turn whose start lies before the scan window.

type codexTurnFormat struct{}

type codexRecordKind int

const (
	codexRecordStart codexRecordKind = iota + 1
	codexRecordComplete
	codexRecordAborted
	codexRecordContext
	// codexRecordMalformed is a line that could be a turn record but did not
	// decode.
	codexRecordMalformed
)

// codexTurnRecord is one rollout line that bears on turns.
type codexTurnRecord struct {
	kind   codexRecordKind
	rawID  string
	at     time.Time
	timed  bool
	failed bool
	// failure is a failed task_complete's error.
	failure TurnFailure
}

// codexTurnContextType is the envelope type of a rollout's turn_context line.
var codexTurnContextType = []byte(`"turn_context"`)

// parseCodexTurnRecord reads line as a turn record. The type-name check is
// only a filter: tool output can contain the names too.
func parseCodexTurnRecord(line []byte) (codexTurnRecord, bool) {
	if !bytes.Contains(line, codexTurnContextType) &&
		!slices.ContainsFunc(codexLifecycleTypes, func(name []byte) bool { return bytes.Contains(line, name) }) {
		return codexTurnRecord{}, false
	}
	var entry codexRawEntry
	if json.Unmarshal(line, &entry) != nil {
		return codexTurnRecord{kind: codexRecordMalformed}, true
	}
	var payload struct {
		Type   string          `json:"type"`
		TurnID string          `json:"turn_id"`
		Error  json.RawMessage `json:"error"`
	}
	if json.Unmarshal(entry.Payload, &payload) != nil {
		return codexTurnRecord{kind: codexRecordMalformed}, true
	}
	rec := codexTurnRecord{rawID: payload.TurnID}
	rec.at, rec.timed = parseTurnTime(entry.Timestamp)
	switch {
	case entry.Type == "turn_context":
		rec.kind = codexRecordContext
	case entry.Type != "event_msg":
		return codexTurnRecord{}, false
	case payload.Type == "task_started":
		rec.kind = codexRecordStart
	case payload.Type == "task_complete":
		rec.kind = codexRecordComplete
		if failure, ok := codexTaskCompleteError(payload.Error); ok {
			rec.failed, rec.failure = true, codexTurnFailure(failure)
		}
	case payload.Type == "turn_aborted":
		rec.kind = codexRecordAborted
	default:
		return codexTurnRecord{}, false
	}
	return rec, true
}

// codexUsageLimitClass is the codex_error_info variant Codex gives a turn the
// provider refused because the account's usage limit is spent.
const codexUsageLimitClass = "usage_limit_exceeded"

// codexTurnFailure is a failed task_complete's error as Codex classified it:
// its codex_error_info variant, the HTTP status the variant carries, and the
// message, cut to MaxTurnFailureMessage.
func codexTurnFailure(e codexEventMsg) TurnFailure {
	code := codexErrorInfoCode(e.CodexErrorInfo)
	return newTurnFailure(code, codexErrorInfoStatus(e.CodexErrorInfo), code == codexUsageLimitClass, e.Message)
}

// codexErrorInfoStatus is the HTTP status a codex_error_info variant with
// data carries ({"http_connection_failed":{"http_status_code":429}}); 0 for
// a variant without one.
func codexErrorInfoStatus(raw json.RawMessage) int {
	var variant map[string]struct {
		HTTPStatusCode int `json:"http_status_code"`
	}
	if json.Unmarshal(bytes.TrimSpace(raw), &variant) != nil || len(variant) != 1 {
		return 0
	}
	for _, data := range variant {
		return data.HTTPStatusCode
	}
	return 0
}

// codexErrorPresent reports whether a task_complete error field holds an
// error rather than null or nothing.
func codexErrorPresent(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && !bytes.Equal(raw, []byte("null")) && !bytes.Equal(raw, []byte(`""`))
}

// scan collects records backward until it has read the start of the oldest
// turn it reports, then folds them oldest first.
func (codexTurnFormat) scan(_ string, r io.ReaderAt, size, budget int64) (turnFold, error) {
	var records []codexTurnRecord // newest first
	starts := 0
	truncated, err := collectTurnLines(r, size, budget, func(line []byte) bool {
		rec, ok := parseCodexTurnRecord(line)
		if !ok {
			return false
		}
		records = append(records, rec)
		if rec.kind == codexRecordStart {
			starts++
		}
		return starts == MaxRecentTurns+1
	})
	if err != nil {
		return nil, err
	}
	if truncated && !slices.ContainsFunc(records, func(rec codexTurnRecord) bool { return rec.kind != codexRecordMalformed }) {
		return nil, ErrTurnStartBeyondBudget
	}
	fold := &codexTurnFold{startUnseen: truncated}
	for _, rec := range slices.Backward(records) {
		fold.addRecord(rec)
	}
	return fold, nil
}

// codexTurnFold folds Codex turn records, oldest first.
type codexTurnFold struct {
	ledger turnLedger
	// startUnseen: the window may begin inside a turn whose task_started
	// lies before it. It holds until the window's first task_started.
	startUnseen bool
}

func (f *codexTurnFold) add(line []byte) {
	if rec, ok := parseCodexTurnRecord(line); ok {
		f.addRecord(rec)
	}
}

func (f *codexTurnFold) appendable() bool { return true }

func (f *codexTurnFold) addRecord(rec codexTurnRecord) {
	l := &f.ledger
	if rec.kind == codexRecordMalformed {
		l.malformed++
		return
	}
	l.noteTime(rec.timed)
	id, ok := validTurnID(rec.rawID)
	if !ok && rec.kind != codexRecordContext {
		l.missingIDs = true
	}
	switch rec.kind {
	case codexRecordStart:
		f.startUnseen = false
		if ok && (l.isCurrent(id) || l.endedIndex(id) >= 0) {
			return
		}
		l.begin(id, rec.at)
	case codexRecordContext:
		// Only a turn begun before the window is opened from its context.
		if ok && f.startUnseen && !l.open && l.endedIndex(id) < 0 {
			l.begin(id, time.Time{})
		}
	case codexRecordComplete, codexRecordAborted:
		f.end(rec, id, ok)
	}
}

// end folds a task_complete or turn_aborted. An end without an id closes the
// open turn; an end for a turn a newer one already closed records how it
// ended; an end before the window's first start records a turn begun before
// the window.
func (f *codexTurnFold) end(rec codexTurnRecord, id string, ok bool) {
	l := &f.ledger
	status := TurnStatusCompleted
	var failure TurnFailure
	switch {
	case rec.kind == codexRecordAborted:
		status = TurnStatusCancelled
	case rec.failed:
		status, failure = TurnStatusFailed, rec.failure
	}
	if !ok || l.isCurrent(id) {
		l.closeFailed(rec.at, status, failure)
		return
	}
	if i := l.endedIndex(id); i >= 0 {
		if l.recent[i].Status == "" {
			l.recent[i].Status = status
			l.recent[i].EndedAt = rec.at
			l.recent[i].Failure = failure
		}
		return
	}
	if f.startUnseen && !l.open {
		l.pushEnded(ScannedTurn{ID: id, EndedAt: rec.at, Status: status, Failure: failure})
	}
}

func (f *codexTurnFold) scan() TurnScan { return f.ledger.snapshot() }
