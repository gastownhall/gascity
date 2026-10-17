package sessionlog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Turn transcript ownership (TI-2b). The worker reads a session's turns only
// from a transcript that is that session's conversation, found by the
// provider session key. For most families the key builds the path, so the
// file is the key's by construction. A Codex rollout carries the key only in
// its filename, which a copied or renamed rollout can share; its first record,
// session_meta, names the conversation the file records.

// TurnTranscriptOwnedBy reports whether provider's transcript at path records
// the conversation whose provider session key is key. A blank key owns
// nothing. A file that cannot be read is an error, never a yes.
func TurnTranscriptOwnedBy(provider, path, key string) (bool, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return false, nil
	}
	if turnFamilyKey(provider) != "codex" {
		return true, nil
	}
	id, err := codexSessionMetaID(path)
	if err != nil {
		return false, err
	}
	return id == key, nil
}

// codexSessionMetaID reads the conversation id from a rollout's session_meta
// record, its first line; "" when the first line is not a complete
// session_meta record.
func codexSessionMetaID(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("opening rollout for its session id: %w", err)
	}
	defer f.Close() //nolint:errcheck // read-only file
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return "", fmt.Errorf("reading rollout for its session id: %w", err)
		}
		return "", nil
	}
	var meta struct {
		Type    string `json:"type"`
		Payload struct {
			ID string `json:"id"`
		} `json:"payload"`
	}
	if json.Unmarshal(scanner.Bytes(), &meta) != nil || meta.Type != "session_meta" {
		return "", nil
	}
	return strings.TrimSpace(meta.Payload.ID), nil
}
