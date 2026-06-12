// Package rppcheck validates an executable against the Runtime Provider
// Protocol (RPP v0) — the engine behind `gc runtime check`, so a runtime
// pack's CI can prove conformance with no Go imports from gascity
// (RUNTIME-RPP-010 in internal/runtime/REQUIREMENTS.md).
//
// The contract being checked is docs/reference/exec-session-provider.md.
// The checker invokes the executable's ops directly rather than going
// through the exec provider: the provider deliberately forgives protocol
// violations (exit 2 reads as success, op errors read as false), while
// conformance needs to observe exit codes and raw output to tell
// "implemented", "absent", and "broken" apart.
package rppcheck

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Status is the outcome of a single conformance check.
type Status string

// Check outcomes. Skip marks an optional op the executable does not
// implement (exit 2) — reported, never a failure (RUNTIME-RPP-010).
const (
	StatusPass Status = "PASS"
	StatusFail Status = "FAIL"
	StatusSkip Status = "SKIP"
)

// Check is one conformance check result.
type Check struct {
	Name   string
	Status Status
	Detail string
}

// Result is the outcome of a conformance run.
type Result struct {
	// Protocol is the parsed handshake; the zero value when the
	// executable has no protocol op or the handshake failed.
	Protocol runtime.ProtocolInfo
	Checks   []Check
}

// Failed reports whether any check failed.
func (r Result) Failed() bool {
	for _, c := range r.Checks {
		if c.Status == StatusFail {
			return true
		}
	}
	return false
}

// Options tune a conformance run. The zero value uses defaults suitable
// for CI: a generated session name, a long-running benign command, and
// the exec provider's production timeouts.
type Options struct {
	// SessionName is the session created for the lifecycle round-trip.
	// Default: a unique generated name.
	SessionName string
	// Command is sent in the start config; backends that run a command
	// inside the session need one that stays alive across the
	// round-trip. Default: "sleep 300".
	Command string
	// WorkDir is sent in the start config. Default: os.TempDir().
	WorkDir string
	// OpTimeout bounds each op invocation. Default: 30s.
	OpTimeout time.Duration
	// StartTimeout bounds the start op, which may include readiness
	// work. Default: 120s.
	StartTimeout time.Duration
}

func (o *Options) applyDefaults() {
	if o.SessionName == "" {
		o.SessionName = fmt.Sprintf("gc-rpp-check-%d", time.Now().UnixNano())
	}
	if o.Command == "" {
		o.Command = "sleep 300"
	}
	if o.WorkDir == "" {
		o.WorkDir = os.TempDir()
	}
	if o.OpTimeout <= 0 {
		o.OpTimeout = 30 * time.Second
	}
	if o.StartTimeout <= 0 {
		o.StartTimeout = 120 * time.Second
	}
}

// startConfig is the start-op stdin payload. It mirrors the wire format
// documented in docs/reference/exec-session-provider.md on purpose — the
// checker speaks the documented protocol, not the Go client's
// serialization helper.
type startConfig struct {
	WorkDir string `json:"work_dir,omitempty"`
	Command string `json:"command,omitempty"`
}

// Run validates the executable against RPP v0 and returns per-check
// results. It returns an error only when the run cannot start at all
// (executable not found); protocol violations are recorded as failed
// checks, never as errors.
func Run(ctx context.Context, executable string, opts Options) (Result, error) {
	path, err := exec.LookPath(executable)
	if err != nil {
		return Result{}, fmt.Errorf("resolving executable %q: %w", executable, err)
	}
	opts.applyDefaults()

	c := &checker{path: path, opts: opts}
	c.checkHandshake(ctx)
	c.checkLifecycle(ctx)
	return c.result, nil
}

// checker accumulates check results across the run.
type checker struct {
	path   string
	opts   Options
	result Result
}

func (c *checker) record(name string, status Status, detail string) {
	c.result.Checks = append(c.result.Checks, Check{Name: name, Status: status, Detail: detail})
}

// opResult is one op invocation's observable outcome.
type opResult struct {
	stdout      string
	unsupported bool  // exit 2 — op not implemented
	err         error // any failure other than exit 2, stderr included
}

// runOp invokes the executable with the op timeout.
func (c *checker) runOp(ctx context.Context, stdin []byte, args ...string) opResult {
	return c.runOpTimeout(ctx, c.opts.OpTimeout, stdin, args...)
}

