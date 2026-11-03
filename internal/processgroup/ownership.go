package processgroup

import "errors"

// ErrUnsupported reports that a process-control operation has no implementation
// on this platform. Callers must surface it rather than treating it as success:
// a cleanup that did not run is not a cleanup that found nothing to do.
var ErrUnsupported = errors.New("processgroup: unsupported on this platform")

// ErrNotOwned reports that the command was not started through StartCommand, so
// no ownership boundary exists to terminate. Killing the direct process alone
// cannot reap descendants, so the operation is refused instead of half-done.
var ErrNotOwned = errors.New("processgroup: command has no ownership boundary")

// Persistence selects what happens to an owned process tree when the launching
// process releases ownership — by calling ReleaseCommand or simply by exiting.
//
// The distinction is a product requirement, not a tuning knob. Canceling a
// helper must reap the helper's whole tree, and detaching a UI from a long-lived
// agent server must leave that server running.
type Persistence int

const (
	// KillOnRelease reaps the entire owned tree as soon as ownership is
	// released. Use it for helper commands whose lifetime is bounded by the
	// operation that spawned them: credential and auth helpers, pack and git
	// helpers, setup commands, transient probes.
	KillOnRelease Persistence = iota

	// SurviveRelease leaves the tree running when ownership is released, so
	// the launcher can exit or detach without taking the tree down. Use it for
	// long-lived agent servers and terminal UIs, which outlive the CLI
	// invocation that started them and are stopped through their own control
	// protocol.
	//
	// Ownership is still real while it is held: an explicit terminate reaps the
	// whole tree. What release does not do is turn into an implicit kill.
	SurviveRelease
)
