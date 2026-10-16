package tmux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/runtime"
)

// inputFenceOption is the pane user option that names the incarnation a
// pane's process was started as: "<digest>.<pane_id>", the fence's digest
// (runtime.InputFence.Digest) and the pane's own id. The id makes a value
// inherited from window, session or global scope (which format lookup
// would show on every pane without its own) read as no fence at all. Like
// the agent-pane marker it belongs to the pane: it survives respawn-pane, and
// a new session's pane starts without it. A new digest format needs a new
// option name, since a pane never re-adopts an own value.
const inputFenceOption = "@gc_fence"

// The tmux formats a fenced operation reads and conditions on.
const (
	// fenceReadFormat reads a pane's fence value, id and death.
	fenceReadFormat = "#{" + inputFenceOption + "}|#{pane_id}|#{pane_dead}"
	// fenceNotOwnCondition is true when the pane has no own fence value.
	fenceNotOwnCondition = "#{?#{m:*.#{pane_id},#{" + inputFenceOption + "}},,1}"
	// fenceMissPrefix leads the output of a guarded command's else branch.
	fenceMissPrefix = "gc-fence-miss:"
	// fenceOwnPrefix leads the output of an adoption that found an own value.
	fenceOwnPrefix = "gc-fence-own:"
	// fenceAdoptMissPrefix leads the output of a caller-proven adoption whose
	// pane runs another process.
	fenceAdoptMissPrefix = "gc-fence-adopt-miss:"
)

// ErrPaneRootChanged reports that AdoptInputFence found another process at
// the pane's root than the one the caller proved: the caller must re-prove.
var ErrPaneRootChanged = errors.New("agent pane root process changed since it was proven")

var _ runtime.FencedInputProvider = (*Provider)(nil)

// InputFencing reports that tmux fences every session's input atomically:
// each input command runs inside a tmux if-shell whose condition is the
// pane's fence. It is a static trait and reads no tmux state, so it reports
// atomic for the one path that is checked, the exception InputFencingAtomic
// names: a detached Gemini session's input through its hidden attach client
// is a PTY write, preceded by a read that its active pane is the verified
// agent pane and holds the fence (paneGuard.checkLive).
func (p *Provider) InputFencing(string) runtime.InputFencing {
	return runtime.InputFencingAtomic
}

// NudgeFenced delivers content as Nudge (NudgeNow when n.Immediate) does,
// with every keystroke conditional on the agent pane's fence naming
// n.Fence's incarnation. tmux cannot deduplicate, so n.DeliveryKey is unused.
func (p *Provider) NudgeFenced(ctx context.Context, name string, content []runtime.ContentBlock, n runtime.FencedNudge) error {
	return p.fenced(ctx, name, n.Fence, true, func(ctx context.Context) error {
		if n.Immediate {
			return p.nudgeNow(ctx, name, content)
		}
		return p.nudge(ctx, name, content)
	})
}

// SendKeysFenced sends keys as SendKeys does, each conditional on fence.
func (p *Provider) SendKeysFenced(ctx context.Context, name string, fence runtime.InputFence, keys ...string) error {
	return p.fenced(ctx, name, fence, false, func(ctx context.Context) error {
		return p.sendKeys(ctx, name, keys...)
	})
}

// InterruptFenced interrupts as Interrupt does, conditional on fence.
func (p *Provider) InterruptFenced(ctx context.Context, name string, fence runtime.InputFence) error {
	return p.fenced(ctx, name, fence, false, func(ctx context.Context) error {
		return p.interrupt(ctx, name)
	})
}

// fencedCtx runs body under the runtime fence ctx carries, or unguarded when
// it carries none: the ctx-taking input methods (ClearInput,
// DismissKnownDialogs, ResetInterruptedTurn) honor a caller's fence this way.
func (p *Provider) fencedCtx(ctx context.Context, name string, body func(context.Context) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if f, ok := runtime.InputFenceFrom(ctx); ok {
		return p.fenced(ctx, name, f, false, body)
	}
	return body(ctx)
}

// withoutInputFence is ctx with neither a runtime input fence nor a fenced
// operation's guard: Start and Relaunch type into the incarnation they are
// creating, never under a caller's fence (FI9).
func withoutInputFence(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	if _, ok := runtime.InputFenceFrom(ctx); ok {
		ctx = runtime.WithInputFence(ctx, runtime.InputFence{})
	}
	if paneGuardFrom(ctx) != nil {
		ctx = withPaneGuard(ctx, nil)
	}
	return ctx
}

