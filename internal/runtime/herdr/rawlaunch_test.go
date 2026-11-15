package herdr

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// captureRawLaunchWarnings redirects rawLaunchWarningOut for one test.
func captureRawLaunchWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := rawLaunchWarningOut
	rawLaunchWarningOut = &buf
	t.Cleanup(func() { rawLaunchWarningOut = prev })
	return &buf
}

const rawLoopCmd = `while true; do echo tick-marker; sleep 1; done`

// A raw launch that never lands inside the bound is reported: Start still
// returns nil (the reconciler tolerates a slow launch), but the operator gets
// a diagnostic naming the session, the pane and the bound. The diagnostic
// never carries the raw command or the session env, which can hold secrets.
func TestStartReportsUnconfirmedRawLaunch(t *testing.T) {
	out := captureRawLaunchWarnings(t)
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)
	p.launchWait = 400 * time.Millisecond
	if err := os.WriteFile(filepath.Join(state, "launch_unlanded"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := runtime.Config{Command: rawLoopCmd, Env: map[string]string{"API_TOKEN": "s3cr3t-token-value"}}
	if err := p.Start(context.Background(), "gastown__worker", cfg); err != nil {
		t.Fatalf("Start must stay best-effort on an unconfirmed launch: %v", err)
	}
	got := out.String()
	for _, want := range []string{"gastown__worker", "%5", "not confirmed", "400ms"} {
		if !strings.Contains(got, want) {
			t.Errorf("diagnostic missing %q; got %q", want, got)
		}
	}
	for _, leak := range []string{"tick-marker", "while true", "s3cr3t-token-value", "API_TOKEN"} {
		if strings.Contains(got, leak) {
			t.Errorf("diagnostic leaks %q: %q", leak, got)
		}
	}
}

// A launch the predicates confirm produces no diagnostic.
func TestStartConfirmedRawLaunchIsSilent(t *testing.T) {
	out := captureRawLaunchWarnings(t)
	p, _ := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)
	p.launchWait = 400 * time.Millisecond

	if err := p.Start(context.Background(), "gastown__worker", runtime.Config{Command: rawLoopCmd}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := out.String(); got != "" {
		t.Errorf("confirmed launch wrote a diagnostic: %q", got)
	}
}

// A caller canceling mid-wait is not an unconfirmed launch: no diagnostic.
func TestWaitPaneLaunchedCanceledIsSilent(t *testing.T) {
	out := captureRawLaunchWarnings(t)
	p, state := newFakeHerdrProvider(t)
	if err := os.WriteFile(filepath.Join(state, "launch_unlanded"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	p.launchWait = 5 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p.reportRawLaunch(ctx, "gastown__worker", "%5", rawLoopCmd)
	if got := out.String(); got != "" {
		t.Errorf("canceled wait wrote a diagnostic: %q", got)
	}
}
