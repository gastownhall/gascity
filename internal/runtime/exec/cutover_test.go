package exec

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestFreshEntryPointsRejectInvalidOwnedStagingBeforeBackendEffects(t *testing.T) {
	for _, entry := range []string{"raw start", "seam start", "raw relaunch", "seam relaunch"} {
		t.Run(entry, func(t *testing.T) {
			// A cached real protocol shape avoids starting an adapter just to
			// discover its capabilities. The absent executable makes an effect
			// attempted before validation distinguishable from contract refusal.
			p := NewProvider(filepath.Join(t.TempDir(), "never-executed"))
			p.handshakeRan = true
			p.handshakeInfo = runtime.ProtocolInfo{Capabilities: []string{
				runtime.ProtocolCapabilityObserveV1, runtime.ProtocolCapabilityReportAttachment,
			}}
			cfg := runtime.Config{
				FreshOnly: true, WorkDir: t.TempDir(),
				ReconcilerOwnedMergeablePaths: []string{".codex/hooks.json"},
			}
			var provider runtime.Provider = p
			if strings.HasPrefix(entry, "seam") {
				provider = seamBacked(p)
			}
			var err error
			if strings.HasSuffix(entry, "relaunch") {
				err = provider.(runtime.RelaunchProvider).Relaunch(context.Background(), "owned", cfg)
			} else {
				err = provider.Start(context.Background(), "owned", cfg)
			}
			if err == nil || !strings.Contains(err.Error(), "requires exactly one copy_file") {
				t.Fatalf("invalid ownership must be refused before an adapter effect: %v", err)
			}
		})
	}
}

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
