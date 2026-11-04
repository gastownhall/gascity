package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// cliSessionName reads session beads through the session-class store (S3c,
// site 4).
//
// `gc prime --hook` (and every one-shot that names an agent's session without
// a store in hand) resolves the session name through cliSessionName, which
// opened the city WORK store and looked session beads up there
// (session.ExactMetadataSessionCandidates). On a converged split city every
// session bead is in the binding, so the lookup missed and the hook fell back
// to the legacy name: a tmux session that does not exist.
//
// The split leg is a real converged one-shot city (file work store, SQLite
// binding); the control is the same city with no [storage] section.

func resetCLISessionNameCache(t *testing.T) {
	t.Helper()
	reset := func() {
		cliStoreCache.mu.Lock()
		if cliStoreCache.store != nil {
			_ = closeBeadStoreHandle(cliStoreCache.store)
		}
		cliStoreCache.path = ""
		cliStoreCache.store = nil
		cliStoreCache.mu.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

func TestCLISessionNameFindsTheSessionBeadOnASplitCity(t *testing.T) {
	for _, split := range []bool{false, true} {
		name := "single-store"
		if split {
			name = "split"
		}
		t.Run(name, func(t *testing.T) {
			var cityPath string
			if split {
				cityPath, _ = migratedOneShotCLICity(t)
			} else {
				cityPath = oneShotCLICity(t, "")
			}
			appendCityTOML(t, cityPath, "\n[[agent]]\nname = \"probe\"\nstart_command = \"true\"\n")
			captureCLIStorageStderr(t)
			resetCLISessionNameCache(t)
			cfg, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, nil)
			if err != nil {
				t.Fatal(err)
			}
			work, err := openCityStoreAt(cityPath)
			if err != nil {
				t.Fatalf("opening the city work store: %v", err)
			}
			t.Cleanup(func() { _ = closeBeadStoreHandle(work) })
			if _, err := cliSessionStore(work, cfg, cityPath).Create(beads.Bead{
				Title:  "probe",
				Type:   session.BeadType,
				Labels: []string{session.LabelSession},
				Metadata: map[string]string{
					"session_name": "probe-live",
					"agent_name":   "probe",
					"template":     "probe",
					"state":        "active",
				},
			}); err != nil {
				t.Fatalf("seeding the session bead in the session store: %v", err)
			}

			if got := cliSessionName(cityPath, loadedCityName(cfg, cityPath), "probe", ""); got != "probe-live" {
				t.Fatalf("cliSessionName(probe) = %q, want the session bead's %q", got, "probe-live")
			}
		})
	}
}
