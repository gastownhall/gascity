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
	// hint sets the runtime's restart hint; nil on a worker site, whose
	// guard holds no runtime metadata.
	hint drainOps
	// poke signals the controller (pokeControllerForRestart).
	poke   func(cityPath string, key reconcilekey.Key) error
	stderr io.Writer
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
// write returns its error and has written nothing anywhere.
func (r rowFirstRestart) recordRestart() error {
	if strings.TrimSpace(r.token) == "" {
		return session.ErrRestartRequestUnfenced
	}
	if r.record == nil {
		return errRestartNoSessionStore
	}
	if err := r.record(r.token); err != nil {
		return err
	}
	if r.hint != nil {
		if err := r.hint.setRestartRequested(r.sessionName); err != nil && !restartFlagNotApplicable(err) {
			fmt.Fprintf(r.stderr, "%s: warning: restart recorded; the runtime restart hint was not set: %v\n", r.verb, err) //nolint:errcheck // best-effort stderr
		}
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
// It never contains the token.
func (r rowFirstRestart) notRecorded(err error) string {
	switch {
	case errors.Is(err, session.ErrStaleRestartRequest):
		return fmt.Sprintf("restart not recorded: this process does not run in session %s's current incarnation (neither its row nor its live runtime carries this process's GC_INSTANCE_TOKEN); from outside the session, gc session reset %s restarts it", r.display, r.display)
	case errors.Is(err, session.ErrRestartRequestUnfenced):
		return "restart not recorded: GC_INSTANCE_TOKEN is not set, so this process cannot prove which incarnation of the session it runs in (an agent CLI that filters *TOKEN* variables from its shell removes it); from outside the session, gc session reset restarts it"
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
		return fenced.ResetFenced(context.Background(), token)
	}
}
