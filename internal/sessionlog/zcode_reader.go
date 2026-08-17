package sessionlog

import (
	"os"
	"path/filepath"
)

// ZCode (Z.ai's GLM harness) keeps its sessions in a sqlite database under
// $HOME/.zcode that it will not relocate, so transcripts reach gc through the
// export mirror the zcode-repl adapter writes after every completed turn
// (internal/worker/adapters/zcode). The mirror is authored in OpenCode's
// `{info, messages}` shape byte-for-byte, so the readers delegate to the
// OpenCode parse/convert helpers. The only ZCode-specific surface is the mirror
// location: ~/.local/share/gascity/zcode-transcripts (env override
// GC_ZCODE_TRANSCRIPT_DIR on the adapter side).

// ReadZCodeFile reads a ZCode session export JSON file and converts it to the
// standard Session format used by gc session logs.
func ReadZCodeFile(path string, tailCompactions int) (*Session, error) {
	return ReadOpenCodeFile(path, tailCompactions)
}

// FindZCodeSessionFile searches ZCode JSON export directories for the most
// recently modified export whose embedded info.directory matches workDir.
func FindZCodeSessionFile(searchPaths []string, workDir string) string {
	return findOpenCodeExportInRoots(mergeZCodeSearchPaths(searchPaths), workDir)
}

func mergeZCodeSearchPaths(extraPaths []string) []string {
	return mergePaths(DefaultZCodeSearchPaths(), extraPaths)
}

// DefaultZCodeSearchPaths returns Gas City's default ZCode transcript mirror
// directory.
func DefaultZCodeSearchPaths() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(home, ".local", "share", "gascity", "zcode-transcripts")}
}
