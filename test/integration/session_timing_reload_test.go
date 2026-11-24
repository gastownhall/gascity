//go:build integration

package integration

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
)

// TestIntegrationSessionTimingReloadRequiresRestart owns the real supervisor /
// tmux boundary. The unit suite owns the notice's key and provider matrix.
func TestIntegrationSessionTimingReloadRequiresRestart(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	root := shortTempDir(t)
	gcHome, runtimeDir, env := newIsolatedEnvRoot(t, false)
	env = replaceEnv(env, "GC_SESSION", "tmux")
	env = replaceEnv(env, "GC_BEADS", "file")
	env = replaceEnv(env, "SHELL", "/bin/sh")
	tmuxDir := filepath.Join(runtimeDir, "tmux")
	if err := os.MkdirAll(tmuxDir, 0o700); err != nil {
		t.Fatal(err)
	}
	env = replaceEnv(env, "TMUX_TMPDIR", tmuxDir)
	cityDir := filepath.Join(root, "city")
	socket := "timing-reload"
	donePath := filepath.Join(root, "setup-completed")
	script := filepath.Join(root, "pre-start.sh")
	// Four seconds is scenario input: it fits the new 10s budget and exceeds
	// the boot 2s budget. It is not a sleep used to wait for test completion.
	if err := os.WriteFile(script, []byte(fmt.Sprintf("#!/bin/sh\nsleep 4\nprintf done > %s\n", quote(donePath))), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := filepath.Join(root, "seed.toml")
	config := fmt.Sprintf(`[workspace]
name = "gctest-timing"
[beads]
provider = "file"
[session]
provider = "tmux"
socket = %q
setup_timeout = "2s"
[daemon]
patrol_interval = "1s"
shutdown_timeout = "2s"
[[agent]]
name = "probe"
scope = "city"
start_command = "sleep 3600"
pre_start = [%q]
[[named_session]]
template = "probe"
scope = "city"
mode = "on_demand"
`, socket, script)
	if err := os.WriteFile(seed, []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	gc := func(args ...string) string {
		t.Helper()
		out, err := runGCWithEnv(env, "", args...)
		if err != nil {
			t.Fatalf("gc %v: %v\n%s", args, err, out)
		}
		return out
	}
	t.Cleanup(func() {
		if t.Failed() {
			out, _ := runGCWithEnv(env, "", "--city", cityDir, "trace", "show", "--template", "probe")
			t.Logf("probe trace:\n%s", out)
			log, _ := os.ReadFile(filepath.Join(gcHome, "supervisor.log"))
			t.Logf("isolated supervisor log:\n%s", log)
		}
		_, _ = runGCWithEnv(env, "", "stop", cityDir)
		_, _ = runGCWithEnv(env, "", "supervisor", "stop", "--wait")
		// This socket exists only under this test's TMUX_TMPDIR. Never connect
		// to the default server, including during cleanup of a failed test.
		_, _ = runCommand("", env, testutil.ExecRaceTimeout, "tmux", "-L", socket, "kill-server")
	})
	gc("init", "--skip-provider-readiness", "--no-start", "--file", seed, cityDir)
	gc("start", cityDir)
	gc("--city", cityDir, "trace", "start", "--template", "probe", "--for", "10m")

	path := filepath.Join(cityDir, "city.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `setup_timeout = "2s"`) {
		t.Fatalf("fixture lost its boot timing: %s", data)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(data), `setup_timeout = "2s"`, `setup_timeout = "10s"`, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	reload := gc("--city", cityDir, "reload")
	// Nonfatal so RED still exercises both runtime observations and cleanup.
	for _, part := range []string{"gc reload: warning: pending restart: ", "[session]", "setup_timeout", "2s", "10s", "not active until the supervisor restarts"} {
		if !strings.Contains(reload, part) {
			t.Errorf("reload output lacks %q:\n%s", part, reload)
		}
	}

	logPath := filepath.Join(gcHome, "supervisor.log")
	gc("--city", cityDir, "session", "wake", "probe")
	failure, cursor := waitSessionTimingStart(t, logPath, 0, func(line string) bool {
		return strings.Contains(line, "pre_start[0]") && strings.Contains(line, "context deadline exceeded")
	})
	t.Logf("after reload, boot setup budget still applies: %s", failure)
	if _, err := os.Stat(donePath); !os.IsNotExist(err) {
		t.Fatalf("setup completed under the old 2s budget (stat error %v)", err)
	}
	// Stop retries immediately; only one failure is needed. The same named
	// session is retained for the post-restart observation, below quarantine.
	gc("--city", cityDir, "session", "suspend", "probe")
	gc("supervisor", "stop", "--wait")
	gc("start", cityDir)
	gc("--city", cityDir, "session", "wake", "probe")
	success, _ := waitSessionTimingStart(t, logPath, cursor, func(line string) bool {
		return strings.Contains(line, "outcome=success")
	})
	t.Logf("after supervisor restart, new setup budget applies: %s", success)
	if done, err := os.ReadFile(donePath); err != nil || string(done) != "done" {
		t.Fatalf("pre_start did not complete after restart: %q, %v", done, err)
	}
	for _, field := range strings.Fields(success) {
		if name, ok := strings.CutPrefix(field, "session="); ok {
			if out, err := runCommand("", env, testutil.ExecRaceTimeout, "tmux", "-L", socket, "has-session", "-t", name); err != nil {
				t.Fatalf("successful supervisor start has no runtime on private socket: %v\n%s", err, out)
			}
			return
		}
	}
	t.Fatalf("successful lifecycle record lacks session identity: %s", success)
}

// waitSessionTimingStart observes the terminal lifecycle log at a black-box
// boundary with no completion subscription for failed provider starts. This
// shared waiter owns both observations in the test above; its ticker is bounded
// by context, and timeout diagnostics include the last complete log contents.
func waitSessionTimingStart(t *testing.T, path string, after int64, accept func(string) bool) (string, int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 6*testutil.ExecRaceTimeout)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var last string
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			last = string(data)
			if int64(len(data)) >= after {
				scanner := bufio.NewScanner(strings.NewReader(string(data[after:])))
				for scanner.Scan() {
					line := scanner.Text()
					if strings.Contains(line, "session lifecycle: op=start ") && strings.Contains(line, "template=probe ") && accept(line) {
						return line, int64(len(data))
					}
				}
				if err := scanner.Err(); err != nil {
					t.Fatal(err)
				}
			}
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for supervisor-driven probe start: %v\nlast log:\n%s", ctx.Err(), last)
		case <-ticker.C:
		}
	}
}