// inputCtx is the context an input command of ctx's operation runs under:
// ctx inside a fenced operation, else context.Background(), as unfenced
// input always ran (FI7).
func inputCtx(ctx context.Context) context.Context {
	if paneGuardFrom(ctx) != nil {
		return ctx
	}
	return context.Background()
}

// fenced runs body as one fenced operation on session name: it validates f,
// checks the agent pane's fence before any input or wait (adopting the
// session's own identity once if the pane holds none), and then runs every
// input command body sends through the guard (Tmux.runCtx). Any miss wins
// over body's own result. textOnly is set for a nudge: only its text (and
// what follows it) makes a miss partial, not the preparatory keys before.
func (p *Provider) fenced(ctx context.Context, name string, f runtime.InputFence, textOnly bool, body func(context.Context) error) error {
	if err := f.Validate(); err != nil {
		return err
	}
	if !validSessionNameRe.MatchString(name) {
		return fmt.Errorf("fenced input to %q: %w", name, ErrInvalidSessionName)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if g := paneGuardFrom(ctx); g != nil && g.name == name && g.digest == f.Digest() {
		return body(ctx) // already inside this fenced operation
	}
	g := newPaneGuard(name, f.Digest(), textOnly)
	ctx = withPaneGuard(ctx, g)
	if err := g.verify(ctx, p.tm); err != nil {
		return err
	}
	return g.finish(body(ctx))
}

// AdoptInputFence records f as the incarnation of name's agent pane, but
// only while that pane's root process is panePID, which the caller proved
// runs f's incarnation (DES-BX D9). It returns ErrPaneRootChanged when the
// pane runs another process: the caller re-proves.
func (p *Provider) AdoptInputFence(name string, f runtime.InputFence, panePID int) error {
	if err := f.Validate(); err != nil {
		return err
	}
	if panePID <= 0 {
		return fmt.Errorf("adopting input fence for %q: invalid pane pid %d", name, panePID)
	}
	target, err := p.tm.agentPaneTarget(name)
	if err != nil {
		return err
	}
	if !guardedPaneTarget(target, name) {
		return fmt.Errorf("adopting input fence for %q: %q is not an agent pane target", name, target)
	}
	set, err := tmuxCommandString("set-option", "-p", "-F", "-t", target, inputFenceOption, f.Digest()+".#{pane_id}")
	if err != nil {
		return err
	}
	miss, err := tmuxCommandString("display-message", "-p", "-t", target, fenceAdoptMissPrefix+"#{pane_pid}")
	if err != nil {
		return err
	}
	out, err := p.tm.run("if-shell", "-F", "-t", target, "#{==:#{pane_pid},"+strconv.Itoa(panePID)+"}", set, miss)
	if err != nil {
		return err
	}
	switch out = strings.TrimSpace(out); {
	case out == "":
		return nil
	case strings.HasPrefix(out, fenceAdoptMissPrefix):
		return fmt.Errorf("adopting input fence for %q: pane root is pid %s, not %d: %w", name, strings.TrimPrefix(out, fenceAdoptMissPrefix), panePID, ErrPaneRootChanged)
	}
	return fmt.Errorf("adopting input fence for %q: unexpected tmux output %q", name, out)
}

// publishInputFence sets a new pane's fence from the environment its
// process was started with, when that names an incarnation. Best-effort: a
// pane left without one is adopted from the session environment by its
// first fenced operation.
func (t *Tmux) publishInputFence(session, pane string, env map[string]string) {
	f, ok := runtime.InputFenceFromEnv(env)
	if !ok {
		return
	}
	target := strings.TrimSpace(pane)
	if !wellFormedTmuxID(target, '%') {
		target = paneTarget(session)
	}
	if _, err := t.run("set-option", "-p", "-F", "-t", target, inputFenceOption, f.Digest()+".#{pane_id}"); err != nil {
		_, _ = fmt.Fprintf(agentPaneWarningOut, "warning: session %q: publishing input fence on %s: %v\n", session, target, err)
	}
}

// republishInputFence sets session's agent-pane fence from the session
// environment, the identity a respawned process inherits. Best-effort, as
// publishInputFence.
//
// respawn-pane keeps the pane's @gc_fence, and this runs after it as its own
// command, so the respawned process briefly (or, if this fails, until the
// next publish) runs under the value the pane held. That is the same
// incarnation while Relaunch cannot change identity. An identity-changing
// relaunch (V2-2) must issue respawn-pane and this set-option as one tmux
// command list, or old-fence input could reach the new process.
func (t *Tmux) republishInputFence(session string) {
	env, err := t.GetAllEnvironment(session)
	if err != nil {
		return
	}
	f, ok := runtime.InputFenceFromEnv(env)
	if !ok {
		return
	}
	target, err := t.agentPaneTarget(session)
	if err != nil {
		return
	}
	if _, err := t.run("set-option", "-p", "-F", "-t", target, inputFenceOption, f.Digest()+".#{pane_id}"); err != nil {
		_, _ = fmt.Fprintf(agentPaneWarningOut, "warning: session %q: republishing input fence: %v\n", session, err)
	}
}

// paneGuard is one fenced operation on one session: the digest its input
// must find on the agent pane, whether any of its effect was applied, and
// the first miss, after which every further input is refused unsent.
type paneGuard struct {
	name   string
	digest string
	// textOnly limits applied to text input and what follows it (a nudge):
	// a copy-mode exit, composer clear or dialog key before the text is not
	// the operation's effect, so a miss after it is not partial.
	textOnly bool

	mu sync.Mutex
	// pane is the agent pane id ("%P") verify proved.
	pane    string
	applied bool
	miss    *runtime.FenceMismatchError
	// unavailable is the first guarded command that found no tmux server.
	// It is the operation's result if body swallowed it, so a fenced
	// operation never reports success it could not deliver.
	unavailable error
}

type paneGuardKey struct{}

func newPaneGuard(name, digest string, textOnly bool) *paneGuard {
	return &paneGuard{name: name, digest: digest, textOnly: textOnly}
}

func withPaneGuard(ctx context.Context, g *paneGuard) context.Context {
	return context.WithValue(ctx, paneGuardKey{}, g)
}

// paneGuardFrom returns the guard of the fenced operation ctx belongs to,
// nil outside one.
func paneGuardFrom(ctx context.Context) *paneGuard {
	if ctx == nil {
		return nil
	}
	g, _ := ctx.Value(paneGuardKey{}).(*paneGuard)
	return g
}

// recordMiss records the operation's first miss and returns it.
func (g *paneGuard) recordMiss(found string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.miss == nil {
		g.miss = &runtime.FenceMismatchError{Session: g.name, Found: found, Partial: g.applied}
	}
	m := *g.miss
	return &m
}

// missed returns the recorded miss, nil when none.
func (g *paneGuard) missed() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.miss == nil {
		return nil
	}
	m := *g.miss
	return &m
}

