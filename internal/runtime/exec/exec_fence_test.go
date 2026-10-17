package exec

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

var execFence = runtime.InputFence{Generation: 2, Epoch: 3, Token: "tok"}

// boxScript is a pack declaring the exec connection whose exec op logs each
// in-box command to calls and answers the fence read with read and a guarded
// command with guarded. separable adds proc.provision.
func boxScript(t *testing.T, read, guarded string, separable bool) (script, calls string) {
	t.Helper()
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls")
	caps := `"proc.exec"`
	if separable {
		caps += `,"proc.provision"`
	}
	script = writeScript(t, dir, `
op="$1"
case "$op" in
  protocol) echo '{"version":0,"capabilities":[`+caps+`]}' ;;
  exec)
    cmd=$(cat)
    echo "$cmd" >> `+calls+`
    case "$cmd" in
      *display-message*gc-fence-miss*) echo '`+guarded+`' ;;
      *if-shell*) echo '`+guarded+`' ;;
      *display-message*) echo '`+read+`' ;;
    esac
    ;;
  start|provision|stop) cat > /dev/null ;;
  *) exit 2 ;;
esac
`)
	return script, calls
}

func readCalls(t *testing.T, calls string) []string {
	t.Helper()
	data, err := os.ReadFile(calls)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

func ownRead(f runtime.InputFence, pane string) string {
	return f.Digest() + "." + pane + "|" + pane + "|0"
}

// Kills an exec provider whose fencing does not follow its pack: a pack
// declaring the exec connection is atomic (the tmux carrier guard), and one
// without it cannot fence and refuses before running anything.
func TestExecInputFencingFollowsTheExecConnection(t *testing.T) {
	script, _ := boxScript(t, "", "", false)
	for name, p := range map[string]runtime.Provider{"raw": NewProvider(script), "seam-backed": NewSeamBacked(script)} {
		if got := runtime.InputFencingFor(p, "s"); got != runtime.InputFencingAtomic {
			t.Errorf("%s: fencing = %q, want atomic", name, got)
		}
	}
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	legacy := writeScript(t, dir, `[ "$1" = protocol ] && exit 2
echo "$1" >> `+calls+`
`)
	p := NewSeamBacked(legacy)
	if got := runtime.InputFencingFor(p, "s"); got != runtime.InputFencingNone {
		t.Fatalf("legacy pack fencing = %q, want none", got)
	}
	fp := p.(runtime.FencedInputProvider)
	for op, err := range map[string]error{
		"nudge":     fp.NudgeFenced(context.Background(), "s", runtime.TextContent("hi"), runtime.FencedNudge{Fence: execFence}),
		"keys":      fp.SendKeysFenced(context.Background(), "s", execFence, "Enter"),
		"interrupt": fp.InterruptFenced(context.Background(), "s", execFence),
	} {
		if !errors.Is(err, runtime.ErrInputFenceUnsupported) {
			t.Errorf("legacy %s = %v, want ErrInputFenceUnsupported", op, err)
		}
	}
	if got := readCalls(t, calls); len(got) != 0 {
		t.Fatalf("legacy pack ran %q under a fence", got)
	}
}

// Kills an exec provider that sends fenced input outside the carrier guard,
// or reports a nudge delivered when the agent was respawned as another
// incarnation between the fence read and the send.
func TestExecFencedNudgeRespawnBetweenCheckAndSendIsRefused(t *testing.T) {
	other := runtime.InputFence{Generation: 3, Epoch: 3, Token: "next"}
	script, calls := boxScript(t, ownRead(execFence, "%1"), "gc-fence-miss:"+ownRead(other, "%1"), false)
	fp := NewSeamBacked(script).(runtime.FencedInputProvider)
	err := fp.NudgeFenced(context.Background(), "s", runtime.TextContent("hello"), runtime.FencedNudge{Fence: execFence})
	var miss *runtime.FenceMismatchError
	if !errors.As(err, &miss) || miss.Found != other.Digest() || miss.Partial {
		t.Fatalf("NudgeFenced = %v, want a whole miss finding %s", err, other.Digest())
	}
	for _, c := range readCalls(t, calls) {
		if strings.HasPrefix(c, "'tmux' 'send-keys'") || strings.HasPrefix(c, "'tmux' 'paste-buffer'") {
			t.Errorf("unguarded input %q", c)
		}
		if strings.Contains(c, execFence.Token+"'") {
			t.Errorf("command %q carries the token", c)
		}
	}
}

// Kills a ctx-fenced DismissKnownDialogs that sends a dialog key unguarded on
// a pack that can fence.
func TestExecDismissKnownDialogsFencesItsKeys(t *testing.T) {
	script, calls := boxScript(t, ownRead(execFence, "%1"), "", false)
	p := NewSeamBacked(script).(runtime.DialogProvider)
	ctx := runtime.WithInputFence(context.Background(), execFence)
	// No dialog in a pane that peeks empty: the call reads and returns.
	if err := p.DismissKnownDialogs(ctx, "s", 300*time.Millisecond); err != nil {
		t.Fatalf("fenced DismissKnownDialogs = %v", err)
	}
	other := runtime.WithInputFence(context.Background(), runtime.InputFence{Generation: 9, Epoch: 9, Token: "other"})
	err := p.DismissKnownDialogs(other, "s", 300*time.Millisecond)
	if !errors.Is(err, runtime.ErrFenceMismatch) {
		t.Fatalf("DismissKnownDialogs under another fence = %v, want ErrFenceMismatch", err)
	}
	for _, c := range readCalls(t, calls) {
		if strings.HasPrefix(c, "'tmux' 'send-keys'") {
			t.Errorf("unguarded dialog key %q", c)
		}
	}
}

// Kills a start or relaunch that leaves the new agent pane without its
// incarnation's fence, or a respawn that keeps its predecessor's.
func TestExecStartAndLaunchPublishTheFence(t *testing.T) {
	env := map[string]string{runtime.RuntimeEpochEnv: "2", runtime.ContinuationEpochEnv: "3", runtime.InstanceTokenEnv: "tok"}
	publish := "'set-option' '-p' '-F' '-t' 'main' '@gc_fence' '" + execFence.Digest() + ".#{pane_id}'"

	script, calls := boxScript(t, "", "", false)
	if err := NewProvider(script).Start(context.Background(), "s", runtime.Config{Env: env}); err != nil {
		t.Fatalf("welded Start: %v", err)
	}
	if got := readCalls(t, calls); len(got) != 1 || got[0] != "'tmux' "+publish {
		t.Fatalf("welded Start ran %q, want the fence published", got)
	}

	script, calls = boxScript(t, "", "", true)
	p := NewProvider(script)
	if err := p.launchAgent(context.Background(), "s", runtime.Config{Command: "agent", Env: env}); err != nil {
		t.Fatalf("launchAgent: %v", err)
	}
	got := readCalls(t, calls)
	if len(got) != 2 || !strings.HasSuffix(got[1], "'agent' ';' "+publish) {
		t.Fatalf("launch ran %q, want the launch and the publish in one tmux command list", got)
	}
	_ = os.Remove(calls)
	if err := p.launchAgent(context.Background(), "s", runtime.Config{Command: "agent"}); err != nil {
		t.Fatalf("launchAgent without identity: %v", err)
	}
	got = readCalls(t, calls)
	if len(got) != 2 || !strings.HasSuffix(got[1], "'agent' ';' 'set-option' '-p' '-u' '-t' 'main' '@gc_fence'") {
		t.Fatalf("identity-less launch ran %q, want the fence cleared in the same command list", got)
	}
}
