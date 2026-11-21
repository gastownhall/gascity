package proctable

import "errors"

// ErrProcessGone means a process identity cannot be read because that process
// no longer exists. Callers may treat it as a safe stale-target outcome.
var ErrProcessGone = errors.New("process is gone")
