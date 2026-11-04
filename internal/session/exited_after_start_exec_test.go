package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionexec "github.com/gastownhall/gascity/internal/runtime/exec"
)

// Kills (PR-3b review SHOULD 1) "no screen" leaking out of an exec
// provider's Peek into a caller's error: a v2-ready exec pack whose dead box
// has no screen (`peek` exit 66) reads, for the startup death message, as
// an empty pane, as it did before gc read dead boxes' screens; one whose
// dead box shows a screen reports it.
func TestExitedAfterStartOnAnExecPackWithNoScreen(t *testing.T) {
	for _, c := range []struct {
		name, peek, want string
	}{
		{"no screen", `exit 66`, `session "s" exited after it started`},
		{"a dead box's screen", `echo "Error: model_not_found"`, "last pane output:\nError: model_not_found"},
	} {
		t.Run(c.name, func(t *testing.T) {
			script := filepath.Join(t.TempDir(), "pack")
			body := `#!/bin/sh
case "$1" in
  protocol) echo '{"version":0,"capabilities":["observe.v1","report-attachment"]}' ;;
  observe) cat >/dev/null; echo '{"state":"dead","object":{"id":"c","created":"t"},"identity":null}' ;;
  is-running) echo false ;;
  peek) ` + c.peek + ` ;;
  *) exit 2 ;;
esac
`
			if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
			err := (&Manager{sp: sessionexec.NewSeamBacked(script)}).exitedAfterStart("s", nil)
			if !errors.Is(err, runtime.ErrSessionDiedDuringStartup) || !strings.HasSuffix(err.Error(), c.want) || strings.Contains(err.Error(), "reading its last pane output") {
				t.Fatalf("exitedAfterStart = %v, want a startup death ending %q", err, c.want)
			}
		})
	}
}

// Kills (PR-3b adoption review) an exec pack's "no screen" stranding a
// stale-key resume: the resumed agent exits at once (its key names no
// conversation), so its box is dead while the start reads its startup
// dialogs, and every peek answers exit 66. That is a blank screen, not a
// failed start: the start returns, the stale-key check finds the box not
// running, the key is cleared, and the session starts fresh.
func TestStaleKeyResumeOnAnExecPackWithNoScreenStartsFresh(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "pack")
	body := fmt.Sprintf(`#!/bin/sh
dir=%q
echo "$1" >> "$dir/ops"
box=$(cat "$dir/box" 2>/dev/null)
case "$1" in
  protocol) echo '{"version":0,"capabilities":["observe.v1","report-attachment"]}' ;;
  start)
    # A resumed agent finds no conversation under its key and exits.
    case "$(cat)" in
      *--resume*) echo resumed >> "$dir/starts"; echo dead > "$dir/box" ;;
      *) echo fresh >> "$dir/starts"; echo running > "$dir/box" ;;
    esac ;;
  observe)
    cat >/dev/null
    case "$box" in
      running | dead) echo '{"state":"'"$box"'","object":{"id":"c","created":"t"},"identity":null}' ;;
      *) echo '{"state":"absent"}' ;;
    esac ;;
  is-running) if [ "$box" = running ]; then echo true; else echo false; fi ;;
  peek) if [ "$box" = running ]; then echo '>'; else exit 66; fi ;;
  stop) rm -f "$dir/box" ;;
  *) exit 2 ;;
esac
`, dir)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	store := beads.NewMemStore()
	mgr := newTestManager(t, store, sessionexec.NewSeamBacked(script), WithStaleKeyDetectionWaiter(immediateStaleKeyDetectionWaiter))
	hints := runtime.Config{WorkDir: t.TempDir(), ProcessNames: []string{"claude"}}
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "worker", Command: "claude", WorkDir: hints.WorkDir, Provider: "claude", Resume: ProviderResume{ResumeFlag: "--resume", SessionIDFlag: "--session-id"}, Hints: hints, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := mgr.Suspend(context.Background(), testActor(mgr, ActorOperator), info.ID, false); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	ops := func() string {
		data, _ := os.ReadFile(filepath.Join(dir, "ops"))
		return strings.Join(strings.Fields(string(data)), " ")
	}
	if _, err := mgr.Send(context.Background(), testActor(mgr, ActorOperator), info.ID, "hello", "claude --resume "+info.SessionKey, hints); err != nil {
		t.Fatalf("Send over a stale key = %v (ops %s), want the session started fresh", err, ops())
	}
	starts, _ := os.ReadFile(filepath.Join(dir, "starts"))
	if got := strings.Fields(string(starts)); !slices.Equal(got, []string{"fresh", "resumed", "fresh"}) {
		t.Fatalf("starts %v (ops %s), want fresh, resumed, then fresh again", got, ops())
	}
	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if key := b.Metadata["session_key"]; key != "" {
		t.Fatalf("session_key after the fresh start = %q, want it cleared", key)
	}
}
