package beads

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLocalSidecarSetGetRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local-strings.json")
	s := newLocalSidecar(path)

	if err := s.Set("gc-1", "last_woke_at", "2026-07-14T00:00:00Z"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := s.Get("gc-1", "last_woke_at")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "2026-07-14T00:00:00Z" {
		t.Fatalf("Get = %q, want persisted value", got)
	}
}

func TestLocalSidecarGetUnsetReturnsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local-strings.json")
	s := newLocalSidecar(path)

	got, err := s.Get("gc-1", "never_set")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "" {
		t.Fatalf("Get unset = %q, want empty", got)
	}
}

func TestLocalSidecarSetEmptyClears(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local-strings.json")
	s := newLocalSidecar(path)

	if err := s.Set("gc-1", "k", "v"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.Set("gc-1", "k", ""); err != nil {
		t.Fatalf("Set empty: %v", err)
	}
	got, err := s.Get("gc-1", "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "" {
		t.Fatalf("Get after clear = %q, want empty", got)
	}
}

func TestLocalSidecarMultipleKeysPerBead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local-strings.json")
	s := newLocalSidecar(path)

	if err := s.Set("gc-1", "k1", "v1"); err != nil {
		t.Fatalf("Set k1: %v", err)
	}
	if err := s.Set("gc-1", "k2", "v2"); err != nil {
		t.Fatalf("Set k2: %v", err)
	}
	if got, _ := s.Get("gc-1", "k1"); got != "v1" {
		t.Fatalf("Get k1 = %q, want v1", got)
	}
	if got, _ := s.Get("gc-1", "k2"); got != "v2" {
		t.Fatalf("Get k2 = %q, want v2", got)
	}
}

func TestLocalSidecarPersistsAcrossFreshInstance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local-strings.json")

	first := newLocalSidecar(path)
	if err := first.Set("gc-1", "k", "v"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	second := newLocalSidecar(path)
	got, err := second.Get("gc-1", "k")
	if err != nil {
		t.Fatalf("Get from fresh instance: %v", err)
	}
	if got != "v" {
		t.Fatalf("Get from fresh instance at same path = %q, want v", got)
	}
}

func TestLocalSidecarEmptyPathIsMemoryOnlyNotShared(t *testing.T) {
	first := newLocalSidecar("")
	if err := first.Set("gc-1", "k", "v"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := first.Get("gc-1", "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "v" {
		t.Fatalf("Get from same instance = %q, want v", got)
	}

	second := newLocalSidecar("")
	got2, err := second.Get("gc-1", "k")
	if err != nil {
		t.Fatalf("Get from second instance: %v", err)
	}
	if got2 != "" {
		t.Fatalf("Get from second in-memory-only instance = %q, want empty (no persistence, no shared state)", got2)
	}
}

func TestLocalSidecarDeleteBeadScopesToSingleBead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local-strings.json")
	s := newLocalSidecar(path)

	if err := s.Set("gc-1", "k", "v1"); err != nil {
		t.Fatalf("Set gc-1: %v", err)
	}
	if err := s.Set("gc-2", "k", "v2"); err != nil {
		t.Fatalf("Set gc-2: %v", err)
	}
	if err := s.DeleteBead("gc-1"); err != nil {
		t.Fatalf("DeleteBead: %v", err)
	}

	if got, err := s.Get("gc-1", "k"); err != nil || got != "" {
		t.Fatalf("Get gc-1 after DeleteBead = (%q, %v), want empty, nil", got, err)
	}
	if got, err := s.Get("gc-2", "k"); err != nil || got != "v2" {
		t.Fatalf("Get gc-2 after deleting gc-1 = (%q, %v), want v2, nil (untouched)", got, err)
	}
}

func TestLocalSidecarWritesJSONFileToDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local-strings.json")
	s := newLocalSidecar(path)

	if err := s.Set("gc-1", "k", "v"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var data map[string]map[string]string
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if data["gc-1"]["k"] != "v" {
		t.Fatalf("on-disk data = %+v, want gc-1.k=v", data)
	}
}
