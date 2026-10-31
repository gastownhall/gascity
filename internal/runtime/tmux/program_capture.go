package tmux

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/programstatus"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// OSC 7501 capture arming (design §5). A session whose runtime.Config
// declares capture gets, in the same tmux command list that creates or
// respawns its agent pane, the pane's tap nonce (@gc_ps_tap, bound to the
// pane id by set-option -F) and a pipe-pane -O into `gc-osc7501 tap`. tmux
// reads pane output only after a command list completes, so the tap sees the
// program's first byte. A relay-declared provider also runs inside
// `gc-osc7501 relay`, which answers the program's support probe.
//
// Nothing here runs for an undeclared session, which is every session while
// the [session] program_status switch is off: those keep today's tmux
// command lines byte for byte.

// The pane options of program-status capture. gc writes the nonce; only the
// tap writes the record, guarded by the nonce.
const (
	programTapOption    = "@gc_ps_tap"
	programStatusOption = "@gc_ps"
	programTextOption   = "@gc_ps_text"
)

// The oldest tmux capture arms under: pane options (-p), set-option -F and
// the read path's #{s/…} all predate it.
const (
	captureMinTmuxMajor = 3
	captureMinTmuxMinor = 2
)

// captureRandBytes is the entropy of a nonce's random part; its unpadded
// base64url form (16 characters of [A-Za-z0-9_-]) is a valid codec rand.
const captureRandBytes = 12

// captureProbeTimeout bounds the companion's version handshake.
const captureProbeTimeout = 5 * time.Second

// captureSocketPathRe is the socket-path alphabet the tap command needs no
// shell quoting for: tmux expands #{socket_path} into it unquoted.
var captureSocketPathRe = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// captureWarningOut receives capture warnings: a failed capability gate
// (once per cause) and an arm that failed after its pane was created or
// respawned. A var so tests can capture it.
var captureWarningOut io.Writer = os.Stderr

// captureCapability is what a passed capability gate resolved.
type captureCapability struct {
	companion string // the gc-osc7501 binary
	tmux      string // the tmux binary the tap publishes through
}

// capturePlan is one arm of one pane: the capability, the session's
// declaration and a fresh nonce rand.
type capturePlan struct {
	capability captureCapability
	app        string
	relay      bool
	rand       string
}

// captureGate is a Tmux's cached capability state (design §5.5). The
// companion is resolved at every arm, because an upgrade can remove a
// resolved path, and its version handshake is cached per path; the tmux
// check is cached per Tmux once tmux answers it. A failure leaves the
// session unarmed and is logged once per cause; heal arms the pane later.
type captureGate struct {
	// probe replaces the whole gate. Nil selects Tmux.probeCapture; tests
	// inject a capability without a companion or a server.
	probe func() (captureCapability, error)

	mu        sync.Mutex
	tmuxDone  bool  // the tmux check answered
	tmuxErr   error // its definitive verdict
	tmuxBin   string
	companion map[string]error // version verdict per companion path
	warned    map[string]bool  // gate failures already logged
}

// planCapture returns the arm plan for a session declaring arm, or nil when
// arm declares no capture or the capability gate fails (logged once per
// cause). It never fails the session.
func (t *Tmux) planCapture(arm runtime.ProgramStatusArm) *capturePlan {
	if !arm.Capture {
		return nil
	}
	if arm.App != "" && !programstatus.ValidApp(arm.App) {
		t.warnCaptureOnce(fmt.Sprintf("program-status app %q is not a codec app name; capture not armed", arm.App))
		return nil
	}
	capability, ok := t.passCaptureGate()
	if !ok {
		return nil
	}
	r, err := newCaptureRand()
	if err != nil {
		_, _ = fmt.Fprintf(captureWarningOut, "warning: program-status capture not armed: %v\n", err)
		return nil
	}
	return &capturePlan{capability: capability, app: arm.App, relay: arm.Relay, rand: r}
}

// passCaptureGate runs the capability gate, logging a failure once per
// cause. Arming and reading share it: while it fails nothing is armed and
// every reading is Unread.
func (t *Tmux) passCaptureGate() (captureCapability, bool) {
	capability, err := t.captureCapability()
	if err != nil {
		t.warnCaptureOnce(fmt.Sprintf("program-status capture unavailable: %v", err))
		return captureCapability{}, false
	}
	return capability, true
}

// captureGatePassed reports whether the capability gate passes (see
// passCaptureGate).
func (t *Tmux) captureGatePassed() bool {
	_, ok := t.passCaptureGate()
	return ok
}

// warnCaptureOnce logs msg the first time this Tmux sees it.
func (t *Tmux) warnCaptureOnce(msg string) {
	g := &t.capture
	g.mu.Lock()
	seen := g.warned[msg]
	if !seen {
		if g.warned == nil {
			g.warned = make(map[string]bool)
		}
		g.warned[msg] = true
	}
	g.mu.Unlock()
	if !seen {
		_, _ = fmt.Fprintf(captureWarningOut, "warning: %s\n", msg)
	}
}

