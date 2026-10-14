package sessionlog

import (
	"bytes"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"time"
)

// Claude Code transcripts (X11 §2.2). Every main-chain user entry of a turn
// carries the turn's promptId: the prompt, its tool results, and the entries
// a self-started turn opens with. A system turn_duration entry closes the open
// turn by position. An API error is an assistant entry flagged
// isApiErrorMessage. An interrupt is a user entry holding only Claude's
// interrupt marker, written by Claude itself, so it carries neither the
// origin nor the promptSource every prompt carries; it is often written just before the next prompt and
// stamped with that prompt's promptId, so the marker closes the open turn and
// its promptId is never read.
//
// A tool result is stamped with whatever promptId Claude holds when the tool
// finishes. Input queued while a tool runs, or a scheduled task firing then,
// stamps the running turn's tool result with the next prompt's promptId. So a
// tool result never starts a turn: it continues the open one, whatever id it
// carries. Only when the scan window begins inside a turn does a tool result
// open that turn, so its id is still known.
//
// An API error does not end a turn by itself: Claude Code retries some errors
// ("Connection lost mid-response") within the same turn. The failure stays
// pending until the turn_duration that follows it or the next prompt, which
// end the turn failed. A later main-chain reply or tool result clears it. A
// CLI that dies right after an error leaves its turn open here; TI-2b's idle
// overlay infers that end.

// claudeInterruptMarker starts the text Claude Code writes as the whole of a
// user entry when the user interrupts a turn ("[Request interrupted by
// user]", "[Request interrupted by user for tool use]").
const claudeInterruptMarker = "[Request interrupted by user"

// claudeToolResultType names a tool result content block. A message without
// it, quoted or escaped, is not decoded for one.
var claudeToolResultType = []byte("tool_result")

type claudeTurnFormat struct{}

type claudeRecordKind int

const (
	claudeRecordUser claudeRecordKind = iota + 1
	claudeRecordInterrupt
	claudeRecordTurnEnd
	claudeRecordAPIError
	claudeRecordReply
	// claudeRecordMalformed is a line that could be a turn record but did not
	// decode.
	claudeRecordMalformed
)

// claudeTurnRecord is one transcript entry that bears on turns.
type claudeTurnRecord struct {
	kind  claudeRecordKind
	rawID string
	at    time.Time
	timed bool
	// toolResult marks a user entry carrying a tool result.
	toolResult bool
}

// claudeTurnLine is the subset of a Claude transcript entry the scan reads.
type claudeTurnLine struct {
	Type             string          `json:"type"`
	Subtype          string          `json:"subtype"`
	IsSidechain      bool            `json:"isSidechain"`
	IsCompactSummary bool            `json:"isCompactSummary"`
	IsAPIError       bool            `json:"isApiErrorMessage"`
	PromptID         string          `json:"promptId"`
	Origin           json.RawMessage `json:"origin"`
	PromptSource     string          `json:"promptSource"`
	Timestamp        string          `json:"timestamp"`
	Message          json.RawMessage `json:"message"`
}

// prompted reports whether the entry carries the origin or promptSource of a
// prompt, which Claude's own interrupt marker never does.
func (e claudeTurnLine) prompted() bool {
	origin := bytes.TrimSpace(e.Origin)
	return e.PromptSource != "" || (len(origin) > 0 && !bytes.Equal(origin, []byte("null")))
}

// claudeTurnTypes are the JSON strings one of which every line that can be a
// turn record holds: its entry type, or the turn_duration subtype. A line
// holding none is not decoded; most of a transcript's bytes are attachments
// and other entries that bear on no turn.
var claudeTurnTypes = [][]byte{[]byte(`"user"`), []byte(`"assistant"`), []byte(`"turn_duration"`)}

// parseClaudeTurnRecord reads line as a turn record. Sidechain entries,
// compaction summaries and entries that do not bear on turns are not
// records. The type-name check is only a filter: any text can hold the
// names too.
func parseClaudeTurnRecord(line []byte) (claudeTurnRecord, bool) {
	if !slices.ContainsFunc(claudeTurnTypes, func(name []byte) bool { return bytes.Contains(line, name) }) {
		return claudeTurnRecord{}, false
	}
	var e claudeTurnLine
	if json.Unmarshal(line, &e) != nil {
		return claudeTurnRecord{kind: claudeRecordMalformed}, true
	}
	if e.IsSidechain {
		return claudeTurnRecord{}, false
	}
	rec := claudeTurnRecord{rawID: e.PromptID}
	rec.at, rec.timed = parseTurnTime(e.Timestamp)
	switch {
	case e.Type == "user" && e.IsCompactSummary:
		return claudeTurnRecord{}, false
	case e.Type == "user":
		rec.kind = claudeRecordUser
		rec.toolResult = claudeToolResultEntry(e.Message)
		if !rec.toolResult && !e.prompted() && claudeInterruptEntry(e.Message) {
			rec.kind = claudeRecordInterrupt
		}
	case e.Type == "system" && e.Subtype == "turn_duration":
		rec.kind = claudeRecordTurnEnd
	case e.Type == "assistant" && e.IsAPIError:
		rec.kind = claudeRecordAPIError
	case e.Type == "assistant":
		rec.kind = claudeRecordReply
	default:
		return claudeTurnRecord{}, false
	}
	return rec, true
}

