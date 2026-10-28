//go:build windows

package herdr

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TestPaneProbeLive pins the Windows pane probe against a real herdr: a bare
// shell pane reads idle, reads busy while a command runs in it, and reads idle
// again once the command has exited. Read the Unix way the pane's shell is not
// an interactive shell by name, so every Windows pane read busy forever: an
// exited agent was never reaped, and every kind launch first waited out
// paneShellReadyWait.
//
// Children the shell starts beside the pane rather than in it, a background
// job and a hidden window, leave it idle. Counted as running, they kept an
// idle prompt busy for as long as they lived (measured). Opt-in live tier: see
// requireLiveHerdr.
func TestPaneProbeLive(t *testing.T) {
	requireLiveHerdr(t)

	p := New("gctest-probe", t.TempDir(), t.TempDir(), 0, 0)
	_ = p.Stop("probe") // clear any leftover from a crashed prior run
	t.Cleanup(func() { _ = p.Stop("probe"); _ = p.TeardownServer() })

	ctx := context.Background()
	if err := p.Start(ctx, "probe", runtime.Config{WorkDir: t.TempDir()}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pane, err := p.GetMeta("probe", metaBoundPane)
	if err != nil || pane == "" {
		t.Fatalf("bound pane after Start = %q, %v; want non-empty", pane, err)
	}

	awaitProbe := func(want paneProbe, within time.Duration) {
		t.Helper()
		deadline := time.Now().Add(within)
		for {
			got, err := p.probePane(ctx, pane)
			if err == nil && got == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("pane %s probed %+v, %v for %s; want %+v", pane, got, err, within, want)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}

	awaitProbe(paneProbe{Exists: true}, 15*time.Second)

	// A background job and a hidden ping, each gone within about eight
	// seconds, so neither outlives the test.
	shellPID, _, err := p.c.processInfo(ctx, pane)
	if err != nil || shellPID == 0 {
		t.Fatalf("pane shell after Start = %d, %v", shellPID, err)
	}
	t.Cleanup(func() { awaitNoChildren(t, shellPID, 30*time.Second) })
	if err := p.c.paneRun(ctx, pane, "$null = Start-Job { Start-Sleep -Seconds 8 }; $null = Start-Process ping.exe -ArgumentList '-n','9','127.0.0.1' -WindowStyle Hidden"); err != nil {
		t.Fatalf("typing background work into the pane: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		children, err := consoleChildren(shellPID)
		if err == nil && len(children) >= 2 {
			for _, child := range children {
				if child.Attached {
					t.Fatalf("%s (PID %d), started beside the pane, shares its console", child.Name, child.PID)
				}
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the pane shell's children never showed the background work: %+v, %v", children, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	for i := 0; i < 5; i++ {
		if got, err := p.probePane(ctx, pane); err != nil || got != (paneProbe{Exists: true}) {
			t.Fatalf("a prompt with only background work probed %+v, %v; want idle", got, err)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// ping runs about five seconds as a child of the pane's shell.
	if err := p.c.paneRun(ctx, pane, "ping.exe -n 6 127.0.0.1"); err != nil {
		t.Fatalf("typing a command into the pane: %v", err)
	}
	awaitProbe(paneProbe{Exists: true, Busy: true}, 10*time.Second)
	awaitProbe(paneProbe{Exists: true}, 20*time.Second)
}

// awaitNoChildren waits for shellPID's children to exit on their own.
func awaitNoChildren(t *testing.T, shellPID int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		children, err := consoleChildren(shellPID)
		if err == nil && len(children) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("children of pane shell %d still running after %s: %+v, %v", shellPID, within, children, err)
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}
