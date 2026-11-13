package tmux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// fenceModel is a tmux executor that models what the input fence relies on:
// the agent pane's own and inherited @gc_fence values, its id, pid and
// death, the session environment, named buffers, and `if-shell -F` with the
// format operators the guard uses (==, !=, &&, m:, ?), running the chosen
// branch's commands. Input that reaches the pane (keys, literal text, a
// pasted buffer) is logged as events and forwarded to inner, a pane
// simulator such as fakeClaudePane, when one is set; a static pane answers
// captures with screen. Every send-keys or paste-buffer run outside an
// if-shell is logged in bare.
type fenceModel struct {
	mu sync.Mutex

	inner  executor
	screen func(m *fenceModel) string

	paneID, pid    string
	own, inherited string // the pane's own @gc_fence value, a window-scope one
	dead, inMode   bool
	gone, noServer bool
	env            map[string]string

	buffers map[string]string
	bare    [][]string // send-keys/paste-buffer argv run outside if-shell
	events  []string   // input that reached the pane
	calls   [][]string
	guards  int

	// beforeGuard runs before the n-th guard condition (1-based) is
	// evaluated, with m locked: a test respawns or kills the pane there.
	beforeGuard func(m *fenceModel, n int)
	// failGuard makes the n-th guard (1-based) fail with its error, unrun.
	failGuard map[int]error
	// failLoad makes the n-th load-buffer (1-based) fail with its error,
	// unrun.
	failLoad map[int]error
	loads    int
	// beforeRead runs before the n-th read of a pane's fence (1-based;
	// display-message of fenceReadFormat), with m locked.
	beforeRead func(m *fenceModel, n int)
	reads      int
	// beforeAdopt runs before an adoption's if-shell, with m locked.
	beforeAdopt func(m *fenceModel)

	// The window's other pane, a human's split: survivorOwn is its own
	// @gc_fence value. active names the session's active pane ("" is the
	// agent pane). paneGone kills the agent pane while the window lives:
	// lenient commands (display-message, if-shell) then fall back to the
	// survivor, as tmux does, strict ones (send-keys, paste-buffer,
	// set-option) fail, counted in strictGone. orphan lists the session's
	// panes with no agent pane (errAgentPaneGone).
	survivorOwn      string
	active           string
	paneGone, orphan bool
	strictGone       int
	onSurvivor       bool // formats expand for the survivor
	// detached answers the attachment probe: no client attached.
	detached bool
	// overlay is captured instead of the pane until key overlayUntil lands.
	overlay, overlayUntil string
	// onEvent lets a static pane react to input (screen transitions).
	onEvent func(m *fenceModel, ev string)
}

const fenceModelSession = "lab"

// fenceModelSurvivor is the id of the agent pane's window's other pane.
const fenceModelSurvivor = "%0"

func newFenceModel(inner executor, env map[string]string) *fenceModel {
	return &fenceModel{
		inner:   inner,
		paneID:  "%1",
		pid:     "4242",
		env:     env,
		buffers: map[string]string{},
	}
}

// fenceEnv is a session environment naming f.
func fenceEnv(f runtime.InputFence) map[string]string {
	return map[string]string{
		runtime.RuntimeEpochEnv:      fmt.Sprint(f.Generation),
		runtime.ContinuationEpochEnv: fmt.Sprint(f.Epoch),
		runtime.InstanceTokenEnv:     f.Token,
	}
}

func (m *fenceModel) execute(args []string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for len(args) > 0 && (args[0] == "-u" || args[0] == "-N") {
		args = args[1:]
	}
	if len(args) > 1 && args[0] == "-L" {
		args = args[2:]
	}
	m.calls = append(m.calls, slices.Clone(args))
	return m.dispatch(args, false)
}

func (m *fenceModel) executeCtx(ctx context.Context, args []string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return m.execute(args)
}

