package runtimecontract

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/runtime"
)

// probe checks one requirement against the executable and returns its
// status and a human detail. Probes are self-contained: each sets up and
// tears down its own session(s), so a broken behavior fails only its own
// requirement rather than cascading. The handshake is passed in for
// capability-gated probes.
type probe func(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string)

// probes maps every catalog Code to its check. TestProbesCoverCatalog
// asserts this map covers the catalog exactly.
var probes = map[Code]probe{
	ReqProtocolHandshake:          probeProtocolHandshake,
	ReqLifecycleStartRunning:      probeStartRunning,
	ReqLifecycleDuplicateErr:      probeDuplicateErr,
	ReqLifecycleStopNotRunning:    probeStopNotRunning,
	ReqLifecycleStopIdempotent:    probeStopIdempotent,
	ReqLifecycleUnknownNotRunning: probeUnknownNotRunning,
	ReqConnectionExec:             probeExec,
}

func probeProtocolHandshake(ctx context.Context, r *runner, _ runtime.ProtocolInfo) (Status, string) {
	res := r.op(ctx, "protocol")
	switch {
	case res.unsupported:
		return StatusPass, "no protocol op (exit 2) — version 0, no optional capabilities"
	case res.err != nil:
		return StatusFail, res.err.Error()
	case strings.TrimSpace(res.stdout) == "":
		return StatusPass, "empty handshake — version 0, no optional capabilities"
	}
	var info runtime.ProtocolInfo
	if err := json.Unmarshal([]byte(res.stdout), &info); err != nil {
		return StatusFail, fmt.Sprintf("invalid handshake JSON: %v", err)
	}
	if err := info.Validate(); err != nil {
		return StatusFail, err.Error()
	}
	return StatusPass, fmt.Sprintf("version %d, capabilities %v", info.Version, info.Capabilities)
}

func probeStartRunning(ctx context.Context, r *runner, _ runtime.ProtocolInfo) (Status, string) {
	name := r.nextName()
	if status, detail := requireStart(ctx, r, name); status != StatusPass {
		return status, detail
	}
	defer r.stop(ctx, name)
	return expectRunning(ctx, r, name, true, "after start")
}

func probeDuplicateErr(ctx context.Context, r *runner, _ runtime.ProtocolInfo) (Status, string) {
	name := r.nextName()
	if status, detail := requireStart(ctx, r, name); status != StatusPass {
		return status, detail
	}
	defer r.stop(ctx, name)

	again := r.start(ctx, name)
	switch {
	case again.unsupported:
		return StatusFail, "second start returned exit 2; start is a required op"
	case again.ok():
		return StatusFail, "start on an already-running session succeeded; it must fail (exit 1) so gc never double-launches a session"
	default:
		return StatusPass, "duplicate start rejected"
	}
}

func probeStopNotRunning(ctx context.Context, r *runner, _ runtime.ProtocolInfo) (Status, string) {
	name := r.nextName()
	if status, detail := requireStart(ctx, r, name); status != StatusPass {
		return status, detail
	}
	stop := r.stop(ctx, name)
	switch {
	case stop.unsupported:
		return StatusFail, "stop returned exit 2; stop is a required op"
	case stop.err != nil:
		return StatusFail, "stop failed: " + stop.err.Error()
	}
	return expectRunning(ctx, r, name, false, "after stop")
}

func probeStopIdempotent(ctx context.Context, r *runner, _ runtime.ProtocolInfo) (Status, string) {
	name := r.nextName() // never started
	stop := r.stop(ctx, name)
	switch {
	case stop.unsupported:
		return StatusFail, "stop returned exit 2; stop is a required op"
	case stop.err != nil:
		return StatusFail, "stop on a missing session must succeed (idempotent), got: " + stop.err.Error()
	}
	return StatusPass, "stop idempotent on a missing session"
}

func probeUnknownNotRunning(ctx context.Context, r *runner, _ runtime.ProtocolInfo) (Status, string) {
	name := r.nextName() // never started
	return expectRunning(ctx, r, name, false, "for a never-started session")
}

// probeExec verifies the slim connection primitive: exec runs the piped
// command inside the box, the command's output reaches stdout, and the
// command's exit code becomes the op's exit code. This is the wire op a
// carrier drives the legacy driving ops through. It is Optional for now: gc
// still delivers input/observation via the driving-op methods, so a runtime
// that has not implemented exec SKIPs (rather than failing) until the carrier
// rewrite makes exec the way gc drives every runtime.
func probeExec(ctx context.Context, r *runner, _ runtime.ProtocolInfo) (Status, string) {
	name := r.nextName()
	if status, detail := requireStart(ctx, r, name); status != StatusPass {
		return status, detail
	}
	defer r.stop(ctx, name)

	const sentinel = "GC_RPP_CONN_EXEC_OK"
	out := r.execOp(ctx, name, "echo "+sentinel)
	switch {
	case out.unsupported:
		return StatusSkip, "exec not implemented (exit 2) — optional until gc drives the legacy driving ops over exec"
	case out.err != nil:
		return StatusFail, "exec failed: " + out.err.Error()
	}
	if got := strings.TrimSpace(out.stdout); got != sentinel {
		return StatusFail, fmt.Sprintf("exec stdout = %q, want %q (the command's output must reach the caller)", got, sentinel)
	}

	// The op's exit code must mirror the command's: a command exiting 7 must
	// surface as op exit 7 — not merely "some error".
	const wantCode = 7
	nz := r.execOp(ctx, name, "exit 7")
	switch {
	case nz.unsupported:
		return StatusFail, "exec ran the first command but returned exit 2 on the exit-code check — exec must be implemented consistently"
	case nz.exitCode != wantCode:
		return StatusFail, fmt.Sprintf("exec op exit = %d, want %d (op exit must mirror the command's exit code)", nz.exitCode, wantCode)
	}
	return StatusPass, "exec runs the command in the box; output and exit code propagate"
}

// requireStart starts name and returns a non-pass status if start itself is
// broken (a precondition for the lifecycle probes that need a live session).
func requireStart(ctx context.Context, r *runner, name string) (Status, string) {
	res := r.start(ctx, name)
	switch {
	case res.unsupported:
		return StatusFail, "start returned exit 2; start is a required op"
	case res.err != nil:
		return StatusFail, "start failed: " + res.err.Error()
	}
	return StatusPass, ""
}

// expectRunning asserts is-running(name) equals want, returning a failed
// status with context otherwise.
func expectRunning(ctx context.Context, r *runner, name string, want bool, when string) (Status, string) {
	res := r.isRunning(ctx, name)
	switch {
	case res.unsupported:
		return StatusFail, "is-running returned exit 2; is-running is a required op"
	case res.err != nil:
		return StatusFail, "is-running failed: " + res.err.Error()
	}
	got := strings.TrimSpace(res.stdout)
	wantStr := boolText(want)
	if got != wantStr {
		return StatusFail, fmt.Sprintf("is-running %s = %q, want %q", when, got, wantStr)
	}
	return StatusPass, fmt.Sprintf("is-running %s = %s", when, wantStr)
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
