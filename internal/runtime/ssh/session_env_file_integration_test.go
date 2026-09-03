//go:build integration

package ssh

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestStagingScriptFailsClosedOnAShortWrite drives the real shell down the
// truncation path — the one an unchecked heredoc would have let through — and
// pins both halves of the contract: non-zero exit, and no half-written
// credential left on the box.
func TestStagingScriptFailsClosedOnAShortWrite(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	envBody := "K='v'\nexport K\n"
	script, err := stagingScript(envBody, "")
	if err != nil {
		t.Fatalf("stagingScript: %v", err)
	}
	// Corrupt the expected byte count the way a short write would: the file on
	// disk no longer matches what we asked the box to store.
	short := strings.Replace(script, fmt.Sprintf("-eq %d ]", len(envBody)), "-eq 999 ]", 1)
	if short == script {
		t.Fatalf("test needs updating: no byte-count check found in\n%s", script)
	}

	tmp := t.TempDir()
	cmd := exec.Command("sh")
	cmd.Stdin = strings.NewReader(short)
	cmd.Env = append(os.Environ(), "TMPDIR="+tmp)
	out, err := cmd.Output()
	if err == nil {
		t.Fatal("staging script must exit non-zero when a staged file is short")
	}
	if dir := lastLine(string(out)); strings.HasPrefix(dir, "/") {
		t.Errorf("staging script reported a directory %q despite failing", dir)
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("failed staging left files behind: %v", entries)
	}
}

// TestStagingScriptRunsAgainstARealShell executes the generated script for real
// and checks the artifacts it is supposed to leave behind: a 0700 directory
// holding 0600 files whose bytes match what we asked for.
func TestStagingScriptRunsAgainstARealShell(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	envBody := shellEnvFile(map[string]string{"K": "a'b\"c$d#e\nf"})
	tmuxBody := tmuxCommandLine([]string{"new-session", "-d", "-e", "K=a'b"}) + "\n"
	script, err := stagingScript(envBody, tmuxBody)
	if err != nil {
		t.Fatalf("stagingScript: %v", err)
	}

	tmp := t.TempDir()
	cmd := exec.Command("sh")
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = append(os.Environ(), "TMPDIR="+tmp)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run staging script: %v", err)
	}
	dir := lastLine(string(out))
	if !strings.HasPrefix(dir, tmp) {
		t.Fatalf("staging script reported %q, want a directory under %q", dir, tmp)
	}
	if info, err := os.Stat(dir); err != nil {
		t.Fatalf("stat staged dir: %v", err)
	} else if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("staged dir mode = %04o, want 0700", got)
	}
	for name, want := range map[string]string{envFileName: envBody, tmuxFileName: tmuxBody} {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %04o, want 0600", name, got)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(body) != want {
			t.Errorf("%s =\n%q\nwant\n%q", name, body, want)
		}
	}

	// Sourcing the env file must reproduce the value byte for byte.
	got, err := exec.Command("sh", "-c", ". "+shellQuote([]string{filepath.Join(dir, envFileName)})+`; printf %s "$K"`).Output()
	if err != nil {
		t.Fatalf("source staged env file: %v", err)
	}
	if string(got) != "a'b\"c$d#e\nf" {
		t.Errorf("sourced value = %q", got)
	}
}

// TestStagingScriptSweepsStaleDirectories covers the crash case: a gc killed
// between mktemp and its cleanup leaves a secret-bearing directory on the box
// with nothing to remove it, so the next staging run collects it.
func TestStagingScriptSweepsStaleDirectories(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	if _, err := exec.LookPath("find"); err != nil {
		t.Skip("no find")
	}
	tmp := t.TempDir()
	stale := filepath.Join(tmp, stagedDirPrefix+"orphan")
	if err := os.Mkdir(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, envFileName), []byte("K='secret'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * staleStagedDirMinutes * time.Minute)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	// A same-aged directory that is not ours must survive.
	bystander := filepath.Join(tmp, "someone-elses-dir")
	if err := os.Mkdir(bystander, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(bystander, old, old); err != nil {
		t.Fatal(err)
	}

	script, err := stagingScript("K='v'\nexport K\n", "")
	if err != nil {
		t.Fatalf("stagingScript: %v", err)
	}
	cmd := exec.Command("sh")
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = append(os.Environ(), "TMPDIR="+tmp)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run staging script: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale staged directory survived the sweep: %v", err)
	}
	if _, err := os.Stat(bystander); err != nil {
		t.Errorf("sweep removed an unrelated directory: %v", err)
	}
	// The sweep must not take the run's own fresh directory with it.
	if dir := lastLine(string(out)); dir == "" {
		t.Error("staging script reported no directory")
	} else if _, err := os.Stat(dir); err != nil {
		t.Errorf("sweep removed the directory this run just created: %v", err)
	}
}