// markApplied records input that reached the fenced incarnation: text, or
// keys. Keys before a nudge's text are not its effect (textOnly).
func (g *paneGuard) markApplied(text bool) {
	if g.textOnly && !text {
		return
	}
	g.mu.Lock()
	g.applied = true
	g.mu.Unlock()
}

func (g *paneGuard) verifiedPane() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pane
}

// finish is the operation's result: its miss when it had one, else err, or
// the server it lost when body swallowed that.
func (g *paneGuard) finish(err error) error {
	if miss := g.missed(); miss != nil {
		return miss
	}
	g.mu.Lock()
	unavailable := g.unavailable
	g.mu.Unlock()
	if err == nil && unavailable != nil {
		return unavailable
	}
	return err
}

// gone turns an error the unfenced path would absorb as "session gone, so
// nothing to do" into what a fenced operation reports: a missing session or
// agent pane is a miss (absent), a missing server is ErrRuntimeUnavailable.
// Other errors pass through.
func (g *paneGuard) gone(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrNoCurrentTarget), errors.Is(err, ErrSessionNotFound), errors.Is(err, errAgentPaneGone):
		return g.recordMiss(runtime.FenceFoundAbsent)
	case errors.Is(err, ErrNoServer):
		return &fenceUnavailableError{session: g.name, cause: err}
	}
	return err
}

// fenceUnavailableError is a fenced operation that found no tmux server:
// runtime.ErrRuntimeUnavailable, unwrapping to the tmux error too. Its text
// leaves out tmux's own, which runtime.IsSessionGone reads as gone: a missing
// server is unavailable, not gone (F9).
type fenceUnavailableError struct {
	session string
	cause   error
}