// captureCapability runs the capability gate.
func (t *Tmux) captureCapability() (captureCapability, error) {
	if t.capture.probe != nil {
		return t.capture.probe()
	}
	return t.probeCapture()
}

// probeCapture is the production gate: the companion resolves and writes
// this gc's codec, and the city's tmux server is 3.2 or later on a socket
// path the tap command can carry unquoted.
func (t *Tmux) probeCapture() (captureCapability, error) {
	companion := t.cfg.ProgramStatusBin
	if companion == "" {
		var err error
		if companion, err = programstatus.LookupCompanion(); err != nil {
			return captureCapability{}, err
		}
	}
	if err := t.checkCompanion(companion); err != nil {
		return captureCapability{}, err
	}
	tmuxBin, err := t.checkCaptureTmux()
	if err != nil {
		return captureCapability{}, err
	}
	return captureCapability{companion: companion, tmux: tmuxBin}, nil
}

// checkCompanion runs the version handshake. A companion that answers with
// this gc's codec, or with another codec, is not asked again; one that does
// not answer is asked again at the next arm.
func (t *Tmux) checkCompanion(path string) error {
	g := &t.capture
	g.mu.Lock()
	verdict, done := g.companion[path]
	g.mu.Unlock()
	if done {
		return verdict
	}
	ctx, cancel := context.WithTimeout(context.Background(), captureProbeTimeout)
	defer cancel()
	_, err := programstatus.ProbeCompanion(ctx, path)
	if err != nil && !errors.Is(err, programstatus.ErrCompanionCodec) {
		return err
	}
	g.mu.Lock()
	if g.companion == nil {
		g.companion = make(map[string]error)
	}
	g.companion[path] = err
	g.mu.Unlock()
	return err
}

// checkCaptureTmux checks the city's tmux server once it answers. A server
// that is not running yet is started by the check (start-server), exactly as
// the new-session it precedes would start it.
func (t *Tmux) checkCaptureTmux() (string, error) {
	g := &t.capture
	g.mu.Lock()
	if g.tmuxDone {
		bin, err := g.tmuxBin, g.tmuxErr
		g.mu.Unlock()
		return bin, err
	}
	g.mu.Unlock()

	out, err := t.run("start-server", ";", "display-message", "-p", "#{version}|#{socket_path}")
	if err != nil {
		return "", fmt.Errorf("reading tmux version and socket path: %w", err)
	}
	bin, verdict := captureTmuxVerdict(strings.TrimSpace(out))
	g.mu.Lock()
	g.tmuxDone, g.tmuxBin, g.tmuxErr = true, bin, verdict
	g.mu.Unlock()
	return bin, verdict
}

// captureTmuxVerdict judges display-message's "<version>|<socket_path>" and
// resolves the tmux binary the tap publishes through.
func captureTmuxVerdict(out string) (string, error) {
	version, socket, ok := strings.Cut(out, "|")
	if !ok {
		return "", fmt.Errorf("tmux reported %q, not <version>|<socket_path>", out)
	}
	if !tmuxVersionAtLeast(version, captureMinTmuxMajor, captureMinTmuxMinor) {
		return "", fmt.Errorf("tmux %q is older than %d.%d", version, captureMinTmuxMajor, captureMinTmuxMinor)
	}
	if !captureSocketPathRe.MatchString(socket) {
		return "", fmt.Errorf("tmux socket path %q holds characters outside [A-Za-z0-9._/-]", socket)
	}
	bin, err := exec.LookPath("tmux")
	if err != nil {
		return "", fmt.Errorf("resolving the tmux binary for the tap: %w", err)
	}
	return bin, nil
}

// tmuxVersionAtLeast reports whether a #{version} string ("3.4", "3.2a",
// "next-3.5", "master") is at least major.minor. A development build
// ("master", "next-…") counts as new enough.
func tmuxVersionAtLeast(version string, major, minor int) bool {
	if version == "master" {
		return true
	}
	version = strings.TrimPrefix(version, "next-")
	maj, rest, ok := strings.Cut(version, ".")
	if !ok {
		return false
	}
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	gotMajor, err := strconv.Atoi(maj)
	if err != nil {
		return false
	}
	gotMinor, err := strconv.Atoi(rest[:end])
	if err != nil {
		return false
	}
	return gotMajor > major || (gotMajor == major && gotMinor >= minor)
}

