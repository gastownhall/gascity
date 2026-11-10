package sessionlog

import (
	"encoding/json"
	"errors"
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
// A JSONL scan reads back to the newest snapshot. A window the budget cuts
// off before one is an error: a user line in it may edit a message written
// before the window rather than add one, and only the earlier lines tell
// which.

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
		return scanGeminiJSONTurns(r, size, budget)
	}
	var lines [][]byte // newest first
	truncated, err := collectTurnLines(r, size, budget, func(line []byte) bool {
		lines = append(lines, line)
		var parsed geminiTurnLine
		if json.Unmarshal(line, &parsed) != nil {
			return false
		}
		_, isSnapshot := parsed.snapshot()
		return isSnapshot
	})
	if err != nil {
		return nil, err
	}
	if truncated {
		// The walk stops at a snapshot, so a truncated window holds none.
		return nil, fmt.Errorf("%w: no snapshot in the window", ErrTurnStartBeyondBudget)
	}
	fold := &geminiTurnFold{jsonl: true, seen: make(map[string]bool)}
	for _, line := range slices.Backward(lines) {
		fold.add(line)
	}
	return fold, nil
}

// scanGeminiJSONTurns reads a whole-document chat file. The document is
// rewritten as a whole, so it is read whole, at most budget bytes, and never
// followed by appends.
func scanGeminiJSONTurns(r io.ReaderAt, size, budget int64) (turnFold, error) {
	if size > budget {
		return nil, fmt.Errorf("%w: %d bytes", ErrTurnTranscriptBeyondBudget, size)
	}
	data := make([]byte, size)
	if _, err := r.ReadAt(data, 0); err != nil {
		return nil, fmt.Errorf("reading transcript for turns: %w", err)
	}
	var doc struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		var syntax *json.SyntaxError
		if errors.As(err, &syntax) {
			// A document caught mid-rewrite is torn.
			return nil, fmt.Errorf("%w: %w", ErrTornTurnRecord, err)
		}
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
	seen map[string]bool
}

func (f *geminiTurnFold) add(line []byte) {
	var parsed geminiTurnLine
	if json.Unmarshal(line, &parsed) != nil {
		f.ledger.malformed++
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
		if json.Unmarshal(raw, &msg) != nil {
			f.ledger.malformed++
			continue
		}
		f.addMessage(msg)
	}
}

func (f *geminiTurnFold) addMessage(msg geminiTurnMessage) {
	if msg.Type != "user" {
		return
	}
	id, ok := validTurnID(msg.ID)
	switch {
	case !ok:
		f.ledger.missingIDs = true
	case f.seen[id]:
		return
	default:
		f.seen[id] = true
	}
	at, timed := parseTurnTime(msg.Timestamp)
	f.ledger.noteTime(timed)
	f.ledger.begin(id, at)
}

func (f *geminiTurnFold) scan() TurnScan { return f.ledger.snapshot() }
