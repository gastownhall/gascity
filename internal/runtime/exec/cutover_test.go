package exec

import (
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestSeamBackedCapabilitiesParity(t *testing.T) {
	dir := t.TempDir()
	counterFile := filepath.Join(dir, "protocol-calls")
	handshake := `{"version":0,"capabilities":["report-attachment","report-activity","proc.stream","tty.attach"]}`
	script := writeScript(t, dir, protocolScript(handshake, counterFile))

	raw := NewProvider(script)
	want := raw.Capabilities()
	if !want.CanStream || !want.CanAttachTTY {
		t.Fatalf("raw provider must declare stream+tty for this test; got %+v", want)
	}

	seam := NewSeamBacked(script)
	got := seam.Capabilities()
	if got != want {
		t.Fatalf("seam-backed Capabilities = %+v, want parity with raw %+v", got, want)
	}
}

func TestSeamBackedPreservesReconcilerOwnershipCapability(t *testing.T) {
	dir := t.TempDir()
	counterFile := filepath.Join(dir, "protocol-calls")
	handshake := `{"version":0,"capabilities":["staging.reconciler-owned-mergeable-paths"]}`
	script := writeScript(t, dir, protocolScript(handshake, counterFile))

	seam := NewSeamBacked(script)
	capability, ok := seam.(runtime.ReconcilerOwnedMergeablePathProvider)
	if !ok {
		t.Fatal("seam-backed exec provider hides reconciler ownership capability")
	}
	if !capability.SupportsReconcilerOwnedMergeablePaths() {
		t.Fatal("seam-backed exec provider dropped declared reconciler ownership capability")
	}

	legacyScript := writeScript(t, t.TempDir(), allOpsScript())
	legacy := NewSeamBacked(legacyScript).(runtime.ReconcilerOwnedMergeablePathProvider)
	if legacy.SupportsReconcilerOwnedMergeablePaths() {
		t.Fatal("pre-handshake exec provider unexpectedly claimed reconciler ownership capability")
	}
}
