package runtimecontract

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	sessionexec "github.com/gastownhall/gascity/internal/runtime/exec"
)

// This file holds the observation group ported from RunProviderTests
// (RPP-OBSERVATION-001..004): Peek_NoError, GetLastActivity_NoError,
// ClearScrollback_NoError and CopyTo_NoError, each over the wire. Like the
// other ported groups, each probe asks about its op on a name no box holds
// before it starts a session, so an op that answers exit 2 SKIPs (or FAILs,
// when the handshake makes the op mandatory) even if start is broken, and
// a present op that fails on a running box FAILs. The group is advisory
// (advisory): its FAILs do not enter v2_failed yet.

// copyToMissingSource is the source copy-to is asked to copy: a path that
// does not exist, as CopyTo_NoError asks, since copy-to is best-effort.
const copyToMissingSource = "/nonexistent-path-for-conformance"

// observationOp is one observation op: its name, its arguments after the
// session name, and what the handshake declares, if anything, that makes
// its exit 2 a failure.
type observationOp struct {
	op        string
	args      []string
	mandatory func(runtime.ProtocolInfo) string
}

// run invokes the op on name.
func (o observationOp) run(ctx context.Context, r *runner, name string) outcome {
	return r.op(ctx, append([]string{o.op, name}, o.args...)...)
}

// unsupported is the verdict on an op that answered exit 2: SKIP, unless
// the handshake makes the op mandatory.
func (o observationOp) unsupported(hs runtime.ProtocolInfo) (Status, string) {
	if declared := o.mandatory(hs); declared != "" {
		return StatusFail, fmt.Sprintf("%s returned exit 2 although %s, which makes it a required op", o.op, declared)
	}
	return StatusSkip, o.op + " not implemented (exit 2)"
}

// declaredOf returns a mandatory func naming the first of tokens hs
// declares.
func declaredOf(tokens ...string) func(runtime.ProtocolInfo) string {
	return func(hs runtime.ProtocolInfo) string {
		for _, token := range tokens {
			if hs.Has(token) {
				return token + " is declared"
			}
		}
		return ""
	}
}

// peekMandatory names what makes peek's exit 2 a failure, as the exec
// provider reads it (exitTwoIsStrict): carrier.ops, whose carrier reads the
// screen through peek, or the v2 profile, under which peek is an op a
// v2-ready script must implement.
func peekMandatory(hs runtime.ProtocolInfo) string {
	if hs.Has(runtime.ProtocolCapabilityCarrierOps) {
		return runtime.ProtocolCapabilityCarrierOps + " is declared"
	}
	if len(sessionexec.V2ProfileGaps(hs)) == 0 {
		return "the v2 profile (" + strings.Join(sessionexec.V2ProfileCapabilities(), ", ") + ") is declared"
	}
	return ""
}

var (
	// peek is mandatory under carrier.ops or the v2 profile
	// (peekMandatory).
	observationPeek = observationOp{
		op: "peek", args: []string{"10"},
		mandatory: peekMandatory,
	}
	// get-last-activity is mandatory under report-activity, which tells gc
	// its answer is meaningful for idle and health decisions.
	observationLastActivity = observationOp{
		op:        "get-last-activity",
		mandatory: declaredOf(runtime.ProtocolCapabilityReportActivity),
	}
	// clear-scrollback is mandatory under carrier.ops (one of its five
	// driving ops) and under input-outcome (a declared driving op's exit 2
	// is a failure).
	observationClearScrollback = observationOp{
		op:        "clear-scrollback",
		mandatory: declaredOf(runtime.ProtocolCapabilityCarrierOps, runtime.ProtocolCapabilityInputOutcome),
	}
	// copy-to is optional under every handshake.
	observationCopyTo = observationOp{
		op: "copy-to", args: []string{copyToMissingSource, ""},
		mandatory: declaredOf(),
	}
)

// observeRunningBox asks o about a name no box holds (absent = SKIP or
// FAIL, before any session starts), then runs o on a running box and
// returns its outcome; ok is false with the verdict when the probe ends
// before that.
func observeRunningBox(ctx context.Context, r *runner, hs runtime.ProtocolInfo, o observationOp) (outcome, Status, string, bool) {
	if o.run(ctx, r, r.nextName()).unsupported {
		s, d := o.unsupported(hs)
		return outcome{}, s, d, false
	}
	name := r.nextName()
	if s, d := requireStart(ctx, r, name); s != StatusPass {
		return outcome{}, s, d, false
	}
	defer r.stop(ctx, name)
	res := o.run(ctx, r, name)
	switch {
	case res.unsupported:
		s, d := o.unsupported(hs)
		return outcome{}, s, d + " on a running box", false
	case res.err != nil:
		return outcome{}, StatusFail, fmt.Sprintf("%s of a running box failed: %v", o.op, res.err), false
	}
	return res, StatusPass, "", true
}

func probeObservationPeek(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if _, s, d, ok := observeRunningBox(ctx, r, hs, observationPeek); !ok {
		return s, d
	}
	return StatusPass, "peek of a running box exits 0"
}

// probeObservationLastActivity also requires the answer gc parses: the
// exec provider reads an empty answer as no activity and silently reads a
// malformed one as no activity too, so a malformed time is a FAIL.
func probeObservationLastActivity(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	res, s, d, ok := observeRunningBox(ctx, r, hs, observationLastActivity)
	if !ok {
		return s, d
	}
	got := strings.TrimSpace(res.stdout)
	if got == "" {
		return StatusPass, "get-last-activity of a running box exits 0 with no time"
	}
	if _, err := time.Parse(time.RFC3339, got); err != nil {
		return StatusFail, fmt.Sprintf("get-last-activity of a running box = %q, not an RFC3339 time: gc reads it as no activity", got)
	}
	return StatusPass, "get-last-activity of a running box exits 0 with an RFC3339 time"
}

func probeObservationClearScrollback(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if _, s, d, ok := observeRunningBox(ctx, r, hs, observationClearScrollback); !ok {
		return s, d
	}
	return StatusPass, "clear-scrollback of a running box exits 0"
}

func probeObservationCopyTo(ctx context.Context, r *runner, hs runtime.ProtocolInfo) (Status, string) {
	if _, s, d, ok := observeRunningBox(ctx, r, hs, observationCopyTo); !ok {
		return s, d
	}
	return StatusPass, "copy-to of a missing source into a running box exits 0"
}