func (c *checker) runOpTimeout(ctx context.Context, timeout time.Duration, stdin []byte, args ...string) opResult {
	opCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(opCtx, c.path, args...)
	// Force pipe closure shortly after the deadline even when grandchild
	// processes hold them open; a conformance run should report a hang,
	// not inherit it.
	cmd.WaitDelay = 2 * time.Second

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}

	err := cmd.Run()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 2 {
			return opResult{unsupported: true}
		}
		msg := strings.TrimSpace(stderr.String())
		switch {
		case ctx.Err() != nil:
			// Caller cancellation, not the executable's fault — never
			// report it as an op timeout.
			msg = strings.TrimSpace(fmt.Sprintf("canceled: %s", msg))
		case errors.Is(opCtx.Err(), context.DeadlineExceeded):
			msg = strings.TrimSpace(fmt.Sprintf("timed out after %s %s", timeout, msg))
		case msg == "":
			msg = err.Error()
		}
		return opResult{err: fmt.Errorf("%s: %s", strings.Join(args, " "), msg)}
	}
	return opResult{stdout: strings.TrimRight(stdout.String(), "\n")}
}

// knownCapabilities maps declared capability strings to the op each one
// promises. Unknown declared strings are ignored (forward compatibility,
// RUNTIME-RPP-008).
var knownCapabilities = map[string]string{
	runtime.ProtocolCapabilityReportAttachment: "is-attached",
	runtime.ProtocolCapabilityReportActivity:   "get-last-activity",
}

const handshakeCheck = "protocol handshake"

// checkHandshake runs the protocol op. Handshake errors are hard
// failures here (RUNTIME-RPP-008): production degrades to the
// zero-capability floor, but a conformance run must surface them.
func (c *checker) checkHandshake(ctx context.Context) {
	res := c.runOp(ctx, nil, "protocol")
	if res.err != nil {
		c.record(handshakeCheck, StatusFail, res.err.Error())
		return
	}
	if res.unsupported || res.stdout == "" {
		c.record(handshakeCheck, StatusPass, "no protocol op — version 0, no optional capabilities")
		return
	}
	var info runtime.ProtocolInfo
	if err := json.Unmarshal([]byte(res.stdout), &info); err != nil {
		c.record(handshakeCheck, StatusFail, fmt.Sprintf("invalid handshake JSON: %v", err))
		return
	}
	if err := info.Validate(); err != nil {
		c.record(handshakeCheck, StatusFail, err.Error())
		return
	}
	c.result.Protocol = info

	detail := fmt.Sprintf("version %d, capabilities %v", info.Version, info.Capabilities)
	var unknown []string
	for _, capability := range info.Capabilities {
		if _, ok := knownCapabilities[capability]; !ok {
			unknown = append(unknown, capability)
		}
	}
	if len(unknown) > 0 {
		detail += fmt.Sprintf("; ignoring unknown capabilities %v", unknown)
	}
	c.record(handshakeCheck, StatusPass, detail)
}

// checkLifecycle runs the required round-trip — start → is-running true
// → stop → is-running false → stop idempotent — and exercises capability
// and optional ops while the session is up. Required ops returning exit 2
// are failures: a runtime that cannot start or stop a session is not a
// runtime.
func (c *checker) checkLifecycle(ctx context.Context) {
	name := c.opts.SessionName

	cfg, err := json.Marshal(startConfig{WorkDir: c.opts.WorkDir, Command: c.opts.Command})
	if err != nil {
		c.record("lifecycle: start", StatusFail, fmt.Sprintf("marshaling start config: %v", err))
		return
	}

	start := c.runOpTimeout(ctx, c.opts.StartTimeout, cfg, "start", name)
	switch {
	case start.unsupported:
		c.record("lifecycle: start", StatusFail, "required op not implemented (exit 2)")
	case start.err != nil:
		c.record("lifecycle: start", StatusFail, start.err.Error())
	default:
		c.record("lifecycle: start", StatusPass, "")
	}
	if start.unsupported || start.err != nil {
		// Best-effort cleanup in case the session half-started, then
		// report every remaining check as skipped so the report always
		// carries the full check set.
		c.runOp(ctx, nil, "stop", name)
		c.skipRemainingChecks("skipped: start failed")
		return
	}

	// An interrupted run must not leak the session: the inline stop ops
	// below no-op once ctx is canceled, so clean up on a
	// cancellation-detached context.
	defer func() {
		if ctx.Err() == nil {
			return
		}
		c.runOpTimeout(context.WithoutCancel(ctx), c.opts.OpTimeout, nil, "stop", name)
	}()

	c.checkRequiredBool(ctx, "lifecycle: is-running after start", name, "true")
	c.checkSessionOps(ctx, name)

	stop := c.runOp(ctx, nil, "stop", name)
	switch {
	case stop.unsupported:
		c.record("lifecycle: stop", StatusFail, "required op not implemented (exit 2)")
	case stop.err != nil:
		c.record("lifecycle: stop", StatusFail, stop.err.Error())
	default:
		c.record("lifecycle: stop", StatusPass, "")
	}

	c.checkRequiredBool(ctx, "lifecycle: is-running after stop", name, "false")

	again := c.runOp(ctx, nil, "stop", name)
	switch {
	case again.unsupported:
		c.record("lifecycle: stop idempotent", StatusFail, "required op not implemented (exit 2)")
	case again.err != nil:
		c.record("lifecycle: stop idempotent", StatusFail, fmt.Sprintf("second stop must succeed on a missing session: %s", again.err))
	default:
		c.record("lifecycle: stop idempotent", StatusPass, "")
	}

	// Interrupt is probed after the round-trip: on backends where it is
	// session-fatal (SIGINT forwarded to the session command) an earlier
	// probe would tear the session down and let a broken stop op pass
	// the is-running-after-stop assertion on interrupt's side effect.
	// Post-stop it exercises the documented missing-session contract:
	// best-effort, exit 0.
	c.checkOptional(ctx, "optional: interrupt", nil, "interrupt", name)
}

