package sessionlog

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
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
	return mergePaths(append(DefaultZCodeSearchPaths(), DefaultZCodeArchiveSearchPaths()...), extraPaths)
}

// DefaultZCodeArchiveSearchPaths returns the archive root the zcode adapter
// moves superseded conversation scopes into on a reset.
//
// It is deliberately a different tree from the live mirror root: the live root
// is what the model browses, so a stale conversation sitting beside the fresh
// one there is the leak that was actually observed. Discovery still unions the
// archive, because gc's reset contract reads the pre-reset transcript AFTER the
// reset is issued — a rotated conversation has to stay resolvable by its own
// scope even though it is no longer the current one.
func DefaultZCodeArchiveSearchPaths() []string {
	if state := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); state != "" {
		return []string{filepath.Join(state, "gascity", "zcode", "archived-transcripts")}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return []string{filepath.Join(home, ".local", "state", "gascity", "zcode", "archived-transcripts")}
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

// FindZCodeSessionFileByID resolves a ZCode mirror by provider session id. The
// adapter names each mirror "<session-id>.json", so the id keys the file
// exactly; the embedded info.directory is still checked so an id from another
// work dir can never match. Returns "" when the id is empty or unsafe as a
// path component.
func FindZCodeSessionFileByID(searchPaths []string, workDir, sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	workDir = cleanOpenCodeWorkDir(workDir)
	if sessionID == "" || workDir == "" {
		return ""
	}
	if strings.Contains(sessionID, "..") || strings.ContainsAny(sessionID, `/\`) {
		return ""
	}
	// The adapter sanitizes the id before using it as a filename, so compare
	// against the same form or a legal id containing, say, a colon never
	// resolves.
	sessionID = sanitizeZCodeComponent(sessionID)
	var (
		bestPath string
		bestTime time.Time
	)
	for _, root := range mergeZCodeSearchPaths(searchPaths) {
		_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil || entry.IsDir() || entry.Name() != sessionID+".json" {
				return nil //nolint:nilerr // a missing root is simply no match
			}
			if cleanOpenCodeWorkDir(openCodeExportDirectory(path)) != workDir {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return nil
			}
			if bestPath == "" || info.ModTime().After(bestTime) {
				bestPath = path
				bestTime = info.ModTime()
			}
			return nil
		})
	}
	return bestPath
}

// ZCodeMirrorScope is the per-session, per-conversation subdirectory the zcode
// adapter writes its mirrors into: "<session-name>#<continuation-epoch>",
// sanitized the same way the adapter sanitizes it. Returns "" when either part
// is missing.
func ZCodeMirrorScope(sessionName, continuationEpoch string) string {
	sessionName = strings.TrimSpace(sessionName)
	if sessionName == "" {
		return ""
	}
	epoch := strings.TrimSpace(continuationEpoch)
	if epoch == "" {
		epoch = "1"
	}
	for _, r := range epoch {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return sanitizeZCodeComponent(sessionName) + "#" + epoch
}

// FindZCodeSessionFileByScope resolves the mirror for a session by the identity
// the adapter actually persists.
//
// gc never learns zcode's provider session id — the family has no session-id
// flag and no hook plugin, so session_key stays empty and every id-keyed lookup
// misses. But the adapter names its mirror directory
// "<session-name>#<continuation-epoch>", and both values live on the session
// bead, so this resolves a specific session's transcript exactly: two workers
// sharing a work dir each find their own, and a mirror left behind by a dead
// session in a reused work dir is not surfaced for a fresh one.
func FindZCodeSessionFileByScope(searchPaths []string, workDir, sessionName, continuationEpoch string) string {
	scope := ZCodeMirrorScope(sessionName, continuationEpoch)
	workDir = cleanOpenCodeWorkDir(workDir)
	if scope == "" || workDir == "" {
		return ""
	}
	var (
		bestPath        string
		bestTime        time.Time
		bestPending     string
		bestPendingTime time.Time
	)
	for _, root := range mergeZCodeSearchPaths(searchPaths) {
		dir := filepath.Join(root, scope)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".json") {
				continue
			}
			path := filepath.Join(dir, name)
			// The placeholder embeds its work dir (written through load_export
			// when a boot turn is canceled), so it is scoped like a real mirror.
			if cleanOpenCodeWorkDir(openCodeExportDirectory(path)) != workDir {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				continue
			}
			// The placeholder holds turns canceled before a session id existed.
			// A real mirror always wins — it adopts the placeholder on the first
			// successful turn — but a first-turn failure leaves ONLY the
			// placeholder, and it is then the sole record of what happened, so
			// it must stay resolvable instead of the scope reading as empty.
			if strings.HasPrefix(name, "pending-") {
				if bestPending == "" || info.ModTime().After(bestPendingTime) {
					bestPending = path
					bestPendingTime = info.ModTime()
				}
				continue
			}
			if bestPath == "" || info.ModTime().After(bestTime) {
				bestPath = path
				bestTime = info.ModTime()
			}
		}
	}
	if bestPath != "" {
		return bestPath
	}
	return bestPending
}

// sanitizeZCodeComponent mirrors the adapter's path-component sanitization
// (tr -c 'A-Za-z0-9._-' '_'), so a lookup and the writer agree on the name.
func sanitizeZCodeComponent(value string) string {
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}
