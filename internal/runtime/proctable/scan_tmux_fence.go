package proctable

import (
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/runtime"
)

// The shared half of ScanBySessionIDOutsideTmux's fence, which the Linux
// /proc walk (scan_tmux_fence_linux.go) and the Darwin ps snapshot
// (scan_darwin_records.go) each decide per root. It carries no build tag, so
// Linux CI runs the Darwin half's tests too.

// errRootGone marks a root that exited after the scan reported it. The fence
// leaves it out with no error, as the scan skips a process that dies under
// it.
var errRootGone = errors.New("the root exited")

// fenceRoots keeps the roots of found that hostedBySessionTmux reports no
// tmux carrying a session's GC_SESSION_ID hosts. A root whose ancestry it
// cannot decide is left out with an EntryError, since such a tmux may host
// it; a root that exited since the scan (errRootGone) is left out silently.
func fenceRoots(found []runtime.LiveRuntime, hostedBySessionTmux func(pid int) (bool, error)) ([]runtime.LiveRuntime, error) {
	kept := make([]runtime.LiveRuntime, 0, len(found))
	var errs []error
	for _, live := range found {
		hosted, err := hostedBySessionTmux(live.PID)
		switch {
		case errors.Is(err, errRootGone):
		case err != nil:
			errs = append(errs, &EntryError{PID: live.PID, Err: fmt.Errorf("reading the ancestry of pid %d: %w", live.PID, err)})
		case !hosted:
			kept = append(kept, live)
		}
	}
	return kept, errors.Join(errs...)
}