func (m *fenceModel) dispatch(args []string, nested bool) (string, error) {
	if len(args) == 0 {
		return "", nil
	}
	if m.noServer {
		return "", ErrNoServer
	}
	switch args[0] {
	case "load-buffer":
		m.loads++
		if err := m.failLoad[m.loads]; err != nil {
			return "", err
		}
		data, err := os.ReadFile(args[len(args)-1])
		if err != nil {
			return "", err
		}
		m.buffers[flagValue(args, "-b")] = string(data)
		return "", nil
	case "delete-buffer":
		delete(m.buffers, flagValue(args, "-b"))
		return "", nil
	}
	// tmux 3.4 answers display-message -t and if-shell -F -t for a session
	// that no longer exists (while the server lives) with exit 0 and every
	// pane field empty; the other commands fail to find their target.
	if m.gone && args[0] != "display-message" && args[0] != "if-shell" {
		return "", ErrSessionNotFound
	}
	switch args[0] {
	case "send-keys", "paste-buffer", "set-option":
		if m.paneGone && m.namesAgentPane(flagValue(args, "-t")) {
			m.strictGone++
			return "", ErrSessionNotFound // tmux: can't find pane / no such pane
		}
	}
	switch args[0] {
	case "if-shell":
		return m.ifShell(args)
	case "set-option":
		if slices.Contains(args, inputFenceOption) {
			m.own = m.expand(args[len(args)-1])
			return "", nil
		}
	case "display-message":
		format := args[len(args)-1]
		switch {
		case strings.Contains(format, inputFenceOption), strings.HasPrefix(format, fenceAdoptMissPrefix):
			if format == fenceReadFormat {
				m.reads++
				if m.beforeRead != nil {
					m.beforeRead(m, m.reads)
				}
			}
			return m.expandAt(flagValue(args, "-t"), format), nil
		case m.gone:
			return m.expand(format), nil
		case m.detached && format == "#{session_name}|#{session_attached}":
			return fenceModelSession + "|0", nil
		case format == "#{pane_in_mode}":
			if m.inMode {
				return "1", nil
			}
			return "0", nil
		}
	case "show-environment":
		if len(args) <= 3 {
			var lines []string
			for k, v := range m.env {
				lines = append(lines, k+"="+v)
			}
			slices.Sort(lines)
			return strings.Join(lines, "\n"), nil
		}
		if v, ok := m.env[args[len(args)-1]]; ok {
			return args[len(args)-1] + "=" + v, nil
		}
		if m.inner == nil {
			return "", errors.New("unknown variable")
		}
	case "send-keys", "paste-buffer":
		if !nested {
			m.bare = append(m.bare, slices.Clone(args))
		}
		return m.input(args)
	case "capture-pane":
		if m.overlay != "" {
			return m.overlay, nil
		}
		if m.inner == nil {
			if m.screen == nil {
				return "", nil
			}
			return m.screen(m), nil
		}
	case "list-panes":
		if row, ok := fakeAgentPaneRow(args); ok {
			if m.orphan || m.paneGone {
				return "orphan\t@1.%0", nil
			}
			return row, nil
		}
	}
	if m.inner != nil {
		return m.inner.execute(args)
	}
	return "", nil
}

// input applies a send-keys or paste-buffer to the pane.
func (m *fenceModel) input(args []string) (string, error) {
	target := flagValue(args, "-t")
	var evs []string
	forward := [][]string{}
	if args[0] == "paste-buffer" {
		name := flagValue(args, "-b")
		text, ok := m.buffers[name]
		if !ok {
			return "", fmt.Errorf("no buffer %s", name)
		}
		if slices.Contains(args, "-d") {
			delete(m.buffers, name)
		}
		evs = append(evs, "text:"+text)
		forward = append(forward, append([]string{"send-keys", "-t", target, "-l"}, runtime.TmuxLiteralArgs(text)...))
	} else {
		in, err := parseInputCommand(args)
		if err != nil {
			return "", err
		}
		switch {
		case in.copy:
			m.inMode = false
			evs = append(evs, "copy:"+strings.Join(in.keys, " "))
		case in.literal:
			evs = append(evs, "text:"+in.keys[0])
			forward = append(forward, slices.Clone(args))
		default:
			for _, k := range in.keys {
				evs = append(evs, "key:"+k)
			}
			forward = append(forward, slices.Clone(args))
		}
	}
	for _, ev := range evs {
		m.events = append(m.events, ev)
		if m.overlay != "" && ev == "key:"+m.overlayUntil {
			m.overlay = ""
		}
		if m.onEvent != nil {
			m.onEvent(m, ev)
		}
	}
	if m.inner != nil {
		for _, f := range forward {
			if _, err := m.inner.execute(f); err != nil {
				return "", err
			}
		}
	}
	return "", nil
}

