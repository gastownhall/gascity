package exec

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// launchPackCaps is the handshake of a pack without tmux in its box that
// un-welds start: provision prepares the box, launch starts the agent.
const launchPackCaps = `{"version":0,"capabilities":["proc.provision","proc.launch"]}`

// launchPackOps are the op bodies of a pack without tmux in its box. Each
// keeps its stdin (provision.stdin, launch.stdin), the input it was given in
// $dir/input (one line per nudge or send-keys, in order), and launch shows
// the bypass-permissions dialog when $dir/dialog-at-start exists, until
// Enter answers it. exec answers exit 2: there is no exec, and no tmux.
func launchPackOps(overrides map[string]string) map[string]string {
	ops := map[string]string{
		"provision":  `cat > "$dir/provision.stdin"`,
		"launch":     `cat > "$dir/launch.stdin"; : > "$dir/running"; if [ -f "$dir/dialog-at-start" ]; then : > "$dir/dialog"; fi`,
		"exec":       `cat > /dev/null; exit 2`,
		"is-running": `if [ -f "$dir/running" ]; then echo true; else echo false; fi`,
		"stop":       `rm -f "$dir/running"`,
		"peek":       `if [ -f "$dir/dialog" ]; then echo "Bypass Permissions mode"; else echo "❯ "; fi`,
		"send-keys":  `shift 2; echo "keys $*" >> "$dir/input"; if [ "$1" = Enter ]; then rm -f "$dir/dialog"; fi`,
		"nudge":      `txt=$(cat); echo "nudge $txt" >> "$dir/input"`,
	}
	for op, body := range overrides {
		ops[op] = body
	}
	return ops
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
	script, dir := observePack(t, launchPackCaps, launchPackOps(nil))
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
		script, _ := observePack(t, tc.caps, launchPackOps(nil))
		if _, tp := NewProvider(script).Seams(); tp.Capabilities().SeparableLaunch != tc.separable {
			t.Errorf("handshake %s: SeparableLaunch = %v, want %v", tc.caps, !tc.separable, tc.separable)
		}
	}
}

// Kills an un-welded start that never delivers its first turn (the startup
// prompt rides in it for a pack runtime), types it before the startup
// dialog is answered, types it twice, or still sends it to provision or
// launch, which cannot type it: gc sends it once, through the nudge op,
// after the dialog pass.
func TestUnweldedPathDeliversFirstTurn(t *testing.T) {
	script, dir := observePack(t, launchPackCaps, launchPackOps(nil))
	writeFile(t, filepath.Join(dir, "dialog-at-start"), "")
	cfg := runtime.Config{Command: "agent", Nudge: "first turn", EmitsPermissionWarning: true, ReadyDelayMs: 10}
	if err := NewSeamBacked(script).Start(t.Context(), "s", cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	got := inputLines(t, dir)
	enter, nudge := slices.Index(got, "keys Enter"), slices.Index(got, "nudge first turn")
	if enter < 0 || nudge < 0 || enter > nudge {
		t.Fatalf("input = %q, want the dialog answered (Enter) before the first turn is typed", got)
	}
	nudges := 0
	for _, line := range got {
		if strings.HasPrefix(line, "nudge ") {
			nudges++
		}
	}
	if nudges != 1 {
		t.Errorf("input = %q: the first turn was typed %d times, want once", got, nudges)
	}
	for _, op := range []string{"provision", "launch"} {
		if sent := readLog(t, filepath.Join(dir, op+".stdin")); strings.Contains(sent, `"nudge"`) {
			t.Errorf("%s stdin = %q: gc delivers the first turn itself, so it leaves nudge out", op, sent)
		}
	}

	script, dir = observePack(t, launchPackCaps, launchPackOps(nil))
	if err := NewSeamBacked(script).Start(t.Context(), "quiet", runtime.Config{Command: "agent"}); err != nil {
		t.Fatalf("Start without a first turn: %v", err)
	}
	if got := inputLines(t, dir); len(got) != 0 {
		t.Errorf("input = %q, want nothing typed for a start without a first turn", got)
	}
}

// Kills an un-welded first turn the runtime refused that is dropped,
// retried, or left to a later start; one delivered (or delivered
// unobserved, or possibly delivered) that is handed back, which would type
// it twice; and a gone answer read as a started session: a refused turn is
// handed back once (runtime.TakeStartupNudgeHandback, which the
// orchestrator queues as the user's submit), and gone is the agent dying
// during startup.
func TestUnweldedFirstTurnHandedBackOnRefusal(t *testing.T) {
	caps := `{"version":0,"capabilities":["proc.provision","proc.launch","input-outcome"]}`
	for _, tc := range []struct {
		answer   string // the nudge op's outcome; "" is a plain exit 0
		handBack bool
		died     bool
	}{
		{answer: "busy", handBack: true},
		{answer: "pending_interaction", handBack: true},
		{answer: "rejected", handBack: true},
		{answer: "not_sent", handBack: true},
		{answer: "unavailable", handBack: true},
		{answer: "delivered_unobserved"},
		{answer: "unconfirmed"},
		{answer: ""},
		{answer: "gone", died: true},
	} {
		t.Run("answer "+tc.answer, func(t *testing.T) {
			nudge := `txt=$(cat); echo "nudge $txt" >> "$dir/input"; `
			switch tc.answer {
			case "":
			case "delivered_unobserved":
				nudge += `echo '{"outcome":"delivered_unobserved"}'`
			default:
				nudge += `echo '{"outcome":"` + tc.answer + `"}'; exit 80`
			}
			script, dir := observePack(t, caps, launchPackOps(map[string]string{"nudge": nudge}))
			sp := NewSeamBacked(script)
			err := sp.Start(t.Context(), "s", runtime.Config{Command: "agent", Nudge: "first turn"})
			if tc.died != errors.Is(err, runtime.ErrSessionDiedDuringStartup) || (!tc.died && err != nil) {
				t.Fatalf("Start = %v, want died during startup %v", err, tc.died)
			}
			if tc.died && !slices.Contains(opsRan(t, dir), "stop") {
				t.Error("a start whose agent died left its box: no stop ran")
			}
			if got := inputLines(t, dir); len(got) != 1 {
				t.Errorf("input = %q, want one attempt at the first turn", got)
			}
			turn, ok := runtime.TakeStartupNudgeHandback(sp, "s")
			if ok != tc.handBack || (ok && turn != "first turn") {
				t.Fatalf("handback = %q, %v; want handed back %v", turn, ok, tc.handBack)
			}
			if _, again := runtime.TakeStartupNudgeHandback(sp, "s"); again {
				t.Error("the handback was taken twice")
			}
		})
	}

	// A start clears what an earlier start of the name left untaken, so a
	// stale turn is never queued after a later, delivered, one.
	answer := filepath.Join(t.TempDir(), "answer")
	writeFile(t, answer, "busy")
	nudge := `txt=$(cat); w=$(cat "` + answer + `"); if [ "$w" != ok ]; then echo "{\"outcome\":\"$w\"}"; exit 80; fi`
	script, _ := observePack(t, caps, launchPackOps(map[string]string{"nudge": nudge}))
	sp := NewSeamBacked(script)
	if err := sp.Start(t.Context(), "s", runtime.Config{Command: "agent", Nudge: "stale turn"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(answer, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := sp.Start(t.Context(), "s", runtime.Config{Command: "agent", Nudge: "fresh turn"}); err != nil {
		t.Fatal(err)
	}
	if turn, ok := runtime.TakeStartupNudgeHandback(sp, "s"); ok {
		t.Errorf("handback %q left over from an earlier start", turn)
	}
}
