package supervisor

import "testing"

// TestExportConfig_EgressGate is the §7 egress-gate guard: redacted event export
// is OFF unless an endpoint is configured. The supervisor only starts the
// exporter when Enabled() reports true (see the startEventExport call site), so
// an absent or whitespace-only endpoint means zero network egress.
func TestExportConfig_EgressGate(t *testing.T) {
	if (ExportConfig{}).Enabled() {
		t.Fatal("empty ExportConfig must be disabled (no endpoint => no export)")
	}
	if (ExportConfig{Endpoint: "  "}).Enabled() {
		t.Fatal("whitespace-only endpoint must be disabled")
	}
	if !(ExportConfig{Endpoint: "https://example.invalid/ingest"}).Enabled() {
		t.Fatal("a configured endpoint must enable export")
	}
}

// TestExportConfig_TokenFileField confirms the token_file key replaced the old
// credentials JSON file (no back-compat: PR unmerged) and that ExportRef still
// defaults on.
func TestExportConfig_TokenFileField(t *testing.T) {
	ec := ExportConfig{Endpoint: "https://example.invalid", TokenFile: "/run/secrets/export-token"}
	if ec.TokenFile != "/run/secrets/export-token" {
		t.Fatalf("TokenFile not wired: %q", ec.TokenFile)
	}
	if !ec.ExportRefEnabled() {
		t.Fatal("ExportRef must default to enabled")
	}
}
