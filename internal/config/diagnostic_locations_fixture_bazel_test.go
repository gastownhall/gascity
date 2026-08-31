package config

import (
	_ "embed"
	"testing"
)

// diagnosticLocatorFixture is shared by the Go and Bazel execution paths so
// the bounded target exercises an actual checked-in testdata file.
//
//go:embed testdata/diagnostic_locator.toml
var diagnosticLocatorFixture []byte

func TestDiagnosticLocatorEmbeddedFixtureKeepsHashInsideQuotedName(t *testing.T) {
	locator := NewDiagnosticLocator(diagnosticLocatorFixture)

	if got := locator.LineForRigPath("rig#one"); got != 7 {
		t.Fatalf("LineForRigPath = %d, want 7", got)
	}
}
