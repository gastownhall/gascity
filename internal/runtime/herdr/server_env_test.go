package herdr

import (
	"net"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// The shared herdr daemon outlives the gc process that starts it, and every
// pane it later spawns inherits the daemon's environment underneath that
// pane's own --env flags. A daemon started from one agent's shell therefore
// stamps that agent's identity (GC_ALIAS, GC_INSTANCE_TOKEN, ...) onto panes
// of every other session — ga-bon9pea7, upstream #8934.

// startServerCapturingEnv runs c.startServer against a fake herdr whose
// `server` verb records its environment, and returns that environment as a
// key->value map. A goroutine plays the daemon's part of the contract: it
// binds the session socket once the fake has been launched, so startServer
// sees a live server and returns.
func startServerCapturingEnv(t *testing.T, c *client) map[string]string {
	t.Helper()
	if goruntime.GOOS == "windows" {
		t.Skip("fake herdr CLI is a POSIX shell script")
	}
	dir := t.TempDir()
	dump := filepath.Join(dir, "server.env")
	c.bin = filepath.Join(dir, "herdr")
	script := `#!/bin/sh
# herdr --session <name> server: record the environment this process was given.
env > '` + dump + `.tmp' && mv '` + dump + `.tmp' '` + dump + `'
`
	if err := os.WriteFile(c.bin, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake herdr: %v", err)
	}

	sock := c.socketPath()
	if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Millisecond):
			}
			if _, err := os.Stat(dump); err != nil {
				continue
			}
			addr, err := net.ResolveUnixAddr("unix", sock)
			if err != nil {
				return
			}
			ln, err := net.ListenUnix("unix", addr)
			if err != nil {
				return
			}
			go func() {
				for {
					conn, err := ln.Accept()
					if err != nil {
						return
					}
					_ = conn.Close()
				}
			}()
			<-stop
			_ = ln.Close()
			return
		}
	}()
	t.Cleanup(func() {
		close(stop)
		wg.Wait()
	})

	if err := c.startServer(); err != nil {
		t.Fatalf("startServer() = %v; want nil", err)
	}
	raw, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("fake herdr server never recorded its environment: %v", err)
	}
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			got[k] = v
		}
	}
	return got
}

// A server started from a process that carries one session's identity must
// not carry it: neither the per-session identity keys, the agent's location
// keys, nor the pane/tab/workspace keys a herdr pane exports.
func TestStartServerDropsStarterSessionIdentity(t *testing.T) {
	shortHome(t)
	leaked := map[string]string{
		// Session identity (execenv.SessionScopedEnvKeys).
		"GC_SESSION_ID":          "starter-session-id",
		"GC_SESSION_NAME":        "starter-session-name",
		"GC_AGENT":               "starter-agent",
		"GC_ALIAS":               "starter/alias",
		"GC_TEMPLATE":            "starter/template",
		"GC_INSTANCE_TOKEN":      "starter-token",
		"GC_RUNTIME_EPOCH":       "7",
		"GC_CONTINUATION_EPOCH":  "3",
		"GC_SESSION_ORIGIN":      "ephemeral",
		"BEADS_ACTOR":            "starter/actor",
		"BEADS_HOLDER_TOKEN":     "starter-holder",
		"CLAUDE_CODE_SESSION_ID": "starter-claude-session",
		// The starter agent's working directory and rig scope.
		"GC_DIR":              "/starter/workdir",
		"GC_RIG":              "starter-rig",
		"GC_RIG_ROOT":         "/starter/rig",
		"GC_BEADS_SCOPE_ROOT": "/starter/scope",
		"BEADS_DIR":           "/starter/rig/.beads",
		// Per-pane keys a herdr pane exports to its own children.
		"HERDR_PANE_ID":      "w9:p9",
		"HERDR_WORKSPACE_ID": "w9",
		"HERDR_TAB_ID":       "w9:t9",
	}
	for k, v := range leaked {
		t.Setenv(k, v)
	}
	t.Setenv("GC_CITY_STARTER_MARKER", "kept") // unrelated GC_* must survive

	c := newClient("envscrub", t.TempDir())
	got := startServerCapturingEnv(t, c)

	for k, v := range leaked {
		if gv, ok := got[k]; ok {
			t.Errorf("server environment carries %s=%q (the starter's %q); want it absent", k, gv, v)
		}
	}
	if got["GC_CITY_STARTER_MARKER"] != "kept" {
		t.Errorf("server environment GC_CITY_STARTER_MARKER = %q; want it kept: only per-session keys are scrubbed", got["GC_CITY_STARTER_MARKER"])
	}
}

// What the daemon needs to run and to find its own socket must survive the
// scrub: PATH (it spawns shells and agents), HOME and XDG_CONFIG_HOME (its
// config and socket directory, the same rule socketPath resolves), locale,
// and herdr's own session/socket settings.
func TestStartServerKeepsWhatTheDaemonNeeds(t *testing.T) {
	shortHome(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", "/usr/bin:/bin:/needed/by/daemon")
	t.Setenv("LANG", "C.UTF-8")
	t.Setenv("HERDR_SOCKET_PATH", "/settings/herdr.sock")
	t.Setenv("HERDR_CONFIG_PATH", "/settings/config.toml")

	c := newClient("envkeep", t.TempDir())
	want := map[string]string{
		"PATH":              os.Getenv("PATH"),
		"HOME":              os.Getenv("HOME"),
		"XDG_CONFIG_HOME":   os.Getenv("XDG_CONFIG_HOME"),
		"LANG":              "C.UTF-8",
		"HERDR_SOCKET_PATH": "/settings/herdr.sock",
		"HERDR_CONFIG_PATH": "/settings/config.toml",
	}
	got := startServerCapturingEnv(t, c)
	for k, v := range want {
		if got[k] != v {
			t.Errorf("server environment %s = %q; want %q kept", k, got[k], v)
		}
	}
}
