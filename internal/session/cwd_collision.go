package session

import (
	"context"
	"errors"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/pidutil"
)

// LivenessScanner reports the live working directories of processes on the
// host, backing the working-directory collision guard (ga-ighomh.1).
// Production callers inject pidutil.LiveCWDs; tests inject a deterministic
// stub.
type LivenessScanner func() pidutil.LiveState

// ErrWorkDirCollision reports that a session create or resume was refused
// because its working directory is already occupied by another live session.
var ErrWorkDirCollision = errors.New("working directory is occupied by another live session")

// ErrWorkDirLivenessUnavailable reports that a session create or resume was
// refused because the live-process scan or the session-bead read could not be
// completed, so working-directory safety cannot be verified.
var ErrWorkDirLivenessUnavailable = errors.New("cannot verify working directory is unoccupied: liveness scan unavailable")

// checkNoCWDCollision refuses to start or resume session id, whose bead is b,
// in workDir when another live session occupies it.
func (m *Manager) checkNoCWDCollision(_ context.Context, _ string, _ beads.Bead, _ string) error {
	return nil
}

// RED scaffold: referenced until the start paths call the guard.
var _ = (*Manager).checkNoCWDCollision
