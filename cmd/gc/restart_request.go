package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// rowFirstRestart is one in-session restart request, recorded row first
// (SG-3; the reconciler-v2 owner's ruling of 2026-10-27). The session row's
// restart_requested and continuation_reset_pending are the request: they are
// written first, fenced to the caller's incarnation. The runtime's
// GC_RESTART_REQUESTED follows as a hint, in local mode only, and the
// controller poke comes last, also only a hint.
type rowFirstRestart struct {
	verb        string // the command, as its diagnostics name it
	cityPath    string
	sessionName string // the runtime session name: the hint's and the poke's key
	display     string // the session's address in diagnostics
	token       string // the caller's GC_INSTANCE_TOKEN; a capability, never printed
	// record writes the fenced row marker (worker.FencedResetHandle).
	record func(token string) error
	// check decides record's write without making it; nil decides nothing.
	check func(token string) error
	// hint sets the runtime's restart hint; nil on a worker site, whose
	// guard holds no runtime metadata.
	hint drainOps
	// poke signals the controller (pokeControllerForRestart).
	poke   func(cityPath string, key reconcilekey.Key) error
	stderr io.Writer
	// ops records the request as operation-journal ops (OJ-6); nil records
	// nothing.
	ops *restartOps
}

// restartNotice is what notifyController learned about the controller.
type restartNotice int

const (
	// restartNotified: the controller took the poke.
	restartNotified restartNotice = iota
	// restartRemote: a worker site; the remote controller acts at its next
	// reconcile, and no poke reaches it from the box.
	restartRemote
	// restartUnnotified: the poke failed; the controller acts at its next pass.
	restartUnnotified
)

// errRestartNoSessionStore: the session store did not open, so there is no
// row to record the request on.
var errRestartNoSessionStore = errors.New("the session store is unavailable")

// recordRestart writes the fenced row marker and then, outside a worker site,
// the runtime hint, whose failure is only a warning. A refused or failed row
// write returns its error and has written nothing anywhere. A recorded
// restart op closes from the row write, before the hint.
func (r rowFirstRestart) recordRestart() error {
	err := r.recordRow()
	r.ops.finishRestart(err)
	if err != nil {
		return err
	}
	r.setHint()
	return nil
}

// recordRow writes the fenced row marker: the request itself.
func (r rowFirstRestart) recordRow() error {
	if strings.TrimSpace(r.token) == "" {
		return session.ErrRestartRequestUnfenced
	}
	if r.record == nil {
		return errRestartNoSessionStore
	}
	return r.record(r.token)
}

// setHint sets the runtime's restart hint outside a worker site; a failure
// is a warning, since the row already records the request.
func (r rowFirstRestart) setHint() {
	if r.hint == nil {
		return
	}
	if err := r.hint.setRestartRequested(r.sessionName); err != nil && !restartFlagNotApplicable(err) {
		fmt.Fprintf(r.stderr, "%s: warning: restart recorded; the runtime restart hint was not set: %v\n", r.verb, err) //nolint:errcheck // best-effort stderr
	}
}

// refusedBeforeMail is the request's refusal decided before a self handoff
// sends its mail, so a refused caller sends none: no token, or a check of the
// row that refuses this incarnation, the row being closed or it and its live
// runtime naming another incarnation, or that refuses the write itself (a
// store with no conditional writer in a city that records only by
// compare-and-swap, or require meeting a store that cannot fence). A check
// that cannot decide (no session store or row, a failed read) refuses nothing
// here: the fenced write after the mail stays the authority and reports why
// it recorded nothing.
func (r rowFirstRestart) refusedBeforeMail() error {
	if strings.TrimSpace(r.token) == "" {
		return session.ErrRestartRequestUnfenced
	}
	if r.check == nil {
		return nil
	}
	if err := r.check(r.token); errors.Is(err, session.ErrStaleRestartRequest) || errors.Is(err, session.ErrSessionClosed) ||
		errors.Is(err, session.ErrRestartRequestNeedsConditionalWrite) || beads.IsConditionalWritesRequired(err) {
		return err
	}
	return nil
}

// notifyController pokes the controller once the request is recorded. It
// never fails the command: a worker site prints errWorkerSiteNoController,
// and a failed poke prints the pinned warning.
func (r rowFirstRestart) notifyController() restartNotice {
	// Name-only key: sessionName is authoritative, GC_SESSION_ID can be stale.
	err := r.poke(r.cityPath, reconcilekey.SessionNamed(r.sessionName))
	switch {
	case err == nil:
		return restartNotified
	case noteWorkerSitePoke(r.stderr, r.verb, err):
		return restartRemote
	default:
		fmt.Fprintf(r.stderr, "%s: warning: restart recorded; controller not notified, it will act on its next pass: %v\n", r.verb, err) //nolint:errcheck // best-effort stderr
		return restartUnnotified
	}
}