// claudeInterruptEntry reports whether a user message holds nothing but
// Claude's interrupt marker. A prompt that merely mentions the marker is not
// one.
func claudeInterruptEntry(message json.RawMessage) bool {
	if !bytes.Contains(message, []byte(claudeInterruptMarker)) {
		return false
	}
	var msg struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(unwrapJSONString(message), &msg) != nil || len(msg.Content) == 0 {
		return false
	}
	var text string
	if json.Unmarshal(msg.Content, &text) != nil {
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(msg.Content, &blocks) != nil || len(blocks) != 1 || blocks[0].Type != "text" {
			return false
		}
		text = blocks[0].Text
	}
	text = strings.TrimSpace(text)
	return strings.HasPrefix(text, claudeInterruptMarker) && strings.HasSuffix(text, "]") && !strings.Contains(text, "\n")
}

// claudeToolResultEntry reports whether a user message carries a tool result
// block. It reads the blocks' types only, never their text.
func claudeToolResultEntry(message json.RawMessage) bool {
	if !bytes.Contains(message, claudeToolResultType) {
		return false
	}
	var msg struct {
		Content []struct {
			Type string `json:"type"`
		} `json:"content"`
	}
	if json.Unmarshal(unwrapJSONString(message), &msg) != nil {
		return false
	}
	for _, block := range msg.Content {
		if block.Type == "tool_result" {
			return true
		}
	}
	return false
}

// scan collects records backward until it has passed the first entry of
// the oldest turn it reports, then folds them oldest first.
func (claudeTurnFormat) scan(_ string, r io.ReaderAt, size, budget int64) (turnFold, error) {
	var records []claudeTurnRecord // newest first
	ids := make(map[string]bool)
	truncated, err := collectTurnLines(r, size, budget, func(line []byte) bool {
		rec, ok := parseClaudeTurnRecord(line)
		if !ok {
			return false
		}
		if id, ok := validTurnID(rec.rawID); ok && rec.kind == claudeRecordUser && !ids[id] {
			if len(ids) == MaxRecentTurns+1 {
				return true
			}
			ids[id] = true
		}
		records = append(records, rec)
		return false
	})
	if err != nil {
		return nil, err
	}
	if truncated && !slices.ContainsFunc(records, claudeTurnRecord.marksTurn) {
		return nil, ErrTurnStartBeyondBudget
	}
	fold := &claudeTurnFold{startUnseen: truncated}
	for _, rec := range slices.Backward(records) {
		fold.addRecord(rec)
	}
	return fold, nil
}

// marksTurn reports whether rec starts or ends a turn, rather than only
// telling how the open one is going.
func (rec claudeTurnRecord) marksTurn() bool {
	return rec.kind != claudeRecordAPIError && rec.kind != claudeRecordReply && rec.kind != claudeRecordMalformed
}

// claudeTurnFold folds Claude turn records, oldest first.
type claudeTurnFold struct {
	ledger turnLedger
	// startUnseen: the window may begin inside a turn, so the first turn
	// opened before any boundary has an unknown start.
	startUnseen bool
	// failed is set while an API error of the open turn is pending: no
	// reply, tool result or prompt of the turn has followed it yet. failedAt
	// is the error's time.
	failedAt time.Time
	failed   bool
}

func (f *claudeTurnFold) add(line []byte) {
	if rec, ok := parseClaudeTurnRecord(line); ok {
		f.addRecord(rec)
	}
}

func (f *claudeTurnFold) appendable() bool { return true }

func (f *claudeTurnFold) addRecord(rec claudeTurnRecord) {
	if rec.kind == claudeRecordMalformed {
		f.ledger.malformed++
		return
	}
	f.ledger.noteTime(rec.timed)
	switch rec.kind {
	case claudeRecordUser:
		f.addUser(rec)
	case claudeRecordInterrupt:
		f.end(rec.at, TurnStatusCancelled)
	case claudeRecordTurnEnd:
		status := TurnStatusCompleted
		if f.failed {
			status = TurnStatusFailed
		}
		f.end(rec.at, status)
	case claudeRecordAPIError:
		if f.ledger.open {
			f.failed, f.failedAt = true, rec.at
		}
	case claudeRecordReply:
		f.failed = false
	}
}

func (f *claudeTurnFold) addUser(rec claudeTurnRecord) {
	l := &f.ledger
	if rec.toolResult {
		switch {
		case l.open:
			// It continues the open turn, whatever promptId it carries.
			f.failed = false
			return
		case !f.startUnseen:
			// A turn's first entry is never a tool result.
			return
		}
	}
	id, ok := validTurnID(rec.rawID)
	switch {
	case !ok:
		// Boundary only: an entry without an id continues the open turn
		// or opens an anonymous one.
		l.missingIDs = true
		if l.open {
			f.failed = false
			return
		}
	case l.isCurrent(id):
		f.failed = false
		return
	case l.endedIndex(id) >= 0:
		// A late entry of a turn already ended opens nothing.
		return
	}
	if f.failed {
		l.close(f.failedAt, TurnStatusFailed)
		f.failed = false
	}
	at := rec.at
	if f.startUnseen {
		at = time.Time{}
	}
	f.startUnseen = false
	l.begin(id, at)
}

func (f *claudeTurnFold) end(at time.Time, status TurnStatus) {
	f.startUnseen = false
	f.failed = false
	f.ledger.close(at, status)
}

// scan reports the fold. A pending API error leaves its turn open.
func (f *claudeTurnFold) scan() TurnScan { return f.ledger.snapshot() }
