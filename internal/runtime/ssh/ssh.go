// Package ssh is an SSH connection backend for the Runtime Provider Protocol.
// It realizes the exec connection primitive — run a command in the box — over
// SSH, so [runtime.NewTmuxCarrier] drives a remote tmux-in-a-box session the
// same way it drives a Kubernetes pod (Nudge/Peek/SendKeys/Interrupt/
// ClearScrollback become tmux commands shipped over ssh). This is the Exec
// realization of the connection backend; the Stream (ssh -T) and AttachTTY
// (ssh -t) primitives are deferred.
//
// It is selectable via the "ssh:" runtime prefix (ssh:[user@]host[:port]; key and
// known_hosts resolve from ~/.ssh/config). The backend serves the direct-ssh
// topology — gc dials a reachable box — carrying exec today and a future stream
// (ssh -T) / attach (ssh -t) over one connection. It does NOT subsume a sandbox
// runtime's controller reach-back (the reverse, sandbox-to-controller env.ledger
// path, e.g. an in-sandbox tunnel bootstrap): a separate concern.
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
	"sync"
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

// ParseEndpoint parses a "[user@]host[:port]" target into an Endpoint, leaving
// KeyPath/KnownHostsPath to ssh's own resolution (~/.ssh/config). It backs the
// anonymous "ssh:" selection prefix; the named, structured form (with explicit
// key/known_hosts) is a [runtimes.<name>] declaration. An IPv6 host must be
// bracketed, e.g. "[::1]:22".
func ParseEndpoint(target string) (Endpoint, error) {
	s := strings.TrimSpace(target)
	if s == "" {
		return Endpoint{}, fmt.Errorf("ssh endpoint: empty target")
	}
	var ep Endpoint
	if at := strings.LastIndex(s, "@"); at >= 0 {
		if at == 0 {
			return Endpoint{}, fmt.Errorf("ssh endpoint %q: empty user before @", target)
		}
		ep.User, s = s[:at], s[at+1:]
	}
	switch {
	case strings.HasPrefix(s, "["): // bracketed IPv6, optional :port after "]"
		end := strings.Index(s, "]")
		if end < 0 {
			return Endpoint{}, fmt.Errorf("ssh endpoint %q: unterminated IPv6 bracket", target)
		}
		ep.Host = s[1:end]
		if rest := s[end+1:]; strings.HasPrefix(rest, ":") {
			port, err := parsePort(rest[1:])
			if err != nil {
				return Endpoint{}, fmt.Errorf("ssh endpoint %q: %w", target, err)
			}
			ep.Port = port
		} else if rest != "" {
			return Endpoint{}, fmt.Errorf("ssh endpoint %q: trailing %q after host", target, rest)
		}
	case strings.LastIndex(s, ":") >= 0: // host:port (unbracketed)
		colon := strings.LastIndex(s, ":")
		host := s[:colon]
		// A colon remaining in the host means this is an unbracketed IPv6
		// literal — reject it rather than mis-split it into host + bogus port.
		if strings.Contains(host, ":") {
			return Endpoint{}, fmt.Errorf("ssh endpoint %q: unbracketed IPv6 host; use [host]:port", target)
		}
		port, err := parsePort(s[colon+1:])
		if err != nil {
			return Endpoint{}, fmt.Errorf("ssh endpoint %q: %w", target, err)
		}
		ep.Port, ep.Host = port, host
	default:
		ep.Host = s
	}
	if ep.Host == "" {
		return Endpoint{}, fmt.Errorf("ssh endpoint %q: missing host", target)
	}
	return ep, nil
}

// parsePort parses a TCP port in the valid range 1-65535.
func parsePort(s string) (int, error) {
	p, err := strconv.Atoi(s)
	if err != nil || p < 1 || p > 65535 {
		return 0, fmt.Errorf("invalid port %q", s)
	}
	return p, nil
}

// runner runs a remote command over the connection and returns its standard
// output and exit code. stdin, when non-nil, is fed to the remote command
// (used to ship a setup script to a remote sh). It is the transport seam: the
// default shells the ssh client; a future ControlMaster / x/crypto/ssh backend
// can replace it, and tests inject a fake. Such a backend MUST deliver stdin to
// the specific remote command invocation (per call), not a shared channel, so
// the execScript contract holds. A runner errs only when the remote command
// produced no result, and wraps [errAuthFailed] when the box refused this
// client.
type runner interface {
	run(ctx context.Context, ep Endpoint, remoteArgv []string, stdin []byte) (stdout []byte, code int, err error)
}

// errAuthFailed marks a connection the box refused at authentication: it
// rejected this client's key, or its host key did not match the key pinned
// for it. A refusal is the box's answer, not a box that could not answer, and
// it lasts until an operator fixes the key or the pin, so it is not
// [runtime.ErrRuntimeUnavailable]: a start fails with ssh's message and backs
// off instead of deferring on every tick, and a changed host key, a security
// signal, reaches the operator.
var errAuthFailed = errors.New("authentication failed")

