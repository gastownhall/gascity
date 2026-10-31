package proctable

import "errors"

// ErrUnsupported reports that this platform has no process-table mechanism
// behind the requested operation.
//
// It is not "nothing found". A scan that could not run has surfaced no rows AND
// proven nothing about what is out there, so a caller deciding whether it is
// safe to start a replacement must refuse rather than read the empty result as
// a clean host.
var ErrUnsupported = errors.New("proctable: unsupported on this platform")

// ErrScanIncomplete reports a read that saw every process but could not
// inspect some of them. The result it accompanies is still usable: it is
// missing only what inspection would have supplied, on the processes that
// could not be inspected. What it cannot do is establish that nothing matching
// is running, since any of those processes could be the one a caller is
// looking for.
var ErrScanIncomplete = errors.New("proctable: the process table could not be fully inspected")
