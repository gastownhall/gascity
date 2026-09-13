package k8s

import (
	"context"
	"fmt"
	"testing"

	execerr "k8s.io/client-go/util/exec"
)

// TestProviderExec covers the new ExecProvider adapter directly — in
// particular the exit-code extraction, which the driving-method tests (which
// discard Exec's error) do not exercise.
func TestProviderExec(t *testing.T) {
	t.Run("success returns stdout and code 0", func(t *testing.T) {
		fake := newFakeK8sOps()
		p := newProviderWithOps(fake)
		addRunningPod(fake, "s", "s")
		fake.setExecResult("s", []string{"echo", "hi"}, "hi\n", nil)

		out, code, err := p.Exec(context.Background(), "s", []string{"echo", "hi"})
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if code != 0 {
			t.Errorf("code = %d, want 0", code)
		}
		if string(out) != "hi\n" {
			t.Errorf("out = %q, want %q", out, "hi\n")
		}
	})

	t.Run("non-zero command exit returns the code, not an error", func(t *testing.T) {
		fake := newFakeK8sOps()
		p := newProviderWithOps(fake)
		addRunningPod(fake, "s", "s")
		// k8s remotecommand surfaces a non-zero in-pod exit as a util/exec.ExitError.
		fake.setExecResult("s", []string{"false"}, "", execerr.CodeExitError{Err: fmt.Errorf("command terminated with exit code 7"), Code: 7})

		_, code, err := p.Exec(context.Background(), "s", []string{"false"})
		if err != nil {
			t.Fatalf("a non-zero command exit must not be a transport error: %v", err)
		}
		if code != 7 {
			t.Errorf("code = %d, want 7 (extracted from the ExitError)", code)
		}
	})

	t.Run("transport failure returns code -1 and an error", func(t *testing.T) {
		fake := newFakeK8sOps()
		p := newProviderWithOps(fake)
		addRunningPod(fake, "s", "s")
		fake.setExecResult("s", []string{"echo"}, "", fmt.Errorf("stream closed unexpectedly"))

		_, code, err := p.Exec(context.Background(), "s", []string{"echo"})
		if err == nil {
			t.Fatal("want a transport error")
		}
		if code != -1 {
			t.Errorf("code = %d, want -1 on transport failure", code)
		}
	})

	t.Run("no running pod returns code -1 and an error", func(t *testing.T) {
		fake := newFakeK8sOps()
		p := newProviderWithOps(fake)
		// no pod added

		_, code, err := p.Exec(context.Background(), "missing", []string{"echo"})
		if err == nil {
			t.Fatal("want an error when no running pod exists")
		}
		if code != -1 {
			t.Errorf("code = %d, want -1 when the box is unreachable", code)
		}
	})
}
