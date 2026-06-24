package worker

import (
	"log/slog"
	"strings"

	"github.com/gastownhall/gascity/internal/transcriptmeta"
)

// writeTranscriptSessionMeta records the worker's gc session id in a sidecar
// next to its provider transcript, so an out-of-band reader that sees only the
// transcript file can correlate it with this session. It is a cheap no-op
// unless correlation is enabled for the process, and is safe to call repeatedly
// from any successful-turn path: a per-handle guard skips the work once the
// sidecar is current, and the write is deferred (retried on a later call) until
// the transcript exists on disk.
//
// It resolves the transcript via the manager's KEYED path only — written solely
// when gc can map the transcript to this session 1:1 by a captured per-session
// id (claude/kimi/pi/antigravity by keyed path, and codex by its rollout-id
// filename suffix). It is skipped for gemini/opencode/mimocode, which have no
// 1:1 by-id lookup, so only the ambiguous workdir/mtime fallback would be
// available — and that could mis-attribute one session's transcript to another.
func (h *SessionHandle) writeTranscriptSessionMeta() {
	if !transcriptmeta.Enabled() {
		return
	}
	id := h.currentSessionID()
	if id == "" {
		return
	}
	path, err := h.manager.KeyedTranscriptPath(id, h.adapter.SearchPaths)
	if err != nil || strings.TrimSpace(path) == "" {
		return
	}

	// id is the session bead id (currentSessionID == session.Info.ID), the same
	// value the event stream emits as session_id (via session.woke/session.stopped
	// and routed work beads) — so the sidecar and the stream join on a common key.
	key := path + "\x00" + id
	h.sidecarMu.Lock()
	done := h.sidecarLast == key
	h.sidecarMu.Unlock()
	if done {
		return
	}

	ok, err := transcriptmeta.Write(path, id)
	if err != nil {
		// A real write failure (e.g. read-only/full fs); best-effort, never
		// fatal to the turn. Leave the guard unset so a later call retries.
		slog.Debug("transcript session sidecar write failed", "session", id, "err", err)
		return
	}
	if !ok {
		return // transcript not on disk yet — retry on a later turn
	}
	h.sidecarMu.Lock()
	h.sidecarLast = key
	h.sidecarMu.Unlock()
}
