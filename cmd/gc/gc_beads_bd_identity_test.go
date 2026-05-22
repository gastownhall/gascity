package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestEnsureDoltIdentityErrorMessages exercises the ensure_dolt_identity
// helper from examples/bd/assets/scripts/gc-beads-bd.sh against stub `dolt`
// and `git` binaries on PATH. The bug being guarded against: when a user
// has set ONLY `dolt config --global --add user.name`, the previous
// implementation reported "git user.name not available" and told the user
// to set user.name (which they already had). The corrected helper reports
// the field that is actually missing — user.email.
func TestEnsureDoltIdentityErrorMessages(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping shell-function test")
	}

	root := repoRootForLint(t)
	scriptPath := filepath.Join(root, "examples", "bd", "assets", "scripts", "gc-beads-bd.sh")
	scriptBytes, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	fnSrc := extractShellFunction(t, string(scriptBytes), "ensure_dolt_identity")

	type fakeStore struct {
		name  string
		email string
	}
	type wantOutcome struct {
		exitOK             bool
		mustContain        []string
		mustNotContain     []string
		expectDoltNameSet  string
		expectDoltEmailSet string
	}
	cases := []struct {
		name string
		dolt fakeStore
		git  fakeStore
		want wantOutcome
	}{
		{
			name: "dolt_has_both_returns_ok",
			dolt: fakeStore{name: "Roger", email: "roger@example.com"},
			want: wantOutcome{exitOK: true},
		},
		{
			name: "dolt_only_name_git_empty_reports_email_missing_not_name",
			dolt: fakeStore{name: "Roger"},
			want: wantOutcome{
				exitOK:         false,
				mustContain:    []string{"user.email"},
				mustNotContain: []string{`add user.name "Your Name"`},
			},
		},
		{
			name: "dolt_only_email_git_empty_reports_name_missing_not_email",
			dolt: fakeStore{email: "roger@example.com"},
			want: wantOutcome{
				exitOK:         false,
				mustContain:    []string{"user.name"},
				mustNotContain: []string{`add user.email "you@example.com"`},
			},
		},
		{
			name: "dolt_empty_git_empty_reports_both_missing",
			want: wantOutcome{
				exitOK:      false,
				mustContain: []string{"user.name", "user.email"},
			},
		},
		{
			name: "dolt_empty_git_has_both_backfills_dolt",
			git:  fakeStore{name: "Roger", email: "roger@example.com"},
			want: wantOutcome{
				exitOK:             true,
				expectDoltNameSet:  "Roger",
				expectDoltEmailSet: "roger@example.com",
			},
		},
		{
			name: "dolt_name_git_email_backfills_only_email",
			dolt: fakeStore{name: "Roger"},
			git:  fakeStore{email: "roger@example.com"},
			want: wantOutcome{
				exitOK:             true,
				expectDoltEmailSet: "roger@example.com",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			binDir := t.TempDir()
			writeFakeDolt(t, binDir, tc.dolt.name, tc.dolt.email)
			writeFakeGit(t, binDir, tc.git.name, tc.git.email)

			doltLog := filepath.Join(binDir, "dolt-set.log")
			origPath := os.Getenv("PATH")

			script := fnSrc + "\n" +
				"die() { printf '%s\\n' \"$*\" >&2; exit 1; }\n" +
				"ensure_dolt_identity\n"

			cmd := exec.Command("bash", "-c", script)
			cmd.Env = append(os.Environ(),
				"PATH="+binDir+string(os.PathListSeparator)+origPath,
				"FAKE_DOLT_LOG="+doltLog,
			)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			runErr := cmd.Run()

			if tc.want.exitOK {
				if runErr != nil {
					t.Fatalf("expected success, got %v\nstderr:\n%s", runErr, stderr.String())
				}
			} else {
				if runErr == nil {
					t.Fatalf("expected non-zero exit, got success\nstderr:\n%s", stderr.String())
				}
			}
			out := stderr.String()
			for _, frag := range tc.want.mustContain {
				if !strings.Contains(out, frag) {
					t.Errorf("stderr missing %q:\n%s", frag, out)
				}
			}
			for _, frag := range tc.want.mustNotContain {
				if strings.Contains(out, frag) {
					t.Errorf("stderr should not contain %q (it is misleading guidance):\n%s", frag, out)
				}
			}
			if tc.want.expectDoltNameSet != "" {
				if !logContains(doltLog, "set user.name "+tc.want.expectDoltNameSet) {
					t.Errorf("expected dolt user.name to be set to %q; log:\n%s",
						tc.want.expectDoltNameSet, readFile(doltLog))
				}
			}
			if tc.want.expectDoltEmailSet != "" {
				if !logContains(doltLog, "set user.email "+tc.want.expectDoltEmailSet) {
					t.Errorf("expected dolt user.email to be set to %q; log:\n%s",
						tc.want.expectDoltEmailSet, readFile(doltLog))
				}
			}
		})
	}
}

func extractShellFunction(t *testing.T, script, name string) string {
	t.Helper()
	// Match the function header and capture lines until the matching
	// closing brace at column 0. The script uses the conventional
	// `name() {` ... `\n}` shape.
	pattern := regexp.MustCompile(`(?ms)^` + regexp.QuoteMeta(name) + `\(\)\s*\{.*?\n\}`)
	loc := pattern.FindStringIndex(script)
	if loc == nil {
		t.Fatalf("could not find shell function %q in script", name)
	}
	return script[loc[0]:loc[1]]
}

func writeFakeDolt(t *testing.T, dir, name, email string) {
	t.Helper()
	body := `#!/usr/bin/env bash
# Stub: only handles "config --global --get|--add user.name|user.email".
set -e
log_file=${FAKE_DOLT_LOG:-/dev/null}
case "$1 $2" in
  "config --global")
    case "$3" in
      --get)
        case "$4" in
          user.name)
` + emitGetIf(name) + `
            ;;
          user.email)
` + emitGetIf(email) + `
            ;;
        esac
        ;;
      --add)
        echo "set $4 $5" >> "$log_file"
        exit 0
        ;;
    esac
    ;;
esac
exit 0
`
	writeExecutable(t, filepath.Join(dir, "dolt"), body)
}

func writeFakeGit(t *testing.T, dir, name, email string) {
	t.Helper()
	body := `#!/usr/bin/env bash
set -e
case "$1 $2" in
  "config --global")
    case "$3" in
      user.name)
` + emitGetIf(name) + `
        ;;
      user.email)
` + emitGetIf(email) + `
        ;;
    esac
    ;;
esac
exit 0
`
	writeExecutable(t, filepath.Join(dir, "git"), body)
}

func emitGetIf(value string) string {
	if value == "" {
		return "            exit 1"
	}
	return "            echo " + value + "; exit 0"
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func logContains(path, want string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), want)
}

func readFile(path string) string {
	data, _ := os.ReadFile(path)
	return string(data)
}
