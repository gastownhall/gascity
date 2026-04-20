//go:build acceptance_c

package tierc_test

import (
	"os"
	"path/filepath"
	"testing"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

func TestBdCmdSeparatesStdoutAndStderrOnError(t *testing.T) {
	dir := t.TempDir()
	gcHome := filepath.Join(dir, "gc-home")
	runtimeDir := filepath.Join(dir, "runtime")
	if err := os.MkdirAll(gcHome, 0o755); err != nil {
		t.Fatalf("mkdir gc-home: %v", err)
	}
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatalf("mkdir runtime dir: %v", err)
	}

	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	bdPath := filepath.Join(binDir, "bd")
	script := "#!/bin/sh\nprintf 'json payload'\nprintf 'warning text' >&2\nexit 1\n"
	if err := os.WriteFile(bdPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}

	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	env := helpers.NewEnv("", gcHome, runtimeDir).With("PATH", binDir+":"+os.Getenv("PATH"))
	out, err := bdCmd(env, dir, "list", "--json")
	if err == nil {
		t.Fatal("expected bdCmd to return an error")
	}
	if out != "json payload\nwarning text" {
		t.Fatalf("bdCmd should separate stdout and stderr on error\nwant: %q\ngot:  %q", "json payload\nwarning text", out)
	}
}
