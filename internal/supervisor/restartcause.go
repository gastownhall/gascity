package supervisor

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/fsys"
)

// Previous-exit classifications carried by the supervisor.started event.
// They describe how the previous supervisor instance on this machine
// exited, derived from the clean-shutdown handoff token below.
//
// Attribution is best-effort across binary up/downgrades and
// misattributes at most one start per mixed-version window: a
// token-unaware binary neither writes nor consumes the token, so its
// crash can be masked by a stale token from an earlier token-aware
// clean stop, and its clean stop reads as a crash on the next
// token-aware start. Both directions self-correct after one full cycle.
const (
	// PreviousExitClean means the previous instance completed its
	// orderly STOPPING path and left the handoff token behind.
	PreviousExitClean = "clean"
	// PreviousExitCrash means a previous instance ran but exited
	// without completing its STOPPING path (no handoff token).
	PreviousExitCrash = "crash"
	// PreviousExitUnknown means there is no evidence either way —
	// typically the first start on this machine, or after a reboot
	// cleared the runtime dir holding the prior instance's lock file.
	PreviousExitUnknown = "unknown"
)

// shutdownMarkerName is the filename of the clean-shutdown handoff token.
// The token is written atomically as the final step of the supervisor's
// STOPPING path and consumed (removed) by the next instance at startup.
// It is a handoff token between consecutive instances, not a liveness or
// status file: it never describes a running process, so it cannot go
// stale — its presence means exactly "the previous shutdown completed",
// and consuming it on startup re-arms the signal for the next cycle.
const shutdownMarkerName = "supervisor.shutdown-complete"

// ShutdownMarkerPath returns the clean-shutdown handoff token path under
// the given GC home directory. The token lives in the home directory
// (not the runtime dir) so clean-shutdown attribution survives reboots
// that wipe a tmpfs-backed runtime dir.
func ShutdownMarkerPath(home string) string {
	return filepath.Join(home, shutdownMarkerName)
}

// WriteShutdownMarker atomically writes the clean-shutdown handoff token
// under home. Called as the final step of the supervisor STOPPING path,
// after all managed cities have been stopped.
func WriteShutdownMarker(home string) error {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return fmt.Errorf("creating home dir for shutdown handoff token: %w", err)
	}
	if err := fsys.WriteFileAtomic(fsys.OSFS{}, ShutdownMarkerPath(home), []byte("clean\n"), 0o600); err != nil {
		return fmt.Errorf("writing shutdown handoff token: %w", err)
	}
	return nil
}

// ConsumePreviousExit removes the clean-shutdown handoff token under home
// and classifies how the previous supervisor instance exited. Callers
// must already hold the supervisor instance lock so exactly one instance
// consumes the token. priorInstanceRan reports whether any artifact of a
// previous instance exists (the supervisor lock file, observed before
// this instance recreated it); with the token absent it distinguishes a
// crashed prior instance from a first start.
//
// detail is non-nil only when the token could not be removed for a
// reason other than absence (permissions, IO error). The class is then
// PreviousExitUnknown — the classification refuses to guess — and the
// detail distinguishes an unremovable stale token from a true first
// start. The unremoved token stays armed, so a later start that does
// remove it reports a stale clean; surfacing the detail keeps that
// window observable.
func ConsumePreviousExit(home string, priorInstanceRan bool) (class string, detail error) {
	err := os.Remove(ShutdownMarkerPath(home))
	switch {
	case err == nil:
		return PreviousExitClean, nil
	case !os.IsNotExist(err):
		// The token may exist but cannot be removed (permissions, IO
		// error). Refuse to guess a classification.
		return PreviousExitUnknown, fmt.Errorf("removing shutdown handoff token: %w", err)
	case priorInstanceRan:
		return PreviousExitCrash, nil
	default:
		return PreviousExitUnknown, nil
	}
}
