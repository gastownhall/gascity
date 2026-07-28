package session

import (
	"errors"

	"github.com/gastownhall/gascity/internal/pidutil"
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
