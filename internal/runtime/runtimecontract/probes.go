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
	ReqProvisionBoxWithoutAgent:   probeProvision,
	ReqRemoteCreateIdempotent:     probeRemoteCreateIdempotent,
	ReqRemoteAdoptIdentity:        probeRemoteAdoptIdentity,
	ReqRemoteStatusErrors:         probeRemoteStatusErrors,
	ReqRemoteFollowUpFenced:       probeRemoteFollowUpFenced,
	ReqRemoteTranscriptBound:      probeRemoteTranscriptBound,
	ReqRemoteCancelFenced:         probeRemoteCancelFenced,
	ReqRemoteCloseFenced:          probeRemoteCloseFenced,
}

type remoteEnvelope[T any] struct {
	OK     bool                        `json:"ok"`
	Result *T                          `json:"result,omitempty"`
	Error  *runtime.RemoteSessionError `json:"error,omitempty"`
}

func decodeRemoteEnvelope[T any](out outcome) (*T, *runtime.RemoteSessionError, error) {
	if out.unsupported {
		return nil, nil, fmt.Errorf("declared operation returned exit 2")
	}
	if out.err != nil {
		return nil, nil, out.err
	}
	var envelope remoteEnvelope[T]
	if err := json.Unmarshal([]byte(out.stdout), &envelope); err != nil {
		return nil, nil, fmt.Errorf("invalid JSON envelope: %w", err)
	}
	if envelope.OK {
		if envelope.Result == nil || envelope.Error != nil {
			return nil, nil, fmt.Errorf("successful envelope must contain result only")
		}
		return envelope.Result, nil, nil
	}
	if envelope.Error == nil || envelope.Result != nil {
		return nil, nil, fmt.Errorf("failed envelope must contain error only")
	}
	if err := envelope.Error.Validate(); err != nil {
		return nil, nil, err
	}
	return nil, envelope.Error, nil
}

func remoteSkip(hs runtime.ProtocolInfo, operation runtime.RemoteSessionOperation) (Status, string, bool) {
	if hs.Has(string(operation)) {
		return "", "", false
	}
	return StatusSkip, fmt.Sprintf("%s capability not declared", operation), true
}

func remoteSnapshot(out outcome) (runtime.RemoteSessionSnapshot, *runtime.RemoteSessionError, error) {
	result, remoteErr, err := decodeRemoteEnvelope[runtime.RemoteSessionSnapshot](out)
	if err != nil || remoteErr != nil {
		return runtime.RemoteSessionSnapshot{}, remoteErr, err
	}
	if err := result.Validate(); err != nil {
		return runtime.RemoteSessionSnapshot{}, nil, err
	}
	return *result, nil, nil
}

func probeRemoteCreateIdempotent(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if status, detail, skip := remoteSkip(hs, runtime.RemoteSessionCreate); skip {
		return status, detail
	}
	request := runtime.RemoteCreateRequest{
		RequestID: "conformance-create-request",
		Fence:     runtime.RemoteOwnershipFence{Token: "owner-generation"},
		Prompt:    runtime.TextContent("conformance prompt"),
		Source:    runtime.RemoteSource{Repository: "https://example.invalid/org/repo", Ref: "main"},
	}
	name := r.nextName()
	first, remoteErr, err := remoteSnapshot(r.remoteOp(ctx, name, "remote-create", request))
	if err != nil || remoteErr != nil {
		return StatusFail, fmt.Sprintf("first remote-create failed: result error=%v provider error=%v", err, remoteErr)
	}
	second, remoteErr, err := remoteSnapshot(r.remoteOp(ctx, name, "remote-create", request))
	if err != nil || remoteErr != nil {
		return StatusFail, fmt.Sprintf("replayed remote-create failed: result error=%v provider error=%v", err, remoteErr)
	}
	if first.Ref != second.Ref {
		return StatusFail, fmt.Sprintf("same request_id returned different identities: first=%+v second=%+v", first.Ref, second.Ref)
	}
	return StatusPass, "remote-create preserves one opaque identity for a replayed request_id"
}

