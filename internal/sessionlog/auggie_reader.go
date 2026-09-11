package sessionlog

// ReadAuggieFile reads a captured Auggie ACP JSONL session and converts it to
// the standard Session format used by GC session logs.
func ReadAuggieFile(path string, tailCompactions int) (*Session, error) {
	return readCapturedACPFile(path, tailCompactions, "auggie")
}

// DefaultAuggieSearchPaths intentionally returns no local default. Auggie
// documents ACP and hook surfaces, but GC does not yet rely on a stable
// retrospective local transcript store.
func DefaultAuggieSearchPaths() []string {
	return nil
}

// FindAuggieSessionFileByID resolves a captured Auggie ACP JSONL file by
// session ID when one has been written into the configured transcript search
// paths.
func FindAuggieSessionFileByID(searchPaths []string, workDir, sessionID string) string {
	return findCapturedACPSessionFileByID(searchPaths, DefaultAuggieSearchPaths(), workDir, sessionID)
}

// FindAuggieSessionFile searches configured Auggie capture directories for the
// newest ACP JSONL file whose recorded cwd matches workDir.
func FindAuggieSessionFile(searchPaths []string, workDir string) string {
	return findCapturedACPSessionFile(searchPaths, DefaultAuggieSearchPaths(), workDir)
}
