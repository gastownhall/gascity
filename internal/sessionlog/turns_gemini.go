package sessionlog

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
)

// Gemini CLI chat files (X11 §2.2). Each user message carries an id and a
// timestamp, and opens a turn; Gemini records no turn end, so each turn runs
// until the next one starts. A file is either one JSON document with a
// messages array, or a JSONL mutation stream: message lines that add or
// replace a message by id, and snapshot lines that replace the whole list.

type geminiTurnFormat struct{}

// geminiTurnMessage is the subset of a Gemini message the scan reads.
type geminiTurnMessage struct {
	ID        string `json:"id"`
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
}

// geminiTurnLine is the subset of a Gemini JSONL line the scan reads.
type geminiTurnLine struct {
	geminiTurnMessage
	Set      *geminiTurnSnapshot `json:"$set"`
	RawSet   *geminiTurnSnapshot `json:"set"`
	Messages []json.RawMessage   `json:"messages"`
}

type geminiTurnSnapshot struct {
	Messages *[]json.RawMessage `json:"messages"`
}

// snapshot returns the message list a line replaces the whole list with.
func (l geminiTurnLine) snapshot() ([]json.RawMessage, bool) {
	switch {
	case l.Set != nil && l.Set.Messages != nil:
		return *l.Set.Messages, true
	case l.RawSet != nil && l.RawSet.Messages != nil:
		return *l.RawSet.Messages, true
	case len(l.Messages) > 0:
		return l.Messages, true
	}
	return nil, false
}

func (geminiTurnFormat) scan(path string, r io.ReaderAt, size, budget int64) (turnFold, error) {
	if !strings.EqualFold(filepath.Ext(path), ".jsonl") {
		return scanGeminiJSONTurns(r, size)
	}
	var lines [][]byte // newest first
	truncated, err := collectTurnLines(r, size, budget, func(line []byte) bool {
		var parsed geminiTurnLine
		if json.Unmarshal(line, &parsed) != nil {
			return false
		}
		lines = append(lines, line)
		_, isSnapshot := parsed.snapshot()
		return isSnapshot
	})
	if err != nil {
		return nil, err
	}
	fold := &geminiTurnFold{jsonl: true, seen: make(map[string]bool)}
	for _, line := range slices.Backward(lines) {
		fold.add(line)
	}
	if truncated && !fold.sawUser {
		return nil, ErrTurnStartBeyondBudget
	}
	return fold, nil
}

// scanGeminiJSONTurns reads a whole-document chat file. The document is
// rewritten as a whole, so it is read whole and never followed by appends.
func scanGeminiJSONTurns(r io.ReaderAt, size int64) (turnFold, error) {
	data := make([]byte, size)
	if _, err := r.ReadAt(data, 0); err != nil {
		return nil, fmt.Errorf("reading transcript for turns: %w", err)
	}
	var doc struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if !json.Valid(data) {
		return nil, ErrTornTurnRecord
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("decoding Gemini chat file for turns: %w", err)
	}
	fold := &geminiTurnFold{seen: make(map[string]bool)}
	fold.reset(doc.Messages)
	return fold, nil
}

// geminiTurnFold folds Gemini messages, oldest first.
type geminiTurnFold struct {
	ledger turnLedger
	jsonl  bool
	// seen holds the user message ids already folded, so a mutation of a
	// message opens no new turn.
	seen    map[string]bool
	sawUser bool
}

func (f *geminiTurnFold) add(line []byte) {
	var parsed geminiTurnLine
	if json.Unmarshal(line, &parsed) != nil {
		return
	}
	if messages, ok := parsed.snapshot(); ok {
		f.reset(messages)
		return
	}
	f.addMessage(parsed.geminiTurnMessage)
}

// appendable is false for a whole-document file, which is rewritten rather
// than appended to.
func (f *geminiTurnFold) appendable() bool { return f.jsonl }

// reset replaces the fold with one of messages.
func (f *geminiTurnFold) reset(messages []json.RawMessage) {
	f.ledger = turnLedger{}
	clear(f.seen)
	for _, raw := range messages {
		var msg geminiTurnMessage
		if json.Unmarshal(raw, &msg) == nil {
			f.addMessage(msg)
		}
	}
}

func (f *geminiTurnFold) addMessage(msg geminiTurnMessage) {
	if msg.Type != "user" {
		return
	}
	f.sawUser = true
	id, ok := validTurnID(msg.ID)
	switch {
	case !ok:
		f.ledger.missingIDs = true
	case f.seen[id]:
		return
	default:
		f.seen[id] = true
	}
	f.ledger.begin(id, parseTurnTime(msg.Timestamp))
}

func (f *geminiTurnFold) scan() TurnScan { return f.ledger.snapshot() }
