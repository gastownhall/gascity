package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestStartStandalone_OrphanReleaseCallSite_RetainsLiveAndWakeProtectedWork is
// the production-call-site control for the ONE-SHOT release call in
// doStartStandalone (cmd_start.go) — the second of the two sites, and per the
// acceptance review the more important one: it is the path that runs when the
// controller STARTS (a bounce IS a controller start), and it has no follow-up
// patrol tick to repair a wrong release.
//
// Same two-leg discrimination as the daemon-site control
// (city_runtime_release_callsite_test.go):
//
//   - W1 (sessionStore cure): assigned to a running rig-scoped session whose
//     bead lives ONLY in the relocated sessions-class binding. LEG D
//     reachable-red: repoint the call site's sessionStore argument at the
//     work store and W1 is falsely released.
//   - W2 (protectedWakeWork cure): bare-template assignee with no session
//     bead of its own, wake-reachable via a running same-template session.
//     LEG E reachable-red: drop the preWakeCandidates hunk and W2 is
//     reopened.
func TestStartStandalone_OrphanReleaseCallSite_RetainsLiveAndWakeProtectedWork(t *testing.T) {
	cityPath := t.TempDir()
	clearInheritedBeadsEnv(t)
	t.Chdir(t.TempDir())

	for _, rig := range []string{"rigA", "rigB"} {
		if err := os.MkdirAll(filepath.Join(cityPath, rig), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", rig, err)
		}
	}
	cityTOML := `[workspace]
name = "oneshot-callsite-city"
provider = "shell"

[providers.shell]
command = "echo"

[beads]
provider = "file"

[storage.classes]
work = "work"
graph = "infra"
sessions = "infra"
messaging = "infra"
orders = "infra"
nudges = "infra"

[storage.bindings.infra]
provider = "sqlite-beads"
path = ".gc/session-class-store"

[[agent]]
name = "worker"
scope = "city"
max_active_sessions = 2

[[agent]]
name = "rigworker"
dir = "rigA"
max_active_sessions = 2

[[rigs]]
name = "rigA"
path = "rigA"
prefix = "ra"

[[rigs]]
name = "rigB"
path = "rigB"
prefix = "rb"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityTOML), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	if err := ensureScopedFileStoreLayout(cityPath); err != nil {
		t.Fatalf("ensureScopedFileStoreLayout: %v", err)
	}
	for _, root := range []string{cityPath, filepath.Join(cityPath, "rigA"), filepath.Join(cityPath, "rigB")} {
		if err := ensurePersistedScopeLocalFileStore(root); err != nil {
			t.Fatalf("ensurePersistedScopeLocalFileStore(%q): %v", root, err)
		}
	}

	rigBStore, err := openScopeLocalFileStore(filepath.Join(cityPath, "rigB"))
	if err != nil {
		t.Fatalf("open rigB store: %v", err)
	}
	inProgress := "in_progress"
	seedWorkIn := func(store beads.Store, title, assignee, routedTo string) beads.Bead {
		t.Helper()
		wb, err := store.Create(beads.Bead{
			Title:    title,
			Assignee: assignee,
			Metadata: map[string]string{beadmeta.RoutedToMetadataKey: routedTo},
		})
		if err != nil {
			t.Fatalf("create %s: %v", title, err)
		}
		if err := store.Update(wb.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
			t.Fatalf("%s in_progress: %v", title, err)
		}
		return wb
	}
	// W1 lives in rigB's store while its assignee is rigA's session: the
	// cross-rig claim keeps snapshot-ownership and wake-reachability ref
	// mismatched, so retention falls to the sessionStore liveness probe —
	// the argument Leg D mutates.
	w1 := seedWorkIn(rigBStore, "w1: assignee live only in the relocated sessions binding", "rigworker-w1-live", "rigA/rigworker")
	w2 := seedWorkIn(rigBStore, "w2: staleness-window bare-template assignee", "worker", "worker")
	// Keeps the worker-1 session bead owning open work so no sweep closes it
	// before the release arm needs it for W2's wake reachability.
	seedWorkIn(rigBStore, "w3: anchors the worker-1 session through sweeps", "worker-1", "worker")
	// Canary: a genuinely orphaned bead. If the release arm runs at all, this
	// one MUST be released; it staying assigned means the fixture never
	// reached the release gates and every retention above is vacuous.
	w0 := seedWorkIn(rigBStore, "w0 canary: dead assignee, releasable", "ghost-nobody", "worker")

	// Sessions-class beads live ONLY in the relocated binding.
	sessStore, relocated := cliStorageRoutes(cityPath).storeFor(coordclass.ClassSessions)
	if !relocated || sessStore == nil {
		t.Fatalf("sessions class did not relocate to the sqlite binding; storeFor(sessions) relocated=%v", relocated)
	}
	seedSession := func(sessionName, template, agentLabel string) {
		t.Helper()
		if _, err := sessStore.Create(beads.Bead{
			Title:  "session " + sessionName,
			Type:   sessionBeadType,
			Status: "open",
			Labels: []string{sessionBeadLabel, "agent:" + agentLabel},
			Metadata: map[string]string{
				"session_name": sessionName,
				"template":     template,
				"agent_name":   agentLabel,
				"state":        "active",
			},
		}); err != nil {
			t.Fatalf("create session bead %s: %v", sessionName, err)
		}
	}
	seedSession("rigworker-w1-live", "rigA/rigworker", "rigworker")
	seedSession("worker-1", "worker", "worker")

	// The provider reports both sessions RUNNING so reconciliation does not
	// kill their beads before the release arm reads them.
	fake := runtime.NewFake()
	for _, name := range []string{"rigworker-w1-live", "worker-1"} {
		if err := fake.Start(context.Background(), name, runtime.Config{}); err != nil {
			t.Fatalf("fake start %s: %v", name, err)
		}
	}
	oldBuild := buildSessionProviderByName
	t.Cleanup(func() { buildSessionProviderByName = oldBuild })
	buildSessionProviderByName = func(_ *config.City, _ string, _ config.SessionConfig, _, _ string) (runtime.Provider, error) {
		return fake, nil
	}

	var stdout, stderr bytes.Buffer
	if code := doStartStandalone([]string{cityPath}, false, &stdout, &stderr); code != 0 {
		t.Fatalf("doStartStandalone exit = %d\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}

	// Reachability canary: the release arm must have actually run and released w0.
	if got, err := rigBStore.Get(w0.ID); err != nil {
		t.Fatalf("get canary: %v", err)
	} else if got.Assignee == "ghost-nobody" {
		t.Fatalf("canary w0 still assigned — the release arm never gated this fixture; retention assertions are vacuous\nstderr:\n%s", stderr.String())
	}

	// Bead IDs collide across independent stores (both minted "gc-1"), so each
	// row carries its own store — the same store-scoped-key lesson
	// readyAssignedFlagsForBeads documents.
	for _, tc := range []struct {
		store              beads.Store
		id, assignee, cure string
	}{
		{rigBStore, w1.ID, "rigworker-w1-live", "sessionStore (one-shot liveness read must hit the relocated sessions class, not the work store)"},
		{rigBStore, w2.ID, "worker", "protectedWakeWork (one-shot release must not reopen work this run's wake arm serves)"},
	} {
		got, err := tc.store.Get(tc.id)
		if err != nil {
			t.Fatalf("get %s: %v", tc.id, err)
		}
		if got.Assignee != tc.assignee || got.Status != inProgress {
			t.Errorf("%s was released at the one-shot call site: assignee=%q status=%q, want assignee=%q status=%q — lost cure: %s\nstderr:\n%s",
				tc.id, got.Assignee, got.Status, tc.assignee, inProgress, tc.cure, stderr.String())
		}
	}
}
