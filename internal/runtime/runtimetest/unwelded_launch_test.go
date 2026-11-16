package runtimetest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	sessionexec "github.com/gastownhall/gascity/internal/runtime/exec"
	"github.com/gastownhall/gascity/internal/runtime/runtimecontract/reference"
)

// unweldedGolden writes the conformance golden reference in launch mode
// over input outcomes, without an exec op: a pack without tmux in its box
// that declares proc.provision and proc.launch. It returns the script and
// its state directory.
func unweldedGolden(t *testing.T) (script, state string) {
	t.Helper()
	dir := t.TempDir()
	state = filepath.Join(dir, "state")
	if err := os.Mkdir(state, 0o755); err != nil {
		t.Fatal(err)
	}
	bodies := reference.LaunchBodies(reference.InputOutcomeBodies(reference.Bodies()))
	bodies["exec"] = `cat >/dev/null; exit 2`
	script = filepath.Join(dir, "golden")
	if err := os.WriteFile(script, []byte(reference.Script(state, bodies)), 0o755); err != nil {
		t.Fatal(err)
	}
	return script, state
}

// goldenInput reads the input the golden typed into name's box.
func goldenInput(t *testing.T, state string) func(string) []string {
	return func(name string) []string {
		paths, err := filepath.Glob(filepath.Join(state, "city*", name, "typed"))
		if err != nil || len(paths) != 1 {
			return nil
		}
		data, err := os.ReadFile(paths[0])
		if err != nil {
			t.Fatal(err)
		}
		var lines []string
		for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
			if line != "" {
				lines = append(lines, line)
			}
		}
		return lines
	}
}

// TestUnweldedLaunchContractOnExec runs RUNTIME-CONTRACT-020 over the
// seam-backed exec provider and the golden reference in launch mode,
// proc.provision with proc.launch and no exec: gc launches the agent with
// the launch op, answers the dialog, and hands the first turn back, which
// the contract's stand-in queue sends through the nudge op. The golden in
// this mode has no idle boundary, so the turn carries ready_delay_ms.
func TestUnweldedLaunchContractOnExec(t *testing.T) {
	RunUnweldedLaunchContract(t, UnweldedLaunchFixture{
		New: func(t *testing.T, refuse runtime.InputOutcome) (runtime.Provider, func(string) []string, func()) {
			script, state := unweldedGolden(t)
			if refuse != "" {
				t.Setenv("GC_RPP_FAULT", "refuse_"+string(refuse))
			}
			accept := func() { t.Setenv("GC_RPP_FAULT", "") }
			return sessionexec.NewSeamBacked(script), goldenInput(t, state), accept
		},
		Config: func(t *testing.T, dialog bool) runtime.Config {
			cfg := runtime.Config{WorkDir: t.TempDir(), Command: "sleep 300", ReadyDelayMs: 50}
			if dialog {
				cfg.EmitsPermissionWarning = true
				cfg.Env = map[string]string{reference.StartupDialogEnv: "1"}
			}
			return cfg
		},
		ReadyFloor: true,
	})
}

// unweldedFake is an un-welded runtime over runtime.Fake whose every
// RUNTIME-CONTRACT-020 duty can be broken on its own, so each check is shown
// to catch the violation it names.
type unweldedFake struct {
	*runtime.Fake
	refuse         runtime.InputOutcome
	typesAtStart   bool // types the first turn in Start
	dropsTurn      bool // hands back nothing
	handsBackTwice bool // hands the turn back on every take
	skipsDialog    bool // leaves the startup dialog unanswered
	typesTwice     bool // a nudge types its text twice
	noFloor        bool // hands the turn back without its ready floor
	typesRefusal   bool // types a nudge it refuses
	untypedRefusal bool // refuses with an untyped error
	refusalOK      bool // answers a refused nudge nil
	failsStart     bool // fails Start while refusing input
	noIdentity     bool // records no identity a get-meta reads
	keepsIdentity  bool // a relaunch leaves the first launch's identity
	noEpochs       bool // records the token but neither epoch

	mu       sync.Mutex
	accepted bool
	typed    []string
	handback map[string]runtime.StartupNudge
}

func (f *unweldedFake) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if err := f.Fake.Start(ctx, name, cfg); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if cfg.EmitsPermissionWarning && !f.skipsDialog {
		f.typed = append(f.typed, "Enter")
	}
	if f.typesAtStart {
		f.typed = append(f.typed, cfg.Nudge)
	}
	if f.failsStart && f.refuse != "" {
		return fmt.Errorf("first turn refused %s", f.refuse)
	}
	f.publishIdentity(name, cfg)
	if !f.dropsTurn {
		turn := runtime.StartupNudge{Text: cfg.Nudge}
		if !f.noFloor {
			turn.NotBefore = time.Now().Add(time.Duration(cfg.ReadyDelayMs) * time.Millisecond)
		}
		if f.handback == nil {
			f.handback = map[string]runtime.StartupNudge{}
		}
		f.handback[name] = turn
	}
	return nil
}

