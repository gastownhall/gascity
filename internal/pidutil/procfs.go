package pidutil

import "errors"

// ErrUnsupported reports that a process-identity mechanism does not exist on
// this platform.
//
// It is deliberately distinct from "the process does not match" and from "the
// process is gone". A caller that gates a destructive action, or that creates a
// singleton on the strength of a negative answer, must treat this as identity
// UNKNOWN and refuse the action: collapsing it into a negative turns a check
// that could not run into a false clearance.
var ErrUnsupported = errors.New("pidutil: unsupported on this platform")