func (m *fenceModel) ifShell(args []string) (string, error) {
	if len(args) != 7 || args[1] != "-F" || args[2] != "-t" {
		return "", fmt.Errorf("model: unsupported if-shell %q", args)
	}
	cond := args[4]
	if strings.HasPrefix(cond, "#{&&:") {
		m.guards++
		if err := m.failGuard[m.guards]; err != nil {
			return "", err
		}
		if m.beforeGuard != nil {
			m.beforeGuard(m, m.guards)
		}
	}
	if cond == fenceNotOwnCondition && m.beforeAdopt != nil {
		m.beforeAdopt(m)
	}
	branch := args[6]
	if v := m.expandAt(args[3], cond); v != "" && v != "0" {
		branch = args[5]
	}
	cmds, err := parseCommandString(branch)
	if err != nil {
		return "", err
	}
	var outs []string
	for _, cmd := range cmds {
		out, err := m.dispatch(cmd, true)
		if err != nil {
			return "", err
		}
		if out != "" {
			outs = append(outs, out)
		}
	}
	return strings.Join(outs, "\n"), nil
}

// namesAgentPane reports whether target addresses the agent pane by id.
func (m *fenceModel) namesAgentPane(target string) bool {
	return strings.HasSuffix(target, "."+m.paneID)
}

// expandAt expands s in target's context: the agent pane, or the survivor
// when target is the session's active pane and that is the survivor, or
// when it names the agent pane and that is gone (tmux's fallback to the
// window's active pane).
func (m *fenceModel) expandAt(target, s string) string {
	survivor := m.paneGone || (!m.namesAgentPane(target) && m.active != "" && m.active != m.paneID)
	m.onSurvivor = survivor
	defer func() { m.onSurvivor = false }()
	return m.expand(s)
}

// expand expands the tmux formats the fence uses.
func (m *fenceModel) expand(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if strings.HasPrefix(s[i:], "#{") {
			j := matchingBrace(s, i+1)
			b.WriteString(m.evalFormat(s[i+2 : j]))
			i = j + 1
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func (m *fenceModel) evalFormat(f string) string {
	truthy := func(v string) bool { return v != "" && v != "0" }
	bool01 := func(b bool) string {
		if b {
			return "1"
		}
		return "0"
	}
	if rest, ok := strings.CutPrefix(f, "?"); ok {
		parts := splitFormatArgs(rest)
		if truthy(m.expand(parts[0])) {
			return m.expand(parts[1])
		}
		return m.expand(parts[2])
	}
	for _, op := range []string{"==:", "!=:", "&&:", "m:"} {
		rest, ok := strings.CutPrefix(f, op)
		if !ok {
			continue
		}
		parts := splitFormatArgs(rest)
		a, b := m.expand(parts[0]), m.expand(parts[1])
		switch op {
		case "==:":
			return bool01(a == b)
		case "!=:":
			return bool01(a != b)
		case "&&:":
			return bool01(truthy(a) && truthy(b))
		default:
			matched, _ := path.Match(a, b)
			return bool01(matched)
		}
	}
	if m.gone {
		return "" // no session: tmux expands every pane field empty
	}
	if m.onSurvivor {
		switch f {
		case inputFenceOption:
			return m.survivorOwn
		case "pane_id":
			return fenceModelSurvivor
		case "pane_dead":
			return "0"
		case "pane_pid":
			return "777"
		}
	}
	switch f {
	case inputFenceOption:
		if m.own != "" {
			return m.own
		}
		return m.inherited
	case "pane_id":
		return m.paneID
	case "pane_dead":
		return bool01(m.dead)
	case "pane_pid":
		return m.pid
	}
	return ""
}

func matchingBrace(s string, open int) int {
	depth := 0
	for k := open; k < len(s); k++ {
		switch s[k] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return k
			}
		}
	}
	return len(s) - 1
}