// checkRequiredBool asserts is-running prints exactly the wanted value
// with exit 0. The exec provider reads errors as false, but the
// documented contract is `true` or `false` on stdout and conformance
// holds executables to it.
func (c *checker) checkRequiredBool(ctx context.Context, check, name, want string) {
	res := c.runOp(ctx, nil, "is-running", name)
	switch {
	case res.unsupported:
		c.record(check, StatusFail, "required op not implemented (exit 2)")
	case res.err != nil:
		c.record(check, StatusFail, res.err.Error())
	case res.stdout != want:
		c.record(check, StatusFail, fmt.Sprintf("stdout %q, want %q", res.stdout, want))
	default:
		c.record(check, StatusPass, "")
	}
}

// remainingCheckNames lists every check that follows a successful start,
// in report order, so a failed start can still report them as skipped
// instead of silently dropping them from the summary.
func (c *checker) remainingCheckNames() []string {
	names := []string{"lifecycle: is-running after start"}
	for _, capability := range []string{runtime.ProtocolCapabilityReportAttachment, runtime.ProtocolCapabilityReportActivity} {
		if c.result.Protocol.Has(capability) {
			names = append(names, fmt.Sprintf("capability %s: %s", capability, knownCapabilities[capability]))
		} else {
			names = append(names, "optional: "+knownCapabilities[capability])
		}
	}
	return append(names,
		"optional: process-alive",
		"optional: nudge",
		"optional: metadata round-trip",
		"optional: peek",
		"optional: list-running",
		"lifecycle: stop",
		"lifecycle: is-running after stop",
		"lifecycle: stop idempotent",
		"optional: interrupt",
	)
}

func (c *checker) skipRemainingChecks(detail string) {
	for _, name := range c.remainingCheckNames() {
		c.record(name, StatusSkip, detail)
	}
}

// checkSessionOps exercises capability-declared and optional ops against
// the running session. Declared capabilities are never trusted from the
// handshake alone: the promised op must succeed with parseable output.
// Optional ops may be absent (exit 2 → SKIP) but must behave when
// present.
func (c *checker) checkSessionOps(ctx context.Context, name string) {
	c.checkCapabilityOp(ctx, runtime.ProtocolCapabilityReportAttachment, name, c.validateIsAttached)
	c.checkCapabilityOp(ctx, runtime.ProtocolCapabilityReportActivity, name, c.validateLastActivity)

	c.checkProcessAlive(ctx, name)
	c.checkOptional(ctx, "optional: nudge", []byte("gc runtime check probe"), "nudge", name)
	c.checkMetadataRoundTrip(ctx, name)
	c.checkOptional(ctx, "optional: peek", nil, "peek", name, "10")
	c.checkListRunning(ctx, name)
}

// checkCapabilityOp runs the op a capability promises. Declared: exit 2
// or unparseable output is a failure. Undeclared: probed as an optional
// op for diagnostics — gc will not call it either way.
func (c *checker) checkCapabilityOp(ctx context.Context, capability, name string, validate func(string) error) {
	op := knownCapabilities[capability]
	res := c.runOp(ctx, nil, op, name)

	if !c.result.Protocol.Has(capability) {
		check := "optional: " + op
		switch {
		case res.unsupported:
			c.record(check, StatusSkip, "not implemented (exit 2)")
		case res.err != nil:
			c.record(check, StatusFail, res.err.Error())
		default:
			if err := validate(res.stdout); err != nil {
				c.record(check, StatusFail, err.Error())
				return
			}
			c.record(check, StatusPass, fmt.Sprintf("implemented but not declared — gc ignores it without the %s capability", capability))
		}
		return
	}

	check := fmt.Sprintf("capability %s: %s", capability, op)
	switch {
	case res.unsupported:
		c.record(check, StatusFail, "declared in the handshake but not implemented (exit 2)")
	case res.err != nil:
		c.record(check, StatusFail, res.err.Error())
	default:
		if err := validate(res.stdout); err != nil {
			c.record(check, StatusFail, err.Error())
			return
		}
		c.record(check, StatusPass, "")
	}
}

