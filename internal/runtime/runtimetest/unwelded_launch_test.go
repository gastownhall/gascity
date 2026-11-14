package runtimetest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

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
// the launch op, delivers the first turn through the nudge op after the
// dialog pass, and hands back a turn the pack refused.
func TestUnweldedLaunchContractOnExec(t *testing.T) {
	RunUnweldedLaunchContract(t, UnweldedLaunchFixture{
		New: func(t *testing.T, refuse runtime.InputOutcome) (runtime.Provider, func(string) []string) {
			script, state := unweldedGolden(t)
			switch refuse {
			case "":
			case runtime.InputOutcomeGone:
				t.Setenv("GC_RPP_FAULT", "dead")
			default:
				t.Setenv("GC_RPP_FAULT", "refuse_"+string(refuse))
			}
			return sessionexec.NewSeamBacked(script), goldenInput(t, state)
		},
		Config: func(t *testing.T, dialog bool) runtime.Config {
			cfg := runtime.Config{WorkDir: t.TempDir(), Command: "sleep 300", ReadyDelayMs: 10}
			if dialog {
				cfg.EmitsPermissionWarning = true
				cfg.Env = map[string]string{reference.StartupDialogEnv: "1"}
			}
			return cfg
		},
	})
}

// unweldedFake is an un-welded runtime over runtime.Fake whose every
// RUNTIME-CONTRACT-020 duty can be broken on its own, so each check is shown
// to catch the violation it names.
type unweldedFake struct {
	*runtime.Fake
	refuse         runtime.InputOutcome
	typesTwice     bool // types the first turn twice
	beforeDialog   bool // types the first turn before answering the dialog
	dropsRefusal   bool // hands back nothing for a refused turn
	typesRefusal   bool // types a refused turn anyway
	failsOnRefusal bool // fails the start whose turn was refused
	goneStarts     bool // reads gone as a started session

	mu       sync.Mutex
	typed    []string
	handback map[string]string
}

func (f *unweldedFake) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if err := f.Fake.Start(ctx, name, cfg); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	dialog := cfg.EmitsPermissionWarning
	if dialog && f.beforeDialog {
		f.typed = append(f.typed, cfg.Nudge)
		dialog = false
	}
	if dialog {
		f.typed = append(f.typed, "Enter")
	}
	switch f.refuse {
	case "":
		if !f.beforeDialog || !cfg.EmitsPermissionWarning {
			f.typed = append(f.typed, cfg.Nudge)
		}
		if f.typesTwice {
			f.typed = append(f.typed, cfg.Nudge)
		}
		return nil
	case runtime.InputOutcomeGone:
		if f.goneStarts {
			return nil
		}
		return fmt.Errorf("%w: %s", runtime.ErrSessionDiedDuringStartup, name)
	}
	if f.typesRefusal {
		f.typed = append(f.typed, cfg.Nudge)
	}
	if !f.dropsRefusal {
		if f.handback == nil {
			f.handback = map[string]string{}
		}
		f.handback[name] = cfg.Nudge
	}
	if f.failsOnRefusal {
		return fmt.Errorf("first turn refused %s", f.refuse)
	}
	return nil
}

func (f *unweldedFake) TakeStartupNudgeHandback(name string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	nudge, ok := f.handback[name]
	delete(f.handback, name)
	return nudge, ok
}

func (f *unweldedFake) input(string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.typed...)
}

// Kills a RUNTIME-CONTRACT-020 check that passes a runtime breaking the duty
// it names, and one that fails a runtime keeping it.
func TestUnweldedLaunchChecksCatchTheirViolation(t *testing.T) {
	cfg := runtime.Config{Command: "agent"}
	dialogCfg := runtime.Config{Command: "agent", EmitsPermissionWarning: true}
	refused := runtime.InputOutcomeBusy
	for _, tc := range []struct {
		name   string
		fake   *unweldedFake
		check  func(t *testing.T, f *unweldedFake) error
		wantOK bool
	}{
		{"delivered", &unweldedFake{}, func(t *testing.T, f *unweldedFake) error {
			return checkFirstTurnDelivered(t, f, f.input, cfg)
		}, true},
		{"typed twice", &unweldedFake{typesTwice: true}, func(t *testing.T, f *unweldedFake) error {
			return checkFirstTurnDelivered(t, f, f.input, cfg)
		}, false},
		{"dialog answered first", &unweldedFake{}, func(t *testing.T, f *unweldedFake) error {
			return checkDialogAtStart(t, f, f.input, dialogCfg)
		}, true},
		{"typed before the dialog", &unweldedFake{beforeDialog: true}, func(t *testing.T, f *unweldedFake) error {
			return checkDialogAtStart(t, f, f.input, dialogCfg)
		}, false},
		{"refusal handed back", &unweldedFake{refuse: refused}, func(t *testing.T, f *unweldedFake) error {
			return checkRefusedHandedBack(t, f, f.input, cfg, refused)
		}, true},
		{"refusal dropped", &unweldedFake{refuse: refused, dropsRefusal: true}, func(t *testing.T, f *unweldedFake) error {
			return checkRefusedHandedBack(t, f, f.input, cfg, refused)
		}, false},
		{"refusal typed", &unweldedFake{refuse: refused, typesRefusal: true}, func(t *testing.T, f *unweldedFake) error {
			return checkRefusedHandedBack(t, f, f.input, cfg, refused)
		}, false},
		{"refusal fails the start", &unweldedFake{refuse: refused, failsOnRefusal: true}, func(t *testing.T, f *unweldedFake) error {
			return checkRefusedHandedBack(t, f, f.input, cfg, refused)
		}, false},
		{"gone dies", &unweldedFake{refuse: runtime.InputOutcomeGone}, func(t *testing.T, f *unweldedFake) error {
			return checkGoneDiesDuringStartup(t, f, cfg)
		}, true},
		{"gone starts", &unweldedFake{refuse: runtime.InputOutcomeGone, goneStarts: true}, func(t *testing.T, f *unweldedFake) error {
			return checkGoneDiesDuringStartup(t, f, cfg)
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.fake.Fake = runtime.NewFake()
			if err := tc.check(t, tc.fake); (err == nil) != tc.wantOK {
				t.Errorf("check = %v, want ok %v", err, tc.wantOK)
			}
		})
	}
}
