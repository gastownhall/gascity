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

// ZCodeMirrorScope is the name-only, per-conversation subdirectory the zcode
// adapter writes its mirrors into when it holds no session bead id:
// "<session-name>#<continuation-epoch>", sanitized the same way the adapter
// sanitizes it. Returns "" when either part is missing.
func ZCodeMirrorScope(sessionName, continuationEpoch string) string {
	sessionName = strings.TrimSpace(sessionName)
	epoch := zcodeScopeEpoch(continuationEpoch)
	if sessionName == "" || epoch == "" {
		return ""
	}
	return sanitizeZCodeComponent(sessionName) + "#" + epoch
}

// ZCodeSeatMirrorScope is the per-seat mirror subdirectory:
// "<session-name>@<session-bead-id>#<continuation-epoch>".
//
// Two seats can share a session name and continuation epoch — a pool slot
// re-seated within one run does exactly that — so the name-only scope
// collapsed both onto one mirror and one seat's transcript showed for both.
// The adapter therefore keys its state by the session bead id gc exports to
// the pane as GC_SESSION_ID whenever it has one; a resumed seat keeps its bead
// id, which is what makes the key stable across restarts. "@" cannot survive
// the adapter's component sanitization, so it is an unambiguous delimiter.
// Returns "" when any part is missing, so a caller falls back to the name-only
// scope.
func ZCodeSeatMirrorScope(sessionName, sessionBeadID, continuationEpoch string) string {
	sessionName = strings.TrimSpace(sessionName)
	sessionBeadID = strings.TrimSpace(sessionBeadID)
	epoch := zcodeScopeEpoch(continuationEpoch)
	if sessionName == "" || sessionBeadID == "" || epoch == "" {
		return ""
	}
	return sanitizeZCodeComponent(sessionName) + "@" + sanitizeZCodeComponent(sessionBeadID) + "#" + epoch
}

// zcodeScopeEpoch normalizes the continuation epoch the way the adapter does
// (empty means the first epoch) and returns "" for anything non-numeric.
func zcodeScopeEpoch(continuationEpoch string) string {
	epoch := strings.TrimSpace(continuationEpoch)
	if epoch == "" {
		return "1"
	}
	for _, r := range epoch {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return epoch
}

// FindZCodeSessionFileByScope resolves the mirror for a session by the identity
// the adapter actually persists.
//
// gc never learns zcode's provider session id — the family has no session-id
// flag and no hook plugin, so session_key stays empty and every id-keyed lookup
// misses. But the adapter names its mirror directory from the session name,
// the session bead id and the continuation epoch (ZCodeSeatMirrorScope), and
// all three live on the session bead, so this resolves a specific seat's
// transcript exactly: two seats sharing a work dir — or a session name — each
// find their own, and a mirror left behind by a dead session in a reused work
// dir is not surfaced for a fresh one.
//
// Mirrors written before the scope carried the seat, or by an adapter that was
// not handed a session bead id, live under the name-only scope
// (ZCodeMirrorScope). That scope is consulted only when the seat scope holds
// nothing for this work dir — including when no seat id was supplied: once the
// seat has written anything for this work dir, a name-only mirror is a sibling
// seat's or stale.
func FindZCodeSessionFileByScope(searchPaths []string, workDir, sessionName, sessionBeadID, continuationEpoch string) string {
	workDir = cleanOpenCodeWorkDir(workDir)
	if workDir == "" {
		return ""
	}
	roots := mergeZCodeSearchPaths(searchPaths)
	if seat := ZCodeSeatMirrorScope(sessionName, sessionBeadID, continuationEpoch); seat != "" {
		if path := findZCodeMirrorInScope(roots, seat, workDir); path != "" {
			return path
		}
	}
	scope := ZCodeMirrorScope(sessionName, continuationEpoch)
	if scope == "" {
		return ""
	}
	return findZCodeMirrorInScope(roots, scope, workDir)
}

// findZCodeMirrorInScope returns the newest mirror for workDir under scope
// across roots, preferring a real mirror over a canceled-boot placeholder.
func findZCodeMirrorInScope(roots []string, scope, workDir string) string {
	var (
		bestPath        string
		bestTime        time.Time
		bestPending     string
		bestPendingTime time.Time
	)
	for _, root := range roots {
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
