package docsync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTutorial01SaysTheBeadStoreIsReadyAfterInit pins the prose a new user
// reads right after `gc init`: the bead store already exists and bd runs its
// own Dolt server, so the bd recovery commands a stray error might suggest are
// never part of the happy path, and gc doctor is where to look instead.
func TestTutorial01SaysTheBeadStoreIsReadyAfterInit(t *testing.T) {
	text := readTutorial01(t)
	const anchor = "Gas City created the city directory"
	start := strings.Index(text, anchor)
	if start < 0 {
		t.Fatalf("tutorial 01 lost the post-init paragraph starting %q", anchor)
	}
	afterInit := text[start:]
	if end := strings.Index(afterInit, "```shell"); end >= 0 {
		afterInit = afterInit[:end]
	}
	for _, want := range []string{
		"`gc init` already created",
		"bead store",
		"starts and stops its own Dolt server",
		"`bd dolt start`",
		"`bd init`",
		"`bd bootstrap`",
		"`gc doctor`",
	} {
		if !strings.Contains(afterInit, want) {
			t.Errorf("prose after the gc init transcript is missing %q:\n%s", want, afterInit)
		}
	}
}

// TestTutorial01NamesTheBdDogBinding keeps the background-agent paragraph on
// the binding `gc status` prints (bd.dog), matching the transcript above it and
// the tutorial 01 acceptance golden.
func TestTutorial01NamesTheBdDogBinding(t *testing.T) {
	text := readTutorial01(t)
	for _, want := range []string{"The `bd.dog` pool", "the `bd.` prefix is the import binding"} {
		if !strings.Contains(text, want) {
			t.Errorf("tutorial 01 is missing %q", want)
		}
	}
	for _, stale := range []string{"`dolt.dog`", "`dolt.` prefix"} {
		if strings.Contains(text, stale) {
			t.Errorf("tutorial 01 still names %s, which gc status no longer prints", stale)
		}
	}
}

func readTutorial01(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(), "docs", "tutorials", "01-cities-and-rigs.md"))
	if err != nil {
		t.Fatalf("reading tutorial 01: %v", err)
	}
	// Prose wraps at arbitrary words, so phrases are matched across line breaks.
	return strings.Join(strings.Fields(string(data)), " ")
}
