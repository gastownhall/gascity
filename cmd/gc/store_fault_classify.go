package main

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"syscall"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/dispatch"
)

// One vocabulary for "the store did not answer". Two consumers read it, and
// they must not drift: gc hook's work-query classification
// (classifyWorkQueryStoreUnavailable: exit 2 and the store-unavailable token,
// not an empty queue) and the storage gate's transient faults
// (isTransientStoreFault: the drain fence fails open, the CLI does not
// memoize). A marker added for one classifies for the other.

// storeNoAnswerMarkers extends the bd tables (bdTransportRetryableMarkers,
// bdConnectionErrorMarkers) with answers that mean "try again" rather than
// "no": a hosted bd serve that is busy, has lost its database, or sits behind
// a gateway that could not reach it; a client that cannot reach bd serve at
// all; a bd process killed under it; a process out of file descriptors. They
// are kept out of bdTransportRetryableMarkers because the managed retry reads
// that table and re-runs any bd command, writes included.
var storeNoAnswerMarkers = []string{
	"answered 502",
	"answered 503",
	"answered 504",
	"db_unavailable",
	"cannot reach bd serve",
	"signal: killed",
	"too many open files",
}

// storeGotNoAnswer reports whether err (with lowerText, the lowercased text a
// caller chose to scan) says the store gave no answer: typed timeouts and
// disconnects, the dispatch availability tier, and the marker tables.
func storeGotNoAnswer(err error, lowerText string) bool {
	if err == nil {
		return false
	}
	for _, sentinel := range []error{beads.ErrStoreUnavailable, context.DeadlineExceeded, os.ErrDeadlineExceeded, io.ErrUnexpectedEOF} {
		if errors.Is(err, sentinel) {
			return true
		}
	}
	for _, errno := range []syscall.Errno{syscall.ETIMEDOUT, syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.EHOSTUNREACH, syscall.ENETUNREACH, syscall.EMFILE} {
		if errors.Is(err, errno) {
			return true
		}
	}
	// A syscall.Errno is a net.Error too, and EAGAIN (EWOULDBLOCK) is a
	// timeout: the no-answer gc hook --help promises it.
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	if dispatch.ClassifyControllerError(workQueryFailure{err: err, text: lowerText}) == dispatch.TierAvailability {
		return true
	}
	return messageHasMarker(lowerText, bdTransportRetryableMarkers) ||
		messageHasMarker(lowerText, bdConnectionErrorMarkers) ||
		messageHasMarker(lowerText, storeNoAnswerMarkers)
}

// storeStandingMarkers are answers that hold until an operator acts, even when
// a no-answer marker also appears in the text (they take precedence in the
// gate; gc hook does not read them, so a hook's work query that hits one still
// gets the store-unavailable token, as it did before this table existed): a host name that does not
// resolve (a typo in config, which "dial tcp" would otherwise match), refused
// credentials, a full or failing disk, a corrupt database.
var storeStandingMarkers = []string{
	"no such host",
	"answered 401",
	"answered 403",
	"unauthenticated",
	"disk i/o error",
	"database disk image is malformed",
	"no space left on device",
	// A TLS or URL misconfiguration (an expired or untrusted certificate, a
	// mistyped scheme) reaches gc as "cannot reach bd serve", a no-answer
	// marker, but holds until an operator fixes it.
	"x509:",
	"tls:",
	"unsupported protocol scheme",
}

// isTransientStoreFault reports whether a read the storage gate made failed
// for a reason that may clear on its own: store I/O that got no answer
// (storeGotNoAnswer). It is deliberately narrow on the other side. A decode
// error on a local file (a malformed manifest annotation, an undecodable
// note), a permission error, a corrupt database, a misconfigured store, a
// store without conditional writes and a busy mount point are standing facts
// about the city that hold until an operator acts, so they are not transient
// and the drain fence stays closed on them.
func isTransientStoreFault(err error) bool {
	if err == nil {
		return false
	}
	// EBUSY (a busy mount point) needs no guard: nothing in storeGotNoAnswer
	// reads it, so it is standing by default, and the table test pins it.
	if errors.Is(err, os.ErrPermission) || errors.Is(err, beads.ErrConditionalWriteUnsupported) ||
		errors.Is(err, syscall.ENOSPC) {
		return false
	}
	text := strings.ToLower(err.Error())
	if messageHasMarker(text, storeStandingMarkers) {
		return false
	}
	return storeGotNoAnswer(err, text)
}
