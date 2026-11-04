package tmux

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

const (
	bootstrapInputOption  = "@gc_bootstrap_input"
	bootstrapInputMiss    = "gc-bootstrap-input-miss"
	bootstrapInputVersion = "v1"
)

// The pane owns a versioned, issued first-input credit. Controller memory caches
// it but never recreates it: recovery can only recover the exact unspent READY
// marker after native fence/process and pristine-screen bookends.
type bootstrapInputCredit struct {
	fence  runtime.InputFence
	token  string
	family string
	ready  bool
}

func bootstrapInputEligible(cfg runtime.Config) bool {
	return cfg.BootstrapInputOrigin == runtime.BootstrapInputFreshBuiltin &&
		(cfg.ProviderName == "claude" || cfg.ProviderName == "codex") &&
		cfg.Command != "" && cfg.Lifecycle != runtime.LifecycleOneShot &&
		cfg.PromptSuffix == "" && cfg.Nudge == "" && len(cfg.SessionSetup) == 0 &&
		cfg.SessionSetupScript == "" && len(cfg.SessionLive) == 0
}

func (t *Tmux) prepareBootstrapInput(name string, cfg runtime.Config) error {
	if !bootstrapInputEligible(cfg) {
		t.forgetBootstrapInput(name)
		return nil
	}
	fence, ok := runtime.InputFenceFromEnv(cfg.Env)
	if !ok || !bootstrapFreshFence(fence) {
		t.forgetBootstrapInput(name)
		return nil
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("seeding first-input credit: %w", err)
	}
	t.bootstrapMu.Lock()
	defer t.bootstrapMu.Unlock()
	if t.bootstrapInputs == nil {
		t.bootstrapInputs = make(map[string]bootstrapInputCredit)
	}
	t.bootstrapInputs[name] = bootstrapInputCredit{fence: fence, token: hex.EncodeToString(nonce[:]), family: cfg.ProviderName}
	return nil
}

func (t *Tmux) bootstrapCredit(name string) (bootstrapInputCredit, bool) {
	t.bootstrapMu.Lock()
	defer t.bootstrapMu.Unlock()
	credit, ok := t.bootstrapInputs[name]
	return credit, ok
}

func (t *Tmux) forgetBootstrapInput(name string) {
	t.bootstrapMu.Lock()
	defer t.bootstrapMu.Unlock()
	delete(t.bootstrapInputs, name)
}

func bootstrapInputValue(state string, credit bootstrapInputCredit) string {
	return bootstrapInputVersion + "." + string(runtime.BootstrapInputFreshBuiltin) + "." + credit.family + "." + state + "." + credit.token + "." + credit.fence.Digest() + ".#{pane_id}.#{pane_pid}"
}

// seedBootstrapInputCommands runs in the same native create command list as
// new-session, before startup code can type into the pane. Promotion is separate
// and conditional: ordinary input burns this candidate, never reseeds it.
// Only raw Start can preserve it through parser-confirmed startup controls.
func (t *Tmux) seedBootstrapInputCommands(name string) [][]string {
	credit, ok := t.bootstrapCredit(name)
	if !ok {
		return nil
	}
	return [][]string{{"set-option", "-p", "-F", "-t", paneTarget(name), bootstrapInputOption, bootstrapInputValue("candidate", credit)}}
}

func (t *Tmux) promoteBootstrapInput(ctx context.Context, name string) error {
	credit, ok := t.bootstrapCredit(name)
	if !ok {
		return nil
	}
	target, err := t.agentPaneTarget(name)
	if err != nil {
		return err
	}
	candidate := bootstrapInputValue("candidate", credit)
	ready := bootstrapInputValue("ready", credit)
	condition := "#{&&:#{==:#{" + bootstrapInputOption + "}," + candidate + "},#{==:#{" + inputFenceOption + "}," + credit.fence.Digest() + ".#{pane_id}}}"
	set, err := runtime.TmuxCommandString(func(s string) bool { return s == ready }, "set-option", "-p", "-F", "-t", target, bootstrapInputOption, ready)
	if err != nil {
		return err
	}
	// A spent candidate is a normal no-credit start, not a failed session.
	miss, err := tmuxCommandString("display-message", "-p", bootstrapInputMiss)
	if err != nil {
		return err
	}
	out, err := t.runCtxRaw(ctx, "if-shell", "-F", "-t", target, condition, set, miss)
	if err != nil {
		return err
	}
	if out = strings.TrimSpace(out); out == bootstrapInputMiss {
		t.forgetBootstrapInput(name)
		return nil
	} else if out != "" {
		return fmt.Errorf("promoting first-input credit: unexpected tmux result %q", out)
	}
	t.bootstrapMu.Lock()
	credit.ready = true
	t.bootstrapInputs[name] = credit
	t.bootstrapMu.Unlock()
	return nil
}

