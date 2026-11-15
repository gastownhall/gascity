package exec

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/runtimecontract/reference"
)

// launchPackCaps is the handshake of a pack without tmux in its box that
// un-welds start: provision prepares the box, launch starts the agent.
const launchPackCaps = `{"version":0,"capabilities":["proc.provision","proc.launch"]}`

// launchPackOps are the op bodies of a pack without tmux in its box. Each
// keeps its stdin (provision.stdin, launch.stdin), the input it was given in
// $dir/input (one line per nudge or send-keys, in order), and launch shows
// the bypass-permissions dialog when $dir/dialog-at-start exists, until
// Enter answers it. exec answers exit 2: there is no exec, and no tmux.
func launchPackOps() map[string]string {
	return map[string]string{
		"provision":  `cat > "$dir/provision.stdin"`,
		"launch":     `cat > "$dir/launch.stdin"; : > "$dir/running"; if [ -f "$dir/dialog-at-start" ]; then : > "$dir/dialog"; fi`,
		"exec":       `cat > /dev/null; exit 2`,
		"is-running": `if [ -f "$dir/running" ]; then echo true; else echo false; fi`,
		"stop":       `rm -f "$dir/running"`,
		"peek":       `if [ -f "$dir/dialog" ]; then echo "Bypass Permissions mode"; else echo "❯ "; fi`,
		"send-keys":  `shift 2; echo "keys $*" >> "$dir/input"; if [ "$1" = Enter ]; then rm -f "$dir/dialog"; fi`,
		"nudge":      `txt=$(cat); echo "nudge $txt" >> "$dir/input"`,
	}
}

// inputLines is the input the pack was given, in order.
func inputLines(t *testing.T, dir string) []string {
	t.Helper()
	data := readLog(t, filepath.Join(dir, "input"))
	if data == "" {
		return nil
	}
	return strings.Split(strings.TrimRight(data, "\n"), "\n")
}

