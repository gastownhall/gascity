package sessionlog

import (
	"os"
	"path/filepath"
)

// MiMo Code (Xiaomi's `mimo` CLI) is an OpenCode fork whose session exports
// share OpenCode's `{info, messages}` JSON shape byte-for-byte, so the
// readers delegate to the OpenCode parse/convert helpers. The only MiMo
// Code-specific surface is the transcript mirror location: the gascity
// plugin mirrors exports into ~/.local/share/gascity/mimocode-transcripts
// (env override GC_MIMOCODE_TRANSCRIPT_DIR on the plugin side).

// ReadMimoCodeFile reads a MiMo Code session export JSON file and converts it
// to the standard Session format used by gc session logs.
func ReadMimoCodeFile(path string, tailCompactions int) (*Session, error) {
	return ReadOpenCodeFile(path, tailCompactions)
}

// FindMimoCodeSessionFile searches MiMo Code JSON export directories for the
// most recently modified export whose embedded info.directory matches workDir.
func FindMimoCodeSessionFile(searchPaths []string, workDir string) string {
	return findOpenCodeExportInRoots(mergeMimoCodeSearchPaths(searchPaths), workDir)
}

func mergeMimoCodeSearchPaths(extraPaths []string) []string {
	return mergePaths(DefaultMimoCodeSearchPaths(), extraPaths)
}

// DefaultMimoCodeSearchPaths returns Gas City's default MiMo Code transcript
// mirror directory.
func DefaultMimoCodeSearchPaths() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(home, ".local", "share", "gascity", "mimocode-transcripts")}
}
