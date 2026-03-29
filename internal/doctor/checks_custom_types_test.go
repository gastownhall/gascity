package doctor

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCustomTypesCheck_NoBeadsDir(t *testing.T) {
	dir := t.TempDir()
	c := NewCustomTypesCheck(dir, "test")
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK (no .beads dir)", r.Status)
	}
}

func TestCustomTypesCheck_MissingTypes(t *testing.T) {
	dir := t.TempDir()
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	c := NewCustomTypesCheck(dir, "test")
	// This will fail because bd isn't initialized in the temp dir.
	// The check should report a warning (can't read config).
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status == StatusOK {
		t.Fatal("expected non-OK status when bd config fails")
	}
	if !c.CanFix() {
		t.Fatal("CanFix should return true")
	}
}

func TestCustomTypesCheck_RequiredTypesIncludeSpec(t *testing.T) {
	found := false
	for _, typ := range RequiredCustomTypes {
		if typ == "spec" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("RequiredCustomTypes must include 'spec'")
	}
}

func TestCustomTypesCheck_RequiredTypesComplete(t *testing.T) {
	expected := map[string]bool{
		"molecule": true, "convoy": true, "message": true,
		"event": true, "gate": true, "merge-request": true,
		"agent": true, "role": true, "rig": true,
		"session": true, "spec": true,
	}
	for _, typ := range RequiredCustomTypes {
		if !expected[typ] {
			t.Errorf("unexpected required type: %q", typ)
		}
		delete(expected, typ)
	}
	for typ := range expected {
		t.Errorf("missing required type: %q", typ)
	}
}
