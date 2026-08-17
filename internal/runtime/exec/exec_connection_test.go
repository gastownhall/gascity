package exec

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// writeExecOpScript writes a minimal RPP script whose case body is `caseBody`
// (e.g. `exec) sh -c "$(cat)" ;;`). Unlisted ops exit 2.
func writeExecOpScript(t *testing.T, caseBody string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gc-runtime-exec")
	script := "#!/bin/sh\nop=\"$1\"; name=\"$2\"\ncase \"$op\" in\n" + caseBody + "\n  *) exit 2 ;;\nesac\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestProviderExec_RunsCommandAndReturnsOutput(t *testing.T) {
	p := NewProvider(writeExecOpScript(t, `  exec) sh -c "$(cat)" ;;`))
	out, code, err := p.Exec(context.Background(), "s", []string{"echo", "hello world"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if got := string(out); got != "hello world\n" {
		t.Errorf("output = %q, want %q", got, "hello world\n")
	}
}

func TestProviderExec_PropagatesNonZeroExitAsCodeNotError(t *testing.T) {
	p := NewProvider(writeExecOpScript(t, `  exec) sh -c "$(cat)" ;;`))
	_, code, err := p.Exec(context.Background(), "s", []string{"sh", "-c", "exit 7"})
	if err != nil {
		t.Fatalf("Exec returned err for a non-zero command exit; want code only: %v", err)
	}
	if code != 7 {
		t.Errorf("code = %d, want 7 (op exit must mirror command exit)", code)
	}
}

func TestProviderExec_ShellQuotesArguments(t *testing.T) {
	// An argument containing a single quote must survive POSIX quoting.
	p := NewProvider(writeExecOpScript(t, `  exec) sh -c "$(cat)" ;;`))
	out, _, err := p.Exec(context.Background(), "s", []string{"printf", "%s", "a'b"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := string(out); got != "a'b" {
		t.Errorf("output = %q, want %q (single quote must be preserved through shell-quoting)", got, "a'b")
	}
}

func TestProviderExec_UnimplementedReturnsErrExecUnsupported(t *testing.T) {
	// A script with no exec op exits 2 → ErrExecUnsupported (so a carrier can
	// fall back to the legacy driving op).
	p := NewProvider(writeExecOpScript(t, `  start) cat >/dev/null ;;`))
	_, _, err := p.Exec(context.Background(), "s", []string{"echo", "hi"})
	if !errors.Is(err, runtime.ErrExecUnsupported) {
		t.Errorf("err = %v, want runtime.ErrExecUnsupported", err)
	}
}

func TestProviderExec_TimeoutIsTransportError(t *testing.T) {
	// A command killed by the op timeout must surface as an error, not a
	// silent (output, -1) "command result". Guards the cmdCtx.Err() branch.
	p := NewProvider(writeExecOpScript(t, `  exec) sleep 5 ;;`))
	p.timeout = 50 * time.Millisecond
	_, _, err := p.Exec(context.Background(), "s", []string{"true"})
	if err == nil {
		t.Fatal("Exec on a timed-out command returned nil err; want a transport error")
	}
	if errors.Is(err, runtime.ErrExecUnsupported) {
		t.Errorf("timeout misclassified as ErrExecUnsupported: %v", err)
	}
}
