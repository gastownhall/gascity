package dashboardbff

import (
	"net/http"
	"strings"
	"testing"
)

// On a read-only deployment (the public factory floor), a run whose execution
// folder is outside the allowed roots can never diff — the visitor should be
// told the diff is unavailable on this dashboard, not "invalid execution path".
func TestRunDiffOutsideRootsReadOnlyExplains(t *testing.T) {
	cityDir := t.TempDir()
	outside := t.TempDir()
	p := New(Deps{Resolver: mapResolver{"alpha": cityDir}, ReadOnly: true})

	rec := postRunDiff(t, p, "/api/city/alpha/runs/gc-run-1/diff",
		`{"executionPath":{"kind":"known","path":`+jsonString(outside)+`}}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 on the read-only floor", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "read-only dashboard") {
		t.Errorf("body = %s, want a read-only-dashboard explanation", rec.Body.String())
	}
}
