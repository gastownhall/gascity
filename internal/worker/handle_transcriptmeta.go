package worker

import (
	"log/slog"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/transcriptmeta"
)

const (
	transcriptSessionMetaRetryDelay    = time.Second
	transcriptSessionMetaRetryAttempts = 3
)

// recordTranscriptSessionMetaAfterTurn writes the sidecar after successful
// delivery. A Codex rollout can appear just after tmux receives that delivery,
// so the export-enabled supervisor retries a bounded number of initial misses.
// One-shot CLI processes leave transcriptmeta disabled and do no retry work.
func (h *SessionHandle) recordTranscriptSessionMetaAfterTurn() {
	if !transcriptmeta.Enabled() {
		return
	}
	if h.writeTranscriptSessionMeta() {
		return
	}
	h.scheduleTranscriptSessionMetaRetry()
}

func (h *SessionHandle) scheduleTranscriptSessionMetaRetry() {
	id := h.currentSessionID()
	if id == "" || !transcriptmeta.Enabled() {
		return
	}

	h.sidecarMu.Lock()
	if h.sidecarRetryID != id {
		h.sidecarRetryID = id
		h.sidecarRetryAttempts = 0
		h.sidecarRetryScheduled = false
	}
	if h.sidecarRetryScheduled || h.sidecarRetryAttempts >= transcriptSessionMetaRetryAttempts {
		h.sidecarMu.Unlock()
		return
	}
	h.sidecarRetryScheduled = true
	h.sidecarRetryAttempts++
	schedule := h.sidecarRetrySchedule
	h.sidecarMu.Unlock()

	if schedule == nil {
		schedule = func(delay time.Duration, retry func()) { time.AfterFunc(delay, retry) }
	}
	schedule(transcriptSessionMetaRetryDelay, func() {
		h.sidecarMu.Lock()
		if h.sidecarRetryID == id {
			h.sidecarRetryScheduled = false
		}
		h.sidecarMu.Unlock()

		if h.currentSessionID() != id || !transcriptmeta.Enabled() || h.writeTranscriptSessionMeta() {
			return
		}
		h.scheduleTranscriptSessionMetaRetry()
	})
}

// writeTranscriptSessionMeta records the worker's gc session id in a sidecar
// next to its provider transcript, so an out-of-band reader that sees only the
// transcript file can correlate it with this session. It is a cheap no-op
// unless correlation is enabled for the process, and is safe to call repeatedly
// from any successful-turn path: once the sidecar is confirmed current for the
// session id, a per-handle guard short-circuits before the keyed-path resolve
// and the write; before then the write is deferred (retried on a later call)
// until the transcript exists on disk.
//
// It resolves the transcript via the manager's KEYED path only — written solely
// when gc can map the transcript to this session 1:1 by a captured per-session
// id (claude/kimi/pi/antigravity by keyed path, and codex by its rollout-id
// filename suffix). It is skipped for gemini/opencode/mimocode, which have no
// 1:1 by-id lookup, so only the ambiguous workdir/mtime fallback would be
// available — and that could mis-attribute one session's transcript to another.
func (h *SessionHandle) writeTranscriptSessionMeta() bool {
	if !transcriptmeta.Enabled() {
		return true
	}
	id := h.currentSessionID()
	if id == "" {
		return true
	}
	// Fast path: once the sidecar is confirmed current for this session id, skip
	// the keyed-path resolve (a bead read plus transcript discovery, including
	// codex day-dir scans) and the write. The keyed path is stable once the
	// session key exists, so a matching id means the sidecar already holds it.
	h.sidecarMu.Lock()
	done := h.sidecarDoneID == id
	h.sidecarMu.Unlock()
	if done {
		return true
	}

	path, err := h.manager.KeyedTranscriptPath(id, h.adapter.SearchPaths)
	if err != nil {
		// Best-effort, never fatal to the turn. Log at debug so a persistent
		// resolve failure (e.g. a transient bead-store read error) is diagnosable,
		// mirroring the write-error path below; leave the guard unset so a later
		// call retries.
		slog.Debug("transcript session sidecar resolve failed", "session", id, "err", err)
		return false
	}
	if strings.TrimSpace(path) == "" {
		return false // no keyed path yet (key not persisted, or a workdir-only provider)
	}

	// id is the session bead id (currentSessionID == session.Info.ID), the same
	// value the event stream emits as session_id (via session.woke/session.stopped
	// and routed work beads) — so the sidecar and the stream join on a common key.
	ok, err := transcriptmeta.Write(path, id)
	if err != nil {
		// A real write failure (e.g. read-only/full fs); best-effort, never
		// fatal to the turn. Leave the guard unset so a later call retries.
		slog.Debug("transcript session sidecar write failed", "session", id, "err", err)
		return false
	}
	if !ok {
		return false // transcript not on disk yet — retry on a later turn
	}
	h.sidecarMu.Lock()
	h.sidecarDoneID = id
	h.sidecarMu.Unlock()
	return true
}
