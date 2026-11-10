package runtimecontract

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/tmux"
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
	ReqConnectionIdleBoundary:     probeIdleBoundary,
	ReqProvisionBoxWithoutAgent:   probeProvision,

	ReqObserveDeclaredOpsRespond:   probeObserveDeclaredOps,
	ReqObserveAbsentNeverStarted:   probeObserveAbsentNeverStarted,
	ReqObserveRunningWithIdentity:  probeObserveRunningWithIdentity,
	ReqObserveAbsentAfterStop:      probeObserveAbsentAfterStop,
	ReqObserveObjectStable:         probeObserveObjectStable,
	ReqObserveAgentLiveness:        probeObserveAgentLiveness,
	ReqObserveInventoryComplete:    probeObserveInventoryComplete,
	ReqObserveMetaIsIdentity:       probeObserveMetaIsIdentity,
	ReqObserveFreshOnlyRefuses:     probeObserveFreshOnly,
	ReqObserveKillObjectExact:      probeObserveKillObject,
	ReqObserveAttachedIsStrict:     probeObserveAttachedStrict,
	ReqObserveFaultsReadUnknown:    probeObserveFaults,
	ReqObserveWithinBound:          probeObserveWithinBound,
	ReqObserveCityScoped:           probeObserveCityScoped,
	ReqObserveInventoryHasActivity: probeObserveInventoryActivity,
	ReqObservePeekNoScreen:         probeObservePeekNoScreen,

	ReqObserveAttachedUnattachedFalse: probeObserveAttachedUnattached,
	ReqObserveAttachedMissingFalse:    probeObserveAttachedMissing,

	ReqMetaRoundTrip:               probeMetaRoundTrip,
	ReqMetaUnsetKeyEmpty:           probeMetaUnsetKeyEmpty,
	ReqMetaRemoveThenEmpty:         probeMetaRemoveThenEmpty,
	ReqMetaEmptyValueOverwrites:    probeMetaEmptyValueOverwrites,
	ReqMetaOverwritesPrevious:      probeMetaOverwritesPrevious,
	ReqMetaMultipleKeysIndependent: probeMetaMultipleKeys,

	ReqDiscoveryFindsSessions:       probeDiscoveryFindsSessions,
	ReqDiscoveryPrefixFiltering:     probeDiscoveryPrefixFiltering,
	ReqDiscoveryExcludesStopped:     probeDiscoveryExcludesStopped,
	ReqDiscoveryEmptyPrefix:         probeDiscoveryEmptyPrefix,
	ReqDiscoveryFailureIsNotAbsence: probeDiscoveryFailureIsNotAbsence,

	ReqProcessEmptyNamesAlive: probeProcessEmptyNamesAlive,
	ReqProcessFalseAfterStop:  probeProcessFalseAfterStop,

	ReqConcurrencyStart:        probeConcurrencyStart,
	ReqConcurrencyStop:         probeConcurrencyStop,
	ReqConcurrencyIsRunning:    probeConcurrencyIsRunning,
	ReqConcurrencyProcessAlive: probeConcurrencyProcessAlive,
	ReqConcurrencyListRunning:  probeConcurrencyListRunning,

	ReqServerStopIdempotent:        probeServerStopIdempotent,
	ReqServerStartAfterStop:        probeServerStartAfterStop,
	ReqServerStopEndsSession:       probeServerStopEndsSession,
	ReqServerStopSparesOtherCities: probeServerStopSparesOtherCities,

	ReqFSHostSharedView:        probeFSHostSharedView,
	ReqFSHostSameUser:          probeFSHostSameUser,
	ReqFSHostLocal:             probeFSHostLocal,
	ReqFSHostRunsWhatItCarries: probeFSHostRunsWhatItCarries,
	ReqFSHostAgentPIDOnHost:    probeFSHostAgentPIDOnHost,
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

// idleBoundaryCommands are the in-box tmux commands the idle-boundary probe
// runs over exec, quoted as gc quotes every exec command (each argument
// single-quoted): the session the agent runs in, then the capture gc's idle
// wait takes through the tmux carrier.
//
// TODO(gh#8903 PR 2): once the carrier is selectable per pack, check the
// declared carrier's idle read instead of assuming tmux.
var idleBoundaryCommands = []string{
	quoteArgv("tmux", "has-session", "-t", "main"),
	quoteArgv("tmux", "capture-pane", "-t", "main", "-p", "-S", "-"+strconv.Itoa(tmux.PromptObservationLines)),
}

// quoteArgv renders argv as the exec provider sends a command on the exec
// op's stdin: each argument single-quoted, with an embedded single quote
// closed, backslash-escaped and reopened.
func quoteArgv(argv ...string) string {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}

// idleBoundaryPeers are the capabilities idle-boundary takes effect with:
// without all of them gc gives the pack no idle boundary whatever it
// declares.
var idleBoundaryPeers = []string{
	runtime.ProtocolCapabilityConnectionExec,
	runtime.ProtocolCapabilityReportActivity,
	runtime.ProtocolCapabilityReportAttachment,
}

