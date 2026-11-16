package exec

import (
	"context"
	"errors"
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

// Kills a first turn lost to the start's deadline: under fs.host a slow
// host session_setup spent the rest of the start's ctx, the nudge op never forked, Start returned nil and nothing was handed
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
// returns (the queue's claim then holds the handed-back turn until it expires, and
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

// Kills a pack that un-welds start but answers its metadata ops exit 2,
// read as the forward-compatible no-op: the launch's identity write is
// reported done and every read answers unset, so the claim cannot read the
// launch its first turn is bound to and holds the turn, and the sends
// behind it, until the turn expires. Such a pack's metadata ops are
// required (runtime.ErrMetaUnsupported), whether it launches through the
// launch op or tmux over exec; a welded pack's exit 2 stays the no-op.
func TestUnweldedPackMetaExitTwoIsUnsupported(t *testing.T) {
	for _, tc := range []struct {
		caps     string
		unwelded bool
	}{
		{launchPackCaps, true},
		{`{"version":0,"capabilities":["proc.provision","proc.exec"]}`, true},
		{`{"version":0,"capabilities":["proc.provision","proc.exec","carrier.ops"]}`, false},
		{`{"version":0,"capabilities":["proc.exec"]}`, false},
	} {
		script, _ := observePack(t, tc.caps, launchPackOps())
		p := NewProvider(script)
		_, getErr := p.GetMeta("s", runtime.InstanceTokenEnv)
		setErr := p.SetMeta("s", runtime.InstanceTokenEnv, "tok")
		removeErr := p.RemoveMeta("s", runtime.InstanceTokenEnv)
		for op, err := range map[string]error{"get-meta": getErr, "set-meta": setErr, "remove-meta": removeErr} {
			if got := errors.Is(err, runtime.ErrMetaUnsupported); got != tc.unwelded {
				t.Errorf("handshake %s: %s exit 2 = %v; want ErrMetaUnsupported %v", tc.caps, op, err, tc.unwelded)
			}
		}
	}
}

// Kills a separable launch that leaves the replaced launch's token readable
// while the new agent starts, or a mixed identity after a failed write: a
// claim in that window judges an older queued turn live (on a pack that
// cannot fence it is typed into the new agent), and a fenced send against
// new counters and the old token misses and dead-letters the live turn.
// The launch withdraws the token before the agent starts, and a write that
// fails leaves no token, so the queue reads the launch unknown and holds.
func TestUnweldedLaunchWithdrawsTheReplacedToken(t *testing.T) {
	ops := identityPackOps()
	ops["launch"] = `cat > "$dir/launch.stdin"; : > "$dir/running"; if [ -f "$dir/meta.` + runtime.InstanceTokenEnv + `" ]; then cp "$dir/meta.` + runtime.InstanceTokenEnv + `" "$dir/token-at-launch"; else rm -f "$dir/token-at-launch"; fi`
	ops["set-meta"] = `if [ -f "$dir/fail-set.$3" ]; then echo "store down" >&2; exit 1; fi; ` + ops["set-meta"]
	ops["remove-meta"] = `rm -f "$dir/meta.$3"`
	script, dir := observePack(t, launchPackCaps, ops)
	sp := NewSeamBacked(script)
	if err := sp.Start(t.Context(), "s", identityConfig("tok-first", 3, 2)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	relaunch := sp.(runtime.RelaunchProvider)
	if err := relaunch.Relaunch(t.Context(), "s", identityConfig("tok-second", 4, 5)); err != nil {
		t.Fatalf("Relaunch: %v", err)
	}
	if seen := readLog(t, filepath.Join(dir, "token-at-launch")); seen != "" {
		t.Fatalf("the launch op ran while get-meta still answered token %q: the replaced launch's token must be withdrawn first", seen)
	}
	if id, ok := runtime.ReadLaunchID(sp, "s"); !ok || id != runtime.LaunchID("tok-second") {
		t.Fatalf("ReadLaunchID after the relaunch = %q, %v; want the second launch", id, ok)
	}

	writeFile(t, filepath.Join(dir, "fail-set."+runtime.ContinuationEpochEnv), "")
	if err := relaunch.Relaunch(t.Context(), "s", identityConfig("tok-third", 5, 6)); err != nil {
		t.Fatalf("Relaunch with a failing store: %v", err)
	}
	if id, ok := runtime.ReadLaunchID(sp, "s"); ok {
		t.Fatalf("ReadLaunchID after a failed identity write = %q; want unknown, never a mixed or replaced identity", id)
	}
}

// Kills a welded-provision marker that outlives its box: every welded seam
// start stores one, and only a Launch, which a welded pack's start never
// runs, cleared it, so the provider kept one per session name for its
// lifetime. Stop forgets it.
func TestStopForgetsTheWeldedProvisionMarker(t *testing.T) {
	ops := launchPackOps()
	ops["start"] = `cat > /dev/null; : > "$dir/running"`
	script, _ := observePack(t, `{"version":0,"capabilities":[]}`, ops)
	p := NewProvider(script)
	rt, _ := p.Seams()
	if _, err := rt.Provision(t.Context(), "s", runtime.ProvisionRequest{Config: runtime.Config{Command: "agent"}}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if _, marked := p.weldedProvision.Load("s"); !marked {
		t.Fatal("a welded provision stored no marker: the fixture does not model the welded path")
	}
	if err := p.Stop("s"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, marked := p.weldedProvision.Load("s"); marked {
		t.Fatal("the welded-provision marker outlived Stop")
	}
}