func splitFormatArgs(s string) []string {
	var parts []string
	depth, start := 0, 0
	for k := 0; k < len(s); k++ {
		switch s[k] {
		case '{':
			depth++
		case '}':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, s[start:k])
				start = k + 1
			}
		}
	}
	parts = append(parts, s[start:])
	for len(parts) < 3 {
		parts = append(parts, "")
	}
	return parts
}

// parseCommandString parses an if-shell branch: single-quoted words, and
// " ; " between commands.
func parseCommandString(s string) ([][]string, error) {
	var cmds [][]string
	var cur []string
	for i := 0; i < len(s); {
		switch s[i] {
		case ' ':
			i++
		case '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return nil, fmt.Errorf("model: unterminated quote in %q", s)
			}
			cur = append(cur, s[i+1:i+1+j])
			i += j + 2
		case ';':
			cmds = append(cmds, cur)
			cur = nil
			i++
		default:
			return nil, fmt.Errorf("model: unquoted word in %q", s)
		}
	}
	if len(cur) > 0 {
		cmds = append(cmds, cur)
	}
	return cmds, nil
}

func flagValue(args []string, flag string) string {
	if i := slices.Index(args, flag); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

func (m *fenceModel) snapshot() (bare [][]string, events []string, buffers int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.bare), slices.Clone(m.events), len(m.buffers)
}

// fenceModelProvider is a tmux Provider over m with fast nudge timings.
func fenceModelProvider(m *fenceModel) *Provider {
	cfg := DefaultConfig()
	cfg.DebounceMs = 0
	cfg.NudgeIdleTimeout = 200 * time.Millisecond
	cfg.NudgeReadyTimeout = time.Second
	cfg.NudgeRetryInterval = 10 * time.Millisecond
	tm := NewTmuxWithConfig(cfg)
	tm.exec = m
	return &Provider{tm: tm, cfg: cfg, cache: NewStateCache(nil, time.Hour), workDirs: map[string]string{}}
}

var testFence = runtime.InputFence{Generation: 2, Epoch: 3, Token: "model-token"}

// ownFenced makes m's pane hold f as its own fence.
func (m *fenceModel) ownFenced(f runtime.InputFence) *fenceModel {
	m.own = f.Digest() + "." + m.paneID
	return m
}

// staticShellScreen is an idle non-agent pane.
func staticShellScreen(*fenceModel) string { return "$ " }

// fencedScenario is one fenced entry point on one pane.
type fencedScenario struct {
	name  string
	model func() *fenceModel
	run   func(ctx context.Context, p *Provider, f runtime.InputFence) error
	// hidden installs a hidden attach client (detached Gemini).
	hidden bool
	// want is an input event the scenario must deliver: proof it reached the
	// path it names.
	want string
}

func nudgeOp(text string, immediate bool) func(context.Context, *Provider, runtime.InputFence) error {
	return func(ctx context.Context, p *Provider, f runtime.InputFence) error {
		return p.NudgeFenced(ctx, fenceModelSession, runtime.TextContent(text), runtime.FencedNudge{Fence: f, Immediate: immediate})
	}
}

func claudeModel(draft string) func() *fenceModel {
	return func() *fenceModel {
		pane := newFakeClaudePane(draft)
		return newFenceModel(pane, fenceEnv(testFence)).ownFenced(testFence)
	}
}

