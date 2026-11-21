package configedit_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/configedit"
	"github.com/gastownhall/gascity/internal/fsys"
)

// TestEdit_DoesNotLoseConcurrentEditFromAnotherEditor adopts the #7654 triage
// repro: two Editor transactions racing on one city.toml. The repro's bare
// form — two Editors with no lock between them — is the bug itself (the
// Editor's mutex cannot see another process, so the last writer silently
// discards the other's agent; that loss is inherently racy to demonstrate).
// The adopted form models the supported shape per the approved design: each
// whole transaction is wrapped in the city config lock, the way the CLI and
// the controller now run, and both edits must survive.
func TestEdit_DoesNotLoseConcurrentEditFromAnotherEditor(t *testing.T) {
	dir := t.TempDir()
	tomlPath := writeTOML(t, dir, minimalCity())

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		name := fmt.Sprintf("agent-%d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := configedit.LockCity(fsys.OSFS{}, dir, nil)
			if err != nil {
				t.Errorf("LockCity for the editor transaction adding %s: %v", name, err)
				return
			}
			defer release()
			editor := configedit.NewEditor(fsys.OSFS{}, tomlPath)
			if err := editor.CreateAgent(config.Agent{Name: name, Provider: "claude"}); err != nil {
				t.Errorf("CreateAgent(%s): %v", name, err)
			}
		}()
	}
	wg.Wait()

	cfg := readTOML(t, tomlPath)
	for _, name := range []string{"agent-0", "agent-1"} {
		found := false
		for _, a := range cfg.Agents {
			if a.Name == name {
				found = true
			}
		}
		if !found {
			t.Errorf("agent %q lost: one editor's write discarded the other's (#7654)", name)
		}
	}
}