// publishIdentity records cfg's launch identity as get-meta answers it,
// unless the fake breaks that duty.
func (f *unweldedFake) publishIdentity(name string, cfg runtime.Config) {
	if f.noIdentity {
		return
	}
	for _, key := range []string{runtime.InstanceTokenEnv, runtime.RuntimeEpochEnv, runtime.ContinuationEpochEnv} {
		if f.noEpochs && key != runtime.InstanceTokenEnv {
			continue
		}
		_ = f.SetMeta(name, key, cfg.Env[key])
	}
}

func (f *unweldedFake) Relaunch(_ context.Context, name string, cfg runtime.Config) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.keepsIdentity {
		f.publishIdentity(name, cfg)
	}
	return nil
}

func (f *unweldedFake) TakeStartupNudgeHandback(name string) (runtime.StartupNudge, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	nudge, ok := f.handback[name]
	if !f.handsBackTwice {
		delete(f.handback, name)
	}
	return nudge, ok
}

func (f *unweldedFake) Nudge(_ string, content []runtime.ContentBlock) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	text := content[0].Text
	if f.refuse != "" && !f.accepted {
		if f.typesRefusal {
			f.typed = append(f.typed, text)
		}
		switch {
		case f.refusalOK:
			return nil
		case f.untypedRefusal:
			return fmt.Errorf("refused %s", f.refuse)
		}
		return fmt.Errorf("%w: refused", f.refuse.Sentinel())
	}
	f.typed = append(f.typed, text)
	if f.typesTwice {
		f.typed = append(f.typed, text)
	}
	return nil
}

func (f *unweldedFake) accept() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accepted = true
}

func (f *unweldedFake) input(string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.typed...)
}

// Kills a RUNTIME-CONTRACT-020 check that passes a runtime breaking the duty
// it names, and one that fails a runtime keeping it.
func TestUnweldedLaunchChecksCatchTheirViolation(t *testing.T) {
	cfg := runtime.Config{Command: "agent", ReadyDelayMs: 20}
	dialogCfg := runtime.Config{Command: "agent", EmitsPermissionWarning: true}
	refused := runtime.InputOutcomeBusy
	queued := func(t *testing.T, f *unweldedFake) error { return checkFirstTurnQueued(t, f, f.input, cfg) }
	dialog := func(t *testing.T, f *unweldedFake) error { return checkDialogAtStart(t, f, f.input, dialogCfg) }
	once := func(t *testing.T, f *unweldedFake) error { return checkQueueDeliversOnce(t, f, f.input, cfg, true) }
	identified := func(t *testing.T, f *unweldedFake) error { return checkLaunchIdentified(t, f, cfg) }
	retried := func(t *testing.T, f *unweldedFake) error {
		return checkRefusedRetriedByQueue(t, f, f.input, f.accept, cfg, refused)
	}
	for _, tc := range []struct {
		name   string
		fake   *unweldedFake
		check  func(t *testing.T, f *unweldedFake) error
		wantOK bool
	}{
		{"queued", &unweldedFake{}, queued, true},
		{"typed at start", &unweldedFake{typesAtStart: true}, queued, false},
		{"turn dropped", &unweldedFake{dropsTurn: true}, queued, false},
		{"handed back twice", &unweldedFake{handsBackTwice: true}, queued, false},
		{"dialog answered at start", &unweldedFake{}, dialog, true},
		{"dialog left", &unweldedFake{skipsDialog: true}, dialog, false},
		{"typed before the dialog", &unweldedFake{typesAtStart: true}, dialog, false},
		{"delivered once", &unweldedFake{}, once, true},
		{"delivered twice", &unweldedFake{typesTwice: true}, once, false},
		{"no ready floor", &unweldedFake{noFloor: true}, once, false},
		{"launch identified", &unweldedFake{}, identified, true},
		{"launch unreadable", &unweldedFake{noIdentity: true}, identified, false},
		{"relaunch reads the old launch", &unweldedFake{keepsIdentity: true}, identified, false},
		{"fence without its epochs", &unweldedFake{noEpochs: true}, identified, false},
		{"refusal retried", &unweldedFake{refuse: refused}, retried, true},
		{"refusal typed", &unweldedFake{refuse: refused, typesRefusal: true}, retried, false},
		{"refusal untyped", &unweldedFake{refuse: refused, untypedRefusal: true}, retried, false},
		{"refusal reads delivered", &unweldedFake{refuse: refused, refusalOK: true}, retried, false},
		{"refusal fails the start", &unweldedFake{refuse: refused, failsStart: true}, retried, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.fake.Fake = runtime.NewFake()
			if err := tc.check(t, tc.fake); (err == nil) != tc.wantOK {
				t.Errorf("check = %v, want ok %v", err, tc.wantOK)
			}
		})
	}
}