func fencedScenarios() []fencedScenario {
	long := strings.Repeat("long line; with $HOME and #{x} and 'quotes'\n", maxSendKeysLiteralLen/40+2)
	return []fencedScenario{
		{name: "claude short immediate", model: claudeModel(""), run: nudgeOp("run the tests;", true), want: "text:run the tests;"},
		{name: "claude short wait-idle", model: claudeModel(""), run: nudgeOp("run the tests", false), want: "text:run the tests"},
		{name: "claude long", model: claudeModel(""), run: nudgeOp(long, true), want: "text:" + long},
		{name: "claude with a draft to clear", model: claudeModel("stale draft"), run: nudgeOp("hello", true), want: "key:C-k"},
		{name: "claude copy mode parked", model: func() *fenceModel {
			m := claudeModel("")()
			m.inMode = true
			return m
		}, run: nudgeOp("hello", true), want: "copy:cancel"},
		{name: "claude mid-session dialog", model: func() *fenceModel {
			m := claudeModel("")()
			m.overlay, m.overlayUntil = resumeDialogPane, "Enter"
			return m
		}, run: nudgeOp("hello", true), want: "text:hello"},
		{name: "claude feedback survey", model: func() *fenceModel {
			m := claudeModel("")()
			m.overlay, m.overlayUntil = feedbackSurveySessionFixture, "0"
			return m
		}, run: nudgeOp("hello", true), want: "key:0"},
		{name: "model-switch modal while not idle", model: func() *fenceModel {
			m := newFenceModel(nil, fenceEnv(testFence)).ownFenced(testFence)
			m.overlay, m.overlayUntil = "Approaching rate limits\nSwitch to gpt-5.4-mini for lower credit usage?\n  2. Keep current model\nPress enter to confirm or esc to go back", "Enter"
			m.screen = staticShellScreen
			return m
		}, run: nudgeOp("hello", false), want: "key:Down"},
		{name: "codex", model: func() *fenceModel {
			m := newFenceModel(nil, fenceEnv(testFence)).ownFenced(testFence)
			m.env["GC_PROVIDER"] = "codex"
			busy := false
			m.screen = func(*fenceModel) string {
				if busy {
					return "• Working (1s • esc to interrupt)\n› "
				}
				return "› "
			}
			m.onEvent = func(_ *fenceModel, ev string) { busy = busy || ev == "key:Enter" }
			return m
		}, run: nudgeOp("hello", true), want: "key:Enter"},
		{name: "fallback family", model: func() *fenceModel {
			m := newFenceModel(nil, fenceEnv(testFence)).ownFenced(testFence)
			m.env["GC_PROVIDER"] = "opencode"
			m.screen = staticShellScreen
			return m
		}, run: nudgeOp("hello", true), want: "key:Enter"},
		{name: "send keys", model: claudeModel(""), run: func(ctx context.Context, p *Provider, f runtime.InputFence) error {
			return p.SendKeysFenced(ctx, fenceModelSession, f, "Escape", "C-u", "Enter")
		}, want: "key:Escape"},
		{name: "interrupt", model: claudeModel("x"), run: func(ctx context.Context, p *Provider, f runtime.InputFence) error {
			return p.InterruptFenced(ctx, fenceModelSession, f)
		}, want: "key:C-c"},
		{name: "clear input", model: claudeModel("stale draft"), run: func(ctx context.Context, p *Provider, f runtime.InputFence) error {
			return p.ClearInput(runtime.WithInputFence(ctx, f), fenceModelSession, 0)
		}, want: "key:C-u"},
		{name: "dismiss known dialogs", model: func() *fenceModel {
			m := newFenceModel(nil, fenceEnv(testFence)).ownFenced(testFence)
			m.overlay, m.overlayUntil = "Resume from summary\nResume full session as-is\nEnter to confirm", "Enter"
			m.screen = staticShellScreen
			return m
		}, run: func(ctx context.Context, p *Provider, f runtime.InputFence) error {
			return p.DismissKnownDialogs(runtime.WithInputFence(ctx, f), fenceModelSession, 10*time.Millisecond)
		}, want: "key:Enter"},
		{name: "reset interrupted turn", model: geminiRewindModel, run: func(ctx context.Context, p *Provider, f runtime.InputFence) error {
			return p.ResetInterruptedTurn(runtime.WithInputFence(ctx, f), fenceModelSession)
		}, want: "key:Up"},
		{name: "hidden attach keys", model: claudeModel(""), hidden: true, run: func(ctx context.Context, p *Provider, f runtime.InputFence) error {
			return p.SendKeysFenced(ctx, fenceModelSession, f, "Up", "Enter")
		}},
		{name: "hidden attach text", model: claudeModel(""), hidden: true, run: nudgeOp("hello", true)},
	}
}

