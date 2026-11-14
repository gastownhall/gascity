package runtimetest

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	sessionexec "github.com/gastownhall/gascity/internal/runtime/exec"
	"github.com/gastownhall/gascity/internal/runtime/runtimecontract/reference"
)

// outcomeGolden writes the conformance golden reference in input-outcome
// mode. carrierOps selects the dedicated driving ops; without it the pack
// answers exec with exit 2, so gc falls back to them. failFirstHandshake
// makes the first `protocol` fail, so a provider built over it holds a
// failed handshake (and reads no token) for the retry window.
func outcomeGolden(t *testing.T, carrierOps, failFirstHandshake bool) string {
	t.Helper()
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	if err := os.Mkdir(state, 0o755); err != nil {
		t.Fatal(err)
	}
	bodies := reference.InputOutcomeBodies(reference.Bodies())
	if carrierOps {
		bodies = reference.InputOutcomeBodies(reference.NonTmuxBodies())
	} else {
		bodies["exec"] = `cat >/dev/null; exit 2`
	}
	if failFirstHandshake {
		bodies["protocol"] = `if [ ! -f "$S/handshake-tried" ]; then : > "$S/handshake-tried"; echo "handshake broke" >&2; exit 1; fi; ` + bodies["protocol"]
	}
	script := filepath.Join(dir, "golden")
	if err := os.WriteFile(script, []byte(reference.Script(state, bodies)), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

// startOutcomeBox starts name on script through the pack's own start op, so
// no provider handshake runs first.
func startOutcomeBox(t *testing.T, script, name string) {
	t.Helper()
	cmd := exec.Command(script, "start", name)
	cmd.Stdin = strings.NewReader(`{"command":"sleep 300","process_names":["sleep"]}`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("start %s: %v (%s)", name, err, out)
	}
}

// warmOutcomeProvider returns the seam-backed exec provider over script
// after one nudge delivered to a running box of its own, so the provider
// has done its handshake and, in the dedicated-op fallback, learned the pack
// does not implement exec: it then reads the pack's outcomes, and a later
// driving op is answered by the pack itself, never by gc's presence
// pre-check (which a fresh fallback provider still runs).
func warmOutcomeProvider(t *testing.T, script string) runtime.Provider {
	t.Helper()
	startOutcomeBox(t, script, "warm")
	sp := sessionexec.NewSeamBacked(script)
	if err := sp.Nudge("warm", runtime.TextContent("warm-up")); err != nil {
		t.Fatalf("warm-up nudge to a running box = %v, want delivered", err)
	}
	return sp
}

// TestInputOutcomeMissingSessionContractOnExec runs RUNTIME-CONTRACT-019
// over the production seam-backed exec provider for a pack declaring
// input-outcome, whose outcomes are read (warmOutcomeProvider): the pack's
// own gone answer, with no presence pre-check, keeps a vanished session from
// reading as delivered.
func TestInputOutcomeMissingSessionContractOnExec(t *testing.T) {
	for name, carrierOps := range map[string]bool{"carrier.ops": true, "dedicated-op fallback": false} {
		t.Run(name, func(t *testing.T) {
			RunMissingSessionInputContract(t, func(t *testing.T) runtime.Provider {
				return warmOutcomeProvider(t, outcomeGolden(t, carrierOps, false))
			})
		})
	}
}

// TestInputOutcomeRefusalNeverDeliveredOnExec runs RefusalNeverDelivered over
// the seam-backed exec provider for a pack declaring input-outcome: with gc
// reading the outcomes, and with gc's handshake failed, so the pack's exit
// 80 reaches a gc that does not read the token. A blocked box and an
// unreachable backend refuse the nudge either way.
func TestInputOutcomeRefusalNeverDeliveredOnExec(t *testing.T) {
	for name, failHandshake := range map[string]bool{"outcomes read": false, "handshake failed": true} {
		t.Run(name, func(t *testing.T) {
			fixture := func(fault string) func(t *testing.T) (runtime.Provider, string) {
				return func(t *testing.T) (runtime.Provider, string) {
					script := outcomeGolden(t, false, failHandshake)
					startOutcomeBox(t, script, "refusing")
					t.Setenv("GC_RPP_FAULT", fault)
					return sessionexec.NewSeamBacked(script), "refusing"
				}
			}
			RunInputRefusalContract(t, InputRefusalFixtures{Blocked: fixture("blocked"), Unavailable: fixture("unavailable")})

			// The fixtures exercise the path their name says: the typed
			// sentinel only when gc reads the outcomes.
			sp, name := fixture("blocked")(t)
			err := sp.Nudge(name, runtime.TextContent("refused input"))
			if typed := errors.Is(err, runtime.ErrNudgeRefusedPendingInteraction); typed == failHandshake {
				t.Errorf("nudge to a blocked box = %v; typed %t, want typed only when outcomes are read", err, typed)
			}
		})
	}
}

// TestInputOutcomeDeadBoxIsGoneOnExec pins gone to "no running box holds the
// name" over the seam-backed exec provider for a pack declaring
// input-outcome: a box observe reports dead (the name held, nothing running)
// answers every driving op gone, which gc reads as runtime.ErrSessionNotFound
// with no presence pre-check (warmOutcomeProvider), never as delivered.
func TestInputOutcomeDeadBoxIsGoneOnExec(t *testing.T) {
	for name, carrierOps := range map[string]bool{"carrier.ops": true, "dedicated-op fallback": false} {
		t.Run(name, func(t *testing.T) {
			script := outcomeGolden(t, carrierOps, false)
			startOutcomeBox(t, script, "corpse")
			sp := warmOutcomeProvider(t, script)
			t.Setenv("GC_RPP_FAULT", "dead")
			for op, call := range map[string]func() error{
				"Nudge":           func() error { return sp.Nudge("corpse", runtime.TextContent("to a dead box")) },
				"SendKeys":        func() error { return sp.SendKeys("corpse", "Enter") },
				"Interrupt":       func() error { return sp.Interrupt("corpse") },
				"ClearScrollback": func() error { return sp.ClearScrollback("corpse") },
			} {
				if err := call(); !errors.Is(err, runtime.ErrSessionNotFound) {
					t.Errorf("%s to a dead box = %v, want runtime.ErrSessionNotFound", op, err)
				}
			}
		})
	}
}
