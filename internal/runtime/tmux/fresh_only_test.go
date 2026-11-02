package tmux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// heldNameCases are the held-name shapes ensureFreshSession meets after
// createSession returns ErrSessionExists.
var heldNameCases = []struct {
	name         string
	paneRunning  bool
	agentRunning bool
	processNames []string
	legacyCalls  []string
}{
	{"dead pane", false, false, nil, []string{"createSession", "isSessionRunning", "killSession", "createSession"}},
	{"zombie", true, false, []string{"claude"}, []string{"createSession", "isSessionRunning", "isRuntimeRunning", "killSession", "createSession"}},
	{"live agent", true, true, []string{"claude"}, []string{"createSession", "isSessionRunning", "isRuntimeRunning"}},
	{"live without process names", true, false, nil, []string{"createSession", "isSessionRunning"}},
}

// LL6, I24: with FreshOnly a held name, live or dead, is refused and never
// recycled. Kills an unfenced dead-pane or zombie recycle under FreshOnly.
func TestTmuxFreshOnlyNeverRecycles(t *testing.T) {
	for _, tc := range heldNameCases {
		t.Run(tc.name, func(t *testing.T) {
			running := tc.paneRunning
			ops := &fakeStartOps{
				isSessionRunningResult: &running,
				isRuntimeRunningResult: tc.agentRunning,
				createErrs:             []error{ErrSessionExists},
			}
			err := ensureFreshSession(ops, "gc-test", runtime.Config{
				Command:      "claude",
				ProcessNames: tc.processNames,
				FreshOnly:    true,
			})
			if !errors.Is(err, runtime.ErrSessionExists) {
				t.Fatalf("ensureFreshSession = %v, want runtime.ErrSessionExists", err)
			}
			assertCallSequence(t, ops, []string{"createSession"})
		})
	}
}

// Legacy never sets FreshOnly, so its recycle paths still run. Kills a
// FreshOnly gate that fires without the flag.
func TestStartWithoutFreshOnlyUnchanged(t *testing.T) {
	for _, tc := range heldNameCases {
		t.Run(tc.name, func(t *testing.T) {
			running := tc.paneRunning
			ops := &fakeStartOps{
				isSessionRunningResult: &running,
				isRuntimeRunningResult: tc.agentRunning,
				createErrs:             []error{ErrSessionExists},
			}
			_ = ensureFreshSession(ops, "gc-test", runtime.Config{
				Command:      "claude",
				ProcessNames: tc.processNames,
			})
			assertCallSequence(t, ops, tc.legacyCalls)
		})
	}

	t.Run("Start issues no held-name probe", func(t *testing.T) {
		fe := &fakeExecutor{err: errors.New("tmux unavailable")}
		p := &Provider{tm: &Tmux{exec: fe}, cache: NewStateCache(nil, time.Hour), workDirs: map[string]string{}}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if err := p.Start(ctx, "gc-test", runtime.Config{Command: "claude"}); !errors.Is(err, context.Canceled) {
			t.Fatalf("Start = %v, want context.Canceled", err)
		}
		for _, call := range fe.calls {
			if len(call) > 1 && call[1] == "has-session" {
				t.Fatalf("legacy Start issued has-session: %v", fe.calls)
			}
		}
	})
}

// LL6: the held-name check runs before stageStartFiles and runPreStart, so a
// refused start stages no file, runs no pre_start hook and kills nothing.
// Kills moving the check after staging or into ensureFreshSession only.
func TestTmuxFreshOnlyHeldNameCheckBeforeStaging(t *testing.T) {
	for _, tc := range []struct {
		name     string
		probeErr error
		wantErr  error
	}{
		{"held", nil, runtime.ErrSessionExists},
		{"probe fails closed", errors.New("permission denied"), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workDir, srcDir := t.TempDir(), t.TempDir()
			src := filepath.Join(srcDir, "seed.txt")
			if err := os.WriteFile(src, []byte("seed"), 0o600); err != nil {
				t.Fatalf("write seed: %v", err)
			}
			marker := filepath.Join(srcDir, "pre-start-ran")
			fe := &fakeExecutor{err: tc.probeErr}
			p := &Provider{tm: &Tmux{exec: fe}, cache: NewStateCache(nil, time.Hour), workDirs: map[string]string{}}

			err := p.Start(context.Background(), "gc-test", runtime.Config{
				WorkDir:   workDir,
				Command:   "claude",
				CopyFiles: []runtime.CopyEntry{{Src: src, RelDst: "copied.txt"}},
				PreStart:  []string{"touch " + marker},
				FreshOnly: true,
			})
			if err == nil || (tc.wantErr != nil && !errors.Is(err, tc.wantErr)) {
				t.Fatalf("Start = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && errors.Is(err, runtime.ErrSessionExists) {
				t.Fatalf("Start = %v; a failed probe must not read as held", err)
			}
			if _, statErr := os.Stat(filepath.Join(workDir, "copied.txt")); !os.IsNotExist(statErr) {
				t.Errorf("refused start staged a file: %v", statErr)
			}
			if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
				t.Errorf("refused start ran pre_start: %v", statErr)
			}
			if len(fe.calls) != 1 || fe.calls[0][1] != "has-session" {
				t.Fatalf("tmux calls = %v, want exactly one has-session", fe.calls)
			}
			if _, ok := p.workDirs["gc-test"]; ok {
				t.Error("refused start recorded a workDir")
			}
		})
	}
}

// v5 F2 own-runtime row: after a failed FreshOnly Start the token-fenced
// cleanupFailedStart still removes only this attempt's runtime. Kills a
// cleanup that kills a name held under another token.
func TestFreshOnlyCleanupFailedStartOnlyOwnRuntime(t *testing.T) {
	fe := &fakeExecutor{out: "GC_INSTANCE_TOKEN=theirs"}
	p := &Provider{tm: &Tmux{exec: fe}, cache: NewStateCache(nil, time.Hour)}

	p.cleanupFailedStart("gc-test", runtime.Config{
		Env:       map[string]string{"GC_INSTANCE_TOKEN": "mine"},
		FreshOnly: true,
	})
	if len(fe.calls) != 1 || fe.calls[0][1] != "show-environment" {
		t.Fatalf("tmux calls = %v, want only the token read", fe.calls)
	}
}

// Production builds tmux seam-backed (NewSeamBackedWithConfig), whose Start
// goes through Provision. FreshOnly must survive that path. Kills a seam that
// rebuilds the Config and drops the flag.
func TestTmuxSeamBackedStartPassesFreshOnly(t *testing.T) {
	fe := &fakeExecutor{}
	raw := &Provider{tm: &Tmux{exec: fe}, cache: NewStateCache(nil, time.Hour), workDirs: map[string]string{}}
	rt, tp := raw.Seams()
	p := &seamBackedProvider{Provider: raw, seams: runtime.NewProviderFromSeams(rt, tp)}

	err := p.Start(context.Background(), "gc-test", runtime.Config{Command: "claude", FreshOnly: true})
	if !errors.Is(err, runtime.ErrSessionExists) {
		t.Fatalf("Start = %v, want runtime.ErrSessionExists", err)
	}
	if len(fe.calls) != 1 || fe.calls[0][1] != "has-session" {
		t.Fatalf("tmux calls = %v, want exactly one has-session", fe.calls)
	}
}
