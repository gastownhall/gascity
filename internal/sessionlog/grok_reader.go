package sessionlog

// ReadGrokFile reads a captured Grok ACP JSONL session and converts it to the
// standard Session format used by GC session logs.
func ReadGrokFile(path string, tailCompactions int) (*Session, error) {
	return readCapturedACPFile(path, tailCompactions, "grok")
}

// DefaultGrokSearchPaths intentionally returns no local default. Grok documents
// ACP and streaming JSON interfaces, but GC does not yet rely on a stable
// retrospective local transcript store.
func DefaultGrokSearchPaths() []string {
	return nil
}

// FindGrokSessionFileByID resolves a captured Grok ACP JSONL file by session
// ID when one has been written into the configured transcript search paths.
func FindGrokSessionFileByID(searchPaths []string, workDir, sessionID string) string {
	return findCapturedACPSessionFileByID(searchPaths, DefaultGrokSearchPaths(), workDir, sessionID)
}

// FindGrokSessionFile searches configured Grok capture directories for the
// newest ACP JSONL file whose recorded cwd matches workDir.
func FindGrokSessionFile(searchPaths []string, workDir string) string {
	return findCapturedACPSessionFile(searchPaths, DefaultGrokSearchPaths(), workDir)
}