// Kills a launch that still issues tmux argv over exec for a pack declaring
// proc.launch, so a pack without tmux in its box could not use the un-welded
// path: Start provisions, then runs the launch op, and nothing reaches exec;
// a Relaunch runs launch again without reprovisioning. Separable launch is
// proc.provision with proc.exec or proc.launch, and carrier.ops no longer
// welds a pack declaring proc.launch.
func TestLaunchOpReplacesTmuxArgv(t *testing.T) {
	script, dir := observePack(t, launchPackCaps, launchPackOps())
	p := NewProvider(script)
	if _, tp := p.Seams(); !tp.Capabilities().SeparableLaunch {
		t.Fatal("SeparableLaunch = false for proc.provision + proc.launch")
	}
	if err := NewSeamBacked(script).Start(t.Context(), "s", runtime.Config{Command: "agent --serve"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got, want := opsRan(t, dir), []string{"provision", "launch"}; !slices.Equal(got, want) {
		t.Fatalf("ops = %v, want %v: provision then launch, no exec and no welded start", got, want)
	}
	if launched := readLog(t, filepath.Join(dir, "launch.stdin")); !strings.Contains(launched, `"command":"agent --serve"`) {
		t.Errorf("launch stdin = %q, want the start config with the agent's command", launched)
	}
	if err := p.Relaunch(t.Context(), "s", runtime.Config{Command: "agent --resume"}); err != nil {
		t.Fatalf("Relaunch: %v", err)
	}
	if got, want := opsRan(t, dir), []string{"provision", "launch", "launch"}; !slices.Equal(got, want) {
		t.Fatalf("ops after Relaunch = %v, want %v: a relaunch runs launch into the warm box", got, want)
	}
	for _, log := range []string{"ops", "launch.stdin", "provision.stdin"} {
		if strings.Contains(readLog(t, filepath.Join(dir, log)), "tmux") {
			t.Errorf("%s mentions tmux: a proc.launch pack is sent no tmux argv", log)
		}
	}

	for _, tc := range []struct {
		caps      string
		separable bool
	}{
		{`{"version":0,"capabilities":["proc.provision","proc.launch","carrier.ops"]}`, true},
		{`{"version":0,"capabilities":["proc.provision","proc.exec","carrier.ops"]}`, false},
		{`{"version":0,"capabilities":["proc.provision"]}`, false},
		{`{"version":0,"capabilities":["proc.launch"]}`, false},
	} {
		script, _ := observePack(t, tc.caps, launchPackOps())
		if _, tp := NewProvider(script).Seams(); tp.Capabilities().SeparableLaunch != tc.separable {
			t.Errorf("handshake %s: SeparableLaunch = %v, want %v", tc.caps, !tc.separable, tc.separable)
		}
	}
}

// isNudgeLine reports a pack input line that is a typed nudge.
func isNudgeLine(line string) bool { return strings.HasPrefix(line, "nudge ") }

// Kills an un-welded start that types its first turn (the startup prompt
// rides in it for a pack runtime), loses it, hands it back twice, leaves the
// dialog unanswered, or still sends it to provision or launch, which cannot
// type it: Start answers the dialog and types nothing else, and hands the
// turn back once for its caller to queue. A pack without an idle boundary
// has the turn held until ready_delay_ms after the launch.
func TestUnweldedStartQueuesFirstTurn(t *testing.T) {
	script, dir := observePack(t, launchPackCaps, launchPackOps())
	writeFile(t, filepath.Join(dir, "dialog-at-start"), "")
	sp := NewSeamBacked(script)
	cfg := runtime.Config{Command: "agent", Nudge: "first turn", EmitsPermissionWarning: true, ReadyDelayMs: 500}
	began := time.Now()
	if err := sp.Start(t.Context(), "s", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := inputLines(t, dir); !slices.Contains(got, "keys Enter") || slices.ContainsFunc(got, isNudgeLine) {
		t.Fatalf("input = %q, want the dialog answered (Enter) and no turn typed", got)
	}
	for _, op := range []string{"provision", "launch"} {
		if sent := readLog(t, filepath.Join(dir, op+".stdin")); strings.Contains(sent, `"nudge"`) {
			t.Errorf("%s stdin = %q: gc queues the first turn itself, so it leaves nudge out", op, sent)
		}
	}
	turn, ok := runtime.TakeStartupNudgeHandback(sp, "s")
	if !ok || turn.Text != "first turn" {
		t.Fatalf("handback = %+v, %v; want the first turn", turn, ok)
	}
	if floor := began.Add(500 * time.Millisecond); turn.NotBefore.Before(floor) {
		t.Errorf("NotBefore = %s, want no earlier than %s: a pack without an idle boundary holds the turn ready_delay_ms past the launch", turn.NotBefore, floor)
	}
	if _, again := runtime.TakeStartupNudgeHandback(sp, "s"); again {
		t.Error("the handback was taken twice")
	}

	// A start without a first turn hands nothing back; a later start
	// replaces what an earlier one left untaken.
	if err := sp.Start(t.Context(), "quiet", runtime.Config{Command: "agent"}); err != nil {
		t.Fatalf("Start without a first turn: %v", err)
	}
	if turn, ok := runtime.TakeStartupNudgeHandback(sp, "quiet"); ok {
		t.Errorf("handback %+v for a start without a first turn", turn)
	}
	if err := sp.Start(t.Context(), "s", runtime.Config{Command: "agent", Nudge: "stale turn"}); err != nil {
		t.Fatal(err)
	}
	if err := sp.Start(t.Context(), "s", runtime.Config{Command: "agent"}); err != nil {
		t.Fatal(err)
	}
	if turn, ok := runtime.TakeStartupNudgeHandback(sp, "s"); ok {
		t.Errorf("handback %q left over from an earlier start", turn.Text)
	}

	// A separable Relaunch hands its first turn back the same way.
	if err := NewProvider(script).Relaunch(t.Context(), "s", runtime.Config{Command: "agent", Nudge: "relaunch turn"}); err != nil {
		t.Fatalf("Relaunch: %v", err)
	}
	if got := inputLines(t, dir); slices.ContainsFunc(got, isNudgeLine) {
		t.Errorf("input = %q: a relaunch typed its first turn", got)
	}
}

// Kills a ready floor on a pack whose idle boundary the queue reads: its
// handed-back turn waits for the session's idle, not a fixed delay.
func TestUnweldedIdleBoundaryTurnHasNoFloor(t *testing.T) {
	script, _ := observePack(t, launchIdlePackCaps, launchPackOps())
	sp := NewSeamBacked(script)
	if err := sp.Start(t.Context(), "s", runtime.Config{Command: "agent", Nudge: "first turn", ReadyDelayMs: 500}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	turn, ok := runtime.TakeStartupNudgeHandback(sp, "s")
	if !ok || turn.Text != "first turn" || !turn.NotBefore.IsZero() {
		t.Fatalf("handback = %+v, %v; want the first turn with no NotBefore", turn, ok)
	}
}

// launchIdlePackCaps is a pack without tmux that un-welds start under
// carrier.ops and declares the idle triple, so the queue's idle wait gates
// its handed-back first turn.
const launchIdlePackCaps = `{"version":0,"capabilities":["proc.provision","proc.launch","carrier.ops","report-activity","report-attachment","idle-boundary","input-outcome"]}`

// Kills a first turn lost to the start's deadline (review round 2's HIGH):
// under fs.host a slow host session_setup spent the rest of the start's
// ctx, the nudge op never forked, Start returned nil and nothing was handed
// back. Start no longer sends the turn, so whatever the setup spends, the
// turn is handed back, and the fresh (createFresh) start does the same.
func TestUnweldedSlowHostSetupStillHandsBackFirstTurn(t *testing.T) {
	for _, fresh := range []bool{false, true} {
		t.Run(fmt.Sprintf("fresh_only=%v", fresh), func(t *testing.T) {
			sp, _, _ := hostFSGoldenFixtureWith(t, func(b map[string]string) { reference.LaunchBodies(b) })
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			cfg := runtime.Config{WorkDir: t.TempDir(), Command: "sleep 300", Nudge: "first turn", SessionSetup: []string{"sleep 3"}, FreshOnly: fresh}
			t.Cleanup(func() { _ = sp.Stop("s") })
			if err := sp.Start(ctx, "s", cfg); err != nil {
				t.Fatalf("Start = %v, want nil: a session_setup that outlives the start's ctx is a warning", err)
			}
			if turn, ok := runtime.TakeStartupNudgeHandback(sp, "s"); !ok || turn.Text != "first turn" {
				t.Fatalf("handback = %+v, %v; want the first turn: it was lost to the start's deadline", turn, ok)
			}
		})
	}
}
