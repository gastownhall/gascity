package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/materialize"
)

func TestSupportsMCPProviderKindPi(t *testing.T) {
	if !supportsMCPProviderKind(materialize.MCPProviderPi) {
		t.Fatalf("supportsMCPProviderKind(%q) = false, want true", materialize.MCPProviderPi)
	}
}
