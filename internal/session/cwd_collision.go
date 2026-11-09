package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/pathutil"
	"github.com/gastownhall/gascity/internal/pidutil"
	"github.com/gastownhall/gascity/internal/runtime"
)

// LivenessScanner reports the live working directories of processes on the
// host, backing the working-directory collision guard (ga-ighomh.1).
// Production callers inject pidutil.LiveCWDs; tests inject a deterministic
// stub.
type LivenessScanner func() pidutil.LiveState

// ErrWorkDirCollision reports that a session start or respawn was refused
// because its working directory is already occupied by another live session
// (ga-ighomh.1).
var ErrWorkDirCollision = errors.New("working directory is occupied by another live session")

// ErrWorkDirLivenessUnavailable reports that a session start or respawn was
// refused because the live-process scan could not be completed, so
// working-directory safety cannot be verified. The guard fails closed rather
// than risk two sessions sharing a directory (ga-ighomh.1).
var ErrWorkDirLivenessUnavailable = errors.New("cannot verify working directory is unoccupied: liveness scan unavailable")

// checkNoCWDCollision refuses to start or resume a session whose working
// directory is already occupied by another live session. Liveness is
// established by any of three independent signals: the runtime provider's
// own IsRunning check (catches sessions this Manager started), a PID cross-
// reference between the host-wide live-process scan and each candidate's own
// known runtime PID (catches sessions started by another Manager instance or
// process entirely, attributed to the correct candidate), or — only once every
// candidate has been checked and none could be positively attributed — the
// scan confirming some OTHER session-owned live process (any live PID the
// process table attributes to a Gas City session via GC_SESSION_ID, other
// than id itself) occupies the directory. That last, unattributed signal
// still refuses but is recorded without naming a specific candidate, since the
// scan alone cannot say which bead (if any) it actually belongs to.
//
// The fallback is deliberately scoped to session-owned PIDs only: a live
// process with no relationship to any Gas City session at all (the
// supervisor, the controller, an operator's own shell) merely having its cwd
// at the directory must never be enough to refuse a start (ga-thkwp5 B).
//
// The guard fails closed when the host-wide scan could not be completed or
// when the session-bead read was only partial, refusing rather than risking
// two live sessions sharing a directory (ga-ighomh.1, ga-c5yi8m).
func (m *Manager) checkNoCWDCollision(_ context.Context, id string, b beads.Bead, workDir string) error {
	workDir = strings.TrimSpace(workDir)
	if workDir == "" {
		// The launch config names no directory: the process starts in the
		// bead's recorded one.
		workDir = strings.TrimSpace(b.Metadata["work_dir"])
	}
	if workDir == "" {
		return nil
	}
	live := m.livenessScanner()
	if !live.Scanned {
		m.recordCWDRefusal(id, events.SessionStartRefusedReasonLivenessUnavailable, "")
		return fmt.Errorf("%w: %s", ErrWorkDirLivenessUnavailable, workDir)
	}
	normalizedWorkDir := pathutil.NormalizePathForCompare(workDir)
	candidates, err := m.sameWorkDirSessionBeadsChecked(b, "", workDir)
	if err != nil {
		if !beads.IsPartialResult(err) {
			return fmt.Errorf("checking working directory collisions: %w", err)
		}
		// A degraded bead read cannot be told apart from "no other
		// candidates": the dropped rows may include the live occupant's own
		// bead, which would also keep the unattributed fallback below from
		// ever running. Treat it like an unavailable liveness scan.
		m.recordCWDRefusal(id, events.SessionStartRefusedReasonLivenessUnavailable, "")
		return fmt.Errorf("%w: %s: %w", ErrWorkDirLivenessUnavailable, workDir, err)
	}
	hasOtherCandidate := false
	for _, other := range candidates {
		if other.ID == id {
			continue
		}
		hasOtherCandidate = true
		if m.sp.IsRunning(sessionName(other.ID, other)) || m.candidateConfirmedLiveByPID(other, live, normalizedWorkDir) {
			m.recordCWDRefusal(id, events.SessionStartRefusedReasonCollision, other.ID)
			return fmt.Errorf("%w: %s is occupied by session %s", ErrWorkDirCollision, workDir, other.ID)
		}
	}
	if hasOtherCandidate && m.anySessionOwnedPIDLiveAt(id, live, normalizedWorkDir) {
		m.recordCWDRefusal(id, events.SessionStartRefusedReasonCollision, "")
		return fmt.Errorf("%w: %s is occupied by an unattributed live session process", ErrWorkDirCollision, workDir)
	}
	return nil
}

// anySessionOwnedPIDLiveAt reports whether any process the process table
// attributes to some Gas City session (a live runtime with a non-empty
// SessionID other than id) is live at normalizedWorkDir, cross-referenced by
// PID against the host-wide scan. This is the unattributed fallback signal in
// checkNoCWDCollision: it cannot name which candidate bead is occupying the
// directory, but it still distinguishes a real Gas City session occupant from
// a process with no relationship to any session. A process-table error is
// logged and this signal alone then reports false: the caller's other signals
// and its fail-closed scan check still apply.
func (m *Manager) anySessionOwnedPIDLiveAt(id string, live pidutil.LiveState, normalizedWorkDir string) bool {
	scanner, ok := runtime.AsProcessTableScanner(m.sp)
	if !ok {
		return false
	}
	found, err := scanner.FindRuntimesBySessionID("")
	if err != nil {
		log.Printf("session: scanning all session-owned runtimes for cwd collision fallback: %v", err)
	}
	for _, rt := range found {
		if rt.SessionID == "" || rt.SessionID == id || rt.PID == 0 {
			continue
		}
		if cwd, ok := live.PIDCWDs[rt.PID]; ok && cwd == normalizedWorkDir {
			return true
		}
	}
	return false
}

// candidateConfirmedLiveByPID reports whether other's own known runtime PID
// (from the runtime provider's process-table scan, when it supports one) is
// among the live PIDs the scan found at normalizedWorkDir. This is the
// positive-attribution signal that lets checkNoCWDCollision name a specific
// candidate instead of guessing among several sharing the same historical
// work_dir (ga-9x4z1g.1 FR3). A process-table error is logged and this
// candidate simply cannot be confirmed by PID; the caller falls back to its
// other signals.
func (m *Manager) candidateConfirmedLiveByPID(other beads.Bead, live pidutil.LiveState, normalizedWorkDir string) bool {
	scanner, ok := runtime.AsProcessTableScanner(m.sp)
	if !ok {
		return false
	}
	runtimes, err := scanner.FindRuntimesBySessionID(other.ID)
	if err != nil {
		log.Printf("session: finding runtimes for candidate %s: %v", other.ID, err)
	}
	for _, rt := range runtimes {
		if rt.PID == 0 {
			continue
		}
		if cwd, ok := live.PIDCWDs[rt.PID]; ok && cwd == normalizedWorkDir {
			return true
		}
	}
	return false
}

// recordCWDRefusal emits a session.start_refused_cwd event for a working
// directory collision guard refusal. Best-effort: a marshal failure is
// dropped rather than blocking the refusal error it accompanies.
func (m *Manager) recordCWDRefusal(id, reason, collidingSessionID string) {
	payload, err := json.Marshal(events.SessionStartRefusedCwdPayload{
		Reason:             reason,
		CollidingSessionID: collidingSessionID,
	})
	if err != nil {
		return
	}
	m.eventRecorder.Record(events.Event{
		Type:    events.SessionStartRefusedCwd,
		Actor:   "session",
		Subject: id,
		Payload: payload,
	})
}