// notRecorded renders why recordRestart recorded nothing, without the verb.
// It never contains the token. A caller refused for its incarnation, or for
// naming none, is given no command that restarts the session: gc session
// reset is unfenced, so a process left over from an earlier incarnation that
// ran it would restart its successor.
func (r rowFirstRestart) notRecorded(err error) string {
	switch {
	case errors.Is(err, session.ErrStaleRestartRequest):
		return fmt.Sprintf("restart not recorded: this process does not run in session %s's current incarnation (neither its row nor its live runtime carries this process's GC_INSTANCE_TOKEN); a process left over from an earlier incarnation must exit, and only an operator on the controller host can restart the session", r.display)
	case errors.Is(err, session.ErrRestartRequestUnfenced):
		return fmt.Sprintf("restart not recorded: GC_INSTANCE_TOKEN is not set, so this process cannot prove which incarnation of session %s it runs in (an agent CLI that filters *TOKEN* variables from its shell removes it); keep GC_INSTANCE_TOKEN in the agent's environment, and only an operator on the controller host can restart the session", r.display)
	default:
		return fmt.Sprintf("restart not recorded: %v", err)
	}
}

// restartHintOps is the runtime hint target for cityPath: none on a worker
// site, whose guard refuses runtime metadata.
func restartHintOps(cityPath string, sp runtime.Provider) drainOps {
	if onWorkerSite(cityPath) {
		return nil
	}
	return newDrainOps(sp)
}

// sessionRestartRecorder records an in-session restart request on target's
// session row through the worker boundary, fenced to the caller's instance
// token. A nil sessStore records nothing (recordRestart reports it).
func sessionRestartRecorder(cityPath string, sessStore beads.Store, sp runtime.Provider, cfg *config.City, target string) func(token string) error {
	return sessionFencedReset(cityPath, sessStore, sp, cfg, target, worker.FencedResetHandle.ResetFenced)
}

// sessionRestartChecker decides, writing nothing, whether
// sessionRestartRecorder's write would record the request. A nil sessStore
// decides nothing.
func sessionRestartChecker(cityPath string, sessStore beads.Store, sp runtime.Provider, cfg *config.City, target string) func(token string) error {
	return sessionFencedReset(cityPath, sessStore, sp, cfg, target, worker.FencedResetHandle.CheckResetFenced)
}

// sessionFencedReset runs op with the caller's token on target's worker
// handle, which must be bound to a session row. A nil sessStore returns nil.
func sessionFencedReset(cityPath string, sessStore beads.Store, sp runtime.Provider, cfg *config.City, target string,
	op func(worker.FencedResetHandle, context.Context, string, func() bool) error,
) func(token string) error {
	if sessStore == nil {
		return nil
	}
	return func(token string) error {
		handle, err := workerHandleForSessionTargetWithConfig(cityPath, sessStore, sp, cfg, target)
		if err != nil {
			return err
		}
		fenced, ok := handle.(worker.FencedResetHandle)
		if !ok {
			return fmt.Errorf("session %q has no session row to record the restart on", target)
		}
		return op(fenced, context.Background(), token, restartRecordsOnlyByCAS(cityPath, cfg))
	}
}

// restartRecordsOnlyByCAS is the fenced restart write's casOnly for the city
// at cityPath: a v2 city records only by compare-and-swap and never takes the
// checked write (the v2 owner's sign-off of 2026-10-27, Q1). The session
// layer asks it only when a write is due on a store with no conditional
// writer, so the controller is asked its latched mode (cityRunsV2) only then.
// A worker site takes the checked write: no released bd fences, and its
// remote controller is not reachable from the box to ask. Q1 still holds
// there only because no v2 city has worker sites: v2 refuses the session
// runtimes that place sessions on one (exec and pack runtimes, PAR-EXEC; ssh,
// PAR-SSH; k8s and hybrid, PAR-K8S), explicit or by default. Lifting any of
// them must revisit this exemption.
func restartRecordsOnlyByCAS(cityPath string, cfg *config.City) func() bool {
	return func() bool {
		return !onWorkerSite(cityPath) && cityRunsV2(cityPath, cfg)
	}
}
