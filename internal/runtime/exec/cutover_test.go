package exec

import (
	"path/filepath"
	"testing"
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
