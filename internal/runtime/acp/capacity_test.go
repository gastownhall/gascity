package acp

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// An ACP launcher that exits EX_TEMPFAIL before the handshake completes is a
// capacity refusal. A signal-terminated agent (Start's own SIGKILL included)
// or an unreaped one reports ExitCode -1 and says nothing about the endpoint,
// and neither does any other exit status.
func TestACPHandshakeStartError_ProcessExitTempFailIsCapacity(t *testing.T) {
	hsErr := errors.New("connection closed during initialize")

	err := acpHandshakeStartError("worker", hsErr, runtime.ExitCodeTempFail, "broker pool full\n")
	if !runtime.IsProviderCapacity(err) {
		t.Fatalf("exit 75 = %v, want capacity", err)
	}
	var capErr *runtime.CapacityError
	if !errors.As(err, &capErr) || capErr.ExitCode != runtime.ExitCodeTempFail || capErr.Source != runtime.CapacitySourceExitStatus {
		t.Fatalf("CapacityError = %+v, want ExitCode 75 from exit_status", capErr)
	}
	if !errors.Is(err, hsErr) {
		t.Fatalf("exit 75 = %v, want the handshake cause preserved", err)
	}
	for _, want := range []string{`acp handshake for "worker"`, "connection closed during initialize", "agent stderr:\nbroker pool full"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("exit 75 = %q, want substring %q", err, want)
		}
	}

	for name, code := range map[string]int{"signaled or unreaped": -1, "exit 1": 1, "exit 0": 0} {
		err := acpHandshakeStartError("worker", hsErr, code, "")
		if runtime.IsProviderCapacity(err) || !errors.Is(err, hsErr) {
			t.Errorf("%s: error = %v, want the plain handshake error", name, err)
		}
	}
}
