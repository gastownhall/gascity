package exec

import (
	"context"
	"errors"
	"fmt"
	"os"
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
// whose launch op records no identity of its own. set-meta of a key exits 1
// while $dir/fail-set.<key> exists.
func identityPackOps() map[string]string {
	ops := launchPackOps()
	ops["set-meta"] = `if [ -f "$dir/fail-set.$3" ]; then echo "store down" >&2; exit 1; fi; v=$(cat); printf %s "$v" > "$dir/meta.$3"`
	ops["get-meta"] = `if [ -f "$dir/meta.$3" ]; then cat "$dir/meta.$3"; fi`
	return ops
}

// tmuxLaunchPackCaps is the handshake of a pack that un-welds start with
// gc's tmux launch over exec: proc.provision with proc.exec.
const tmuxLaunchPackCaps = `{"version":0,"capabilities":["proc.provision","proc.exec"]}`

// tmuxIdentityPackOps is identityPackOps for a pack whose agent gc starts in
// the box's tmux over exec, with no launch op: has-session answers whether
// the agent runs, and new-session or respawn-pane starts it.
func tmuxIdentityPackOps() map[string]string {
	ops := identityPackOps()
	delete(ops, launchOp)
	ops["exec"] = `cmd=$(cat); case "$cmd" in *has-session*) [ -f "$dir/running" ] ;; *new-session*|*respawn-pane*) : > "$dir/running" ;; *) exit 1 ;; esac`
	return ops
}

// unweldedIdentityPack is a pack that un-welds start over a metadata store
// that records no identity of its own: its handshake and op bodies.
type unweldedIdentityPack struct {
	label, caps string
	ops         map[string]string
}

// unweldedIdentityPacks are such packs, one for each way gc launches the
// agent: the launch op, and gc's tmux launch over exec.
func unweldedIdentityPacks() []unweldedIdentityPack {
	return []unweldedIdentityPack{
		{"launch op", launchPackCaps, identityPackOps()},
		{"tmux over exec", tmuxLaunchPackCaps, tmuxIdentityPackOps()},
	}
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
// publish the instance token and both epochs of the launch they ran, through
// the launch op or the tmux launch over exec, so runtime.ReadLaunchID and
// runtime.ReadInputFence answer that launch, never the one it replaced.
func TestUnweldedLaunchPublishesItsIdentity(t *testing.T) {
	for _, pack := range unweldedIdentityPacks() {
		t.Run(pack.label, func(t *testing.T) {
			script, _ := observePack(t, pack.caps, pack.ops)
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
		})
	}
}

// Kills a launch reported up under an identity get-meta does not answer: on
// a non-fresh start of a pack whose provision seeds no identity, a failed
// set-meta of any identity key left the box without that key's value, yet
// Start returned nil and handed the first turn back, which its caller bound
// to the start's token, so every claim held the turn, and every send to the
// session behind it, until the turn expired a day later. A failed write that
// the box does not already hold fails the launch, by either launch path:
// Start returns an error naming the key, the start's teardown stops the box,
// and nothing is handed back.
func TestFailedIdentityWriteFailsTheStart(t *testing.T) {
	for _, pack := range unweldedIdentityPacks() {
		for _, failing := range launchIdentityKeys {
			t.Run(pack.label+"/"+failing, func(t *testing.T) {
				script, dir := observePack(t, pack.caps, pack.ops)
				sp := NewSeamBacked(script)
				writeFile(t, filepath.Join(dir, "fail-set."+failing), "")
				err := sp.Start(t.Context(), "s", identityConfig("tok-x", 1, 1))
				if err == nil {
					t.Fatalf("Start = nil after a failed %s write: the launch is reported up under an identity get-meta does not answer", failing)
				}
				if !strings.Contains(err.Error(), failing) {
					t.Errorf("Start = %v, want the error to name %s, the key whose write failed", err, failing)
				}
				if turn, ok := runtime.TakeStartupNudgeHandback(sp, "s"); ok {
					t.Fatalf("handback %q after the failed start: its caller would queue it bound to a launch no read names", turn.Text)
				}
				if ran := opsRan(t, dir); !slices.Contains(ran, "stop") {
					t.Fatalf("ops = %v: the failed launch left the box it provisioned", ran)
				}
			})
		}
	}
}

// Kills a warm relaunch reported up under the replaced launch's identity: a
// failed set-meta of one of the new launch's keys left the box holding the
// replaced launch's value for it beside the new values written before it
// (the token is written last, so a failed token write leaves tok-first with
// the new epochs 4 and 5), yet Relaunch returned nil and handed its first
// turn back, which its caller bound to the replaced launch. The relaunch
// fails and hands nothing back, so its caller falls back to the restart.
func TestFailedIdentityWriteFailsTheRelaunch(t *testing.T) {
	for _, failing := range launchIdentityKeys {
		t.Run(failing, func(t *testing.T) {
			script, dir := observePack(t, identityBoxCaps, identityBoxOps())
			sp := seamBacked(NewProvider(script))
			if err := sp.Start(t.Context(), "s", identityConfig("tok-first", 3, 2)); err != nil {
				t.Fatalf("Start: %v", err)
			}
			runtime.TakeStartupNudgeHandback(sp, "s")
			writeFile(t, filepath.Join(dir, "fail-set."+failing), "")
			if err := sp.Relaunch(t.Context(), "s", identityConfig("tok-second", 4, 5)); err == nil {
				fence, known := runtime.ReadInputFence(sp, "s")
				t.Fatalf("Relaunch = nil after a failed %s write, with get-meta answering %+v (known %v): the new launch is reported up under the replaced one's identity", failing, fence, known)
			}
			if turn, ok := runtime.TakeStartupNudgeHandback(sp, "s"); ok {
				t.Fatalf("handback %q after the failed relaunch: its caller would queue it bound to the replaced launch", turn.Text)
			}
		})
	}
}

// Kills a launch reported up over a store the pack does not keep: a pack
// that un-welds start but answers its metadata ops exit 2, against the
// metadata requirement (runtime.ErrMetaUnsupported), had its start succeed
// and hand the first turn back. Its caller could bind that turn to no
// launch, so it queued it as a plain submit, which the session's sends
// overtook. The launch fails by either launch path: Start returns an error
// that wraps runtime.ErrMetaUnsupported and names the key, after the first
// refused write and with no get-meta read-back, the start's teardown stops
// the box, and nothing is handed back.
func TestLaunchWithoutAMetadataStoreFails(t *testing.T) {
	for _, pack := range unweldedIdentityPacks() {
		t.Run(pack.label, func(t *testing.T) {
			ops := make(map[string]string, len(pack.ops))
			for op, body := range pack.ops {
				if op != "set-meta" && op != "get-meta" {
					ops[op] = body
				}
			}
			script, dir := observePack(t, pack.caps, ops)
			sp := NewSeamBacked(script)
			err := sp.Start(t.Context(), "s", identityConfig("tok-x", 1, 1))
			if !errors.Is(err, runtime.ErrMetaUnsupported) {
				t.Fatalf("Start = %v, want the launch failed on the missing metadata store (runtime.ErrMetaUnsupported)", err)
			}
			if !strings.Contains(err.Error(), runtime.RuntimeEpochEnv) {
				t.Errorf("Start = %v, want the error to name %s, the first identity key the store refused", err, runtime.RuntimeEpochEnv)
			}
			ran := opsRan(t, dir)
			writes := 0
			for _, op := range ran {
				if op == "set-meta" {
					writes++
				}
			}
			if writes != 1 || slices.Contains(ran, "get-meta") || !slices.Contains(ran, "stop") {
				t.Fatalf("ops = %v; want the one set-meta the store refused, no read-back, and the failed launch's teardown stopping the box", ran)
			}
			if turn, ok := runtime.TakeStartupNudgeHandback(sp, "s"); ok {
				t.Fatalf("handback %q after the failed start: its caller would queue it bound to no launch, and the session's sends would overtake it", turn.Text)
			}
		})
	}
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

// identityBoxCaps is the handshake of a v2-ready pack that un-welds start
// through the launch op.
const identityBoxCaps = `{"version":0,"capabilities":["proc.provision","proc.launch","observe.v1","report-attachment"]}`

// identityBoxOps is launchPackOps over one identity, as rules 4 and 5 of
// observe.v1 require: `provision` creates the box ($dir/box) with the
// identity its config's env carries, seeded into the metadata store
// ($dir/meta.<key>) before the box is visible (a fresh_only provision into
// a held name exits 73); `get-meta`, `set-meta` and `remove-meta` read and
// write that store, and `observe` reports the box with the store's token.
// `stop` destroys the box and its store. The launch op exits 1 while
// $dir/fail-launch exists, and set-meta of a key exits 1 while
// $dir/fail-set.<key> exists (identityPackOps).
func identityBoxOps() map[string]string {
	ops := identityPackOps()
	ops["provision"] = `cat > "$dir/provision.stdin"; ` +
		`if [ -f "$dir/box" ] && grep -q '"fresh_only":true' "$dir/provision.stdin"; then exit 73; fi; ` +
		`for k in ` + strings.Join(launchIdentityKeys, " ") + `; do ` +
		`v=$(sed -n "s/.*\"$k\":\"\([^\"]*\)\".*/\1/p" "$dir/provision.stdin"); ` +
		`if [ -n "$v" ]; then printf %s "$v" > "$dir/meta.$k"; fi; done; : > "$dir/box"`
	ops["launch"] = `cat > "$dir/launch.stdin"; if [ -f "$dir/fail-launch" ]; then echo "launch refused" >&2; exit 1; fi; : > "$dir/running"`
	ops["remove-meta"] = `rm -f "$dir/meta.$3"`
	ops["observe"] = `cat > /dev/null; if [ -f "$dir/box" ]; then tok=; ` +
		`if [ -f "$dir/meta.` + runtime.InstanceTokenEnv + `" ]; then tok=$(cat "$dir/meta.` + runtime.InstanceTokenEnv + `"); fi; ` +
		`printf '{"state":"running",` + testObject + `,"identity":{"` + runtime.InstanceTokenEnv + `":"%s"}}\n' "$tok"; ` +
		`else echo '{"state":"absent"}'; fi`
	ops["stop"] = `rm -f "$dir/box" "$dir/running" "$dir"/meta.*`
	return ops
}

// observedToken is the instance token observe reports for name's box.
func observedToken(t *testing.T, p *Provider, name string) string {
	t.Helper()
	o, err := p.observe(t.Context(), name, nil)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if !o.present() {
		t.Fatalf("observe reads %q absent", name)
	}
	return o.identity[runtime.InstanceTokenEnv]
}

// freshIdentityConfig is identityConfig for a FreshOnly start.
func freshIdentityConfig(token string, generation, epoch int) runtime.Config {
	cfg := identityConfig(token, generation, epoch)
	cfg.FreshOnly = true
	return cfg
}

// Kills a launch that strips the box's instance token before it runs: a
// fresh start whose launch op fails then leaves a box whose identity no
// longer carries the token provision seeded, so the failed start's cleanup
// cannot prove the box its own and leaks it, and the retry finds the name
// held (ErrSessionExists). The launch touches no identity before it runs,
// so the cleanup stops the box and the retry starts.
func TestFailedFreshUnweldedLaunchStopsItsBox(t *testing.T) {
	script, dir := observePack(t, identityBoxCaps, identityBoxOps())
	p := NewProvider(script)
	sp := seamBacked(p)
	writeFile(t, filepath.Join(dir, "fail-launch"), "")
	if err := sp.Start(t.Context(), "s", freshIdentityConfig("tok-first", 1, 1)); err == nil {
		t.Fatal("Start succeeded with a failing launch op: the fixture does not model the failure")
	}
	if ran := opsRan(t, dir); !slices.Contains(ran, "stop") {
		t.Fatalf("ops = %v: the failed fresh start left the box it provisioned", ran)
	}
	if err := os.Remove(filepath.Join(dir, "fail-launch")); err != nil {
		t.Fatal(err)
	}
	if err := sp.Start(t.Context(), "s", freshIdentityConfig("tok-retry", 1, 1)); err != nil {
		t.Fatalf("retry Start: %v", err)
	}
	if got := observedToken(t, p, "s"); got != "tok-retry" {
		t.Fatalf("observe token after the retry = %q, want tok-retry", got)
	}
}

// Kills a separable Relaunch that strips the running agent's token before
// its launch runs: when the launch fails before replacing the agent, the
// agent still runs but get-meta and observe no longer name it, so its
// identity reads unknown. The agent keeps its token.
func TestFailedUnweldedRelaunchKeepsTheRunningToken(t *testing.T) {
	script, dir := observePack(t, identityBoxCaps, identityBoxOps())
	p := NewProvider(script)
	sp := seamBacked(p)
	if err := sp.Start(t.Context(), "s", identityConfig("tok-first", 3, 2)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	writeFile(t, filepath.Join(dir, "fail-launch"), "")
	if err := sp.Relaunch(t.Context(), "s", identityConfig("tok-second", 4, 5)); err == nil {
		t.Fatal("Relaunch succeeded with a failing launch op: the fixture does not model the failure")
	}
	if id, ok := runtime.ReadLaunchID(sp, "s"); !ok || id != runtime.LaunchID("tok-first") {
		t.Fatalf("ReadLaunchID after the failed relaunch = %q, %v; want the running agent's", id, ok)
	}
	if got := observedToken(t, p, "s"); got != "tok-first" {
		t.Fatalf("observe token after the failed relaunch = %q, want tok-first", got)
	}
}

// Kills a failed identity write that removes the token, or fails a launch
// whose identity reads: on a fresh start provision seeded the launch's own
// token, so removing it after a failed write of any identity key deletes the
// new launch's correct identity, and the queue holds its first turn as if
// the launch were unknown. A failed write removes nothing and is read back,
// and a key that already holds the launch's value lets the start succeed.
func TestFailedIdentityWriteKeepsTheSeededToken(t *testing.T) {
	for _, failing := range launchIdentityKeys {
		t.Run(failing, func(t *testing.T) {
			script, dir := observePack(t, identityBoxCaps, identityBoxOps())
			p := NewProvider(script)
			sp := seamBacked(p)
			writeFile(t, filepath.Join(dir, "fail-set."+failing), "")
			if err := sp.Start(t.Context(), "s", freshIdentityConfig("tok-fresh", 1, 1)); err != nil {
				t.Fatalf("Start: %v", err)
			}
			if !slices.Contains(opsRan(t, dir), "set-meta") {
				t.Fatal("no set-meta ran: the fixture does not model the failed write")
			}
			if id, ok := runtime.ReadLaunchID(sp, "s"); !ok || id != runtime.LaunchID("tok-fresh") {
				t.Fatalf("ReadLaunchID after a failed %s write = %q, %v; want the new launch", failing, id, ok)
			}
			if got := observedToken(t, p, "s"); got != "tok-fresh" {
				t.Fatalf("observe token after a failed %s write = %q, want tok-fresh", failing, got)
			}
		})
	}
}

// Kills identity writes bound to the start's remaining budget: a start that
// spent its ctx on the launch would drop the launch's identity, and the
// queue would hold its first turn. Each write runs under the provider's own
// op timeout, past the start's ctx.
func TestLaunchIdentityOutlivesTheStartContext(t *testing.T) {
	script, dir := observePack(t, launchPackCaps, identityPackOps())
	p := NewProvider(script)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	cfg := identityConfig("tok-late", 2, 3)
	if err := p.publishLaunchIdentity(ctx, "s", cfg); err != nil {
		t.Fatalf("publishLaunchIdentity under a spent ctx: %v", err)
	}
	for _, key := range launchIdentityKeys {
		if got := readLog(t, filepath.Join(dir, "meta."+key)); got != cfg.Env[key] {
			t.Errorf("%s = %q after a publish under a spent ctx, want %q", key, got, cfg.Env[key])
		}
	}
}

// Kills identity writes that each get an op timeout of their own: a store
// that answers slowly, or hangs, held the start up to one op timeout per key
// past its ctx, beyond the reconciler's reserve for a start. The writes, and
// the read-back of a failed one, share one op timeout, and a publish cut off
// by it fails.
func TestLaunchIdentityWritesShareOneOpTimeout(t *testing.T) {
	ops := identityPackOps()
	ops["set-meta"] = `sleep 0.4; ` + ops["set-meta"]
	script, _ := observePack(t, launchPackCaps, ops)
	p := NewProvider(script)
	p.timeout = time.Second
	if err := p.publishLaunchIdentity(t.Context(), "s", identityConfig("tok-slow", 2, 3)); err == nil {
		t.Fatal("three 0.4s writes published within a 1s op timeout: each write ran under a timeout of its own")
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

// Kills a non-fresh separable start that reads a provision refused because
// the name is taken as a plain provider error: Start returns a
// session-exists, and runs neither launch nor the teardown stop, which would
// end the live session that owns the name.
func TestProvisionCollisionIsSessionExists(t *testing.T) {
	for _, phrase := range []string{`session "s" already exists`, `session "s" already running`} {
		ops := launchPackOps()
		ops["provision"] = `cat >/dev/null; echo '` + phrase + `' >&2; exit 1`
		script, dir := observePack(t, launchPackCaps, ops)
		err := NewSeamBacked(script).Start(t.Context(), "s", runtime.Config{Command: "agent"})
		if !errors.Is(err, runtime.ErrSessionExists) {
			t.Errorf("%s: Start = %v, want ErrSessionExists", phrase, err)
		}
		if got, want := opsRan(t, dir), []string{"provision"}; !slices.Equal(got, want) {
			t.Errorf("%s: ops = %v, want %v: no launch and no stop", phrase, got, want)
		}
	}
}
