package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/packman"
)

// TestSupervisorHealthIncludesPacksLockSHA256 asserts /health surfaces
// the SHA-256 digest of the first managed city's packs.lock contents.
// Drift checkers compare this against the committed lockfile copy
// instead of shelling into the city directory (ga-qcnpu1).
func TestSupervisorHealthIncludesPacksLockSHA256(t *testing.T) {
	s := newFakeState(t)
	content := []byte("schema = 1\n\n[packs.\"https://example.com/tools.git\"]\nversion = \"1.4.2\"\ncommit = \"aaaa\"\nfetched = \"2026-01-02T03:04:05Z\"\n")
	if err := os.WriteFile(filepath.Join(s.CityPath(), packman.LockfileName), content, 0o644); err != nil {
		t.Fatalf("write packs.lock: %v", err)
	}
	sum := sha256.Sum256(content)
	want := hex.EncodeToString(sum[:])

	sm := newTestSupervisorMux(t, map[string]*fakeState{"test-city": s})

	req := httptest.NewRequest("GET", "/health", nil)
	rec := httptest.NewRecorder()
	sm.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, _ := resp["packs_lock_sha256"].(string); got != want {
		t.Fatalf("packs_lock_sha256 = %q, want %q\nbody: %s", got, want, rec.Body.String())
	}
}

// TestSupervisorHealthOmitsPacksLockSHA256WhenMissing confirms the
// field is omitted (not emitted as an empty string) when the first
// managed city has no packs.lock on disk, matching the omitempty
// semantics used by build_id.
func TestSupervisorHealthOmitsPacksLockSHA256WhenMissing(t *testing.T) {
	s := newFakeState(t)
	sm := newTestSupervisorMux(t, map[string]*fakeState{"test-city": s})

	req := httptest.NewRequest("GET", "/health", nil)
	rec := httptest.NewRecorder()
	sm.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, present := resp["packs_lock_sha256"]; present {
		t.Fatalf("packs_lock_sha256 present despite missing packs.lock; got: %v", resp["packs_lock_sha256"])
	}
}
