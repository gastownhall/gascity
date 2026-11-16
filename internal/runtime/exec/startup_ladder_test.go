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

// identityPackOps is launchPackOps with a metadata store ($dir/meta.<key>)
// whose launch op records no identity of its own.
func identityPackOps() map[string]string {
	ops := launchPackOps()
	ops["set-meta"] = `v=$(cat); printf %s "$v" > "$dir/meta.$3"`
	ops["get-meta"] = `if [ -f "$dir/meta.$3" ]; then cat "$dir/meta.$3"; fi`
	return ops
}

// identityConfig is a start config carrying one launch's identity.
func identityConfig(token string, generation, epoch int) runtime.Config {
	return runtime.Config{Command: "agent", Nudge: "first turn", Env: map[string]string{
		runtime.InstanceTokenEnv:     token,
		runtime.RuntimeEpochEnv:      fmt.Sprint(generation),
		runtime.ContinuationEpochEnv: fmt.Sprint(epoch),
	}}
}

// Kills an un-welded launch whose identity the queue cannot read when Start
// returns (PR 1's claim then holds the handed-back turn until it expires, and
// the pre-send fence read releases it on every pass): Start and a Relaunch
// publish the instance token and both epochs of the launch they ran, so
// runtime.ReadLaunchID and runtime.ReadInputFence answer that launch, never
// the one it replaced.
func TestUnweldedLaunchPublishesItsIdentity(t *testing.T) {
	script, _ := observePack(t, launchPackCaps, identityPackOps())
	sp := NewSeamBacked(script)
	first := identityConfig("tok-first", 3, 2)
	if err := sp.Start(t.Context(), "s", first); err != nil {
		t.Fatalf("Start: %v", err)
	}
	wantIdentity := func(cfg runtime.Config, after string) {
		t.Helper()
		if id, ok := runtime.ReadLaunchID(sp, "s"); !ok || id != runtime.LaunchID(cfg.Env[runtime.InstanceTokenEnv]) {
			t.Fatalf("ReadLaunchID %s = %q, %v; want the launch's id", after, id, ok)
		}
		want, _ := runtime.InputFenceFromEnv(cfg.Env)
		if got, ok := runtime.ReadInputFence(sp, "s"); !ok || got != want {
			t.Fatalf("ReadInputFence %s = %v, %v; want %v", after, got, ok, want)
		}
	}
	wantIdentity(first, "after Start")
	second := identityConfig("tok-second", 4, 5)
	if err := sp.(runtime.RelaunchProvider).Relaunch(t.Context(), "s", second); err != nil {
		t.Fatalf("Relaunch: %v", err)
	}
	wantIdentity(second, "after Relaunch")
}

// Kills a launch after a Provision the welded `start` op served (the
// handshake failed then, so it did not declare a separable launch) that
// launches the agent again, or hands the first turn back, once a handshake
// retry declares one: the welded op already started the agent with the turn
// in its config, so a second launch would replace it and queue the turn a
// second time.
func TestLaunchAfterWeldedProvisionLaunchesNothing(t *testing.T) {
	old := handshakeNow
	now := time.Now()
	handshakeNow = func() time.Time { return now }
	t.Cleanup(func() { handshakeNow = old })
	ops := launchPackOps()
	ops["start"] = `cat > "$dir/start.stdin"; : > "$dir/running"`
	dir := t.TempDir()
	handshake := filepath.Join(dir, "handshake")
	writeFile(t, handshake, "not json")
	script, packDir := observePack(t, "", ops)
	// observePack's empty handshake answers exit 2; serve it from a file
	// instead, so the test can turn it valid between Provision and Launch.
	body := readLog(t, script)
	body = strings.Replace(body, `[ "$op" = protocol ] && exit 2`, fmt.Sprintf(`if [ "$op" = protocol ]; then cat %q; exit 0; fi`, handshake), 1)
	writeFile(t, script, body)

	p := NewProvider(script)
	rt, tp := p.Seams()
	place, err := rt.Provision(t.Context(), "s", runtime.ProvisionRequest{Config: runtime.Config{Command: "agent", Nudge: "first turn"}})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if got := opsRan(t, packDir); !slices.Contains(got, "start") || slices.Contains(got, "provision") {
		t.Fatalf("ops = %v, want the welded start while the handshake fails", got)
	}
	writeFile(t, handshake, launchPackCaps)
	now = now.Add(HandshakeRetryAfter + time.Second)
	if !tp.Capabilities().SeparableLaunch {
		t.Fatal("the handshake retry did not declare a separable launch: the fixture does not model the flip")
	}
	if _, err := tp.Launch(t.Context(), place, runtime.LaunchSpec{Config: runtime.Config{Command: "agent", Nudge: "first turn"}}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if got := opsRan(t, packDir); slices.Contains(got, "launch") {
		t.Fatalf("ops = %v: the launch after a welded provision launched the agent again", got)
	}
	if turn, ok := p.TakeStartupNudgeHandback("s"); ok {
		t.Fatalf("handback = %q after a welded provision: the welded start took the first turn", turn.Text)
	}
}
