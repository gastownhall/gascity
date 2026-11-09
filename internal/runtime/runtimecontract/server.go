package runtimecontract

import (
	"context"

	"github.com/gastownhall/gascity/internal/runtime"
)

// This file is the server group: the server-lifecycle token's `server-stop`
// op (docs/reference/exec-session-provider.md, "Server teardown"), which gc
// runs once every session of the city is drained. Every probe SKIPs unless
// the handshake declares server-lifecycle.

// serverStop runs server-stop and returns a non-pass status when it does not
// answer exit 0: a declared op's exit 2 is a failure, never the unknown-op
// no-op.
func serverStop(ctx context.Context, r *runner, when string) (Status, string) {
	res := r.op(ctx, "server-stop")
	switch {
	case res.unsupported:
		return StatusFail, "server-stop " + when + " returned exit 2 although the handshake declares server-lifecycle; a declared op cannot be unknown"
	case res.err != nil:
		return StatusFail, "server-stop " + when + " failed: " + res.err.Error()
	}
	return StatusPass, ""
}

// serverDeclared reports whether hs declares server-lifecycle, and the skip
// detail when it does not.
func serverDeclared(hs runtime.ProtocolInfo) (bool, string) {
	if !hs.Has(runtime.ProtocolCapabilityServerLifecycle) {
		return false, "server-lifecycle not declared"
	}
	return true, ""
}

// probeServerStopIdempotent checks that server-stop answers exit 0 with no
// server running, after a session brought one up, and again once it is gone.
func probeServerStopIdempotent(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if ok, detail := serverDeclared(hs); !ok {
		return StatusSkip, detail
	}
	if status, detail := serverStop(ctx, r, "with no session started"); status != StatusPass {
		return status, detail
	}
	name := r.nextName()
	if status, detail := requireStart(ctx, r, name); status != StatusPass {
		return status, detail
	}
	defer r.stop(ctx, name)
	if status, detail := serverStop(ctx, r, "with a session running"); status != StatusPass {
		return status, detail
	}
	if status, detail := serverStop(ctx, r, "a second time"); status != StatusPass {
		return status, detail
	}
	return StatusPass, "server-stop exits 0 with no server, with a session, and again after a stop"
}

// probeServerStartAfterStop checks that the runtime serves a new session
// after server-stop: gc stops a city's server, and the next gc start brings
// it back through an ordinary start.
func probeServerStartAfterStop(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if ok, detail := serverDeclared(hs); !ok {
		return StatusSkip, detail
	}
	before := r.nextName()
	if status, detail := requireStart(ctx, r, before); status != StatusPass {
		return status, detail
	}
	defer r.stop(ctx, before)
	if status, detail := serverStop(ctx, r, "with a session running"); status != StatusPass {
		return status, detail
	}
	after := r.nextName()
	res := r.start(ctx, after)
	switch {
	case res.unsupported:
		return StatusFail, "start after server-stop returned exit 2; start is a required op"
	case res.err != nil:
		return StatusFail, "start after server-stop failed: " + res.err.Error()
	}
	defer r.stop(ctx, after)
	return expectRunning(ctx, r, after, true, "for a session started after server-stop")
}

// probeServerStopEndsSession checks that an exit 0 from server-stop leaves
// no session running: gc reads it as the backstop for any session its own
// stops missed.
func probeServerStopEndsSession(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if ok, detail := serverDeclared(hs); !ok {
		return StatusSkip, detail
	}
	names := []string{r.nextName(), r.nextName()}
	defer func() {
		for _, name := range names {
			r.stop(ctx, name)
		}
	}()
	for _, name := range names {
		if status, detail := requireStart(ctx, r, name); status != StatusPass {
			return status, detail
		}
	}
	if status, detail := serverStop(ctx, r, "with sessions running"); status != StatusPass {
		return status, detail
	}
	for _, name := range names {
		if status, detail := expectRunning(ctx, r, name, false, "after server-stop exited 0"); status != StatusPass {
			return status, detail
		}
	}
	return StatusPass, "server-stop exit 0 leaves no session running"
}
