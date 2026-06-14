package runtimecontract

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

// Options tune a conformance run. The zero value is CI-ready.
type Options struct {
	// SessionPrefix prefixes every session name the run creates, so a run
	// never collides with real sessions. Default: a unique generated prefix.
	SessionPrefix string
	// Command is sent in start configs; real backends need one that stays
	// alive across the round-trip. Default: "sleep 300".
	Command string
	// WorkDir is sent in start configs. Default: os.TempDir().
	WorkDir string
	// OpTimeout bounds each op invocation. Default: 30s.
	OpTimeout time.Duration
	// StartTimeout bounds the start op. Default: 120s.
	StartTimeout time.Duration
}

func (o *Options) applyDefaults() {
	if o.SessionPrefix == "" {
		o.SessionPrefix = fmt.Sprintf("gc-rpp-conf-%d", time.Now().UnixNano())
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

// Run validates the executable against the full conformance catalog and
// returns a per-requirement report. It returns an error only when the run
// cannot start at all (executable not resolvable); contract violations are
// recorded as failed results, never as errors.
//
// Run emits exactly one [Result] per [Catalog] requirement, in catalog
// order (enforced by TestRunCoversEveryCatalogRequirement).
func Run(ctx context.Context, executable string, opts Options) (Report, error) {
	path, err := exec.LookPath(executable)
	if err != nil {
		return Report{}, fmt.Errorf("resolving executable %q: %w", executable, err)
	}
	opts.applyDefaults()
	r := &runner{path: path, opts: opts}
	report := Report{Executable: path}

	// Resolve the handshake once; capability-gated probes consult it.
	report.Protocol = r.handshake(ctx)

	for _, req := range catalog {
		probe, ok := probes[req.Code]
		if !ok {
			report.record(req, StatusFail, "no probe registered for this requirement (runtimecontract bug)")
			continue
		}
		status, detail := probe(ctx, r, report.Protocol)
		report.record(req, status, detail)
	}
	return report, nil
}

// runner drives one executable across a conformance run.
type runner struct {
	path    string
	opts    Options
	counter int
}

// nextName returns a fresh, unique session name for a probe.
func (r *runner) nextName() string {
	r.counter++
	return fmt.Sprintf("%s-%d", r.opts.SessionPrefix, r.counter)
}

// outcome is one op invocation's observable result.
type outcome struct {
	stdout      string
	unsupported bool  // exit 2 — op not implemented
	err         error // any failure other than exit 2 (stderr included)
}

func (o outcome) ok() bool { return o.err == nil && !o.unsupported }

// op invokes the executable with the op timeout and no stdin. Ops that send
// a payload (start, and the metadata/signaling groups as they land) call
// opTimeout directly.
func (r *runner) op(ctx context.Context, args ...string) outcome {
	return r.opTimeout(ctx, r.opts.OpTimeout, nil, args...)
}

func (r *runner) opTimeout(ctx context.Context, timeout time.Duration, stdin []byte, args ...string) outcome {
	opCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(opCtx, r.path, args...)
	// Force pipe closure shortly after the deadline even when grandchild
	// processes hold them open: a conformance run reports a hang, not
	// inherits it.
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
			return outcome{unsupported: true}
		}
		msg := strings.TrimSpace(stderr.String())
		switch {
		case ctx.Err() != nil:
			msg = strings.TrimSpace("canceled: " + msg)
		case errors.Is(opCtx.Err(), context.DeadlineExceeded):
			msg = strings.TrimSpace(fmt.Sprintf("timed out after %s %s", timeout, msg))
		case msg == "":
			msg = err.Error()
		}
		return outcome{err: fmt.Errorf("%s: %s", strings.Join(args, " "), msg)}
	}
	return outcome{stdout: strings.TrimRight(stdout.String(), "\n")}
}

// start sends a start config for name and returns the raw outcome.
func (r *runner) start(ctx context.Context, name string) outcome {
	cfg, _ := json.Marshal(struct {
		WorkDir string `json:"work_dir,omitempty"`
		Command string `json:"command,omitempty"`
	}{WorkDir: r.opts.WorkDir, Command: r.opts.Command})
	return r.opTimeout(ctx, r.opts.StartTimeout, cfg, "start", name)
}

// stop stops name and returns the raw outcome.
func (r *runner) stop(ctx context.Context, name string) outcome {
	return r.op(ctx, "stop", name)
}

// isRunning runs is-running and returns the trimmed stdout plus outcome.
func (r *runner) isRunning(ctx context.Context, name string) outcome {
	return r.op(ctx, "is-running", name)
}

// handshake runs the protocol op and parses the result. Absent (exit 2) is
// the documented v0/no-capability floor.
func (r *runner) handshake(ctx context.Context) runtime.ProtocolInfo {
	res := r.op(ctx, "protocol")
	if res.unsupported || res.err != nil || strings.TrimSpace(res.stdout) == "" {
		return runtime.ProtocolInfo{}
	}
	var info runtime.ProtocolInfo
	if err := json.Unmarshal([]byte(res.stdout), &info); err != nil {
		return runtime.ProtocolInfo{}
	}
	return info
}