// bootstrapStartupInput exists only on the raw Start stack, before promotion.
// It can preserve the issued candidate; it cannot recreate or recover credit.
// The public dialog path and every ordinary input path continue to consume it.
type bootstrapStartupInput struct {
	name, target, pid string
	credit            bootstrapInputCredit
	lastScreen        string
	lastLines         int
	observed          bool
}

func (t *Tmux) acceptBootstrapStartupDialogs(ctx context.Context, name string) error {
	credit, known := t.bootstrapCredit(name)
	if !known || credit.ready {
		return t.AcceptStartupDialogs(ctx, name)
	}
	startup := &bootstrapStartupInput{name: name, credit: credit}
	if err := startup.verify(ctx, t, ""); err != nil {
		return err
	}
	return t.dismissKnownDialogs(ctx, name, 8*time.Second, startup)
}

// verify does not adopt a missing fence or write a marker. The first check pins
// the existing agent pane and native PID; every later check requires both.
func (s *bootstrapStartupInput) verify(ctx context.Context, t *Tmux, target string) error {
	cached, ok := t.bootstrapCredit(s.name)
	if !ok || cached != s.credit || cached.ready || !bootstrapFreshFence(cached.fence) {
		return fmt.Errorf("startup input: %w: candidate unavailable", runtime.ErrInputNotSent)
	}
	g := newPaneGuard(s.name, s.credit.fence.Digest(), false)
	actual, fence, err := g.read(ctx, t)
	if err != nil {
		return err
	}
	if fence.Dead || fence.Own() != s.credit.fence.Digest() || (target != "" && actual != target) || (s.target != "" && actual != s.target) {
		return fmt.Errorf("startup input: %w: agent incarnation changed", runtime.ErrInputNotSent)
	}
	out, err := t.runCtxRaw(ctx, "display-message", "-p", "-t", actual, "#{"+bootstrapInputOption+"}|#{pane_id}|#{pane_pid}|#{pane_dead}")
	if err != nil {
		return err
	}
	parts := strings.Split(strings.TrimSpace(out), "|")
	if len(parts) != 4 || parts[1] != fence.Pane || parts[3] != "0" {
		return fmt.Errorf("startup input: %w: native candidate unavailable", runtime.ErrInputNotSent)
	}
	pid, err := strconv.ParseUint(parts[2], 10, 64)
	if err != nil || pid == 0 || strconv.FormatUint(pid, 10) != parts[2] || (s.pid != "" && s.pid != parts[2]) {
		return fmt.Errorf("startup input: %w: native process changed", runtime.ErrInputNotSent)
	}
	want := strings.NewReplacer("#{pane_id}", fence.Pane, "#{pane_pid}", parts[2]).Replace(bootstrapInputValue("candidate", s.credit))
	if parts[0] != want {
		return fmt.Errorf("startup input: %w: candidate already consumed", runtime.ErrInputNotSent)
	}
	s.target, s.pid = actual, parts[2]
	return nil
}

func (s *bootstrapStartupInput) peek(ctx context.Context, t *Tmux, target string, lines int) (string, error) {
	if err := s.verify(ctx, t, target); err != nil {
		return "", err
	}
	out, err := t.CapturePane(target, lines)
	if err != nil {
		return "", err
	}
	if err := s.verify(ctx, t, target); err != nil {
		return "", err
	}
	s.lastScreen, s.lastLines, s.observed = out, lines, true
	return out, nil
}

