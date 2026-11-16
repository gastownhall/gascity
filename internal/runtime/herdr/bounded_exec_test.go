//go:build !windows

package herdr

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

// The bounded-exec tests drive the client against stub herdr scripts under
// millisecond-scale injected bounds. A stub that "hangs" sleeps for hangSecs,
// long enough that an unbounded exec is visibly late and short enough that a
// failing run does not stall the suite.
const (
	hangSecs     = "3"
	testBound    = 200 * time.Millisecond
	testWaitDlay = 100 * time.Millisecond
	testMargin   = 50 * time.Millisecond
	// boundSlack is how far past bound+WaitDelay a correct return may land.
	boundSlack = 700 * time.Millisecond
)

// newBoundedClient builds a client over a stub herdr script with the injected
// bounds.
func newBoundedClient(t *testing.T, script string) *client {
	t.Helper()
	c := newClient("gc-bound-test", t.TempDir())
	c.bin = writeFakeHerdr(t, script)
	c.cliTimeout, c.cliWaitDelay, c.cliMargin = testBound, testWaitDlay, testMargin
	return c
}

// AT-H1: a herdr that writes to stderr and then stalls is killed at the bound,
// and the result is a deadline error, never the stderr text (which here would
// read as "session gone"), with a declared argv secret scrubbed.
func TestBoundedExecKilledAtBoundMapsToDeadline(t *testing.T) {
	c := newBoundedClient(t, "#!/bin/sh\necho 'session not found: hunter2' >&2\nexec sleep "+hangSecs+"\n")
	start := time.Now()
	_, err := c.runWithSecrets(context.Background(), []string{"hunter2"}, "agent", "prompt", "%1", "hunter2")
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v; want it to wrap context.DeadlineExceeded", err)
	}
	if runtime.IsSessionGone(err) {
		t.Fatalf("a timeout reads as session gone: %v", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("declared secret leaked into %q", err)
	}
	if elapsed > testBound+testWaitDlay+boundSlack {
		t.Fatalf("returned after %v; want within bound+WaitDelay", elapsed)
	}
}

// runRaw takes the same path: a stalled `pane read` is cut at the bound.
func TestBoundedExecRawKilledAtBound(t *testing.T) {
	c := newBoundedClient(t, "#!/bin/sh\nexec sleep "+hangSecs+"\n")
	start := time.Now()
	_, err := c.runRaw(context.Background(), "pane", "read", "%1")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v; want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > testBound+testWaitDlay+boundSlack {
		t.Fatalf("returned after %v; want within bound+WaitDelay", elapsed)
	}
}

// AT-H2: a grandchild that keeps stdout open must not hold Output() past
// bound + WaitDelay.
func TestBoundedExecGrandchildHoldingStdoutDoesNotHold(t *testing.T) {
	c := newBoundedClient(t, "#!/bin/sh\nsleep "+hangSecs+" &\nexec sleep "+hangSecs+"\n")
	start := time.Now()
	_, err := c.run(context.Background(), "agent", "list")
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v; want context.DeadlineExceeded", err)
	}
	if elapsed > testBound+testWaitDlay+boundSlack {
		t.Fatalf("a grandchild held Output() for %v; want within bound+WaitDelay", elapsed)
	}
}

// AT-H3: the four verbs that carry their own --timeout run past the (injected)
// default bound, because each is bounded by its own timeout plus the margin.
// Each stub answers after 250ms, past a 100ms default bound.
func TestBoundedExecVerbTimeoutSitesAreNotCutAtDefault(t *testing.T) {
	script := `#!/bin/sh
shift 2
printf '%s\n' "$*" >> "$0.log"
case "$1 $2" in
"agent wait"|"agent start"|"agent prompt") sleep 0.25 ;;
esac
case "$1 $2" in
"agent get") printf '{"result":{"agent":{"name":"x","pane_id":"%%5","agent_status":"idle"}}}' ;;
"agent wait") printf '{"result":{"agent":{"name":"x","pane_id":"%%5","agent_status":"idle"}}}' ;;
"agent start") printf '{"result":{"agent":{"name":"x","pane_id":"%%5"}}}' ;;
"pane read") printf '' ;;
*) printf '{"result":{}}' ;;
esac
`
	for _, tc := range []struct {
		name string
		verb string
		call func(c *client) error
	}{
		{"agent wait", "agent wait", func(c *client) error {
			p := &Provider{c: c}
			_, _, err := p.waitForAgentStatus(context.Background(), "x", time.Minute, "idle")
			return err
		}},
		{"agent start", "agent start", func(c *client) error {
			_, err := c.startAgentKind(context.Background(), "x", "claude", "%5", nil)
			return err
		}},
		{"agent prompt --wait", "agent prompt", func(c *client) error {
			return c.deliverStartupTurn(context.Background(), "%5", "hello", bindModeAgent, nil, "")
		}},
		{"enter-recovery agent wait", "agent wait", func(c *client) error {
			_, err := c.recoverStalledStartup(context.Background(), "%5", errors.New("stalled"), func() (string, error) { return "", nil })
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newBoundedClient(t, script)
			c.cliTimeout = 100 * time.Millisecond
			if err := tc.call(c); err != nil {
				t.Fatalf("a %s answering after 250ms (default bound %v) failed: %v", tc.verb, testBound, err)
			}
			b, _ := os.ReadFile(c.bin + ".log")
			if !strings.Contains(string(b), tc.verb) {
				t.Fatalf("%s never ran:\n%s", tc.verb, b)
			}
		})
	}
}

// AT-H4: a parent cancel returns context.Canceled promptly and is never read as
// a deadline.
func TestBoundedExecParentCancelIsCanceled(t *testing.T) {
	c := newBoundedClient(t, "#!/bin/sh\nexec sleep "+hangSecs+"\n")
	c.cliTimeout = time.Minute // only the parent's cancel can end this call
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	_, err := c.run(ctx, "agent", "list")
	if !errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v; want context.Canceled only", err)
	}
	if elapsed := time.Since(start); elapsed > testWaitDlay+boundSlack {
		t.Fatalf("cancel took %v to return", elapsed)
	}
}

// AT-H9: a hung `session stop` returns within the bound with a wrapped
// deadline error; any other non-zero exit stays nil; and the verb still
// bypasses --session.
func TestStopServerIsBounded(t *testing.T) {
	t.Run("hung", func(t *testing.T) {
		c := newBoundedClient(t, "#!/bin/sh\nexec sleep "+hangSecs+"\n")
		start := time.Now()
		err := c.stopServer()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("stopServer() = %v; want context.DeadlineExceeded", err)
		}
		if elapsed := time.Since(start); elapsed > testBound+testWaitDlay+boundSlack {
			t.Fatalf("stopServer returned after %v; want within the bound", elapsed)
		}
	})
	t.Run("non-zero exit stays nil", func(t *testing.T) {
		c := newBoundedClient(t, "#!/bin/sh\necho 'no such session' >&2\nexit 3\n")
		if err := c.stopServer(); err != nil {
			t.Fatalf("stopServer() = %v; want nil for a non-zero exit", err)
		}
	})
	t.Run("bypasses --session", func(t *testing.T) {
		c := newBoundedClient(t, "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$0.log\"\n")
		if err := c.stopServer(); err != nil {
			t.Fatalf("stopServer() = %v", err)
		}
		b, _ := os.ReadFile(filepath.Clean(c.bin + ".log"))
		if got := strings.TrimSpace(string(b)); got != "session stop gc-bound-test" {
			t.Fatalf("argv = %q; want %q", got, "session stop gc-bound-test")
		}
	})
}
