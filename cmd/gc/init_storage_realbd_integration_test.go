//go:build integration

package main

// The split floor's command-tree judgment against real binaries: gc init must
// reach gc bd's own verdict on a split city. The pinned release bd matches its
// inventory; a wrapper script around the same binary does not (gc bd refuses
// one, for want of bd's Go build info); and the judgment, which runs before the
// new city has a .gc, leaves nothing behind.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/bazeltest"
)

func TestInitJudgeSplitBDMatchesGcBdVerdictOnRealBinaries(t *testing.T) {
	bd := bazeltest.DataPath(t, "GC_TEST_PINNED_BD_BIN")
	if bd == "" {
		t.Skip("needs the pinned release bd (GC_TEST_PINNED_BD_BIN, set by the integration build)")
	}
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	inv, err := initJudgeSplitBD(bd)
	if err != nil {
		t.Fatalf("judging the pinned bd %s: %v", bd, err)
	}
	if inv == nil || inv.Name != "bd-1.3.4" {
		t.Fatalf("the pinned bd %s judged as %v, want inventory bd-1.3.4", bd, inv)
	}

	wrapper := filepath.Join(t.TempDir(), "bd")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec "+shellQuotePOSIXPath(bd)+" \"$@\"\n"), 0o755); err != nil { //nolint:gosec // the wrapper must be executable
		t.Fatal(err)
	}
	inv, err = initJudgeSplitBD(wrapper)
	if err != nil {
		t.Fatalf("judging a wrapper around the pinned bd: %v", err)
	}
	if inv != nil {
		t.Fatalf("a wrapper script around the pinned bd was admitted as %s; a split city's gc bd refuses it", inv.Name)
	}

	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the judgment left %d entries in TMPDIR (first %s)", len(entries), entries[0].Name())
	}
}
