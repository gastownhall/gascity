package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// TestCmdSessionRename_RefusesBlankTitle verifies that `gc session rename`
// refuses a blank or whitespace-only title as a usage error before any store
// write. Without the front-door check the title reached the bead store: bd
// answered with a storage validation error ("title is required"), and stores
// that do not validate silently blanked the session title.
func TestCmdSessionRename_RefusesBlankTitle(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := shortSocketTempDir(t, "gc-session-rename-blank-")
	t.Setenv("GC_CITY", cityDir)
	writeGenericNamedSessionCityTOML(t, cityDir)
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	bead, err := store.Create(beads.Bead{
		Title:  "original title",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession, "template:worker"},
		Metadata: map[string]string{
			"alias":        "sky",
			"template":     "worker",
			"session_name": "s-gc-rename-blank-test",
			"state":        "asleep",
		},
	})
	if err != nil {
		t.Fatalf("store.Create(session bead): %v", err)
	}

	for _, blank := range []string{"", "   ", "\t\n"} {
		var stdout, stderr bytes.Buffer
		if code := cmdSessionRename([]string{bead.ID, blank}, &stdout, &stderr); code != 1 {
			t.Fatalf("cmdSessionRename(%q) = %d, want 1; stdout=%q stderr=%q", blank, code, stdout.String(), stderr.String())
		}
		if got := stderr.String(); !strings.HasPrefix(got, "gc session rename: ") || !strings.Contains(got, "title cannot be empty") {
			t.Fatalf("cmdSessionRename(%q) stderr = %q, want the blank-title usage error", blank, got)
		}
		if got := stdout.String(); got != "" {
			t.Fatalf("cmdSessionRename(%q) stdout = %q, want empty", blank, got)
		}
	}

	reloaded, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt(reload): %v", err)
	}
	got, err := reloaded.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get(session bead): %v", err)
	}
	if got.Title != "original title" {
		t.Fatalf("stored title = %q, want %q untouched", got.Title, "original title")
	}

	// The same harness does reach the store for a real title, so the refusals
	// above are the blank-title check and not an unrelated setup failure.
	var stdout, stderr bytes.Buffer
	if code := cmdSessionRename([]string{bead.ID, "new title"}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionRename(new title) = %d, want 0; stderr=%q", code, stderr.String())
	}
	reloaded, err = openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt(reload): %v", err)
	}
	got, err = reloaded.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get(session bead): %v", err)
	}
	if got.Title != "new title" {
		t.Fatalf("stored title = %q, want %q", got.Title, "new title")
	}
}
