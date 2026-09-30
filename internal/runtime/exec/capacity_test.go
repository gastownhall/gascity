package exec //nolint:revive // internal package, always imported with alias

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Exit 75 is a capacity refusal only from the start op, where it means "the
// endpoint I launch against refused". On any other op it is an ordinary
// failure, and exit 2 keeps its forward-compatible no-op meaning.
func TestClassifyExecExit_TempFailOnlyForStartOp(t *testing.T) {
	base := errors.New("exec provider /pack start worker: broker at capacity")

	err := classifyExecExit("start", runtime.ExitCodeTempFail, "broker at capacity", base)
	if !runtime.IsProviderCapacity(err) {
		t.Fatalf("start+75 = %v, want capacity", err)
	}
	var capErr *runtime.CapacityError
	if !errors.As(err, &capErr) || capErr.ExitCode != runtime.ExitCodeTempFail || capErr.Source != runtime.CapacitySourceExitStatus {
		t.Fatalf("start+75 CapacityError = %+v, want ExitCode 75 from exit_status", capErr)
	}
	if err.Error() != base.Error() || !errors.Is(err, base) {
		t.Fatalf("start+75 = %q, want the formatted adapter error unchanged", err)
	}

	if err := classifyExecExit("stop", runtime.ExitCodeTempFail, "busy", base); err == nil || runtime.IsProviderCapacity(err) {
		t.Fatalf("stop+75 = %v, want a plain error", err)
	}
	if err := classifyExecExit("start", 1, "broker at capacity", base); runtime.IsProviderCapacity(err) || !errors.Is(err, base) {
		t.Fatalf("start+1 = %v, want the plain adapter error", err)
	}
	if err := classifyExecExit("start", 2, "", base); err != nil {
		t.Fatalf("start+2 = %v, want nil (unknown op stays forward compatible)", err)
	}
	if err := classifyExecExit("start", 1, `session "worker" already exists`, base); !errors.Is(err, runtime.ErrSessionExists) {
		t.Fatalf("start collision = %v, want ErrSessionExists", err)
	}
}

// A start collision wins over exit 75: ErrSessionExists is what stops the
// teardown from destroying the live session that owns the name, and a capacity
// classification would let that teardown run.
func TestClassifyExecExit_CollisionBeatsTempFail(t *testing.T) {
	base := errors.New(`exec provider /pack start x: session "x" already exists`)

	err := classifyExecExit("start", runtime.ExitCodeTempFail, `session "x" already exists`, base)
	if !errors.Is(err, runtime.ErrSessionExists) {
		t.Fatalf("collision+75 = %v, want ErrSessionExists", err)
	}
	if runtime.IsProviderCapacity(err) {
		t.Fatalf("collision+75 = %v, want not capacity", err)
	}
}

// A refused start usually leaves the box the adapter provisioned before it
// hit the refusal. Capacity must not skip the teardown that failed starts get.
func TestStart_Exit75TearsDownBoxAndReturnsCapacity(t *testing.T) {
	dir := t.TempDir()
	createFile := filepath.Join(dir, "create.log")
	stopFile := filepath.Join(dir, "stop.log")
	script := writeScript(t, dir, startExitScript(createFile, stopFile, "broker pool full", runtime.ExitCodeTempFail))
	p := NewProvider(script)

	err := p.Start(context.Background(), "test-sess", runtime.Config{})
	if !runtime.IsProviderCapacity(err) {
		t.Fatalf("Start error = %v, want a capacity refusal", err)
	}
	if !strings.Contains(err.Error(), "broker pool full") {
		t.Fatalf("Start error = %v, want the adapter's stderr", err)
	}
	if got := readLog(t, stopFile); !strings.Contains(got, "stop test-sess") {
		t.Fatalf("stop log = %q, want the box torn down after a capacity refusal", got)
	}
}
