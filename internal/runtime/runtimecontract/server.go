package runtimecontract

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/runtime"
)

// This file is the server group: the server-lifecycle token's `server-stop`
// op (docs/reference/exec-session-provider.md, "Server teardown"), which gc
// runs after it stops the city's sessions. Every probe SKIPs unless the
// handshake declares server-lifecycle.
//
// server-stop names no session: it ends every session of the city it runs
// in. So every op of a server probe runs in a private city (privateCity),
// never in the GC_EXEC_CITY_NAME and GC_EXEC_CITY_PATH the run inherited,
// and a run from a city leaves that city's sessions running.

// privateCity returns the env of a city no live city uses, for one server
// probe: GC_EXEC_CITY_NAME and GC_EXEC_CITY_PATH name a fresh temp dir,
// which remove deletes.
func privateCity() (env []string, remove func(), err error) {
	dir, err := os.MkdirTemp("", "gc-rpp-city-")
	if err != nil {
		return nil, nil, fmt.Errorf("creating a private city to run server-stop in: %w", err)
	}
	env = []string{"GC_EXEC_CITY_NAME=" + filepath.Base(dir), "GC_EXEC_CITY_PATH=" + dir}
	return env, func() { _ = os.RemoveAll(dir) }, nil
}

// serverStop runs server-stop in city and returns a non-pass status when it
// does not answer exit 0: a declared op's exit 2 is a failure, never the
// unknown-op no-op.
func serverStop(ctx context.Context, r *runner, city []string, when string) (Status, string) {
	res := r.opEnv(ctx, r.opts.OpTimeout, city, nil, "server-stop")
	switch {
	case res.unsupported:
		return StatusFail, "server-stop " + when + " returned exit 2 although the handshake declares server-lifecycle; a declared op cannot be unknown"
	case res.err != nil:
		return StatusFail, "server-stop " + when + " failed: " + res.err.Error()
	}
	return StatusPass, ""
}

// stopIn stops name in city: a probe's cleanup, whose outcome no probe reads.
func stopIn(ctx context.Context, r *runner, city []string, name string) {
	r.opEnv(ctx, r.opts.OpTimeout, city, nil, "stop", name)
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
	city, removeCity, err := privateCity()
	if err != nil {
		return StatusFail, err.Error()
	}
	defer removeCity()
	if status, detail := serverStop(ctx, r, city, "with no session started"); status != StatusPass {
		return status, detail
	}
	name := r.nextName()
	if status, detail := requireStartIn(ctx, r, city, name); status != StatusPass {
		return status, detail
	}
	defer stopIn(ctx, r, city, name)
	if status, detail := serverStop(ctx, r, city, "with a session running"); status != StatusPass {
		return status, detail
	}
	if status, detail := serverStop(ctx, r, city, "a second time"); status != StatusPass {
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
	city, removeCity, err := privateCity()
	if err != nil {
		return StatusFail, err.Error()
	}
	defer removeCity()
	before := r.nextName()
	if status, detail := requireStartIn(ctx, r, city, before); status != StatusPass {
		return status, detail
	}
	defer stopIn(ctx, r, city, before)
	if status, detail := serverStop(ctx, r, city, "with a session running"); status != StatusPass {
		return status, detail
	}
	after := r.nextName()
	res := r.startIn(ctx, city, after)
	switch {
	case res.unsupported:
		return StatusFail, "start after server-stop returned exit 2; start is a required op"
	case res.err != nil:
		return StatusFail, "start after server-stop failed: " + res.err.Error()
	}
	defer stopIn(ctx, r, city, after)
	return expectRunningIn(ctx, r, city, after, true, "for a session started after server-stop")
}

// probeServerStopEndsSession checks that an exit 0 from server-stop leaves
// no session running: gc reads it as the backstop for any session its own
// stops missed.
func probeServerStopEndsSession(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if ok, detail := serverDeclared(hs); !ok {
		return StatusSkip, detail
	}
	city, removeCity, err := privateCity()
	if err != nil {
		return StatusFail, err.Error()
	}
	defer removeCity()
	names := []string{r.nextName(), r.nextName()}
	defer func() {
		for _, name := range names {
			stopIn(ctx, r, city, name)
		}
	}()
	for _, name := range names {
		if status, detail := requireStartIn(ctx, r, city, name); status != StatusPass {
			return status, detail
		}
	}
	if status, detail := serverStop(ctx, r, city, "with sessions running"); status != StatusPass {
		return status, detail
	}
	for _, name := range names {
		if status, detail := expectRunningIn(ctx, r, city, name, false, "after server-stop exited 0"); status != StatusPass {
			return status, detail
		}
	}
	return StatusPass, "server-stop exit 0 leaves no session running"
}

// probeServerStopSparesOtherCities checks, for a script that declares
// city-scoped, that server-stop ends only its own city's sessions: gc stop of
// one city must leave another city's sessions on a shared server running.
func probeServerStopSparesOtherCities(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if ok, detail := serverDeclared(hs); !ok {
		return StatusSkip, detail
	}
	if !hs.Has(observeCityToken) {
		return StatusSkip, observeCityToken + " not declared"
	}
	cityA, removeA, err := privateCity()
	if err != nil {
		return StatusFail, err.Error()
	}
	defer removeA()
	cityB, removeB, err := privateCity()
	if err != nil {
		return StatusFail, err.Error()
	}
	defer removeB()
	mine, theirs := r.nextName(), r.nextName()
	if status, detail := requireStartIn(ctx, r, cityA, mine); status != StatusPass {
		return status, detail
	}
	defer stopIn(ctx, r, cityA, mine)
	if status, detail := requireStartIn(ctx, r, cityB, theirs); status != StatusPass {
		return status, detail
	}
	// City B's server is this probe's to end too, after its session.
	defer r.opEnv(ctx, r.opts.OpTimeout, cityB, nil, "server-stop")
	defer stopIn(ctx, r, cityB, theirs)
	if status, detail := serverStop(ctx, r, cityA, "in one city"); status != StatusPass {
		return status, detail
	}
	if status, detail := expectRunningIn(ctx, r, cityB, theirs, true, "for another city's session after server-stop"); status != StatusPass {
		return status, detail
	}
	return StatusPass, "server-stop leaves another city's sessions running"
}
