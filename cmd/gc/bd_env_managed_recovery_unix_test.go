//go:build unix

package main

// Managed dolt recovery is cancelled by tearing down a process group, which
// is a POSIX construct. Windows confines a tree in a Job Object instead, and
// managed dolt is gated off there entirely.
import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestNativeDoltOpenEnvForScopeContextCancelsManagedRecovery(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "")
	cityPath := t.TempDir()
	writeManagedBdCityFixture(t, cityPath)

	childPIDFile := filepath.Join(cityPath, "provider.pid")
	script := gcBeadsBdScriptPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	content := `#!/bin/sh
echo $$ > "$GC_TEST_CHILD_PID"
while :; do sleep 1; done
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_TEST_CHILD_PID", childPIDFile)

	ctx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan error, 1)
	go func() {
		_, err := nativeDoltOpenEnvForScopeContext(ctx, cityPath, nil, cityPath)
		resultCh <- err
	}()

	pid := waitForProviderTestChildPID(t, childPIDFile)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	cancel()

	select {
	case err := <-resultCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("native env recovery error = %v, want context canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("native env recovery did not return after parent cancellation")
	}
	waitForProviderTestPIDExit(t, pid, "native env recovery")
}