func probeRemoteAdoptIdentity(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if status, detail, skip := remoteSkip(hs, runtime.RemoteSessionAdopt); skip {
		return status, detail
	}
	ref := runtime.RemoteSessionRef{SessionID: "opaque-session", RunID: "opaque-run"}
	request := runtime.RemoteAdoptRequest{Ref: ref, Fence: runtime.RemoteOwnershipFence{Token: "owner-generation"}}
	snapshot, remoteErr, err := remoteSnapshot(r.remoteOp(ctx, r.nextName(), "remote-adopt", request))
	if err != nil || remoteErr != nil {
		return StatusFail, fmt.Sprintf("remote-adopt failed: result error=%v provider error=%v", err, remoteErr)
	}
	if snapshot.Ref.SessionID != ref.SessionID {
		return StatusFail, fmt.Sprintf("remote-adopt changed persisted session identity: got=%+v want=%+v", snapshot.Ref, ref)
	}
	return StatusPass, "remote-adopt returns the exact persisted opaque identity"
}

func probeRemoteStatusErrors(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if status, detail, skip := remoteSkip(hs, runtime.RemoteSessionStatus); skip {
		return status, detail
	}
	ref := runtime.RemoteSessionRef{SessionID: "opaque-session", RunID: "opaque-run"}
	snapshot, remoteErr, err := remoteSnapshot(r.remoteOp(ctx, r.nextName(), "remote-status", struct {
		Ref runtime.RemoteSessionRef `json:"ref"`
	}{Ref: ref}))
	if err != nil || remoteErr != nil {
		return StatusFail, fmt.Sprintf("remote-status failed: result error=%v provider error=%v", err, remoteErr)
	}
	if snapshot.Ref != ref {
		return StatusFail, fmt.Sprintf("remote-status changed identity: got=%+v want=%+v", snapshot.Ref, ref)
	}
	for _, kind := range []runtime.RemoteFailureKind{runtime.RemoteFailureAuth, runtime.RemoteFailureQuota, runtime.RemoteFailureNetwork} {
		failureRef := runtime.RemoteSessionRef{SessionID: "simulate-" + string(kind)}
		_, gotRemoteErr, gotErr := remoteSnapshot(r.remoteOp(ctx, r.nextName(), "remote-status", struct {
			Ref runtime.RemoteSessionRef `json:"ref"`
		}{Ref: failureRef}))
		if gotErr != nil || gotRemoteErr == nil || gotRemoteErr.Kind != kind {
			return StatusFail, fmt.Sprintf("remote-status %s classification: result error=%v provider error=%v", kind, gotErr, gotRemoteErr)
		}
	}
	return StatusPass, "remote-status returns normalized identity and stable auth/quota/network classifications"
}

func probeRemoteFollowUpFenced(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if status, detail, skip := remoteSkip(hs, runtime.RemoteSessionFollowUp); skip {
		return status, detail
	}
	request := runtime.RemoteFollowUpRequest{
		RequestID: "conformance-follow-up",
		Ref:       runtime.RemoteSessionRef{SessionID: "opaque-session"},
		Fence:     runtime.RemoteOwnershipFence{Token: "owner-generation"},
		Content:   runtime.TextContent("continue"),
	}
	name := r.nextName()
	var first runtime.RemoteReceipt
	for i := 0; i < 2; i++ {
		result, remoteErr, err := decodeRemoteEnvelope[runtime.RemoteReceipt](r.remoteOp(ctx, name, "remote-follow-up", request))
		if err != nil || remoteErr != nil {
			return StatusFail, fmt.Sprintf("remote-follow-up attempt %d failed: result error=%v provider error=%v", i+1, err, remoteErr)
		}
		if err := result.Validate(); err != nil {
			return StatusFail, err.Error()
		}
		if result.RequestID != request.RequestID {
			return StatusFail, fmt.Sprintf("receipt request_id = %q, want %q", result.RequestID, request.RequestID)
		}
		if i == 0 {
			first = *result
		} else if result.ReceiptID != first.ReceiptID || result.RunID != first.RunID {
			return StatusFail, fmt.Sprintf("replayed request returned a different receipt/run: first=%+v second=%+v", first, *result)
		}
	}
	request.Fence.Token = "stale-owner"
	_, remoteErr, err := decodeRemoteEnvelope[runtime.RemoteReceipt](r.remoteOp(ctx, name, "remote-follow-up", request))
	if err != nil || remoteErr == nil || remoteErr.Kind != runtime.RemoteFailureOwnership {
		return StatusFail, fmt.Sprintf("stale fence was not rejected as ownership: result error=%v provider error=%v", err, remoteErr)
	}
	return StatusPass, "remote-follow-up is idempotent and ownership-fenced"
}