func (e *fenceUnavailableError) Error() string {
	return fmt.Sprintf("%s: fenced input to %q: tmux server unreachable", runtime.ErrRuntimeUnavailable.Error(), e.session)
}

func (e *fenceUnavailableError) Unwrap() []error {
	return []error{runtime.ErrRuntimeUnavailable, e.cause}
}

// fenceRead is one read of a pane's fence. A read with no pane id found no
// session: tmux answers display-message -t and if-shell -F -t for a session
// that no longer exists (while the server lives) with exit 0 and every
// field empty.
type fenceRead struct {
	value, pane string
	dead        bool
}

func parseFenceRead(out string) (fenceRead, bool) {
	parts := strings.Split(strings.TrimSpace(out), "|")
	switch {
	case len(parts) != 3:
		return fenceRead{}, false
	case parts[1] == "":
		return fenceRead{}, true
	case !wellFormedTmuxID(parts[1], '%'):
		return fenceRead{}, false
	}
	return fenceRead{value: parts[0], pane: parts[1], dead: parts[2] == "1"}, true
}

// absent reports whether the read found no session.
func (r fenceRead) absent() bool { return r.pane == "" }

// own is the digest the pane holds as its own fence, "" when it holds none
// (no value, or one inherited from another scope or another pane).
func (r fenceRead) own() string {
	digest, ok := strings.CutSuffix(r.value, "."+r.pane)
	if !ok {
		return ""
	}
	return digest
}

// found is what a miss on r reports.
func (r fenceRead) found() string {
	switch {
	case r.absent():
		return runtime.FenceFoundAbsent
	case r.dead:
		return runtime.FenceFoundDead
	case r.own() != "":
		return r.own()
	}
	return runtime.FenceFoundUnproven
}

// read reads the agent pane's fence, returning its target.
func (g *paneGuard) read(ctx context.Context, t *Tmux) (string, fenceRead, error) {
	target, err := t.readAgentPaneCtx(ctx, g.name, "#{window_id}.#{pane_id}")
	if err != nil {
		return "", fenceRead{}, g.gone(err)
	}
	if !strings.HasPrefix(target, "@") || !strings.Contains(target, ".%") {
		return "", fenceRead{}, fmt.Errorf("no panes found in session %s (got %q)", g.name, target)
	}
	target = paneTarget(g.name) + strings.TrimSpace(target)
	out, err := t.runCtxRaw(ctx, "display-message", "-p", "-t", target, fenceReadFormat)
	if err != nil {
		return "", fenceRead{}, g.gone(err)
	}
	r, ok := parseFenceRead(out)
	if !ok {
		return "", fenceRead{}, fmt.Errorf("reading input fence of %q: unexpected tmux output %q", g.name, out)
	}
	if r.absent() || r.pane != guardPaneID(target) {
		// No session, or tmux answered for the window's active pane: the
		// agent pane is gone.
		return "", fenceRead{}, g.gone(errAgentPaneGone)
	}
	return target, r, nil
}

// verify is the operation's precheck, before any input or wait: the agent
// pane must be live and hold the guard's digest as its own fence. A pane
// with no own fence is adopted once from the session environment (the
// identity its process inherited), then compared.
//
// Residual (legacy sessions only): verify proves the pane readAgentPane
// picks. In an unmarked (pre-marker) multi-pane session the nudge paths'
// agentInputTarget may pick another pane by process detection; every
// guarded command to that unadopted pane then misses as unproven. That
// fails closed, and a marked session resolves both the same way.
func (g *paneGuard) verify(ctx context.Context, t *Tmux) error {
	target, r, err := g.read(ctx, t)
	if err != nil {
		return err
	}
	if r.dead {
		return g.recordMiss(runtime.FenceFoundDead)
	}
	own := r.own()
	if own == "" {
		if own, err = g.adopt(ctx, t, target); err != nil {
			return err
		}
	}
	if own != g.digest {
		return g.recordMiss(own)
	}
	g.mu.Lock()
	g.pane = r.pane
	g.mu.Unlock()
	return nil
}

