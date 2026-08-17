package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestGcBeadsBdDefaultBindHostIsLoopback runs the DOLT_HOST default
// assignment from gc-beads-bd.sh and asserts the managed bind default is
// loopback, with GC_DOLT_HOST still honored as the explicit override.
func TestGcBeadsBdDefaultBindHostIsLoopback(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available; skipping shell test")
	}

	line := extractGcBeadsBdDoltHostDefault(t)

	cases := []struct {
		name string
		env  string // "" = unset
		want string
	}{
		{"unset defaults to loopback", "", "127.0.0.1"},
		{"explicit wildcard preserved", "0.0.0.0", "0.0.0.0"},
		{"explicit remote preserved", "db.example.com", "db.example.com"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			script := line + "\nprintf '%s' \"$DOLT_HOST\"\n"
			cmd := exec.Command("sh", "-c", script)
			cmd.Env = environWithoutGCDoltHost()
			if tc.env != "" {
				cmd.Env = append(cmd.Env, "GC_DOLT_HOST="+tc.env)
			}
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("sh -c failed: %v", err)
			}
			if got := string(out); got != tc.want {
				t.Fatalf("DOLT_HOST = %q, want %q (default line: %s)", got, tc.want, line)
			}
		})
	}
}

// TestGcBeadsBdIsRemoteHostClassification behaviorally pins is_remote from
// gc-beads-bd.sh: local-managed means empty OR 127.0.0.1 (the bind default)
// OR 0.0.0.0 (the explicit wildcard opt-out). Anything else names a remote
// target GC must not manage. Without 127.0.0.1 in the local set, an operator
// could not adopt the loopback default without breaking managed-server
// detection.
func TestGcBeadsBdIsRemoteHostClassification(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available; skipping shell-function test")
	}

	root := repoRootForLint(t)
	scriptPath := filepath.Join(root, "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	scriptBytes, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	fnSrc := extractShellFunction(t, string(scriptBytes), "is_remote")

	cases := []struct {
		name       string
		host       string // "" = unset
		wantRemote bool
	}{
		{"unset is local-managed", "", false},
		{"loopback default is local-managed", "127.0.0.1", false},
		{"wildcard opt-out is local-managed", "0.0.0.0", false},
		{"localhost alias is local-managed", "localhost", false},
		{"ipv6 loopback is local-managed", "::1", false},
		{"hostname is remote", "db.example.com", true},
		{"lan address is remote", "10.0.0.5", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("sh", "-c", fnSrc+"\nis_remote\n")
			cmd.Env = environWithoutGCDoltHost()
			if tc.host != "" {
				cmd.Env = append(cmd.Env, "GC_DOLT_HOST="+tc.host)
			}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			err := cmd.Run()
			gotRemote := err == nil // is_remote returns 0 (sh success) when remote
			var exitErr *exec.ExitError
			if err != nil && !errors.As(err, &exitErr) {
				t.Fatalf("sh -c failed: %v\nstderr:\n%s", err, stderr.String())
			}
			if gotRemote != tc.wantRemote {
				t.Fatalf("is_remote with GC_DOLT_HOST=%q: remote=%v, want %v\nfunction:\n%s",
					tc.host, gotRemote, tc.wantRemote, fnSrc)
			}
		})
	}
}

func extractGcBeadsBdDoltHostDefault(t *testing.T) string {
	t.Helper()
	root := repoRootForLint(t)
	scriptPath := filepath.Join(root, "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	data, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	pattern := regexp.MustCompile(`(?m)^DOLT_HOST=.*$`)
	line := pattern.FindString(string(data))
	if line == "" {
		t.Fatal("could not find DOLT_HOST default assignment in gc-beads-bd.sh")
	}
	return line
}

func environWithoutGCDoltHost() []string {
	env := os.Environ()
	filtered := env[:0]
	for _, kv := range env {
		if strings.HasPrefix(kv, "GC_DOLT_HOST=") {
			continue
		}
		filtered = append(filtered, kv)
	}
	return filtered
}
