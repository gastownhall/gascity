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

// ListRunning is not complete here: an adapter that exits 2 (unknown
// operation) reads as zero sessions. An error-free listing is therefore no
// proof of absence, and the provider must stay unattested until its listing
// reports that gap.
// Kills: a listing attestation declared while ListRunning still omits live
// sessions.
func TestListRunningIsNotAttested(t *testing.T) {
	for _, sp := range []any{(*Provider)(nil), (*seamBackedProvider)(nil)} {
		if _, ok := sp.(runtime.ListingAttestation); ok {
			t.Errorf("%T declares runtime.ListingAttestation", sp)
		}
	}
}
