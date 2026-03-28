package session

import (
	"os"
	"path/filepath"
	"strings"
)

// RuntimeSessionID reads a provider-persisted session ID from workDir/.runtime/session_id.
// Returns an empty string when the file is absent or unreadable.
func RuntimeSessionID(workDir string) string {
	if strings.TrimSpace(workDir) == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(workDir, ".runtime", "session_id"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}