// newCaptureRand returns a fresh nonce rand: 16 characters of [A-Za-z0-9_-].
func newCaptureRand() (string, error) {
	var b [captureRandBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generating capture nonce: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// wrapRelay returns command run under `gc-osc7501 relay`, as tmux would have
// run it (/bin/sh -c), when the plan declares the relay; otherwise command.
// The wrap is the leaf's: it never reaches runtime.Config.Command, so the
// launch fingerprint does not see it. Nil-safe.
func (p *capturePlan) wrapRelay(command string) string {
	if p == nil || !p.relay || command == "" {
		return command
	}
	return shellquote.Join([]string{p.capability.companion, "relay", "--", "/bin/sh", "-c", command})
}

// tapCommand is the pipe-pane command: the tap reading the pane's output.
// tmux expands its formats (#{socket_path}, #{pane_id}) when it runs it, so
// every literal '#' is doubled. The nonce's words ("-t <pane_id> -n <rand>")
// come first after "tap", ahead of the paths, so a process listing that cuts
// long argv short still carries them whole.
func (p *capturePlan) tapCommand() string {
	literal := func(s string) string { return strings.ReplaceAll(shellquote.Join([]string{s}), "#", "##") }
	var b strings.Builder
	b.WriteString("exec ")
	b.WriteString(literal(p.capability.companion))
	b.WriteString(" tap -t #{pane_id} -n ")
	b.WriteString(p.rand)
	b.WriteString(" -x ")
	b.WriteString(literal(p.capability.tmux))
	b.WriteString(" -S #{socket_path}")
	if p.app != "" {
		b.WriteString(" -a ")
		b.WriteString(p.app)
	}
	return b.String()
}

// armCommands are the commands arming target's capture: the nonce bound to
// the pane id, then the tap. clearRecord also unsets the record an earlier
// incarnation's tap published, for a pane that outlives its program.
func (p *capturePlan) armCommands(target string, clearRecord bool) [][]string {
	cmds := [][]string{{"set-option", "-p", "-F", "-t", target, programTapOption, p.rand + ".#{pane_id}"}}
	if clearRecord {
		cmds = append(cmds,
			[]string{"set-option", "-pu", "-t", target, programStatusOption},
			[]string{"set-option", "-pu", "-t", target, programTextOption})
	}
	return append(cmds, []string{"pipe-pane", "-O", "-t", target, p.tapCommand()})
}

// disarmCommands are the commands removing target's capture: the nonce, then
// the record, then the tap. With the nonce gone first, the tap's last write
// cannot land; pipe-pane comes last because it fails on an exited pane, which
// would otherwise abort the unsets.
func disarmCommands(target string) [][]string {
	return [][]string{
		{"set-option", "-pu", "-t", target, programTapOption},
		{"set-option", "-pu", "-t", target, programStatusOption},
		{"set-option", "-pu", "-t", target, programTextOption},
		{"pipe-pane", "-t", target},
	}
}

// programStatusMode is the program-status switch this Tmux runs under:
// shadow or on, and off for anything else.
func (t *Tmux) programStatusMode() runtime.ProgramStatusMode {
	switch mode := t.cfg.ProgramStatus; mode {
	case runtime.ProgramStatusShadow, runtime.ProgramStatusOn:
		return mode
	default:
		return runtime.ProgramStatusOff
	}
}

// tmuxArgvList joins commands into one tmux argv, separated by bare ";"
// elements. One command is returned as it is.
func tmuxArgvList(cmds [][]string) []string {
	if len(cmds) == 1 {
		return cmds[0]
	}
	var argv []string
	for i, c := range cmds {
		if i > 0 {
			argv = append(argv, ";")
		}
		argv = append(argv, c...)
	}
	return argv
}

// tmuxOutputError is a failed tmux invocation that printed output first: a
// command list whose later command failed after an earlier one ran and
// printed (new-session -P's pane id, the respawn marker). It wraps the
// invocation's error, so errors.Is and the message are unchanged.
type tmuxOutputError struct {
	Stdout string // the trimmed output printed before the failure
	Err    error
}

func (e *tmuxOutputError) Error() string { return e.Err.Error() }

func (e *tmuxOutputError) Unwrap() error { return e.Err }

// outputBeforeError returns the output a failed invocation printed, "" when
// it printed none.
func outputBeforeError(err error) string {
	var out *tmuxOutputError
	if errors.As(err, &out) {
		return out.Stdout
	}
	return ""
}

// respawnMarkerFormat is what the respawn list's display-message prints once
// respawn-pane has run: the respawned pane's id.
const respawnMarkerFormat = "#{pane_id}"

// respawnPaneList is the one respawn command list: respawn-pane -k on target,
// a display-message marker proving it ran, then follow. Every command that
// must reach the new process before tmux reads its output goes in follow
// (capture's re-arm today; an identity-changing relaunch's input fence).
func respawnPaneList(target, workDir, command string, follow [][]string) [][]string {
	respawn := []string{"respawn-pane", "-k", "-t", target}
	if workDir != "" {
		respawn = append(respawn, "-c", workDir)
	}
	respawn = append(respawn, command)
	cmds := [][]string{respawn, {"display-message", "-p", "-t", target, respawnMarkerFormat}}
	return append(cmds, follow...)
}

// runRespawnList runs respawnPaneList. err is non-nil when the pane was not
// respawned; followErr is non-nil when it was, but a follow command failed.
func (t *Tmux) runRespawnList(target, workDir, command string, follow [][]string) (followErr, err error) {
	_, err = t.run(tmuxArgvList(respawnPaneList(target, workDir, command, follow))...)
	if err == nil {
		return nil, nil
	}
	if wellFormedTmuxID(firstLine(outputBeforeError(err)), '%') {
		return err, nil
	}
	return nil, err
}

// firstLine returns s up to its first newline.
func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(line)
}