// authFailureMessages are the OpenSSH client's words for a refusal at
// authentication: every offered key rejected, the server cutting the offers
// short, and a host key that does not match its pin.
var authFailureMessages = []string{
	"Permission denied (",
	"Too many authentication failures",
	"Host key verification failed",
}

// exit255Error is the error for an ssh client that exited 255 with stderr:
// [errAuthFailed] when stderr names a refusal at authentication, otherwise a
// failure to reach the box.
func exit255Error(ep Endpoint, stderr string) error {
	msg := strings.TrimSpace(stderr)
	if msg == "" {
		msg = "connection failed (ssh exit 255)"
	}
	for _, refusal := range authFailureMessages {
		if strings.Contains(msg, refusal) {
			return fmt.Errorf("ssh %s: %w: %s", ep.target(), errAuthFailed, msg)
		}
	}
	return fmt.Errorf("ssh %s: %s", ep.target(), msg)
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
	return &Conn{ep: ep, run: &shellRunner{}}
}

// Exec runs argv on the box and returns its standard output and exit code (ssh
// propagates the remote command's exit code). The session name is unused: one
// endpoint is one box, and the carrier distinguishes sessions by its tmux
// target. A failure to reach the box (or context cancellation) yields an err
// wrapping [runtime.ErrRuntimeUnavailable]: the box could not answer, which is
// never an answer. A box that refuses this client at authentication yields an
// err wrapping [errAuthFailed] instead. A command that runs and exits non-zero
// (other than 255) yields that code with a nil error. Empty argv is a caller
// error (an empty remote command opens an interactive login shell over ssh),
// so it is rejected.
func (c *Conn) Exec(ctx context.Context, _ string, argv []string) ([]byte, int, error) {
	if len(argv) == 0 {
		return nil, -1, fmt.Errorf("ssh %s: empty argv", c.ep.target())
	}
	return transportResult(c.run.run(ctx, c.ep, argv, nil))
}

// execScript runs `sh` on the box with script piped to its stdin, shipping a
// local setup script to the remote shell (mirroring the k8s exec-stdin path).
func (c *Conn) execScript(ctx context.Context, script []byte) ([]byte, int, error) {
	return transportResult(c.run.run(ctx, c.ep, []string{"sh"}, script))
}

// transportResult marks a runner error as [runtime.ErrRuntimeUnavailable]: a
// runner returns an error only when the remote command produced no result. A
// refusal at authentication ([errAuthFailed]) is the box's answer and keeps
// its own class.
func transportResult(out []byte, code int, err error) ([]byte, int, error) {
	if err != nil && !transportFailed(err) {
		err = fmt.Errorf("%w: %w", runtime.ErrRuntimeUnavailable, err)
	}
	return out, code, err
}

// transportFailed reports whether err is one of the two ways a remote command
// produces no result: the box could not answer
// ([runtime.ErrRuntimeUnavailable]), or it refused this client
// ([errAuthFailed]). A verb that reads its other failures as benign (an empty
// pane, a best-effort write) must still report these: neither is the box's
// answer about the session.
func transportFailed(err error) bool {
	return errors.Is(err, runtime.ErrRuntimeUnavailable) || errors.Is(err, errAuthFailed)
}

// shellRunner runs commands by shelling the ssh client. This is the v0
// transport; a multiplexed (ControlMaster) or in-process backend can replace
// it behind [runner] without changing the carrier-facing contract. One runner
// serves one Conn, so the user's ssh configuration is read once (see
// [shellRunner.configuredFor]).
type shellRunner struct {
	configOnce sync.Once
	configRead chan struct{} // closed once configured holds the probe's answer
	configured sshConfigured
	// readConfig reads the user's ssh configuration for an endpoint; nil is
	// [probeSSHConfigured]. Tests replace it.
	readConfig func(Endpoint) sshConfigured
}

// configuredFor returns what the user's ssh configuration sets for ep. It is
// read once per runner, by a probe that runs apart from every caller under
// its own bound, so a caller's cancellation never leaves the read half done
// and nothing needs retrying. A caller waits for the probe only while its own
// ctx allows; one whose ctx ends first gets every default, and its ssh call
// then fails on that ctx.
func (r *shellRunner) configuredFor(ctx context.Context, ep Endpoint) sshConfigured {
	r.configOnce.Do(func() {
		read := r.readConfig
		if read == nil {
			read = probeSSHConfigured
		}
		r.configRead = make(chan struct{})
		go func() {
			defer close(r.configRead)
			r.configured = read(ep)
		}()
	})
	select {
	case <-r.configRead:
		return r.configured
	case <-ctx.Done():
		return sshConfigured{}
	}
}

