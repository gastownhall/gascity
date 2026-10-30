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
// origin nor the promptSource every prompt carries. It is often written just
// before the next prompt and stamped with that prompt's promptId, so the
// marker closes the open turn and its promptId is never read.
//
// A tool result is stamped with whatever promptId Claude holds when the tool
// finishes. Input queued while a tool runs, or a scheduled task firing then,
// stamps the running turn's tool result with the next prompt's promptId. So a
// tool result never starts a turn: it continues the open one, whatever id it
// carries. Only when the fold begins inside a turn does a tool result open
// that turn, so its id is still known: when the scan window is truncated, or
// before the first turn start or end of a file that begins mid-turn, as a
// session continued into a new file after it ran out of context does. That
// turn's start lies outside what was read, so it has none. If a prompt with
// the same id follows, the tool result carried that prompt's id, so the turn
// it opened keeps no id and the prompt opens its own turn.
//
// A user entry Claude writes itself, with neither origin nor promptSource
// (Stop hook feedback, the entry a resumed session opens with, a local slash
// command, `!` bash-mode input), may likewise carry a queued prompt's
// promptId before that prompt is written. While a turn is open it continues
// that turn, whatever id it carries; with no turn open it opens one, as a
// self-started turn begins. A local command or bash-mode input never runs
// the model, so the turn it opens is one the agent never runs: the scan
// reports it open until the next turn starts (see TurnScan.Open).
//
// An API error does not end a turn by itself: Claude Code retries some errors
// ("Connection lost mid-response") within the same turn. The failure stays
// pending until the turn_duration that follows it or the next prompt, which
// end the turn failed. A later main-chain reply or tool result clears it. A
// CLI that dies right after an error leaves its turn open here; TI-2b's idle
// overlay infers that end.
//
// When the fold begins inside a turn and reads that turn's end, a
// turn_duration or an interrupt, before any turn start, Recent records the end
// as a turn with neither start nor id, and MissingIDs is set. An API error
// pending at that turn_duration makes the turn failed. A prompt read first
// records nothing of the turn the fold began inside, not even an API error
// pending then: the fold holds no id of that turn, so it cannot tell whether
// the prompt continues it, as a prompt with the open turn's id does.

// claudeInterruptMarker starts the text Claude Code writes as the whole of a
// user entry when the user interrupts a turn ("[Request interrupted by
// user]", "[Request interrupted by user for tool use]"). The activity scan's
// isInterruptMessage (tail.go) matches the first form anywhere in the text
// and the second only as the whole text.
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
	// prompted marks a user entry carrying a prompt's origin or
	// promptSource.
	prompted bool
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
		rec.prompted = e.prompted()
		if !rec.toolResult && !rec.prompted && claudeInterruptEntry(e.Message) {
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

// scan collects records backward through the newest prompt of the turn
// before the oldest one it reports, then folds them oldest first, so the fold
// begins at a prompt rather than inside a turn. Only a prompt's id counts
// toward that stop: a tool result, or an entry Claude writes itself, may
// carry the next prompt's id before the prompt is written, and counting it
// would stop the scan a turn early. Such an entry may still open a turn,
// which only makes the scan read further.
func (claudeTurnFormat) scan(_ string, r io.ReaderAt, size, budget int64) (turnFold, error) {
	var records []claudeTurnRecord // newest first
	ids := make(map[string]bool)
	truncated, err := collectTurnLines(r, size, budget, func(line []byte) bool {
		rec, ok := parseClaudeTurnRecord(line)
		if !ok {
			return false
		}
		if id, ok := validTurnID(rec.rawID); ok && rec.kind == claudeRecordUser && rec.prompted && !rec.toolResult && !ids[id] {
			ids[id] = true
			if len(ids) > MaxRecentTurns+1 {
				records = append(records, rec)
				return true
			}
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
	// bounded is set once the fold has seen a turn start or end. Before
	// that, a transcript read whole may still begin inside a turn: a session
	// continued into a new file opens with the running turn's tool results.
	bounded bool
	// provisionalID is the id of a turn a tool result or an entry Claude
	// wrote itself opened at the start of the window. Either may carry the
	// next prompt's id, so a prompt with that id shows the turn was not that
	// prompt's.
	provisionalID string
	// failed is set while an API error of the open turn, or of the turn the
	// fold begins inside, is pending: no reply, tool result or prompt of the
	// turn has followed it yet. failedAt is the error's time.
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
		if f.ledger.open || !f.bounded {
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
		case !f.startUnseen && f.bounded:
			// A turn's first entry is never a tool result. Before the
			// fold's first boundary it may be the first entry read.
			return
		}
	}
	if !rec.prompted && l.open {
		// Claude wrote it itself: it continues the open turn, whatever
		// promptId it carries, and is no reply that clears a failure.
		return
	}
	id, ok := validTurnID(rec.rawID)
	if ok && rec.prompted && id == f.provisionalID {
		f.disownProvisional()
	}
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
	f.provisionalID = ""
	if f.startUnseen || (rec.toolResult && !f.bounded) {
		at = time.Time{}
		if !rec.prompted {
			f.provisionalID = id
		}
	}
	f.startUnseen, f.bounded = false, true
	l.begin(id, at)
}

// disownProvisional drops the id of the turn a tool result or an entry Claude
// wrote itself opened at the start of the window, once a prompt shows the id
// is the prompt's own.
func (f *claudeTurnFold) disownProvisional() {
	l := &f.ledger
	if l.isCurrent(f.provisionalID) {
		l.current.ID = ""
	} else if i := l.endedIndex(f.provisionalID); i >= 0 {
		l.recent[i].ID = ""
	}
	f.provisionalID = ""
}

func (f *claudeTurnFold) end(at time.Time, status TurnStatus) {
	if f.bounded {
		f.ledger.close(at, status)
	} else {
		// The fold begins inside the turn this record ends, and holds
		// neither its start nor its id.
		f.ledger.pushEnded(ScannedTurn{EndedAt: at, Status: status})
		f.ledger.missingIDs = true
	}
	f.startUnseen, f.bounded = false, true
	f.failed = false
}

// scan reports the fold. A pending API error leaves its turn open.
func (f *claudeTurnFold) scan() TurnScan { return f.ledger.snapshot() }
