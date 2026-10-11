//go:build integration

package beads_test

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/test/toolhome"
)

// TestBdStoreClaimOverHTTPAgainstRealBdServe is the claim contract the hook's
// class route decides on (DESIGN C5 G8), against the bd go.mod pins
// (//tools/bd under Bazel) rather than a copied stderr: a scope attached to a
// real bd serve with `bd connect`, claimed through `bd update <id> --claim
// --json`. An absent id must read as ErrNotFound, so the hook escalates to its
// graph store, and a present one must claim.
func TestBdStoreClaimOverHTTPAgainstRealBdServe(t *testing.T) {
	bdPath, err := exec.LookPath("bd")
	if err != nil {
		t.Skip("bd not on PATH")
	}
	if _, err := exec.LookPath("dolt"); err != nil {
		t.Skip("dolt not on PATH: bd serve needs a server-backed workspace")
	}

	home := filepath.Join(beadstest.GuardedTempDir(t), "tool-home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("create bd tool home: %v", err)
	}
	serverDir := newIntegrationGitScope(t)
	// bd serve refuses an embedded workspace. The proxied one it serves starts
	// its own Dolt child in serverDir, which BEADS_TEST_MODE=1 would resolve to
	// bd's 127.0.0.1:1 sentinel instead; `bd dolt stop` reaps it.
	serverEnv := httpClaimScopeEnv(home, serverDir, map[string]string{
		beadstest.EnvBeadsTestMode: "0",
		"BEADS_DOLT_AUTO_START":    "1",
	})
	serverRunner := beads.ExecCommandRunnerWithExactEnvContext(context.Background(), serverEnv)
	if out, err := serverRunner(serverDir, "bd", "serve", "--help"); err != nil {
		t.Skipf("this bd has no serve command (a bd without the http backend; //tools/bd has it): %v\n%s", err, out)
	}
	if out, err := serverRunner(serverDir, "bd", "init", "-p", "tst", "--skip-hooks", "--skip-agents", "--proxied-server"); err != nil {
		t.Fatalf("bd init --proxied-server: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		if out, err := serverRunner(serverDir, "bd", "dolt", "stop"); err != nil {
			t.Logf("bd dolt stop: %v\n%s", err, out)
		}
	})
	present, err := beads.NewBdStore(serverDir, serverRunner).Create(beads.Bead{Title: "claim me over http", Type: "task"})
	if err != nil {
		t.Fatalf("Create on the served scope: %v", err)
	}

	url := startBdServe(t, bdPath, serverDir, serverEnv)

	clientDir := newIntegrationGitScope(t)
	clientRunner := beads.ExecCommandRunnerWithExactEnvContext(context.Background(),
		httpClaimScopeEnv(home, clientDir, map[string]string{"BEADS_ACTOR": "worker-1"}))
	if out, err := clientRunner(clientDir, "bd", "connect", url); err != nil {
		t.Fatalf("bd connect %s: %v\n%s", url, err, out)
	}
	client := beads.NewBdStore(clientDir, clientRunner)

	t.Run("an absent id is ErrNotFound", func(t *testing.T) {
		_, ok, err := client.Claim("tst-absent9")
		if ok || !errors.Is(err, beads.ErrNotFound) {
			t.Fatalf("Claim(absent) = ok %v, err %v; want ErrNotFound so the hook escalates to the graph store", ok, err)
		}
		if errors.Is(err, beads.ErrWispNotClaimable) {
			t.Fatalf("an absent id must not read as a wisp refusal: %v", err)
		}
	})

	t.Run("a present id claims", func(t *testing.T) {
		claimed, ok, err := client.Claim(present.ID)
		if err != nil || !ok {
			t.Fatalf("Claim(%s) = ok %v, err %v; want the claim", present.ID, ok, err)
		}
		if claimed.ID != present.ID || claimed.Assignee != "worker-1" || claimed.Status != "in_progress" {
			t.Fatalf("Claim(%s) = %+v, want it in_progress for worker-1", present.ID, claimed)
		}
	})
}

// httpClaimScopeEnv is a re-homed bd environment pinned to one scope's
// .beads (see newConditionalIntegrationRunner), plus overrides.
func httpClaimScopeEnv(home, scopeDir string, overrides map[string]string) map[string]string {
	env := map[string]string{}
	for _, kv := range toolhome.Environ(os.Environ(), home) {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	env["BEADS_DIR"] = filepath.Join(scopeDir, ".beads")
	for k, v := range overrides {
		env[k] = v
	}
	return beadstest.BdSubprocessEnv(env)
}

// startBdServe runs bd serve on an ephemeral loopback port over scopeDir and
// returns the URL it announces on stdout. It runs through the production
// runner, which hands back stdout only once the command exits, so sh points bd
// serve's stdout at a FIFO the test reads the announcement from; the reader
// drains it until bd serve exits, so a later write never meets a closed pipe.
// The runner bounds the server at its 120s command budget, far beyond the two
// claims it serves; cleanup cancels it sooner, killing its process group.
func startBdServe(t *testing.T, bdPath, scopeDir string, env map[string]string) string {
	t.Helper()
	stdout := filepath.Join(beadstest.GuardedTempDir(t), "bd-serve.stdout")
	if err := syscall.Mkfifo(stdout, 0o600); err != nil {
		t.Fatalf("Mkfifo(%q) = %v", stdout, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	run := beads.ExecCommandRunnerWithExactEnvContext(ctx, env)
	exited := make(chan error, 1)
	go func() {
		_, err := run(scopeDir, "sh", "-c", `exec "$0" serve --addr 127.0.0.1:0 >"$1"`, bdPath, stdout)
		exited <- err
	}()
	// releaseReader unblocks a reader still waiting to open the FIFO because
	// sh never opened it; ENXIO means no reader is waiting.
	releaseReader := func() {
		if f, err := os.OpenFile(stdout, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = f.Close()
		}
	}
	t.Cleanup(func() {
		cancel()
		<-exited
		releaseReader()
	})

	announced := make(chan string, 1)
	go func() {
		// Opening the read end blocks until sh opens the write end.
		f, err := os.Open(stdout)
		if err != nil {
			return
		}
		defer f.Close() //nolint:errcheck // read-only FIFO
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if url, ok := strings.CutPrefix(sc.Text(), "bd serve: listening on "); ok {
				select {
				case announced <- url:
				default:
				}
			}
		}
	}()
	select {
	case url := <-announced:
		return url
	case err := <-exited:
		exited <- err
		t.Fatalf("bd serve exited before announcing its address: %v", err)
	case <-time.After(60 * time.Second):
		t.Fatal("bd serve announced no address within 60s")
	}
	return ""
}
