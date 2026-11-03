package session

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
