package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// gcBeadsBdScriptForPasswordArgv returns the text of gc-beads-bd.sh.
func gcBeadsBdScriptForPasswordArgv(t *testing.T) string {
	t.Helper()
	path := filepath.Join(repoRootForLint(t), "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// The configured Dolt password must never reach a dolt client's argv, where
// any local user can read it from /proc/<pid>/cmdline.
func TestGcBeadsBdPasswordArgvNoConfiguredPasswordFlag(t *testing.T) {
	t.Parallel()
	script := gcBeadsBdScriptForPasswordArgv(t)
	leak := regexp.MustCompile(`--password\s+"?\$\{?DOLT_PASSWORD`)
	for i, line := range strings.Split(script, "\n") {
		if leak.MatchString(line) {
			t.Errorf("gc-beads-bd.sh:%d passes the configured password on argv: %s", i+1, strings.TrimSpace(line))
		}
	}
}

func TestGcBeadsBdPasswordArgvDoltClient(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available; skipping shell-function test")
	}
	fn := extractShellFunction(t, gcBeadsBdScriptForPasswordArgv(t), "dolt_client")

	tests := []struct {
		name         string
		password     string
		wantFlag     bool
		wantEnvValue string
	}{
		{name: "non-empty password travels only in env", password: "s3cret", wantFlag: false, wantEnvValue: "s3cret"},
		{name: "empty password masks profile with explicit empty flag", password: "", wantFlag: true, wantEnvValue: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			binDir := t.TempDir()
			argvFile := filepath.Join(binDir, "argv")
			envFile := filepath.Join(binDir, "env")
			writeExecutable(t, filepath.Join(binDir, "dolt"),
				"#!/bin/sh\nprintf '%s\\n' \"$*\" > \""+argvFile+"\"\n"+
					"printf '%s' \"${DOLT_CLI_PASSWORD-unset}\" > \""+envFile+"\"\n")
			body := "DOLT_PORT=3307; DOLT_USER=root; DOLT_PASSWORD='" + tc.password + "'\n" +
				fn + "\n" +
				"dolt_client 127.0.0.1 sql -q 'SELECT 1'\n" +
				// The password must not leak into the calling shell.
				"[ -z \"${DOLT_CLI_PASSWORD:-}\" ] || { echo 'DOLT_CLI_PASSWORD persisted in caller' >&2; exit 3; }\n"
			env := append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			env = append(env, "DOLT_CLI_PASSWORD=")
			stdout, stderr, err := runGCBeadsBdCommand(t, env, "sh", "-c", body)
			if err != nil {
				t.Fatalf("dolt_client: %v\nstdout=%s\nstderr=%s", err, stdout, stderr)
			}
			argv, err := os.ReadFile(argvFile)
			if err != nil {
				t.Fatalf("read argv record: %v", err)
			}
			envValue, err := os.ReadFile(envFile)
			if err != nil {
				t.Fatalf("read env record: %v", err)
			}
			if tc.password != "" && strings.Contains(string(argv), tc.password) {
				t.Errorf("argv %q contains the password", argv)
			}
			if got := strings.Contains(string(argv), "--password"); got != tc.wantFlag {
				t.Errorf("argv %q: contains --password = %v, want %v", argv, got, tc.wantFlag)
			}
			if !strings.Contains(string(argv), "--host 127.0.0.1 --port 3307 --user root") ||
				!strings.Contains(string(argv), "--no-tls sql -q SELECT 1") {
				t.Errorf("argv %q does not carry the connection flags and command", argv)
			}
			if string(envValue) != tc.wantEnvValue {
				t.Errorf("DOLT_CLI_PASSWORD = %q, want %q", envValue, tc.wantEnvValue)
			}
		})
	}
}