// send is reachable only from the shared startup parser's private callback.
// Require its exact last screen again, then bind the whole authorized key batch
// to the same native candidate, pane, PID and fence in one tmux command. No
// marker is rewritten, so intervening ordinary input permanently wins the race.
func (s *bootstrapStartupInput) send(ctx context.Context, t *Tmux, target string, keys []string) error {
	if !s.observed || len(keys) == 0 {
		return fmt.Errorf("startup input: %w: no parser observation", runtime.ErrInputNotSent)
	}
	if err := s.verify(ctx, t, target); err != nil {
		return err
	}
	current, err := t.CapturePane(target, s.lastLines)
	if err != nil {
		return err
	}
	if current != s.lastScreen {
		return fmt.Errorf("startup input: %w: dialog changed", runtime.ErrInputNotSent)
	}
	if err := s.verify(ctx, t, target); err != nil {
		return err
	}
	for _, key := range keys {
		if key != "Enter" && key != "Down" && key != "Up" && key != "Escape" && key != "Tab" {
			return fmt.Errorf("startup input: %w: unsupported dialog key", runtime.ErrInputNotSent)
		}
	}
	args := append([]string{"send-keys", "-t", target}, keys...)
	command, err := tmuxCommandString(args...)
	if err != nil {
		return err
	}
	miss, err := tmuxCommandString("display-message", "-p", bootstrapInputMiss)
	if err != nil {
		return err
	}
	candidate := bootstrapInputValue("candidate", s.credit)
	condition := "#{&&:#{==:#{" + bootstrapInputOption + "}," + candidate + "},#{&&:#{==:#{" + inputFenceOption + "}," + s.credit.fence.Digest() + ".#{pane_id}},#{&&:#{==:#{pane_id}," + guardPaneID(target) + "},#{&&:#{==:#{pane_pid}," + s.pid + "},#{==:#{pane_dead},0}}}}}"
	s.observed = false // Another batch needs a new shared-parser observation.
	out, err := t.runCtxRaw(ctx, "if-shell", "-F", "-t", target, condition, command, miss)
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) != "" {
		return fmt.Errorf("startup input: %w: native candidate changed", runtime.ErrInputNotSent)
	}
	return s.verify(ctx, t, target)
}

var _ runtime.BootstrapInputObserver = (*Provider)(nil)

