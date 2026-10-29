package legacyop

import "github.com/gastownhall/gascity/internal/opjournal"

// CrashPoint names a point in a recorded legacy execution at which a process
// may die and leave the store as it stands: X12's kill points (DES-O0API §15
// OJ-8). A Recorder reports each one to its CrashPoints hook, which exists for
// crash evidence (cmd/gc's GC_OPS_FAILPOINT) and for in-process tests;
// production recorders carry none.
type CrashPoint string

const (
	// CrashPointAdmitted (K1): Admit holds an Execute admission whose op row
	// is durable, created by this call or adopted, and has not returned.
	// Nothing has answered the request; the legacy effect has not run.
	CrashPointAdmitted CrashPoint = "admitted"
	// CrashPointAnswered (K2): the surface answered the request and its
	// legacy effect has not started. Only the API's asynchronous verbs
	// reach it, from their delivery goroutine, through Admission.Reach.
	CrashPointAnswered CrashPoint = "answered"
	// CrashPointEffected (K3): Finish holds the legacy path's result and
	// nothing about the op has been written. Finish reports it for every
	// recorded execution it finishes, so it is also reached where a surface
	// releases an admission whose effect never started (an API handler that
	// fails after Admit leaves the op open for adoption): a kill there leaves
	// the store as K1 does.
	CrashPointEffected CrashPoint = "effected"
	// CrashPointClosing (K5) is right before each close attempt, Finish's
	// and the background retry's. A non-nil error from the hook fails that
	// attempt as a store error would (K5's injection).
	CrashPointClosing CrashPoint = "closing"
	// CrashPointClosed (K4): Finish's close landed; the surface has not
	// emitted its terminal event, written its answer or printed its output.
	// A close the background retry lands does not report it: Finish has
	// returned by then, so the surface may have answered, which is past K4.
	CrashPointClosed CrashPoint = "closed"
)

// CrashPointFunc is a recorder's crash-point hook. The recorder calls it on
// the executing goroutine with the point and the op as the recorder holds it
// (the closed op at CrashPointClosed). Its error matters only at
// CrashPointClosing; at every other point it is ignored. It may block (a test
// parks a request there) and must be safe for concurrent use: one recorder
// serves many requests. A panic in the hook unwinds through the recorder,
// which releases the request's in-flight entry on the way; at the points
// Finish and its background close retry report (CrashPointEffected,
// CrashPointClosing and CrashPointClosed) a runtime.Goexit releases it too.
// In-process crash tests that need the op held park instead of unwinding.
type CrashPointFunc func(p CrashPoint, op opjournal.Op) error

// Reach reports p for the admission's op to its recorder's hook. It does
// nothing for an admission that is not a recorded Execute, or whose recorder
// has no hook. The hook's error is ignored.
func (a *Admission) Reach(p CrashPoint) {
	if a == nil || a.Decision != Execute || !a.Recorded || a.rec == nil {
		return
	}
	_ = a.rec.crashPoint(p, a.Op)
}

// crashPoint calls the hook, if any, and returns its error.
func (r *Recorder) crashPoint(p CrashPoint, op opjournal.Op) error {
	if r.CrashPoints == nil {
		return nil
	}
	return r.CrashPoints(p, op)
}
