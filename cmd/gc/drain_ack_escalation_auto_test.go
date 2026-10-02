package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	sessionauto "github.com/gastownhall/gascity/internal/runtime/auto"
)

// scannerlessDrainAckProvider hides every optional interface of the wrapped
// provider, modeling a default backend that cannot scan the process table.
type scannerlessDrainAckProvider struct {
	runtime.Provider
}

// A city whose default backend cannot scan still routes some sessions to ACP
// through the auto composite, which implements the scanner interface
// unconditionally. The escalation must report the missing capability, not
// "nothing attributable".
func TestDrainAckEscalationReportsNoProcessTableThroughScannerlessAuto(t *testing.T) {
	sp := sessionauto.New(&scannerlessDrainAckProvider{Provider: runtime.NewFake()}, runtime.NewFake())
	var stderr bytes.Buffer
	got := terminateDrainAckRuntimeByProcessTable(t.TempDir(), sp, "sid", "seat", "", 0, time.Now(), &stderr)
	if got != "no_process_table" {
		t.Fatalf("outcome = %q, want no_process_table", got)
	}
	if !strings.Contains(stderr.String(), "cannot scan the process table") {
		t.Fatalf("stderr = %q, want the missing-capability message", stderr.String())
	}
}
