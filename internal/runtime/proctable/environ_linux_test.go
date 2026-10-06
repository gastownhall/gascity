//go:build linux

package proctable

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProcessEnvValueReadsScanRoot(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "4242")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	environ := "GC_SESSION_ID=sid\x00MARKER=first\x00MARKER=/tmp/x.sock\x00"
	if err := os.WriteFile(filepath.Join(dir, "environ"), []byte(environ), 0o644); err != nil {
		t.Fatal(err)
	}
	restore := SetScanRootForTesting(root)
	defer restore()

	got, err := ProcessEnvValue(4242, "MARKER")
	if err != nil || got != "/tmp/x.sock" {
		t.Fatalf("ProcessEnvValue(MARKER) = %q, %v; want last value /tmp/x.sock", got, err)
	}
	if got, err := ProcessEnvValue(4242, "ABSENT"); err != nil || got != "" {
		t.Fatalf("ProcessEnvValue(ABSENT) = %q, %v; want empty", got, err)
	}
	if got, err := ProcessEnvValue(4343, "MARKER"); err != nil || got != "" {
		t.Fatalf("ProcessEnvValue(gone pid) = %q, %v; want empty, nil", got, err)
	}
}

func TestProcessEnvValueRefusesLiveProcUnderTest(t *testing.T) {
	if _, err := ProcessEnvValue(os.Getpid(), "PATH"); err == nil {
		t.Fatal("ProcessEnvValue on the live /proc under go test = nil error, want refusal")
	}
}