// geminiRewindModel plays Gemini's /rewind picker on a static pane, with a
// provider that needs no hidden attach client (GC_PROVIDER is not gemini).
func geminiRewindModel() *fenceModel {
	m := newFenceModel(nil, fenceEnv(testFence)).ownFenced(testFence)
	m.env["GC_PROVIDER"] = "opencode"
	state := "idle"
	m.screen = func(*fenceModel) string {
		switch state {
		case "picker":
			return "> Rewind\nCancel rewind and stay here"
		case "confirm":
			return "Confirm Rewind\nNo code changes to revert."
		}
		return "❯ "
	}
	enters := 0
	m.onEvent = func(_ *fenceModel, ev string) {
		if ev != "key:Enter" {
			return
		}
		enters++
		switch enters {
		case 1:
			state = "picker"
		case 2:
			state = "confirm"
		default:
			state = "idle"
		}
	}
	return m
}

// installHiddenClient gives p's session a hidden attach client whose PTY
// writes are collected in the returned buffer.
func installHiddenClient(t *testing.T, p *Provider) *syncBuffer {
	t.Helper()
	r, w := io.Pipe()
	got := &syncBuffer{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(got, r)
	}()
	ctx, cancel := context.WithCancel(context.Background())
	client := &hiddenAttachClient{cancel: cancel, done: make(chan error), stdin: w}
	p.tm.hiddenAttachMu.Lock()
	if p.tm.hiddenAttachClients == nil {
		p.tm.hiddenAttachClients = map[string]*hiddenAttachClient{}
	}
	p.tm.hiddenAttachClients[fenceModelSession] = client
	p.tm.hiddenAttachMu.Unlock()
	t.Cleanup(func() {
		_ = w.Close()
		<-done
		_ = ctx
	})
	return got
}

type syncBuffer struct {
	mu sync.Mutex
	b  []byte
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b = append(s.b, p...)
	return len(p), nil
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.b)
}

// The main mutation catcher: every fenced entry point, across the nudge
// families, dialogs, copy mode, composer clearing, the rewind picker and the
// hidden attach client, types into the pane only from inside a guard's
// if-shell. Reverting any one runCtx(ctx, ...) on an input path to run(...)
// leaves a bare send-keys or paste-buffer here.
func TestFencedInputNeverRunsBareInputCommand(t *testing.T) {
	for _, sc := range fencedScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			m := sc.model()
			p := fenceModelProvider(m)
			var hidden *syncBuffer
			if sc.hidden {
				hidden = installHiddenClient(t, p)
			}
			err := sc.run(context.Background(), p, testFence)
			if errors.Is(err, runtime.ErrFenceMismatch) {
				t.Fatalf("matching fence missed: %v", err)
			}
			bare, events, _ := m.snapshot()
			if len(bare) != 0 {
				t.Fatalf("bare input commands ran outside the guard: %q", bare)
			}
			if !sc.hidden && !slices.Contains(events, sc.want) {
				t.Fatalf("input %q lacks %q (err %v): the scenario misses the path it names", events, sc.want, err)
			}
			if sc.hidden && hidden.String() == "" {
				t.Fatalf("no input reached the hidden client (err %v)", err)
			}
		})
	}
}

// Every scenario under another incarnation's fence sends nothing at all and
// fails with a mismatch that is not partial.
func TestFencedInputUnderAnotherFenceSendsNothing(t *testing.T) {
	other := runtime.InputFence{Generation: 2, Epoch: 3, Token: "another-token"}
	for _, sc := range fencedScenarios() {
		t.Run(sc.name, func(t *testing.T) {
			m := sc.model()
			p := fenceModelProvider(m)
			var hidden *syncBuffer
			if sc.hidden {
				hidden = installHiddenClient(t, p)
			}
			err := sc.run(context.Background(), p, other)
			var miss *runtime.FenceMismatchError
			if !errors.As(err, &miss) || miss.Partial || miss.Found != testFence.Digest() {
				t.Fatalf("err = %v, want a non-partial mismatch finding the pane's fence", err)
			}
			if _, events, _ := m.snapshot(); len(events) != 0 {
				t.Fatalf("input reached the pane: %q", events)
			}
			if hidden != nil && hidden.String() != "" {
				t.Fatalf("hidden client received %q", hidden.String())
			}
		})
	}
}

