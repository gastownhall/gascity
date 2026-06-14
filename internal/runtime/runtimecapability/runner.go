package runtimecapability

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

const (
	// sentinelName + sentinelContent are written into the fixture work_dir;
	// the workspace probe reads them back from inside the session.
	sentinelName    = ".gc-cb-sentinel"
	sentinelContent = "GC_CAPABILITY_WORKSPACE_OK"
	// probeSessionEnv is injected and read back by the identity probe.
	probeSessionEnv = "GC_SESSION"
	// ledgerEnv is the gc beads API endpoint injected for the ledger probe;
	// the runtime makes it reachable from the session (directly here, via a
	// sandbox->host tunnel for a remote runtime). A conformant `bd` reads it.
	ledgerEnv = "GC_BEADS_API"
)

// Options tune a capability run. The zero value is CI-ready.
type Options struct {
	SessionName  string
	Command      string // start-config command; default "sleep 300"
	OpTimeout    time.Duration
	StartTimeout time.Duration
}

func (o *Options) applyDefaults() {
	if o.SessionName == "" {
		o.SessionName = fmt.Sprintf("gc-cb-%d", time.Now().UnixNano())
	}
	if o.Command == "" {
		o.Command = "sleep 300"
	}
	if o.OpTimeout <= 0 {
		o.OpTimeout = 30 * time.Second
	}
	if o.StartTimeout <= 0 {
		o.StartTimeout = 120 * time.Second
	}
}

// Run verifies the runtime's declared env.* capabilities. It returns an error
// only when the run cannot start (executable unresolvable); capability
// violations are recorded as failed results.
func Run(ctx context.Context, executable string, opts Options) (Report, error) {
	path, err := exec.LookPath(executable)
	if err != nil {
		return Report{}, fmt.Errorf("resolving executable %q: %w", executable, err)
	}
	opts.applyDefaults()
	r := &runner{path: path, opts: opts}
	report := Report{Executable: path}

	hs := r.handshake(ctx)
	declared := map[Code]bool{}
	for _, c := range hs.Capabilities {
		report.Capabilities = append(report.Capabilities, c)
		declared[Code(c)] = true
	}

	anyDeclared := false
	for _, cb := range catalog {
		if declared[cb.Code] {
			anyDeclared = true
		}
	}
	if !anyDeclared {
		for _, cb := range catalog {
			report.record(cb, false, StatusSkip, "not declared in handshake")
		}
		return report, nil
	}

	// Fixture: a work_dir with a sentinel + a known session identity.
	workDir, err := os.MkdirTemp("", "gc-cb-workspace-")
	if err != nil {
		return Report{}, fmt.Errorf("creating fixture work dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(workDir) }()
	if err := os.WriteFile(filepath.Join(workDir, sentinelName), []byte(sentinelContent), 0o644); err != nil {
		return Report{}, fmt.Errorf("writing sentinel: %w", err)
	}

	// The ledger double: the gc beads API the session's bd must reach. The
	// runtime is handed its URL (via the start-config env) and is responsible
	// for making it reachable from the session. Locally that's direct; for a
	// remote runtime it's a sandbox->host tunnel (the deferred transport).
	ledger := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/v0/beads/ready" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ready":[]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ledger.Close()

	name := opts.SessionName
	if status, detail := r.startSession(ctx, name, workDir, ledger.URL); status != StatusPass {
		// Start itself failed — every declared capability fails with the reason.
		for _, cb := range catalog {
			if declared[cb.Code] {
				report.record(cb, true, StatusFail, "session start failed: "+detail)
			} else {
				report.record(cb, false, StatusSkip, "not declared in handshake")
			}
		}
		return report, nil
	}
	defer r.stop(ctx, name)

	for _, cb := range catalog {
		if !declared[cb.Code] {
			report.record(cb, false, StatusSkip, "not declared in handshake")
			continue
		}
		status, detail := probes[cb.Code](ctx, r, name)
		report.record(cb, true, status, detail)
	}
	return report, nil
}

// runner drives one executable.
type runner struct {
	path string
	opts Options
}

// handshake reads the protocol op's declared capabilities (absent → none).
func (r *runner) handshake(ctx context.Context) runtime.ProtocolInfo {
	out, code, _ := r.invoke(ctx, r.opts.OpTimeout, nil, "protocol")
	if code != 0 || strings.TrimSpace(out) == "" {
		return runtime.ProtocolInfo{}
	}
	var info runtime.ProtocolInfo
	if json.Unmarshal([]byte(out), &info) != nil {
		return runtime.ProtocolInfo{}
	}
	return info
}

// startSession starts a session whose start-config carries work_dir + the
// probe identity env, so a capability-providing runtime materializes them.
func (r *runner) startSession(ctx context.Context, name, workDir, ledgerURL string) (Status, string) {
	cfg, _ := json.Marshal(map[string]any{
		"work_dir": workDir,
		"command":  r.opts.Command,
		"env":      map[string]string{probeSessionEnv: name, ledgerEnv: ledgerURL},
	})
	_, code, err := r.invoke(ctx, r.opts.StartTimeout, cfg, "start", name)
	switch {
	case code == 2:
		return StatusFail, "start returned exit 2 (not implemented)"
	case err != nil || code != 0:
		return StatusFail, fmt.Sprintf("start exit %d: %v", code, err)
	}
	return StatusPass, ""
}

func (r *runner) stop(ctx context.Context, name string) {
	_, _, _ = r.invoke(ctx, r.opts.OpTimeout, nil, "stop", name)
}

// execIn runs a command inside the session via the RPP exec op (command on
// stdin, combined output on stdout, op exit code == command exit code). exit
// 2 signals the exec op is unimplemented.
func (r *runner) execIn(ctx context.Context, name, command string) (output string, exitCode int, unsupported bool) {
	out, code, _ := r.invoke(ctx, r.opts.OpTimeout, []byte(command), "exec", name)
	if code == 2 {
		return "", 0, true
	}
	return out, code, false
}

// invoke runs the executable and returns (stdout, exitCode, runErr). A clean
// exit is code 0; a non-ExitError failure (timeout, spawn) returns a non-nil
// runErr with code -1.
func (r *runner) invoke(ctx context.Context, timeout time.Duration, stdin []byte, args ...string) (string, int, error) {
	opCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(opCtx, r.path, args...)
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
		if errors.As(err, &exitErr) {
			return strings.TrimRight(stdout.String(), "\n"), exitErr.ExitCode(), nil
		}
		return "", -1, fmt.Errorf("%s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimRight(stdout.String(), "\n"), 0, nil
}