func probeRemoteTranscriptBound(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if status, detail, skip := remoteSkip(hs, runtime.RemoteSessionTranscript); skip {
		return status, detail
	}
	query := runtime.RemoteTranscriptQuery{
		Ref:         runtime.RemoteSessionRef{SessionID: "opaque-session"},
		AfterCursor: "opaque-cursor-0",
		Limit:       1,
	}
	result, remoteErr, err := decodeRemoteEnvelope[runtime.RemoteTranscriptPage](r.remoteOp(ctx, r.nextName(), "remote-transcript", query))
	if err != nil || remoteErr != nil {
		return StatusFail, fmt.Sprintf("remote-transcript failed: result error=%v provider error=%v", err, remoteErr)
	}
	if len(result.Events) != 1 || result.Events[0].ID == "" {
		return StatusFail, fmt.Sprintf("remote-transcript events = %+v, want one identified event", result.Events)
	}
	if result.NextCursor == "" || result.NextCursor == query.AfterCursor {
		return StatusFail, fmt.Sprintf("remote-transcript next_cursor = %q, want an advanced opaque cursor", result.NextCursor)
	}
	return StatusPass, "remote-transcript honors the event bound and advances an opaque cursor"
}

func probeRemoteCancelFenced(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	return probeRemoteTerminalMutation(ctx, r, hs, runtime.RemoteSessionCancel, "remote-cancel")
}

func probeRemoteCloseFenced(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	return probeRemoteTerminalMutation(ctx, r, hs, runtime.RemoteSessionClose, "remote-close")
}

func probeRemoteTerminalMutation(ctx context.Context, r *runner, hs runtime.ProtocolInfo, capability runtime.RemoteSessionOperation, wireOperation string) (Status, string) {
	if status, detail, skip := remoteSkip(hs, capability); skip {
		return status, detail
	}
	request := runtime.RemoteMutationRequest{
		RequestID: "conformance-" + wireOperation,
		Ref:       runtime.RemoteSessionRef{SessionID: "opaque-session"},
		Fence:     runtime.RemoteOwnershipFence{Token: "owner-generation"},
	}
	name := r.nextName()
	snapshot, remoteErr, err := remoteSnapshot(r.remoteOp(ctx, name, wireOperation, request))
	if err != nil || remoteErr != nil {
		return StatusFail, fmt.Sprintf("%s failed: result error=%v provider error=%v", wireOperation, err, remoteErr)
	}
	if snapshot.Ref.SessionID != request.Ref.SessionID || !snapshot.Phase.Terminal() {
		return StatusFail, fmt.Sprintf("%s result is not the requested terminal session: %+v", wireOperation, snapshot)
	}
	request.Fence.Token = "stale-owner"
	_, remoteErr, err = remoteSnapshot(r.remoteOp(ctx, name, wireOperation, request))
	if err != nil || remoteErr == nil || remoteErr.Kind != runtime.RemoteFailureOwnership {
		return StatusFail, fmt.Sprintf("%s stale fence was not rejected as ownership: result error=%v provider error=%v", wireOperation, err, remoteErr)
	}
	return StatusPass, wireOperation + " returns a terminal snapshot and rejects a stale fence"
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

// probeProvision verifies the runtime/transport un-weld's box-without-agent op:
// provision must create a reachable box WITHOUT launching the agent. The
// defining behavior — and what separates provision from start — is that
// is-running reports false after provision (no agent), while the box is
// exec-able so the controller can launch the agent over exec. Optional (absent
// = SKIP): a welded start pack that launches the agent in one shot does not
// implement provision and is driven through start instead.
func probeProvision(ctx context.Context, r *runner, _ runtime.ProtocolInfo) (Status, string) {
	name := r.nextName()
	prov := r.provision(ctx, name)
	switch {
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
