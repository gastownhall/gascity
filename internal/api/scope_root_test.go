package api

import (
	"path/filepath"
	"testing"
)

func TestResolveScopeRoot(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "city")
	if got := resolveScopeRoot(cityPath, "repos/beta"); got != filepath.Join(cityPath, "repos", "beta") {
		t.Fatalf("resolveScopeRoot(relative) = %q", got)
	}
	if got := resolveScopeRoot(cityPath, cityPath); got != cityPath {
		t.Fatalf("resolveScopeRoot(city) = %q, want %q", got, cityPath)
	}
	if got := resolveScopeRoot(cityPath, ""); got != cityPath {
		t.Fatalf("resolveScopeRoot(empty) = %q, want %q", got, cityPath)
	}
}
