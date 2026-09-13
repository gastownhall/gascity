// Package transcriptmeta records, in a small sidecar file next to a provider
// transcript, the gc session id that produced that transcript. A reader that
// sees only the transcript file by its path — with no access to gc's internal
// session state — can read the sidecar to recover the session id and correlate
// the transcript with gc's event stream.
//
// Writing is off unless SetEnabled(true) is called for the process. Installs
// that do not opt into event-stream correlation never create these files. The
// gate is per-process: it is armed in the supervisor, which delivers the runs
// this targets. A one-shot CLI process that delivers a turn in-process (no
// supervisor, e.g. GC_NO_API) does not arm it, so those turns write no sidecar
// until the supervisor next touches the transcript — an accepted coverage gap,
// not a correctness risk (it only ever under-writes, never writes when off).
package transcriptmeta

import (
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/gastownhall/gascity/internal/fsys"
)

// Suffix is appended to a transcript's resolved path to form its sidecar path.
// The sidecar lives next to the transcript so a reader holding only the
// transcript path can find it by string concatenation, with no shared key
// derivation to keep in lockstep.
const Suffix = ".gcmeta"

// perm keeps the sidecar owner-only. It carries an opaque id, not a secret, but
// there is no reason to widen it.
const perm = 0o600

var enabled atomic.Bool

// SetEnabled turns sidecar writing on or off for the process. The composition
// root sets it once at startup, when event-stream correlation is configured.
func SetEnabled(on bool) { enabled.Store(on) }

// Enabled reports whether sidecar writing is on for this process.
func Enabled() bool { return enabled.Load() }

// Write records gcSessionID in the sidecar for the transcript at
// transcriptPath. ok reports whether the sidecar now exists and holds
// gcSessionID — true when it was written or was already current, false when
// there was nothing to do yet (writing disabled, blank argument, or the
// transcript not yet on disk). A false/nil result invites the caller to retry
// on a later turn once the provider has written the transcript; a non-nil err
// is a real fault — a symlink resolution that failed for a reason other than the
// transcript not existing yet, or a write failure (e.g. a read-only or full
// filesystem).
//
// The transcript path is symlink-resolved before the suffix is appended so the
// sidecar lands beside the real file, matching a reader that resolves symlinks
// independently. The write is atomic and is skipped when the on-disk contents
// already match, so repeated calls do not churn the file.
func Write(transcriptPath, gcSessionID string) (ok bool, err error) {
	if !enabled.Load() {
		return false, nil
	}
	transcriptPath = strings.TrimSpace(transcriptPath)
	gcSessionID = strings.TrimSpace(gcSessionID)
	if transcriptPath == "" || gcSessionID == "" {
		return false, nil
	}
	resolved, err := filepath.EvalSymlinks(transcriptPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// The transcript is not on disk yet. There is nothing to
			// correlate; the caller retries once the provider writes it.
			return false, nil
		}
		// A genuine fault — a permission error on an ancestor, a symlink loop,
		// or an I/O error. Surface it so the caller's debug log can expose a
		// persistent filesystem fault instead of retrying silently forever.
		return false, err
	}
	if err := fsys.WriteFileIfChangedAtomic(fsys.OSFS{}, resolved+Suffix, []byte(gcSessionID+"\n"), perm); err != nil {
		return false, err
	}
	return true, nil
}
