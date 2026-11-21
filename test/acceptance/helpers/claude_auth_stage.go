package acceptancehelpers

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// claudeAuthStateKeys are the ~/.claude.json fields that carry the Claude
// CLI's login: the OAuth account record, the stable user id, and an API-key
// login's key and its approval list. Nothing else of the operator's state —
// projects, history, per-directory trust — is copied.
var claudeAuthStateKeys = []string{"oauthAccount", "userID", "primaryApiKey", "customApiKeyResponses"}

// StageClaudeAuthHome copies the Claude CLI's credentials from realHome into
// dstHome, so a CLI run with HOME=dstHome is logged in without ever reading or
// writing the real home: ~/.claude/.credentials.json (the OAuth tokens on
// Linux) and the auth fields of ~/.claude.json. realHome is only read. It
// reports whether any credential was found to stage; a Claude CLI that keeps
// its tokens in the macOS keychain has none, and the caller decides whether
// that is a skip.
//
// dstHome must not be the real user home: the whole point is that the copy is
// what the test's CLI may rewrite (a token refresh does).
func StageClaudeAuthHome(realHome, dstHome string) (bool, error) {
	if underRealUserHome(dstHome) {
		return false, fmt.Errorf("acceptance: refusing to stage Claude auth into %s, which is under the real user home", dstHome)
	}
	found := false
	creds, err := os.ReadFile(filepath.Join(realHome, ".claude", ".credentials.json"))
	switch {
	case err == nil:
		dst := filepath.Join(dstHome, ".claude", ".credentials.json")
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return false, err
		}
		if err := os.WriteFile(dst, creds, 0o600); err != nil {
			return false, err
		}
		found = true
	case !os.IsNotExist(err):
		return false, err
	}

	hostState, err := loadClaudeState(filepath.Join(realHome, ".claude.json"))
	if err != nil {
		return false, err
	}
	statePath := filepath.Join(dstHome, ".claude.json")
	staged, err := loadClaudeState(statePath)
	if err != nil {
		return false, err
	}
	copied := false
	for _, key := range claudeAuthStateKeys {
		if v, ok := hostState[key]; ok {
			staged[key] = v
			copied = true
		}
	}
	if _, ok := hostState["primaryApiKey"]; ok {
		found = true
	}
	if copied {
		data, err := json.MarshalIndent(staged, "", "  ")
		if err != nil {
			return false, err
		}
		if err := os.MkdirAll(dstHome, 0o755); err != nil {
			return false, err
		}
		if err := os.WriteFile(statePath, append(data, '\n'), 0o600); err != nil {
			return false, err
		}
	}
	return found, nil
}
