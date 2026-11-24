//go:build !windows

package herdr

import (
	"testing"
	"time"
)

// TestHerdrCallReturnsWhenGrandchildHoldsStdout: a herdr invocation that
// answers and exits while a process it started still holds its stdout must
// not hold the verb until that grandchild exits. Without a WaitDelay,
// cmd.Output() waits for the pipe's EOF, so a context-free verb blocks for as
// long as the grandchild lives (forever, for a daemon). It runs under the
// production bounds: the verb must return within the 2s WaitDelay, well
// before the grandchild's 10s sleep ends.
func TestHerdrCallReturnsWhenGrandchildHoldsStdout(t *testing.T) {
	script := `#!/bin/sh
printf '{"result":{"agent":{"name":"x","pane_id":"%%5"}}}'
sleep 10 &
exit 0
`
	p := New("gc-grandchild-test", t.TempDir(), "/city/root", 0, 0)
	p.c.bin = writeFakeHerdr(t, script)

	done := make(chan struct{})
	start := time.Now()
	go func() {
		defer close(done)
		_ = p.IsRunning("gastown__worker")
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("IsRunning still blocked after 15s: the grandchild's stdout held the call")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("IsRunning returned after %v; want within the 2s WaitDelay (well under 5s)", elapsed)
	}
}