// adopt sets the pane's fence from the session environment unless the pane
// gained an own value meanwhile, and returns the pane's own digest. An
// environment that names no incarnation leaves the pane unproven.
func (g *paneGuard) adopt(ctx context.Context, t *Tmux, target string) (string, error) {
	env, err := t.getAllEnvironmentCtx(ctx, g.name)
	if err != nil {
		return "", g.gone(err)
	}
	f, ok := runtime.InputFenceFromEnv(env)
	if !ok {
		return "", g.recordMiss(runtime.FenceFoundUnproven)
	}
	envDigest := f.Digest()
	set, err := tmuxCommandString("set-option", "-p", "-F", "-t", target, inputFenceOption, envDigest+".#{pane_id}")
	if err != nil {
		return "", err
	}
	show, err := tmuxCommandString("display-message", "-p", "-t", target, fenceOwnPrefix+"#{"+inputFenceOption+"}|#{pane_id}|#{pane_dead}")
	if err != nil {
		return "", err
	}
	out, err := t.runCtxRaw(ctx, "if-shell", "-F", "-t", target, fenceNotOwnCondition, set, show)
	if err != nil {
		return "", g.gone(err)
	}
	switch out = strings.TrimSpace(out); {
	case out == "":
		return envDigest, nil
	case strings.HasPrefix(out, fenceOwnPrefix):
		r, ok := parseFenceRead(strings.TrimPrefix(out, fenceOwnPrefix))
		if ok && (r.absent() || r.pane != guardPaneID(target)) {
			// No session, or tmux evaluated the window's active pane: the
			// agent pane is gone.
			return "", g.gone(errAgentPaneGone)
		}
		if ok && r.own() != "" {
			return r.own(), nil
		}
	}
	return "", fmt.Errorf("adopting input fence of %q: unexpected tmux output %q", g.name, out)
}

// checkLive is the read-then-act check for input that bypasses tmux
// commands (the hidden attach client's PTY writes). tmux delivers a client's
// input to the session's active pane, so that pane must be the agent pane
// verify proved, live, and holding the guard's digest as its own fence. It
// never adopts. Residual: an incarnation that takes the pane between this
// read and the write gets the write (the path is checked, not atomic).
func (g *paneGuard) checkLive(ctx context.Context, t *Tmux) error {
	if miss := g.missed(); miss != nil {
		return miss
	}
	out, err := t.runCtxRaw(ctx, "display-message", "-p", "-t", paneTarget(g.name), fenceReadFormat)
	if err != nil {
		return g.gone(err)
	}
	r, ok := parseFenceRead(out)
	switch {
	case !ok:
		return fmt.Errorf("reading input fence of %q: unexpected tmux output %q", g.name, out)
	case r.absent():
		return g.gone(ErrSessionNotFound)
	case r.pane != g.verifiedPane():
		// Another pane has the client's input: the target is not the agent.
		return g.recordMiss(runtime.FenceFoundUnproven)
	case r.dead || r.own() != g.digest:
		return g.recordMiss(r.found())
	}
	return nil
}

// isPaneInputCommand reports whether args is a tmux command that types into
// a pane: the commands the guard wraps.
func isPaneInputCommand(args []string) bool {
	return len(args) > 0 && (args[0] == "send-keys" || args[0] == "paste-buffer")
}

