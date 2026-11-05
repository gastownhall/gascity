package main

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// nativeTranscript records only this scripted fixture's turns. It is not
// evidence of an authenticated provider, model request or provider reply.
type nativeTranscript struct {
	path, key string
	lastUUID  string
	open      bool
}

func newNativeTranscript(args []string) (*nativeTranscript, error) {
	key := ""
	for i := 0; i < len(args); i++ {
		if args[i] != "--session-id" && args[i] != "--resume" {
			continue
		}
		if i+1 >= len(args) || key != "" {
			return nil, fmt.Errorf("want one native session key")
		}
		i++
		key = args[i]
	}
	if key == "" || key == "." || key == ".." || strings.ContainsAny(key, "/\\") {
		return nil, fmt.Errorf("invalid native session key %q", key)
	}
	root := os.Getenv("CLAUDE_CONFIG_DIR")
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("locating fixture HOME: %w", err)
		}
		root = filepath.Join(home, ".claude")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("locating fixture workdir: %w", err)
	}
	slug := strings.NewReplacer("/", "-", ".", "-").Replace(cwd)
	n := &nativeTranscript{path: filepath.Join(root, "projects", slug, key+".jsonl"), key: key}
	f, err := os.Open(n.path)
	if errors.Is(err, os.ErrNotExist) {
		return n, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading scripted transcript: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only fixture history
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("sizing scripted transcript: %w", err)
	}
	if info.Size() > 0 {
		var last [1]byte
		if _, err := f.ReadAt(last[:], info.Size()-1); err != nil {
			return nil, fmt.Errorf("reading scripted transcript tail: %w", err)
		}
		if last[0] != '\n' {
			return nil, fmt.Errorf("scripted transcript has an unterminated record; preserve it for inspection")
		}
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		var r nativeRecord
		if err := json.Unmarshal(scanner.Bytes(), &r); err != nil {
			return nil, fmt.Errorf("decoding scripted transcript: %w", err)
		}
		if r.SessionID != key || r.UUID == "" {
			return nil, fmt.Errorf("scripted transcript has an unowned or unidentified record")
		}
		n.lastUUID = r.UUID
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanning scripted transcript: %w", err)
	}
	return n, nil
}

type nativeMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type nativeRecord struct {
	Type         string         `json:"type"`
	UUID         string         `json:"uuid"`
	ParentUUID   string         `json:"parentUuid,omitempty"`
	Subtype      string         `json:"subtype,omitempty"`
	SessionID    string         `json:"sessionId"`
	PromptID     string         `json:"promptId,omitempty"`
	PromptSource string         `json:"promptSource,omitempty"`
	Timestamp    string         `json:"timestamp"`
	Message      *nativeMessage `json:"message,omitempty"`
}

func (n *nativeTranscript) append(r nativeRecord) error {
	r.SessionID = n.key
	r.UUID = rand.Text()
	r.ParentUUID = n.lastUUID
	r.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(n.path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(n.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(append(data, '\n'))
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr == nil {
		n.lastUUID = r.UUID
	}
	return closeErr
}

func (n *nativeTranscript) start(text string) error {
	if n.open {
		return nil
	}
	// The fixture alone mints these prompt ids, independently of delivered text.
	id := "scripted-" + rand.Text()
	if err := n.append(nativeRecord{Type: "user", PromptID: id, PromptSource: "typed", Message: &nativeMessage{Role: "user", Content: text}}); err != nil {
		return err
	}
	n.open = true
	return nil
}

func (n *nativeTranscript) end() error {
	if !n.open {
		return nil
	}
	if err := n.append(nativeRecord{Type: "system", Subtype: "turn_duration"}); err != nil {
		return err
	}
	n.open = false
	return nil
}
