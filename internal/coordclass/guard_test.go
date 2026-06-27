package coordclass

import (
	"testing"

	"github.com/gastownhall/gascity/internal/session"
)

// TestContractStringsMatchCanonical pins the contract strings this leaf package
// mirrors against their canonical, importable definitions, so a rename upstream
// fails here instead of silently breaking classification. The cmd/gc-local
// strings (order-tracking, gc:nudge) and the extmsg prefix live in package main
// / internal/extmsg and cannot be imported from a test; they are covered by the
// golden table and their doc-comment citations instead.
func TestContractStringsMatchCanonical(t *testing.T) {
	checks := []struct {
		name      string
		mirrored  string
		canonical string
	}{
		{"session label", labelSession, session.LabelSession},
		{"session type", typeSession, session.BeadType},
		{"wait label", labelWait, session.WaitBeadLabel},
	}
	for _, c := range checks {
		if c.mirrored != c.canonical {
			t.Errorf("%s drift: coordclass has %q, canonical is %q", c.name, c.mirrored, c.canonical)
		}
	}
}