// guardKeyRe is a key name a guarded send-keys may carry: tmux key names
// (Enter, C-c, Up, a digit), never text.
var guardKeyRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9+_-]{0,31}$`)

// pasteInput is a guarded input command broken into its parts.
type pasteInput struct {
	target  string
	literal bool   // send-keys -l: keys[0] is text
	copy    bool   // send-keys -X: keys are a copy-mode command
	buffer  string // paste-buffer -b
	flags   []string
	keys    []string
}

// parseInputCommand parses the send-keys and paste-buffer forms the tmux
// provider sends. Any other flag is refused.
func parseInputCommand(args []string) (pasteInput, error) {
	var in pasteInput
	cmd := args[0]
	for i := 1; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-t" || (cmd == "paste-buffer" && a == "-b"):
			if i+1 >= len(args) {
				return in, fmt.Errorf("fenced %s: %s without a value", cmd, a)
			}
			if a == "-t" {
				in.target = args[i+1]
			} else {
				in.buffer = args[i+1]
			}
			i++
		case cmd == "send-keys" && a == "-l":
			// Everything after -l is text, even an argument that looks
			// like a flag, read as tmux reads it (runtime.TmuxLiteralText).
			text, err := runtime.TmuxLiteralText(args[i+1:])
			if err != nil {
				return in, fmt.Errorf("fenced %w", err)
			}
			in.literal = true
			in.keys = []string{text}
			i = len(args)
		case cmd == "send-keys" && a == "-X":
			in.copy = true
		case cmd == "paste-buffer" && (a == "-p" || a == "-d" || a == "-r"):
			in.flags = append(in.flags, a)
		case strings.HasPrefix(a, "-") && len(a) > 1:
			return in, fmt.Errorf("fenced %s: flag %s is not supported", cmd, a)
		default:
			in.keys = append(in.keys, args[i:]...)
			i = len(args)
		}
	}
	switch {
	case in.target == "":
		return in, fmt.Errorf("fenced %s: no target", cmd)
	case cmd == "paste-buffer" && (in.buffer == "" || len(in.keys) > 0):
		return in, fmt.Errorf("fenced paste-buffer: needs exactly a buffer")
	case in.literal && in.copy:
		return in, fmt.Errorf("fenced send-keys: -l with -X is not supported")
	}
	return in, nil
}

// guardedPaneTarget reports whether target is an agent-pane target for
// session ("=session:@W.%P", see fencedPaneSession) with numeric ids, the
// only form a guarded command may address.
func guardedPaneTarget(target, session string) bool {
	if s, ok := fencedPaneSession(target); !ok || s != session {
		return false
	}
	_, wp, _ := strings.Cut(target, ":@")
	w, p, ok := strings.Cut(wp, ".%")
	return ok && isDigits(w) && isDigits(p)
}

// guardPaneID is the pane id ("%P") of an agent-pane target.
func guardPaneID(target string) string {
	if i := strings.LastIndex(target, ".%"); i >= 0 {
		return target[i+1:]
	}
	return ""
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// run runs one input command of the fenced operation, conditional on the
// target pane being live and holding the guard's digest as its own fence.
// Text never enters a command string or argv: send-keys -l TEXT is staged
// into a private file, loaded into a named buffer, and pasted under the
// guard. After the operation's first miss nothing more is sent.
func (g *paneGuard) run(ctx context.Context, t *Tmux, args []string) (string, error) {
	if miss := g.missed(); miss != nil {
		return "", miss
	}
	in, err := parseInputCommand(args)
	if err != nil {
		return "", err
	}
	if !guardedPaneTarget(in.target, g.name) {
		return "", fmt.Errorf("fenced %s: target %q is not %q's agent pane", args[0], in.target, g.name)
	}
	var inner []string
	buffer := in.buffer
	switch {
	case in.literal:
		if in.keys[0] == "" {
			return "", nil // no text: nothing to type
		}
		buffer, err = t.loadPasteBuffer(ctx, in.keys[0])
		if err != nil {
			return "", err
		}
		inner = []string{"paste-buffer", "-d", "-r", "-b", buffer, "-t", in.target}
	case args[0] == "paste-buffer":
		inner = append(append([]string{"paste-buffer"}, in.flags...), "-b", buffer, "-t", in.target)
	default:
		if len(in.keys) == 0 {
			return "", fmt.Errorf("fenced send-keys: no keys")
		}
		for _, k := range in.keys {
			if !guardKeyRe.MatchString(k) {
				return "", fmt.Errorf("fenced send-keys: %q is not a key name", k)
			}
		}
		inner = []string{"send-keys", "-t", in.target}
		if in.copy {
			inner = append(inner, "-X")
		}
		inner = append(inner, in.keys...)
	}
	guarded, err := guardedInputArgs(in.target, g.digest, inner, buffer)
	if err != nil {
		t.deleteBufferBestEffort(buffer)
		return "", err
	}
	out, err := t.runCtxRaw(ctx, guarded...)
	if err != nil {
		t.deleteBufferBestEffort(buffer)
		if errors.Is(err, ErrSessionNotFound) || errors.Is(err, ErrNoCurrentTarget) {
			return "", g.recordMiss(runtime.FenceFoundAbsent)
		}
		if errors.Is(err, ErrNoServer) {
			err = g.gone(err)
			g.mu.Lock()
			if g.unavailable == nil {
				g.unavailable = err
			}
			g.mu.Unlock()
		}
		return "", err
	}
	switch out = strings.TrimSpace(out); {
	case out == "":
		if !in.copy { // a copy-mode command never reaches the program
			g.markApplied(in.literal || args[0] == "paste-buffer")
		}
		return "", nil
	case strings.HasPrefix(out, fenceMissPrefix):
		if r, ok := parseFenceRead(strings.TrimPrefix(out, fenceMissPrefix)); ok {
			if r.absent() || r.pane != guardPaneID(in.target) {
				// No session, or tmux evaluated the window's active pane: the
				// agent pane is gone.
				return "", g.recordMiss(runtime.FenceFoundAbsent)
			}
			return "", g.recordMiss(r.found())
		}
		return "", g.recordMiss(runtime.FenceFoundUnproven)
	}
	return "", fmt.Errorf("fenced %s on %q: unexpected tmux output %q", args[0], g.name, out)
}

// loadPasteBuffer stages text in a private file and loads it into a new
// named buffer, returning the buffer's name.
func (t *Tmux) loadPasteBuffer(ctx context.Context, text string) (string, error) {
	tmp, err := os.CreateTemp("", "gc-tmux-fenced-*")
	if err != nil {
		return "", fmt.Errorf("creating tmux paste buffer file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := io.WriteString(tmp, text); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("writing tmux paste buffer file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("closing tmux paste buffer file: %w", err)
	}
	name := nextPasteBufferName()
	if _, err := t.runCtxRaw(ctx, "load-buffer", "-b", name, tmpName); err != nil {
		return "", fmt.Errorf("loading tmux paste buffer: %w", err)
	}
	return name, nil
}

func (t *Tmux) deleteBufferBestEffort(name string) {
	if name != "" {
		_, _ = t.runCtxRaw(context.Background(), "delete-buffer", "-b", name)
	}
}

// guardedInputArgs is the if-shell that runs inner only while target is live
// and holds digest as its own fence; otherwise it deletes buffer (when set)
// and prints what it found after fenceMissPrefix. The condition names
// target's pane id too: tmux evaluates it in the window's active pane when
// target's pane is gone, and that context must miss.
func guardedInputArgs(target, digest string, inner []string, buffer string) ([]string, error) {
	if !guardKeyRe.MatchString(digest) {
		return nil, fmt.Errorf("fenced input: invalid digest %q", digest)
	}
	then, err := tmuxCommandString(inner...)
	if err != nil {
		return nil, err
	}
	show, err := tmuxCommandString("display-message", "-p", "-t", target, fenceMissPrefix+fenceReadFormat)
	if err != nil {
		return nil, err
	}
	elseCmd := show
	if buffer != "" {
		del, err := tmuxCommandString("delete-buffer", "-b", buffer)
		if err != nil {
			return nil, err
		}
		elseCmd = del + " ; " + show
	}
	pane := guardPaneID(target)
	if !wellFormedTmuxID(pane, '%') {
		return nil, fmt.Errorf("fenced input: target %q names no pane", target)
	}
	cond := "#{&&:#{==:#{pane_id}," + pane + "},#{&&:#{==:#{" + inputFenceOption + "}," + digest + ".#{pane_id}},#{!=:#{pane_dead},1}}}"
	return []string{"if-shell", "-F", "-t", target, cond, then, elseCmd}, nil
}

// tmuxCommandString renders args as one tmux command for if-shell, each
// argument single-quoted. Arguments tmux would parse or expand (a quote, a
// control byte, or text that is not ours) are refused: only the provider's
// own targets, key names, buffer names and the guard's own formats reach it.
func tmuxCommandString(args ...string) (string, error) {
	quoted := make([]string, len(args))
	for i, a := range args {
		if a == "" {
			return "", fmt.Errorf("fenced tmux command: empty argument")
		}
		for _, r := range a {
			if r == '\'' || r < 0x20 || r == 0x7f {
				return "", fmt.Errorf("fenced tmux command: argument %q is unsafe", a)
			}
		}
		if strings.Contains(a, "#{") && !ownFormatArg(a) {
			return "", fmt.Errorf("fenced tmux command: argument %q holds a format", a)
		}
		quoted[i] = "'" + a + "'"
	}
	return strings.Join(quoted, " "), nil
}

// ownFormatArg reports whether a is one of the formats the guard writes
// itself: a fence value or one of the miss reports.
func ownFormatArg(a string) bool {
	switch {
	case strings.HasSuffix(a, ".#{pane_id}") && guardKeyRe.MatchString(strings.TrimSuffix(a, ".#{pane_id}")):
		return true
	case a == fenceMissPrefix+fenceReadFormat, a == fenceOwnPrefix+fenceReadFormat, a == fenceAdoptMissPrefix+"#{pane_pid}":
		return true
	}
	return false
}
