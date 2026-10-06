package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Log files written under --log-dir.
const (
	methodsLog    = "methods.jsonl"
	initializeLog = "initialize.json"
	promptsLog    = "prompts.jsonl"
	responsesLog  = "responses.jsonl"
	signalsLog    = "signals.jsonl"
)

// recorder appends observation records under --log-dir. Every write
// completes before the fake acts on the observed message, so a test that
// sees the fake's reaction also sees the record. A recorder without a dir is a
// no-op.
type recorder struct {
	dir string
	mu  sync.Mutex
}

func newRecorder(dir string) (*recorder, error) {
	if dir == "" {
		return &recorder{}, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating log dir %q: %w", dir, err)
	}
	return &recorder{dir: dir}, nil
}

func timestamp() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

// appendLine appends one JSON line to file.
func (r *recorder) appendLine(file string, line []byte) {
	if r.dir == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	f, err := os.OpenFile(filepath.Join(r.dir, file), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakeacp: open %s: %v\n", file, err)
		return
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		fmt.Fprintf(os.Stderr, "fakeacp: write %s: %v\n", file, err)
	}
	if err := f.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "fakeacp: close %s: %v\n", file, err)
	}
}

func (r *recorder) appendRecord(file string, rec any) {
	r.appendLine(file, mustJSON(rec))
}

// method records one inbound request or notification.
func (r *recorder) method(msg message) {
	r.appendRecord(methodsLog, struct {
		TS     string          `json:"ts"`
		Method string          `json:"method"`
		ID     json.RawMessage `json:"id,omitempty"`
	}{timestamp(), msg.Method, msg.ID})
}

// initialize stores the raw initialize params.
func (r *recorder) initialize(params json.RawMessage) {
	if r.dir == "" {
		return
	}
	if len(params) == 0 {
		params = json.RawMessage("null")
	}
	if err := os.WriteFile(filepath.Join(r.dir, initializeLog), params, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "fakeacp: write %s: %v\n", initializeLog, err)
	}
}

// prompt records the flattened text of one session/prompt.
func (r *recorder) prompt(id json.RawMessage, sessionID, text string) {
	r.appendRecord(promptsLog, struct {
		TS        string          `json:"ts"`
		ID        json.RawMessage `json:"id"`
		SessionID string          `json:"sessionId"`
		Text      string          `json:"text"`
	}{timestamp(), id, sessionID, text})
}

// response records a client reply to a fake-originated request verbatim.
func (r *recorder) response(raw []byte) {
	r.appendLine(responsesLog, raw)
}

// signal records a received signal and what the fake did about it.
func (r *recorder) signal(sig os.Signal, action string) {
	r.appendRecord(signalsLog, struct {
		TS     string `json:"ts"`
		Signal string `json:"signal"`
		Action string `json:"action"`
	}{timestamp(), sig.String(), action})
}