func (c *checker) validateIsAttached(stdout string) error {
	if stdout != "true" && stdout != "false" {
		return fmt.Errorf("is-attached stdout %q, want \"true\" or \"false\"", stdout)
	}
	return nil
}

func (c *checker) validateLastActivity(stdout string) error {
	if stdout == "" {
		return nil
	}
	if _, err := time.Parse(time.RFC3339, stdout); err != nil {
		return fmt.Errorf("get-last-activity stdout %q is not RFC3339 or empty", stdout)
	}
	return nil
}

// checkProcessAlive probes the optional process-alive op: process names
// ride stdin one per line and stdout must be "true" or "false"
// (docs/reference/exec-session-provider.md). The probe name is derived
// from the session command, but only boolean validity is asserted —
// process visibility varies by backend.
func (c *checker) checkProcessAlive(ctx context.Context, name string) {
	const check = "optional: process-alive"
	probe := "sleep"
	if fields := strings.Fields(c.opts.Command); len(fields) > 0 {
		probe = fields[0]
	}
	res := c.runOp(ctx, []byte(probe+"\n"), "process-alive", name)
	switch {
	case res.unsupported:
		c.record(check, StatusSkip, "not implemented (exit 2)")
	case res.err != nil:
		c.record(check, StatusFail, res.err.Error())
	case res.stdout != "true" && res.stdout != "false":
		c.record(check, StatusFail, fmt.Sprintf("stdout %q, want \"true\" or \"false\"", res.stdout))
	default:
		c.record(check, StatusPass, "")
	}
}

// checkOptional probes an op whose absence is acceptable.
func (c *checker) checkOptional(ctx context.Context, check string, stdin []byte, args ...string) {
	res := c.runOp(ctx, stdin, args...)
	switch {
	case res.unsupported:
		c.record(check, StatusSkip, "not implemented (exit 2)")
	case res.err != nil:
		c.record(check, StatusFail, res.err.Error())
	default:
		c.record(check, StatusPass, "")
	}
}

// checkMetadataRoundTrip validates set-meta/get-meta/remove-meta as one
// unit: absent entirely is a skip, but a partial or lossy implementation
// is a failure — gc's drain and lifecycle coordination depend on
// metadata round-tripping faithfully.
func (c *checker) checkMetadataRoundTrip(ctx context.Context, name string) {
	const (
		check = "optional: metadata round-trip"
		key   = "gc_rpp_check"
		value = "rpp-check-value"
	)

	set := c.runOp(ctx, []byte(value), "set-meta", name, key)
	if set.unsupported {
		c.record(check, StatusSkip, "set-meta not implemented (exit 2)")
		return
	}
	if set.err != nil {
		c.record(check, StatusFail, set.err.Error())
		return
	}

	get := c.runOp(ctx, nil, "get-meta", name, key)
	switch {
	case get.unsupported:
		c.record(check, StatusFail, "set-meta succeeded but get-meta is not implemented (exit 2)")
		return
	case get.err != nil:
		c.record(check, StatusFail, get.err.Error())
		return
	case get.stdout != value:
		c.record(check, StatusFail, fmt.Sprintf("get-meta stdout %q, want %q", get.stdout, value))
		return
	}

	remove := c.runOp(ctx, nil, "remove-meta", name, key)
	switch {
	case remove.unsupported:
		c.record(check, StatusFail, "set-meta succeeded but remove-meta is not implemented (exit 2)")
		return
	case remove.err != nil:
		c.record(check, StatusFail, remove.err.Error())
		return
	}

	after := c.runOp(ctx, nil, "get-meta", name, key)
	switch {
	case after.unsupported:
		c.record(check, StatusFail, "get-meta after remove-meta returned exit 2; unset keys must yield empty output")
	case after.err != nil:
		c.record(check, StatusFail, after.err.Error())
	case after.stdout != "":
		c.record(check, StatusFail, fmt.Sprintf("get-meta after remove-meta returned %q, want empty", after.stdout))
	default:
		c.record(check, StatusPass, "")
	}
}

// checkListRunning requires the conformance session to appear when the
// op is implemented: a list-running that cannot find a session it
// started would break discovery and crash adoption.
func (c *checker) checkListRunning(ctx context.Context, name string) {
	const check = "optional: list-running"
	res := c.runOp(ctx, nil, "list-running", name)
	switch {
	case res.unsupported:
		c.record(check, StatusSkip, "not implemented (exit 2)")
		return
	case res.err != nil:
		c.record(check, StatusFail, res.err.Error())
		return
	}
	for _, line := range strings.Split(res.stdout, "\n") {
		if line == name {
			c.record(check, StatusPass, "")
			return
		}
	}
	c.record(check, StatusFail, fmt.Sprintf("running session %q missing from output %q", name, res.stdout))
}