// probeIdleBoundary verifies that a declared idle-boundary holds: the script
// says its agent runs in the in-box tmux session `main`, so in a started box
// that session must exist and its pane must be readable over exec, which is
// how gc reads the idle boundary. It gates on the declaration, not on
// behavior: a script that does not declare idle-boundary gets no idle wait
// from gc whatever its box runs, so it SKIPs. Whether the pane shows a ready
// prompt depends on the agent, not the pack, so the probe reads the pane
// without classifying it.
func probeIdleBoundary(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if !hs.Has(runtime.ProtocolCapabilityIdleBoundary) {
		return StatusSkip, "idle-boundary not declared — gc reads no idle boundary from the box"
	}
	var missing []string
	for _, c := range idleBoundaryPeers {
		if !hs.Has(c) {
			missing = append(missing, c)
		}
	}
	name := r.nextName()
	if status, detail := requireStart(ctx, r, name); status != StatusPass {
		return status, detail
	}
	defer r.stop(ctx, name)

	for _, command := range idleBoundaryCommands {
		out := r.execOp(ctx, name, command)
		switch {
		case out.unsupported:
			return StatusFail, "the handshake declares idle-boundary but exec is unimplemented (exit 2); gc reads the idle boundary over exec"
		case out.err != nil:
			return StatusFail, fmt.Sprintf("%s over exec failed: %v (idle-boundary declares that the agent runs in the box's tmux session main)", command, out.err)
		}
	}
	if len(missing) > 0 {
		return StatusPass, fmt.Sprintf("the box's tmux session main answers has-session and the idle capture over exec; idle-boundary takes effect only with %s, which the handshake does not declare", strings.Join(missing, ", "))
	}
	return StatusPass, "the box's tmux session main answers has-session and the idle capture over exec"
}

// probeProvision verifies the runtime/transport un-weld's box-without-agent op:
// provision must create a reachable box WITHOUT launching the agent. The
// defining behavior — and what separates provision from start — is that
// is-running reports false after provision (no agent), while the box is
// exec-able so the controller can launch the agent over exec. Optional (absent
// = SKIP): a welded start pack that launches the agent in one shot does not
// implement provision and is driven through start instead.
func probeProvision(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	name := r.nextName()
	prov := r.provision(ctx, name)
	switch {
	case prov.unsupported && hs.Has(runtime.ProtocolCapabilityProvision):
		return StatusFail, "provision returned exit 2 although the handshake declares proc.provision; a declared op cannot be unknown"
	case prov.unsupported:
		return StatusSkip, "provision not implemented (exit 2) — optional; a welded start pack launches the agent in one shot"
	case prov.err != nil:
		return StatusFail, "provision failed: " + prov.err.Error()
	}
	defer r.stop(ctx, name)

	// The agent must NOT be launched: provision is the box half. is-running
	// gates on the agent (e.g. tmux has-session), so it must report false until
	// the controller launches the agent — this is what makes warm-box relaunch
	// possible. A pack that launches in provision has not un-welded.
	switch res := r.isRunning(ctx, name); {
	case res.unsupported:
		return StatusFail, "is-running returned exit 2; is-running is a required op"
	case res.err != nil:
		return StatusFail, "is-running failed: " + res.err.Error()
	case strings.TrimSpace(res.stdout) != "false":
		return StatusFail, fmt.Sprintf("is-running after provision = %q, want \"false\" (provision must NOT launch the agent; the controller launches it over exec)", strings.TrimSpace(res.stdout))
	}

	// The box must be reachable so the controller can launch the agent over
	// exec — provision "returns when the box is exec-able". A provision-capable
	// pack therefore also implements exec (proc.exec); if it cannot, the
	// controller could never launch into the box it provisioned.
	const sentinel = "GC_RPP_PROVISION_OK"
	out := r.execOp(ctx, name, "echo "+sentinel)
	switch {
	case out.unsupported:
		return StatusFail, "provision returned a box but exec is unimplemented (exit 2) — the controller launches the agent over exec, so a provision-capable pack must also implement exec"
	case out.err != nil:
		return StatusFail, "exec into the provisioned box failed: " + out.err.Error()
	}
	if got := strings.TrimSpace(out.stdout); got != sentinel {
		return StatusFail, fmt.Sprintf("exec into the provisioned box: stdout = %q, want %q (the box must be reachable)", got, sentinel)
	}
	return StatusPass, "provision creates a reachable box without launching the agent"
}

// requireStart starts name and returns a non-pass status if start itself is
// broken (a precondition for the lifecycle probes that need a live session).
func requireStart(ctx context.Context, r *runner, name string) (Status, string) {
	return requireStartIn(ctx, r, nil, name)
}

// requireStartIn is requireStart with env added to start's environment (a
// city's GC_EXEC_CITY_NAME and GC_EXEC_CITY_PATH).
func requireStartIn(ctx context.Context, r *runner, env []string, name string) (Status, string) {
	res := r.startIn(ctx, env, name)
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
	return expectRunningIn(ctx, r, nil, name, want, when)
}

// expectRunningIn is expectRunning with env added to is-running's
// environment (a city's GC_EXEC_CITY_NAME and GC_EXEC_CITY_PATH).
func expectRunningIn(ctx context.Context, r *runner, env []string, name string, want bool, when string) (Status, string) {
	res := r.opEnv(ctx, r.opts.OpTimeout, env, nil, "is-running", name)
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