// For a matching fence the pane receives exactly what the unfenced call
// types, with literal text arriving by buffer instead of send-keys -l (E4).
// The model and the simulated Claude pane read send-keys -l text as tmux does
// (runtime.TmuxLiteralText, which refuses text the E5 encoding did not
// produce), so both arms must deliver the scenario's text and leave the pane
// with the same submitted messages and draft; TestFencedNudgeRealTmuxTextIsByteExact
// pins the same on real tmux.
func TestFencedInputMatchesUnfencedInputEvents(t *testing.T) {
	unfenced := map[string]func(p *Provider) error{
		"claude short immediate": func(p *Provider) error {
			return p.NudgeNow(fenceModelSession, runtime.TextContent("run the tests;"))
		},
		"claude long": func(p *Provider) error {
			return p.NudgeNow(fenceModelSession, runtime.TextContent(strings.Repeat("long line; with $HOME and #{x} and 'quotes'\n", maxSendKeysLiteralLen/40+2)))
		},
		"claude with a draft to clear": func(p *Provider) error { return p.NudgeNow(fenceModelSession, runtime.TextContent("hello")) },
		"claude copy mode parked":      func(p *Provider) error { return p.NudgeNow(fenceModelSession, runtime.TextContent("hello")) },
		"claude mid-session dialog":    func(p *Provider) error { return p.NudgeNow(fenceModelSession, runtime.TextContent("hello")) },
		"fallback family":              func(p *Provider) error { return p.NudgeNow(fenceModelSession, runtime.TextContent("hello")) },
		"send keys":                    func(p *Provider) error { return p.SendKeys(fenceModelSession, "Escape", "C-u", "Enter") },
		"interrupt":                    func(p *Provider) error { return p.Interrupt(fenceModelSession) },
		"clear input":                  func(p *Provider) error { return p.ClearInput(context.Background(), fenceModelSession, 0) },
		"reset interrupted turn":       func(p *Provider) error { return p.ResetInterruptedTurn(context.Background(), fenceModelSession) },
	}
	for _, sc := range fencedScenarios() {
		plain, ok := unfenced[sc.name]
		if !ok {
			continue
		}
		t.Run(sc.name, func(t *testing.T) {
			fm := sc.model()
			fencedErr := sc.run(context.Background(), fenceModelProvider(fm), testFence)
			um := sc.model()
			plainErr := plain(fenceModelProvider(um))
			if (fencedErr == nil) != (plainErr == nil) {
				t.Fatalf("fenced err %v, unfenced err %v", fencedErr, plainErr)
			}
			_, got, _ := fm.snapshot()
			_, want, _ := um.snapshot()
			if !slices.Equal(got, want) {
				t.Fatalf("fenced input %q\nunfenced input %q", got, want)
			}
			if !slices.Contains(want, sc.want) {
				t.Errorf("unfenced input %q, want it to include %q", want, sc.want)
			}
			fp, fok := fm.inner.(*fakeClaudePane)
			up, uok := um.inner.(*fakeClaudePane)
			if !fok || !uok {
				return
			}
			fSubmitted, fDraft := fp.state()
			uSubmitted, uDraft := up.state()
			if !slices.Equal(fSubmitted, uSubmitted) || fDraft != uDraft {
				t.Errorf("fenced pane submitted %q, draft %q; unfenced pane submitted %q, draft %q", fSubmitted, fDraft, uSubmitted, uDraft)
			}
			if text, ok := strings.CutPrefix(sc.want, "text:"); ok && !slices.Contains(uSubmitted, text) {
				t.Errorf("unfenced pane submitted %q, want it to include %q", uSubmitted, text)
			}
		})
	}
}
