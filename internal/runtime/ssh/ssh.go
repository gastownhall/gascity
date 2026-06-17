// Package ssh is an SSH connection backend for the Runtime Provider Protocol.
// It realizes the exec connection primitive — run a command in the box — over
// SSH, so [runtime.NewTmuxCarrier] drives a remote tmux-in-a-box session the
// same way it drives a Kubernetes pod (Nudge/Peek/SendKeys/Interrupt/
// ClearScrollback become tmux commands shipped over ssh). This is the Exec
// realization of the connection backend; the Stream (ssh -T) and AttachTTY
// (ssh -t) primitives are deferred.
//
// Once wired into runtime selection it is intended to replace the per-op relay
// shims — daytona's bd-ssh-shim, the in-sandbox Tailscale bootstrap, t3bridge's
// per-RPC WebSocket — with one connection that carries every op. It is not yet
// wired in.
//
// Host-key policy is StrictHostKeyChecking=accept-new (trust-on-first-use): an
// unknown host key is accepted and pinned on first contact, a changed key is
// refused. Supply Endpoint.KnownHostsPath in production to pin keys and avoid
// mutating the controller's default known_hosts.
package ssh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Endpoint addresses a box reachable over SSH.
type Endpoint struct {
	User           string // ssh user; empty addresses the host bare
	Host           string // hostname or IP (tailnet, DNS, or direct)
	Port           int    // ssh port; 0 means ssh's default (22)
	KeyPath        string // private key path; empty uses ssh's default resolution
	KnownHostsPath string // known_hosts path; empty uses ssh's default
}

// target returns the user@host (or bare host) form ssh addresses.
func (e Endpoint) target() string {
	if e.User == "" {
		return e.Host
	}
	return e.User + "@" + e.Host
}

// runner runs a remote command over the connection and returns its standard
// output and exit code. stdin, when non-nil, is fed to the remote command
// (used to ship a setup script to a remote sh). It is the transport seam: the
// default shells the ssh client; a future ControlMaster / x/crypto/ssh backend
// can replace it, and tests inject a fake. Such a backend MUST deliver stdin to
// the specific remote command invocation (per call), not a shared channel, so
// the execScript contract holds.
type runner interface {
	run(ctx context.Context, ep Endpoint, remoteArgv []string, stdin []byte) (stdout []byte, code int, err error)
}

// Conn is an SSH connection to a box. It implements [runtime.ExecProvider], so
// the tmux carrier drives a remote session over it.
type Conn struct {
	ep  Endpoint
	run runner
}

var _ runtime.ExecProvider = (*Conn)(nil)

// New returns a Conn to ep over the default ssh-client transport.
func New(ep Endpoint) *Conn {
	return &Conn{ep: ep, run: shellRunner{}}
}

// Exec runs argv on the box and returns its standard output and exit code (ssh
// propagates the remote command's exit code). The session name is unused: one
// endpoint is one box, and the carrier distinguishes sessions by its tmux
// target. A failure to reach the box (or context cancellation) yields err; a
// command that runs and exits non-zero (other than 255) yields that code with
// a nil error. Empty argv is a caller error (an empty remote command opens an
// interactive login shell over ssh), so it is rejected.
func (c *Conn) Exec(ctx context.Context, _ string, argv []string) ([]byte, int, error) {
	if len(argv) == 0 {
		return nil, -1, fmt.Errorf("ssh %s: empty argv", c.ep.target())
	}
	return c.run.run(ctx, c.ep, argv, nil)
}

// execScript runs `sh` on the box with script piped to its stdin, shipping a
// local setup script to the remote shell (mirroring the k8s exec-stdin path).
func (c *Conn) execScript(ctx context.Context, script []byte) ([]byte, int, error) {
	return c.run.run(ctx, c.ep, []string{"sh"}, script)
}

// shellRunner runs commands by shelling the ssh client. This is the v0
// transport; a multiplexed (ControlMaster) or in-process backend can replace
// it behind [runner] without changing the carrier-facing contract.
type shellRunner struct{}

func (shellRunner) run(ctx context.Context, ep Endpoint, remoteArgv []string, stdin []byte) ([]byte, int, error) {
	cmd := exec.CommandContext(ctx, "ssh", sshArgs(ep, remoteArgv)...)
	cmd.WaitDelay = 2 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}

	if err := cmd.Run(); err != nil {
		// Context cancellation/timeout is a transport failure, not a command exit.
		if ctx.Err() != nil {
			return nil, -1, fmt.Errorf("ssh %s: %w", ep.target(), ctx.Err())
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code := exitErr.ExitCode()
			if code == 255 {
				// ssh reserves 255 for its OWN failures (DNS, connection refused,
				// auth, host-key rejection, ProxyCommand). It is indistinguishable
				// from a remote command that genuinely exits 255, so treat 255 as a
				// transport failure — the safe collapse: never report a dropped
				// connection as a clean command result on the best-effort carrier
				// path (matches the k8s ExecProvider contract).
				msg := strings.TrimSpace(stderr.String())
				if msg == "" {
					msg = "connection failed (ssh exit 255)"
				}
				return nil, -1, fmt.Errorf("ssh %s: %s", ep.target(), msg)
			}
			// A non-zero (non-255) exit is the remote command's own result.
			return stdout.Bytes(), code, nil
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, -1, fmt.Errorf("ssh %s: %s", ep.target(), msg)
	}
	return stdout.Bytes(), 0, nil
}

// sshArgs builds the ssh client argv to run remoteArgv on ep. Option parsing
// is terminated with `--` before the destination so a dash-leading host can
// never be read as an option, and the remote command is POSIX-shell-quoted
// into a single argument the remote shell runs verbatim.
func sshArgs(ep Endpoint, remoteArgv []string) []string {
	args := []string{
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new",
	}
	if ep.KnownHostsPath != "" {
		args = append(args, "-o", "UserKnownHostsFile="+ep.KnownHostsPath)
	}
	if ep.KeyPath != "" {
		args = append(args, "-i", ep.KeyPath)
	}
	if ep.Port != 0 {
		args = append(args, "-p", strconv.Itoa(ep.Port))
	}
	args = append(args, "--", ep.target(), shellQuote(remoteArgv))
	return args
}

// shellQuote renders argv as a single POSIX shell command string (each
// argument single-quoted, embedded single quotes escaped as '\”).
func shellQuote(argv []string) string {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}
