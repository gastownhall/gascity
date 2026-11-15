package exec

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/runtimecontract/reference"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
)

// opLogPrelude makes a script append each op name to $OPLOG, one per line, so
// a test can count the spawns a provider call costs.
func opLogPrelude(logFile string) string {
	return "echo \"$1\" >> '" + logFile + "'\n"
}

// readOpLog returns the ops logged since the last reset, and resets the log.
func readOpLog(t *testing.T, logFile string) []string {
	t.Helper()
	data, err := os.ReadFile(logFile)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.WriteFile(logFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(data))
}

// v0PackFixture is a stateful pack without a `protocol` op (version 0) and
// without `exec`, logging every op it runs. A session is running while
// $STATE/<name>.running exists.
type v0PackFixture struct {
	sp       *seamBackedProvider
	stateDir string
	opLog    string
}

func newV0PackFixture(t *testing.T) v0PackFixture {
	t.Helper()
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	if err := os.Mkdir(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	opLog := filepath.Join(dir, "ops.log")
	script := writeScript(t, dir, opLogPrelude(opLog)+mockProviderScript(stateDir))
	return v0PackFixture{sp: seamBacked(NewProvider(script)), stateDir: stateDir, opLog: opLog}
}

// run marks name running on the pack's backend.
func (f v0PackFixture) run(t *testing.T, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.stateDir, name+".running"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

// goldenPackFixture is the conformance golden reference (v2-ready), with an
// `exec` that runs nothing, so no test drives a host tmux.
func goldenPackFixture(t *testing.T) *seamBackedProvider {
	t.Helper()
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	if err := os.Mkdir(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bodies := reference.Bodies()
	bodies["exec"] = `cat >/dev/null`
	script := filepath.Join(dir, "golden")
	if err := os.WriteFile(script, []byte(reference.Script(stateDir, bodies)), 0o755); err != nil {
		t.Fatal(err)
	}
	sp := seamBacked(NewProvider(script))
	if !sp.raw.v2Ready() {
		t.Fatal("the golden reference is not v2-ready through the exec provider")
	}
	return sp
}

// startGoldenBox creates a box for name through the pack's own start op.
func startGoldenBox(t *testing.T, sp *seamBackedProvider, name string) {
	t.Helper()
	if _, err := sp.raw.run([]byte(`{"command":"sleep 300","process_names":["sleep"]}`), "start", name); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
}

// inputVerbs are the driving calls a caller reads as delivered when they
// return nil.
var inputVerbs = []struct {
	name string
	call func(runtime.Provider, string) error
}{
	{"Nudge", func(sp runtime.Provider, name string) error { return sp.Nudge(name, runtime.TextContent("hello")) }},
	{"SendKeys", func(sp runtime.Provider, name string) error { return sp.SendKeys(name, "Enter") }},
	{"Interrupt", func(sp runtime.Provider, name string) error { return sp.Interrupt(name) }},
	{"ClearScrollback", func(sp runtime.Provider, name string) error { return sp.ClearScrollback(name) }},
}

// TestSeamNudgeMissingSessionNotDelivered pins the exec seam fix: input to a
// session the pack can tell is gone is ErrSessionNotFound, never the nil a
// caller reads as delivered; input to a live session is delivered; and a pack
// that cannot tell is neither certified absent nor refused.
func TestSeamNudgeMissingSessionNotDelivered(t *testing.T) {
	t.Run("v0 pack", func(t *testing.T) {
		f := newV0PackFixture(t)
		f.run(t, "live")
		for _, verb := range inputVerbs {
			if err := verb.call(f.sp, "gone"); !errors.Is(err, runtime.ErrSessionNotFound) {
				t.Errorf("%s to a vanished session = %v, want ErrSessionNotFound", verb.name, err)
			}
			if err := verb.call(f.sp, "live"); err != nil {
				t.Errorf("%s to a live session = %v, want nil", verb.name, err)
			}
		}
	})

	t.Run("v2-ready pack", func(t *testing.T) {
		sp := goldenPackFixture(t)
		startGoldenBox(t, sp, "live")
		for _, verb := range inputVerbs {
			if err := verb.call(sp, "gone"); !errors.Is(err, runtime.ErrSessionNotFound) {
				t.Errorf("%s to a vanished session = %v, want ErrSessionNotFound", verb.name, err)
			}
			if err := verb.call(sp, "live"); err != nil {
				t.Errorf("%s to a live session = %v, want nil", verb.name, err)
			}
		}
	})

	t.Run("v2-ready pack that cannot observe", func(t *testing.T) {
		sp := goldenPackFixture(t)
		startGoldenBox(t, sp, "live")
		t.Setenv("GC_RPP_FAULT", "unavailable")
		for _, verb := range inputVerbs {
			err := verb.call(sp, "live")
			if !errors.Is(err, runtime.ErrRuntimeUnavailable) || errors.Is(err, runtime.ErrSessionNotFound) {
				t.Errorf("%s with observe failing = %v, want ErrRuntimeUnavailable and not ErrSessionNotFound", verb.name, err)
			}
		}
	})

	t.Run("v0 pack that cannot tell", func(t *testing.T) {
		dir := t.TempDir()
		opLog := filepath.Join(dir, "ops.log")
		script := writeScript(t, dir, opLogPrelude(opLog)+`
case "$1" in
  is-running) echo "backend down" >&2; exit 1 ;;
  nudge) cat >/dev/null ;;
  *) exit 2 ;;
esac
`)
		sp := seamBacked(NewProvider(script))
		if err := sp.Nudge("maybe", runtime.TextContent("hello")); err != nil {
			t.Fatalf("Nudge when is-running cannot tell = %v, want nil (proceed)", err)
		}
		if ops := readOpLog(t, opLog); !slices.Contains(ops, "nudge") {
			t.Fatalf("ops = %v, want the nudge op run", ops)
		}
	})
}

// TestExecMissingSessionInputContract runs RUNTIME-CONTRACT-019 against the
// production seam-backed exec provider, over a v0 pack and over the v2-ready
// golden reference.
func TestExecMissingSessionInputContract(t *testing.T) {
	t.Run("v0 pack", func(t *testing.T) {
		runtimetest.RunMissingSessionInputContract(t, func(t *testing.T) runtime.Provider {
			return newV0PackFixture(t).sp
		})
	})
	t.Run("v2-ready golden reference", func(t *testing.T) {
		runtimetest.RunMissingSessionInputContract(t, func(t *testing.T) runtime.Provider {
			return goldenPackFixture(t)
		})
	})
}

// TestSeamNudgeSpawnCount pins the cost of the seam-backed driving verbs and
// pure reads on a warm v0 pack: one presence probe and the op itself, with
// no second is-running and no wasted exec spawn per call.
func TestSeamNudgeSpawnCount(t *testing.T) {
	f := newV0PackFixture(t)
	f.run(t, "live")
	if _, err := f.sp.raw.Protocol(); err != nil {
		t.Fatalf("Protocol: %v", err)
	}
	// The first exec answer is learned and remembered for the provider's life.
	if err := f.sp.Nudge("live", runtime.TextContent("warm")); err != nil {
		t.Fatalf("warm Nudge: %v", err)
	}
	readOpLog(t, f.opLog)

	cases := []struct {
		name string
		call func() error
		want []string
	}{
		{"Nudge", func() error { return f.sp.Nudge("live", runtime.TextContent("hello")) }, []string{"is-running", "nudge"}},
		{"SendKeys", func() error { return f.sp.SendKeys("live", "Enter") }, []string{"is-running", "send-keys"}},
		{"Interrupt", func() error { return f.sp.Interrupt("live") }, []string{"is-running", "interrupt"}},
		{"ClearScrollback", func() error { return f.sp.ClearScrollback("live") }, []string{"is-running", "clear-scrollback"}},
		{"GetLastActivity", func() error { _, err := f.sp.GetLastActivity("live"); return err }, []string{"get-last-activity"}},
		{"IsAttached", func() error { f.sp.IsAttached("live"); return nil }, nil},
	}
	for _, tc := range cases {
		for round := 1; round <= 2; round++ {
			if err := tc.call(); err != nil {
				t.Fatalf("%s round %d: %v", tc.name, round, err)
			}
			if got := readOpLog(t, f.opLog); !slices.Equal(got, tc.want) {
				t.Errorf("%s round %d spawned %v, want %v", tc.name, round, got, tc.want)
			}
		}
	}
}

// TestSeamProcessAliveV2ReadsObserve pins ProcessAlive on a v2-ready pack to
// one `observe`: a live agent is alive, a dead agent or a missing box is not,
// and an unobservable pack reads alive, never a spurious dead.
func TestSeamProcessAliveV2ReadsObserve(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	if err := os.Mkdir(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	opLog := filepath.Join(dir, "ops.log")
	bodies := reference.Bodies()
	bodies["exec"] = `cat >/dev/null`
	script := filepath.Join(dir, "golden")
	content := strings.Replace(reference.Script(stateDir, bodies), "\nop=", "\n"+opLogPrelude(opLog)+"op=", 1)
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	sp := seamBacked(NewProvider(script))
	startGoldenBox(t, sp, "live")
	if !sp.raw.v2Ready() {
		t.Fatal("the golden reference is not v2-ready through the exec provider")
	}
	readOpLog(t, opLog)

	if !sp.ProcessAlive("live", []string{"sleep"}) {
		t.Error("ProcessAlive(live, sleep) = false, want true")
	}
	if got := readOpLog(t, opLog); !slices.Equal(got, []string{"observe"}) {
		t.Errorf("ProcessAlive spawned %v, want [observe]", got)
	}
	if sp.ProcessAlive("live", []string{"claude"}) {
		t.Error("ProcessAlive(live, claude) = true, want false (agent dead)")
	}
	if sp.ProcessAlive("gone", []string{"sleep"}) {
		t.Error("ProcessAlive(gone) = true, want false")
	}
	t.Setenv("GC_RPP_FAULT", "unavailable")
	if !sp.ProcessAlive("live", []string{"claude"}) {
		t.Error("ProcessAlive with observe failing = false, want true (never a spurious dead)")
	}
}

// TestExecUnsupportedMemoOnlyAfterHandshake pins when the provider stops
// spawning `exec` for a pack that does not implement it: only once a
// successful handshake shows the pack does not declare proc.exec. An exit 2
// while the handshake is failing is not remembered, so a proc.exec pack is
// not pinned to the dedicated ops for the provider's life.
func TestExecUnsupportedMemoOnlyAfterHandshake(t *testing.T) {
	t.Run("successful handshake without proc.exec", func(t *testing.T) {
		dir := t.TempDir()
		opLog := filepath.Join(dir, "ops.log")
		script := writeScript(t, dir, opLogPrelude(opLog)+`
case "$1" in
  protocol) echo '{"version":0,"capabilities":["report-activity"]}' ;;
  exec) cat >/dev/null; exit 2 ;;
  *) exit 2 ;;
esac
`)
		p := NewProvider(script)
		for i := 0; i < 3; i++ {
			if _, _, err := p.Exec(context.Background(), "s", []string{"true"}); !errors.Is(err, runtime.ErrExecUnsupported) {
				t.Fatalf("Exec #%d = %v, want ErrExecUnsupported", i+1, err)
			}
		}
		execs := 0
		for _, op := range readOpLog(t, opLog) {
			if op == "exec" {
				execs++
			}
		}
		if execs != 1 {
			t.Errorf("exec op spawned %d times, want 1 (remembered after the first answer)", execs)
		}
	})

	t.Run("failed handshake", func(t *testing.T) {
		now := time.Unix(1_800_000_000, 0)
		old := handshakeNow
		handshakeNow = func() time.Time { return now }
		t.Cleanup(func() { handshakeNow = old })

		dir := t.TempDir()
		opLog := filepath.Join(dir, "ops.log")
		ready := filepath.Join(dir, "ready")
		script := writeScript(t, dir, opLogPrelude(opLog)+`
case "$1" in
  protocol) if [ -f '`+ready+`' ]; then echo '{"version":0,"capabilities":["proc.exec"]}'; else echo "protocol broke" >&2; exit 1; fi ;;
  exec) cat >/dev/null; exit 2 ;;
  *) exit 2 ;;
esac
`)
		p := NewProvider(script)
		for i := 0; i < 2; i++ {
			if _, _, err := p.Exec(context.Background(), "s", []string{"true"}); !errors.Is(err, runtime.ErrExecUnsupported) {
				t.Fatalf("Exec #%d in the failed-handshake window = %v, want ErrExecUnsupported", i+1, err)
			}
		}
		execs := 0
		for _, op := range readOpLog(t, opLog) {
			if op == "exec" {
				execs++
			}
		}
		if execs != 2 {
			t.Errorf("exec op spawned %d times in the failed-handshake window, want 2 (never remembered)", execs)
		}

		// The handshake recovers and declares proc.exec: exit 2 is now the
		// in-box command's own exit code.
		if err := os.WriteFile(ready, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		now = now.Add(handshakeRetryAfter + time.Second)
		_, code, err := p.Exec(context.Background(), "s", []string{"true"})
		if err != nil || code != 2 {
			t.Fatalf("Exec after the handshake recovered = (code %d, %v), want (2, nil)", code, err)
		}
	})
}
