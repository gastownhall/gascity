package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCommittedBeadsHandoffOwnsScopeProjection(t *testing.T) {
	city := t.TempDir()
	beadsDir := filepath.Join(city, ".beads")
	if err := os.Mkdir(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(beadsDir, "ownership-handoff.json")
	write := func(phase, owner string) {
		t.Helper()
		body := []byte(`{"request":{"city_root":"` + city + `","root":"` + city + `"},"phase":"` + phase + `","owner":"` + owner + `"}`)
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("committed", "bd")
	if got, err := committedBeadsHandoffOwnsScope(city); err != nil || !got {
		t.Fatalf("committed projection = %t, %v; want true, nil", got, err)
	}
	write("target_configured", "legacy-gc")
	if _, err := committedBeadsHandoffOwnsScope(city); err == nil {
		t.Fatal("pending projection did not fail closed")
	}
	write("rolled_back", "legacy-gc")
	if got, err := committedBeadsHandoffOwnsScope(city); err != nil || got {
		t.Fatalf("restored rollback projection = %t, %v; want false, nil", got, err)
	}
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := committedBeadsHandoffOwnsScope(city); err == nil {
		t.Fatal("malformed projection did not fail closed")
	}
}
