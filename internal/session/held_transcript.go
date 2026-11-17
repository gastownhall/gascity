package session

import (
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/runtime"
)

// heldTranscriptRootLevels bounds how many directories above a held file's
// own directory ResolveHeldTranscriptPath tries as the transcript's search
// root: enough for a provider that files its transcripts under dated
// subdirectories of its root (root/YYYY/MM/DD/file is three levels).
const heldTranscriptRootLevels = 4

// RuntimeHeldFiles returns the files the agent process of the session info
// describes holds open, as its routed backend names them
// (runtime.HeldFileProvider): only regular files the current user owns.
// supported is false, with no files and no error, when the backend cannot
// name them; err is the backend's failed read.
func (m *Manager) RuntimeHeldFiles(info Info) (files []string, supported bool, err error) {
	hf, ok := runtime.HeldFilesFor(m.sp, info.SessionName)
	if !ok {
		return nil, false, nil
	}
	files, err = hf.HeldFiles(info.SessionName)
	return files, true, err
}

// ResolveHeldTranscriptPath returns the transcript of info's conversation
// among held, the files the session's own agent process holds open, or ""
// when none or more than one is. It finds a transcript the configured search
// roots do not reach (a provider home outside them), and applies the same
// exact keyed rule ResolveKeyedTranscriptPaths does: a held file is the
// session's transcript only when its provider's keyed lookup, rooted at one
// of the file's own ancestor directories, resolves info's session key to
// exactly that file. Only held files whose path names the key are tried, so
// no unrelated file the process holds is ever opened. fallbackProvider is
// consulted as ResolveKeyedTranscriptPaths consults it.
func ResolveHeldTranscriptPath(info Info, held []string, fallbackProvider string) string {
	key := strings.TrimSpace(info.SessionKey)
	if key == "" || !HasKeyedTranscriptLookup(info, fallbackProvider) {
		return ""
	}
	found := ""
	for _, path := range held {
		if !filepath.IsAbs(path) || !strings.Contains(path, key) {
			continue
		}
		path = filepath.Clean(path)
		if path == found || !heldKeyedTranscript(info, path, fallbackProvider) {
			continue
		}
		if found != "" {
			return ""
		}
		found = path
	}
	return found
}

// heldKeyedTranscript reports whether info's keyed lookup, rooted at path's
// directory or one of its heldTranscriptRootLevels nearest ancestors,
// resolves to path. The nearest root is tried first.
func heldKeyedTranscript(info Info, path, fallbackProvider string) bool {
	root := filepath.Dir(path)
	for range heldTranscriptRootLevels + 1 {
		if ResolveKeyedTranscriptPaths([]Info{info}, []string{root}, fallbackProvider)[info.ID] == path {
			return true
		}
		parent := filepath.Dir(root)
		if parent == root {
			return false
		}
		root = parent
	}
	return false
}