func (r *shellRunner) run(ctx context.Context, ep Endpoint, remoteArgv []string, stdin []byte) ([]byte, int, error) {
	cmd := exec.CommandContext(ctx, "ssh", sshArgs(ep, r.configuredFor(ctx, ep), remoteArgv)...)
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
				return nil, -1, exit255Error(ep, stderr.String())
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

// Transport bounds sshArgs adds when the user's ssh configuration leaves them
// unset, so a dead box fails within seconds instead of the kernel's TCP
// timeouts: ConnectTimeout bounds the dial, and the keepalives drop a session
// whose peer stops answering (ServerAliveInterval x ServerAliveCountMax). The
// count is the user's, or ssh's default of 3: sshArgs never sets it, so a
// count configured without an interval still applies to the interval gc adds.
const (
	defaultConnectTimeout      = "10"
	defaultServerAliveInterval = "10"
)

// sshConfiguredProbeTimeout bounds `ssh -G`, which reads the configuration
// files (and runs the command of any Match exec in them).
const sshConfiguredProbeTimeout = 5 * time.Second

// sshConfiguredProbeWaitDelay bounds the wait for `ssh -G`'s output once ssh
// has exited or been killed: a command a Match exec started can outlive ssh
// holding that output open.
const sshConfiguredProbeWaitDelay = time.Second

// sshConfigured records which transport bounds the user's ssh configuration
// (~/.ssh/config and the system file) already sets for an endpoint. A
// command-line -o beats the configuration files, so sshArgs adds a default
// only where the user set none.
type sshConfigured struct {
	connectTimeout bool
	serverAlive    bool // ServerAliveInterval; the count means nothing without it
}

// probeSSHConfigured is [readSSHConfigured] of the installed ssh client,
// bounded by [sshConfiguredProbeTimeout].
func probeSSHConfigured(ep Endpoint) sshConfigured {
	ctx, cancel := context.WithTimeout(context.Background(), sshConfiguredProbeTimeout)
	defer cancel()
	return readSSHConfigured(ctx, "ssh", ep)
}

// readSSHConfigured asks client, an ssh client executable, for its effective
// configuration for ep under ctx (`ssh -G` reads the configuration files and
// dials nothing). A client that cannot answer leaves every default in place.
// The wait for its output ends [sshConfiguredProbeWaitDelay] after ssh exits
// or ctx kills it, whoever still holds that output open.
func readSSHConfigured(ctx context.Context, client string, ep Endpoint) sshConfigured {
	args := []string{"-G"}
	if ep.Port != 0 {
		args = append(args, "-p", strconv.Itoa(ep.Port))
	}
	cmd := exec.CommandContext(ctx, client, append(args, "--", ep.target())...)
	cmd.WaitDelay = sshConfiguredProbeWaitDelay
	out, err := cmd.Output()
	// exec.ErrWaitDelay: ssh answered and exited 0, and only a command it ran
	// still held the output; the answer stands.
	if err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		return sshConfigured{}
	}
	return parseSSHConfigured(string(out))
}

// parseSSHConfigured reads `ssh -G` output, in which an unset ConnectTimeout
// prints "none" and an unset ServerAliveInterval prints "0".
func parseSSHConfigured(out string) sshConfigured {
	var c sshConfigured
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.ToLower(key) {
		case "connecttimeout":
			c.connectTimeout = value != "" && value != "none"
		case "serveraliveinterval":
			c.serverAlive = value != "" && value != "0"
		}
	}
	return c
}

// sshArgs builds the ssh client argv to run remoteArgv on ep. Option parsing
// is terminated with `--` before the destination so a dash-leading host can
// never be read as an option, and the remote command is POSIX-shell-quoted
// into a single argument the remote shell runs verbatim. Each transport bound
// the user's configuration leaves unset (see [sshConfigured]) gets a default.
//
// LogLevel is always ERROR, whatever the user's configuration sets: ssh's own
// error lines are how [exit255Error] tells a refusal from a box it could not
// reach, and under LogLevel QUIET a refusal prints nothing, so it would read
// as an unreachable box and its start would defer on every tick.
// ERROR also keeps pre-auth banners and the known-hosts notice out of that
// text. It governs only the client's own messages: the remote command's
// stderr is untouched.
func sshArgs(ep Endpoint, configured sshConfigured, remoteArgv []string) []string {
	args := []string{
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "LogLevel=ERROR",
	}
	if !configured.connectTimeout {
		args = append(args, "-o", "ConnectTimeout="+defaultConnectTimeout)
	}
	if !configured.serverAlive {
		args = append(args, "-o", "ServerAliveInterval="+defaultServerAliveInterval)
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