// ObserveBootstrapInput proves a narrow first native input opportunity. A
// missing transcript key, unknown turn or generic idle composer proves nothing.
func (p *Provider) ObserveBootstrapInput(ctx context.Context, name string, req runtime.BootstrapInputRequest) (*runtime.BootstrapInputProof, error) {
	if (req.Family != "claude" && req.Family != "codex") || !bootstrapFreshFence(req.Fence) {
		return nil, nil
	}
	cached, known := p.tm.bootstrapCredit(name)
	if known && (!cached.ready || cached.family != req.Family || cached.fence.Digest() != req.Fence.Digest()) {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	guard := newPaneGuard(name, req.Fence.Digest(), false)
	target, before, err := guard.read(ctx, p.tm)
	if err != nil || before.Dead || before.Own() != req.Fence.Digest() {
		return nil, err
	}
	format := "#{" + bootstrapInputOption + "}|#{pane_id}|#{pane_pid}|#{pane_dead}"
	first, err := p.tm.runCtxRaw(ctx, "display-message", "-p", "-t", target, format)
	if err != nil {
		return nil, err
	}
	credit, ok := bootstrapInputReadCredit(first, req, before.Pane)
	if !ok || (known && cached != credit) {
		return nil, nil
	}
	// -e preserves dim placeholders so they cannot be mistaken for a human
	// draft; no -S means only the current native screen is examined.
	screen, err := p.tm.runCtxRaw(ctx, "capture-pane", "-p", "-e", "-J", "-t", target)
	if err != nil || len(screen) > 64<<10 || !pristineBootstrapScreen(screen, req.Family) {
		return nil, err
	}
	afterTarget, after, err := guard.read(ctx, p.tm)
	if err != nil || target != afterTarget || before.Pane != after.Pane || after.Dead || after.Own() != req.Fence.Digest() {
		return nil, err
	}
	last, err := p.tm.runCtxRaw(ctx, "display-message", "-p", "-t", target, format)
	if err != nil || first != last {
		return nil, err
	}
	if !p.tm.rememberBootstrapInput(name, credit) {
		return nil, nil
	}
	return &runtime.BootstrapInputProof{Fence: req.Fence, Token: credit.token}, nil
}

func bootstrapFreshFence(fence runtime.InputFence) bool {
	return fence.Validate() == nil && fence.Generation == 1 && fence.Epoch == 1
}

// bootstrapInputReadCredit recovers authority only from the exact versioned
// native READY marker issued by a successful fresh builtin Start. It never
// mints a nonce or promotes candidate/claimed/spent/legacy state.
func bootstrapInputReadCredit(out string, req runtime.BootstrapInputRequest, pane string) (bootstrapInputCredit, bool) {
	parts := strings.Split(strings.TrimSpace(out), "|")
	if len(parts) != 4 || parts[1] != pane || parts[3] != "0" || !bootstrapFreshFence(req.Fence) {
		return bootstrapInputCredit{}, false
	}
	fields := strings.Split(parts[0], ".")
	if (req.Family != "claude" && req.Family != "codex") || len(fields) != 8 || fields[0] != bootstrapInputVersion || fields[1] != string(runtime.BootstrapInputFreshBuiltin) || fields[2] != req.Family || fields[3] != "ready" || fields[5] != req.Fence.Digest() || fields[6] != pane || fields[7] != parts[2] {
		return bootstrapInputCredit{}, false
	}
	nonce, err := hex.DecodeString(fields[4])
	if err != nil || len(nonce) != 16 || hex.EncodeToString(nonce) != fields[4] {
		return bootstrapInputCredit{}, false
	}
	if !strings.HasPrefix(pane, "%") {
		return bootstrapInputCredit{}, false
	}
	paneNumber, err := strconv.ParseUint(strings.TrimPrefix(pane, "%"), 10, 64)
	if err != nil || "%"+strconv.FormatUint(paneNumber, 10) != pane {
		return bootstrapInputCredit{}, false
	}
	pid, err := strconv.ParseUint(parts[2], 10, 64)
	if err != nil || pid == 0 || strconv.FormatUint(pid, 10) != parts[2] {
		return bootstrapInputCredit{}, false
	}
	return bootstrapInputCredit{fence: req.Fence, token: fields[4], family: req.Family, ready: true}, true
}

func (t *Tmux) rememberBootstrapInput(name string, credit bootstrapInputCredit) bool {
	t.bootstrapMu.Lock()
	defer t.bootstrapMu.Unlock()
	if current, ok := t.bootstrapInputs[name]; ok && current != credit {
		return false
	}
	if t.bootstrapInputs == nil {
		t.bootstrapInputs = make(map[string]bootstrapInputCredit)
	}
	t.bootstrapInputs[name] = credit
	return true
}

func pristineBootstrapScreen(screen, family string) bool {
	lines := strings.Split(strings.ReplaceAll(screen, "\r", ""), "\n")
	plain := make([]string, len(lines))
	for i, line := range lines {
		plain[i] = stripDimText(line)
	}
	if paneShowsRunningTurn(plain, family) || runtime.CodexStartupPending(strings.Join(plain, "\n")) {
		return false
	}
	text := strings.Join(plain, "\n")
	switch family {
	case "codex":
		// Brand/version and the complete compact layout are presentation,
		// not draft content: native Codex draws its version faint and its
		// update link as OSC8. Keep the dim-aware veto/drain checks separate.
		structure, known := codexBootstrapStructure(screen)
		if !known || !strings.Contains(structure, ">_ OpenAI Codex (v") || (!strings.Contains(structure, "To get started, describe a task or try one of these commands:") && !compactCodexBootstrapScreen(screen)) {
			return false
		}
		// The initial native placeholder is an exact allowed empty composer.
		// Current non-dim captures can contain its words too.
		for i, line := range plain {
			if strings.TrimSpace(line) == "› Ask Codex to do anything" {
				plain[i] = "› "
			}
		}
	case "claude":
		if (!strings.Contains(text, "Claude Code v") || !strings.Contains(text, "Welcome")) && !compactClaudeBootstrapScreen(screen) {
			return false
		}
	default:
		return false
	}
	prompts := 0
	marker, ok := stagedDraftMarkerForFamily(family)
	if !ok {
		return false
	}
	for _, line := range plain {
		if matchesPromptPrefix(line, marker.promptPrefix) {
			prompts++
		}
	}
	return prompts == 1 && paneShowsDrainedComposer(plain)
}

// Compact Claude 2.1.308 omits Welcome. Match its complete branded frame,
// leaving composer text intact: the final dim-aware drain must still prove
// emptiness. A literal Try suggestion typed as a draft never becomes empty.
var compactClaudeBootstrapFrame = regexp.MustCompile(
	`^▐▛███▛█ Claude Code v2\.1\.308\n` +
		`▝▜██████▀ (?:Opus|Sonnet|Haiku) [0-9]+(?:\.[0-9]+)*(?: with (?:low|medium|high|max) effort)? · Claude Ultra\n` +
		`▝▝ ▝▝ (?:~/|/)[^\n]+\n` +
		`(?:✔ Update installed · Restart to update\n)?` +
		`─+\n❯(?: [^\n]{0,512})?\n─+\n` +
		`⏵⏵ (?:bypass permissions|don't ask|auto mode|accept edits) on \(shift\+tab to cycle\)(?: · ← for agents)?$`)

func compactClaudeBootstrapScreen(screen string) bool {
	// Unknown controls remain unknown. Never erase arbitrary OSC/CSI content.
	structure := compactCodexBootstrapSGR.ReplaceAllString(screen, "")
	if strings.ContainsRune(structure, '\x1b') {
		return false
	}
	var rows []string
	for _, row := range strings.Split(strings.ReplaceAll(structure, "\r", ""), "\n") {
		if row = strings.Join(strings.Fields(row), " "); row != "" {
			rows = append(rows, row)
		}
	}
	return compactClaudeBootstrapFrame.MatchString(strings.Join(rows, "\n"))
}

// Compact 0.162.1 renders no command menu. Recognize its complete initial UI,
// including only the known optional update notice, never arbitrary transcript
// rows surrounding an empty composer. Known SGR/OSC8 presentation is removed
// only structurally; draft draining and native-turn vetoes stay separate.
var (
	compactCodexBootstrapSGR   = regexp.MustCompile(`\x1b\[[0-9;:]*m`)
	compactCodexBootstrapFrame = regexp.MustCompile(
		`^(?:╭─+╮\n│ ✨ Update available! [0-9]+(?:\.[0-9]+){2} -> [0-9]+(?:\.[0-9]+){2} │\n` +
			`│ Run npm install -g @openai/codex to update\. │\n│ │\n│ See full release notes: │\n` +
			`│ https://github\.com/openai/codex/releases/latest │\n╰─+╯\n)?` +
			`>_ OpenAI Codex \(v0\.162\.1\)\n(?:~/|/)[^\n]+\n(?:permissions: YOLO mode\n)?` +
			`› Ask Codex to do anything\nGPT-[A-Za-z0-9_.-]+ (?:minimal|low|medium|high|xhigh|max) · (?:~/|/)[^\n]+\n` +
			`\? for shortcuts(?: ⚠ [1-9][0-9]* warnings? · f2 to view)?$`)
)

// codexBootstrapStructure removes only complete SGR styling and the exact
// stock0.162.1 OSC8 release-link wrapper, preserving all displayed text. Unknown
// or incomplete escapes remain unknown; do not hide them inside a cwd wildcard.
func codexBootstrapStructure(screen string) (string, bool) {
	const link = "https://github.com/openai/codex/releases/latest"
	// URI bytes are not presentation. Validate raw OSC identities before
	// styling removal, which must not repair an altered hidden target.
	withoutOSC := strings.ReplaceAll(screen, "\x1b]8;;"+link+"\x1b\\", "")
	withoutOSC = strings.ReplaceAll(withoutOSC, "\x1b]8;;\x1b\\", "")
	if strings.Contains(withoutOSC, "\x1b]") {
		return "", false
	}
	screen = compactCodexBootstrapSGR.ReplaceAllString(screen, "")
	screen = strings.ReplaceAll(screen, "\x1b]8;;"+link+"\x1b\\"+link+"\x1b]8;;\x1b\\", link)
	return screen, !strings.ContainsRune(screen, '\x1b')
}

func compactCodexBootstrapScreen(screen string) bool {
	structure, known := codexBootstrapStructure(screen)
	if !known {
		return false
	}
	var visible []string
	for _, line := range strings.Split(strings.ReplaceAll(structure, "\r", ""), "\n") {
		line = strings.Join(strings.Fields(line), " ")
		if line != "" {
			visible = append(visible, line)
		}
	}
	return compactCodexBootstrapFrame.MatchString(strings.Join(visible, "\n"))
}

// ordinaryPaneInput consumes even a startup/trust/clear/answer key before it
// can reach the application. It is also used by unfenced GC input; adoption
// through any Provider burns the same durable native credit.
func (t *Tmux) ordinaryPaneInput(ctx context.Context, args []string) (string, error) {
	in, err := parseInputCommand(args)
	if err != nil {
		return "", err
	}
	if _, err := t.runCtxRaw(ctx, "set-option", "-p", "-t", in.target, bootstrapInputOption, "spent"); err != nil {
		return "", err
	}
	return t.runCtxRaw(ctx, args...)
}

// bootstrapGuardedInputArgs augments the existing fence's native if-shell.
// The exact ready credit is consumed in that same command before the first
// key/paste. Later commands of this operation require its claimed marker;
// replay and any intervening GC input cannot reuse the original ready credit.
func (g *paneGuard) bootstrapGuardedInputArgs(t *Tmux, target string, guarded []string) ([]string, error) {
	if len(guarded) != 7 {
		return nil, fmt.Errorf("invalid guarded input command")
	}
	setValue := "spent"
	if g.bootstrap != nil {
		credit, ok := t.bootstrapCredit(g.name)
		if !ok || !credit.ready || credit.token != g.bootstrap.Token || credit.fence.Digest() != g.digest || g.bootstrap.Fence.Digest() != g.digest {
			return nil, g.bootstrapRefusal()
		}
		state := "ready"
		if g.bootstrapClaimed {
			state = "claimed"
		}
		condition := "#{==:#{" + bootstrapInputOption + "}," + bootstrapInputValue(state, credit) + "}"
		guarded[4] = "#{&&:" + guarded[4] + "," + condition + "}"
		setValue = bootstrapInputValue("claimed", credit)
	}
	set, err := runtime.TmuxCommandString(func(s string) bool { return s == setValue }, "set-option", "-p", "-F", "-t", target, bootstrapInputOption, setValue)
	if err != nil {
		return nil, err
	}
	guarded[5] = set + " ; " + guarded[5]
	return guarded, nil
}

func (g *paneGuard) bootstrapRefusal() error {
	g.mu.Lock()
	applied := g.applied
	g.mu.Unlock()
	if applied {
		return fmt.Errorf("%w: first-input credit changed after partial input", runtime.ErrNudgeDeliveryUnconfirmed)
	}
	return fmt.Errorf("%w: first-input credit is consumed or unavailable", runtime.ErrNudgeNotSent)
}

// taintBootstrapInput refuses opaque live commands unless the native pane
// confirms that no first-input proof can survive them. A no-op RunLive never
// enters this path. Creator memory is dropped even when native state is unknown.
func (t *Tmux) taintBootstrapInput(name string) error {
	t.forgetBootstrapInput(name)
	target, err := t.agentPaneTarget(name)
	if err != nil {
		return err
	}
	ctx := context.Background()
	if _, err := t.runCtxRaw(ctx, "set-option", "-p", "-t", target, bootstrapInputOption, "spent"); err != nil {
		return err
	}
	out, err := t.runCtxRaw(ctx, "display-message", "-p", "-t", target, "#{"+bootstrapInputOption+"}|#{pane_id}|#{pane_pid}|#{pane_dead}")
	if err != nil {
		return err
	}
	parts := strings.Split(strings.TrimSpace(out), "|")
	if len(parts) != 4 || parts[0] != "spent" || parts[1] != guardPaneID(target) || parts[2] == "" {
		return fmt.Errorf("native first-input invalidation could not be confirmed")
	}
	return nil
}
