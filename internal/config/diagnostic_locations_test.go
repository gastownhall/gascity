package config

import "testing"

func TestDiagnosticLocatorLineForRigPathKeepsHashInsideQuotedName(t *testing.T) {
	locator := NewDiagnosticLocator([]byte(`
[workspace]
name = "city"

[[rigs]]
name = "rig#one"
path = "../rig-one"
`))

	if got := locator.LineForRigPath("rig#one"); got != 7 {
		t.Fatalf("LineForRigPath = %d, want 7", got)
	}
}
