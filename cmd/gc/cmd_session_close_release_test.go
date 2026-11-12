package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
)

// TestCmdSessionCloseReleasesAssignedWorkBeads is a regression for
// gastownhall/gascity#2625. After `gc session close`, any work bead still
// assigned to the closed session must be released (Assignee cleared, Status
// reset to open) so the pool scale-check picks up the freed demand on the
// next reconcile tick. Without it, Source-1 CachedReady stays stale, the
// pool scale-check sees scaleCount=0, and no fresh worker spawns even
// though the demand is admittable.
func TestCmdSessionCloseReleasesAssignedWorkBeads(t *testing.T) {
	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
start_command = "true"
max_active_sessions = 1
`)
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_DIR", t.TempDir())
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}

	sessionBead, err := store.Create(beads.Bead{
		Title:  "stranded worker",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "worker-stranded",
			"template":     "worker",
			"state":        "active",
		},
	})
	if err != nil {
		t.Fatalf("Create(session bead): %v", err)
	}

	work, err := store.Create(beads.Bead{
		Title:    "admittable demand",
		Type:     "task",
		Assignee: sessionBead.ID,
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("Create(work bead): %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark work in_progress: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdSessionClose([]string{sessionBead.ID}, "", &stdout, &stderr, false); code != 0 {
		t.Fatalf("cmdSessionClose = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	reopened, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("reopen city store: %v", err)
	}

	gotSession, err := reopened.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("Get(session bead): %v", err)
	}
	if gotSession.Status != "closed" {
		t.Errorf("session bead status = %q, want closed", gotSession.Status)
	}

	gotWork, err := reopened.Get(work.ID)
	if err != nil {
		t.Fatalf("Get(work bead): %v", err)
	}
	if gotWork.Assignee != "" {
		t.Errorf("work bead Assignee = %q, want empty (released after session close)", gotWork.Assignee)
	}
	if gotWork.Status != "open" {
		t.Errorf("work bead Status = %q, want open (reset so the routed queue can re-pick it)", gotWork.Status)
	}
}

// TestCmdSessionCloseClearsClaimInTheSessionStoreOnAMigratedCity pins the store
// `gc session close` hands the claim clear. On a migrated city the session bead
// lives in the sessions-class binding while the release sweep leads with the
// retained work store; clearing the claim back-channel through the work store
// looked the session bead up where it does not live, logged "clearing current
// claim on retired session ...: bead not found", and left current_claim_bead_id
// stamped on the closed session. The helper's own split-store contract is
// TestUnclaimWorkAssignedToRetiredSessionClearsClaimInTheSessionStore; this row
// is the command's call site, which that helper test cannot see.
func TestCmdSessionCloseClearsClaimInTheSessionStoreOnAMigratedCity(t *testing.T) {
	cityPath, cfg := migratedOneShotCLICity(t)
	// The seat's template is a configured agent: a claim with no route of its
	// own is released into the seat's lane.
	appendCityTOML(t, cityPath, "\n[[agent]]\nname = \"worker\"\nstart_command = \"true\"\n")
	captureCLIStorageStderr(t)
	t.Setenv("GC_SESSION", "fake")

	work, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("opening the city work store: %v", err)
	}
	t.Cleanup(func() { _ = closeBeadStoreHandle(work) })
	sessions := cliSessionStore(work, cfg, cityPath)

	sessionBead, err := sessions.Create(beads.Bead{
		Title:  "claiming worker",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "worker-claiming",
			"template":     "worker",
			"state":        "active",
		},
	})
	if err != nil {
		t.Fatalf("seeding the session bead in the session store: %v", err)
	}
	// A claim routed to no configured agent stays on the closed seat, and the
	// close raises session.stranded_claims for it.
	unserved, err := work.Create(beads.Bead{Title: "unserved step", Type: "task", Assignee: sessionBead.ID, Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "nowhere/lane"}})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := work.Create(beads.Bead{
		Title:    "claimed step",
		Type:     "task",
		Assignee: sessionBead.ID,
	})
	if err != nil {
		t.Fatalf("Create(work bead): %v", err)
	}
	inProgress := "in_progress"
	if err := work.Update(claimed.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark work in_progress: %v", err)
	}
	if err := sessions.SetMetadata(sessionBead.ID, beadmeta.CurrentClaimBeadIDMetadataKey, claimed.ID); err != nil {
		t.Fatalf("seeding the claim stamp: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdSessionClose([]string{sessionBead.ID}, "", &stdout, &stderr, false); code != 0 {
		t.Fatalf("cmdSessionClose = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), "clearing current claim") {
		t.Errorf("stderr = %q, want no claim-clear failure: the close must clear the claim in the session store", stderr.String())
	}

	// The funnel's own handle goes first, so the reads below see durable bytes
	// rather than state an open connection is holding.
	if err := closeCLIStorageRoutes(); err != nil {
		t.Fatalf("closing the one-shot routes: %v", err)
	}
	closed, err := openMigratedDestination(t, mustResolveInfraTarget(t, cityPath, cfg)).Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("reading the session bead back from the binding: %v", err)
	}
	if closed.Status != "closed" {
		t.Errorf("session bead status = %q, want closed", closed.Status)
	}
	if got := closed.Metadata[beadmeta.CurrentClaimBeadIDMetadataKey]; got != "" {
		t.Errorf("current claim = %q on the closed session, want cleared in the session store", got)
	}

	reopened, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("reopening the work store: %v", err)
	}
	t.Cleanup(func() { _ = closeBeadStoreHandle(reopened) })
	released, err := reopened.Get(claimed.ID)
	if err != nil {
		t.Fatalf("Get(work bead): %v", err)
	}
	if released.Assignee != "" || released.Status != "open" {
		t.Errorf("work bead = (assignee %q, status %q), want released to (\"\", open) from the work store; stderr=%s", released.Assignee, released.Status, stderr.String())
	}
	// The claim had no route: it takes the seat's, so a lane serves it (the
	// ga-n2d.2 shape).
	if got := released.Metadata[beadmeta.RunTargetMetadataKey]; got != "worker" {
		t.Errorf("released claim's run_target = %q, want the seat's route worker", got)
	}
	if kept, err := reopened.Get(unserved.ID); err != nil || kept.Assignee != sessionBead.ID {
		t.Errorf("unserved claim = %+v (%v), want kept by the closed seat", kept, err)
	}
	evs, err := events.ReadAll(filepath.Join(cityPath, ".gc", "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(evs, func(e events.Event) bool {
		return e.Type == events.SessionStrandedClaims && e.Subject == sessionBead.ID && strings.Contains(string(e.Payload), unserved.ID)
	}) {
		t.Errorf("no session.stranded_claims for %s in the city's events", unserved.ID)
	}
}

// The drain-ack release reads work through the city's work legs over the work
// store it opened, never the sessions store it resolves the session through:
// on a migrated city a claim on the work store is released.
func TestDrainAckReleaseReadsTheWorkStoreOnAMigratedCity(t *testing.T) {
	cityPath, cfg := migratedOneShotCLICity(t)
	captureCLIStorageStderr(t)
	work, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("opening the city work store: %v", err)
	}
	t.Cleanup(func() { _ = closeBeadStoreHandle(work) })
	sessions := cliSessionStore(work, cfg, cityPath)
	sessionBead, err := sessions.Create(beads.Bead{
		Title: "draining worker", Type: session.BeadType, Labels: []string{session.LabelSession},
		Metadata: map[string]string{"session_name": "worker-draining", "state": "active"},
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := work.Create(beads.Bead{Title: "claimed step", Type: "task", Assignee: sessionBead.ID, Metadata: map[string]string{"gc.routed_to": "worker"}})
	if err != nil {
		t.Fatal(err)
	}
	inProgress := "in_progress"
	if err := work.Update(claimed.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	releaseUnexecutedClaimsForSessionStore(cityPath, servingRigsCity(cfg, "worker"), work, nil, "worker-draining", drainAckCaller{}, &stderr)

	got, err := work.Get(claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Assignee != "" || got.Status != "open" {
		t.Fatalf("work bead = (%q, %q), want released from the work store; stderr=%s", got.Assignee, got.Status, stderr.String())
	}
}

// appendCityTOML appends body to cityPath's city.toml.
func appendCityTOML(t *testing.T, cityPath, body string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(cityPath, "city.toml"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck
	if _, err := f.WriteString(body); err != nil {
		t.Fatal(err)
	}
}

// A city whose config does not load refuses the close of a session holding
// claims, whose routes could not be judged, and closes one holding none.
func TestCmdSessionCloseWithoutConfigRefusesOnlyOverClaims(t *testing.T) {
	for _, holds := range []bool{true, false} {
		t.Run(map[bool]string{true: "holding a claim", false: "holding nothing"}[holds], func(t *testing.T) {
			cityDir := t.TempDir()
			writePhase0InterfaceCity(t, cityDir, "[workspace]\nname = \"test-city\"\n\n[beads]\nprovider = \"file\"\n")
			t.Setenv("GC_CITY", cityDir)
			t.Setenv("GC_DIR", t.TempDir())
			t.Setenv("GC_BEADS", "file")
			t.Setenv("GC_SESSION", "fake")
			store, err := openCityStoreAt(cityDir)
			if err != nil {
				t.Fatal(err)
			}
			seat, err := store.Create(beads.Bead{Title: "worker", Type: session.BeadType, Labels: []string{session.LabelSession}, Metadata: map[string]string{"session_name": "worker-1", "template": "worker", "state": "asleep"}})
			if err != nil {
				t.Fatal(err)
			}
			if holds {
				if _, err := store.Create(beads.Bead{Title: "claim", Type: "task", Assignee: seat.ID, Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"}}); err != nil {
					t.Fatal(err)
				}
			}
			appendCityTOML(t, cityDir, "\n[[[not toml\n")
			var stdout, stderr bytes.Buffer
			code := cmdSessionClose([]string{seat.ID}, "", &stdout, &stderr, false)
			refused := strings.Contains(stderr.String(), "refusing to close")
			if holds && (code == 0 || !refused) || !holds && (code != 0 || refused) {
				t.Fatalf("cmdSessionClose = %d, stderr %q; want refused %t", code, stderr.String(), holds)
			}
		})
	}
}

// namedCloseAgentTOML is a city with the one agent "worker" and no named
// session.
const namedCloseAgentTOML = `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
start_command = "true"
`

// namedCloseCityTOML is namedCloseAgentTOML with "worker" backing the
// still-configured named session "reviewer".
const namedCloseCityTOML = namedCloseAgentTOML + `
[[named_session]]
name = "reviewer"
template = "worker"
scope = "city"
mode = "on_demand"
`

// seedNamedCloseSession writes cityTOML, points the CLI at it, and seeds one
// open configured-named session row for the identity "reviewer" under
// runtime name sessionName, backed by the "worker" template.
func seedNamedCloseSession(t *testing.T, cityTOML, sessionName string) (string, beads.Store, beads.Bead) {
	const identity = "reviewer"
	t.Helper()
	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, cityTOML)
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_DIR", t.TempDir())
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	sessionBead, err := store.Create(beads.Bead{
		Title:  identity,
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name":               sessionName,
			"template":                   "worker",
			"state":                      "start-pending",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: identity,
			namedSessionModeMetadata:     "on_demand",
		},
	})
	if err != nil {
		t.Fatalf("Create(session bead): %v", err)
	}
	return cityDir, store, sessionBead
}

// seedCloseClaim creates a task assigned to assignee with metadata, then
// moves it to status.
func seedCloseClaim(t *testing.T, store beads.Store, assignee, status string, metadata map[string]string) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Title:    "claim for " + assignee,
		Type:     "task",
		Assignee: assignee,
		Metadata: metadata,
	})
	if err != nil {
		t.Fatalf("Create(work bead for %s): %v", assignee, err)
	}
	if status != "open" {
		if err := store.Update(b.ID, beads.UpdateOpts{Status: &status}); err != nil {
			t.Fatalf("mark %s %s: %v", b.ID, status, err)
		}
	}
	got, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", b.ID, err)
	}
	return got
}

// runSessionCloseAndReopen runs `gc session close <id>`, requires success,
// and returns a freshly reopened city store.
func runSessionCloseAndReopen(t *testing.T, cityDir, sessionID string) beads.Store {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := cmdSessionClose([]string{sessionID}, "", &stdout, &stderr, false); code != 0 {
		t.Fatalf("cmdSessionClose = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	reopened, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("reopen city store: %v", err)
	}
	gotSession, err := reopened.Get(sessionID)
	if err != nil {
		t.Fatalf("Get(session bead): %v", err)
	}
	if gotSession.Status != "closed" {
		t.Fatalf("session bead status = %q, want closed", gotSession.Status)
	}
	return reopened
}

// assertCloseClaimKept requires that before's claim still has its assignee,
// status and routing, and that the close did not stamp it stranded.
func assertCloseClaimKept(t *testing.T, store beads.Store, before beads.Bead) {
	t.Helper()
	got, err := store.Get(before.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", before.ID, err)
	}
	if got.Assignee != before.Assignee || got.Status != before.Status {
		t.Errorf("claim %s = (assignee %q, status %q), want kept (%q, %q)", before.ID, got.Assignee, got.Status, before.Assignee, before.Status)
	}
	if got.Metadata[beadmeta.RoutedToMetadataKey] != before.Metadata[beadmeta.RoutedToMetadataKey] {
		t.Errorf("claim %s %s = %q, want kept %q", before.ID, beadmeta.RoutedToMetadataKey, got.Metadata[beadmeta.RoutedToMetadataKey], before.Metadata[beadmeta.RoutedToMetadataKey])
	}
	if stamp := got.Metadata[beadmeta.StrandedMetadataKey]; stamp != "" {
		t.Errorf("claim %s %s = %q, want no stranded stamp on kept work", before.ID, beadmeta.StrandedMetadataKey, stamp)
	}
}

// assertCloseClaimReleased requires that id's claim was released open and
// unassigned into the "worker" lane, read from its routing metadata key:
// gc.routed_to for a claim that carried its own route, gc.run_target for the
// seat-template fallback a routeless claim is stamped with.
func assertCloseClaimReleased(t *testing.T, store beads.Store, id, key string) {
	t.Helper()
	const route = "worker"
	got, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	if got.Assignee != "" || got.Status != "open" {
		t.Errorf("claim %s = (assignee %q, status %q), want released (\"\", open)", id, got.Assignee, got.Status)
	}
	if got.Metadata[key] != route {
		t.Errorf("claim %s %s = %q, want %q", id, key, got.Metadata[key], route)
	}
}

// TestCmdSessionCloseKeepsConfiguredNamedIdentityClaims pins
// gastownhall/gascity#8179: closing one incarnation of a still-configured
// named session leaves work assigned to the named identity in place, because
// the controller re-materializes the identity and serves it. The bead-ID
// claim is the control row: it is still released, so the sweep ran, and the
// closed row's claim back-channel is still cleared.
func TestCmdSessionCloseKeepsConfiguredNamedIdentityClaims(t *testing.T) {
	for _, status := range []string{"open", "in_progress"} {
		t.Run(status, func(t *testing.T) {
			cityDir, store, sessionBead := seedNamedCloseSession(t, namedCloseCityTOML, "reviewer")
			identityWork := seedCloseClaim(t, store, "reviewer", status, map[string]string{beadmeta.RoutedToMetadataKey: ""})
			control := seedCloseClaim(t, store, sessionBead.ID, "in_progress", map[string]string{beadmeta.RoutedToMetadataKey: "worker"})
			if err := store.SetMetadata(sessionBead.ID, beadmeta.CurrentClaimBeadIDMetadataKey, identityWork.ID); err != nil {
				t.Fatalf("stamp current claim: %v", err)
			}

			reopened := runSessionCloseAndReopen(t, cityDir, sessionBead.ID)

			assertCloseClaimKept(t, reopened, identityWork)
			assertCloseClaimReleased(t, reopened, control.ID, beadmeta.RoutedToMetadataKey)
			gotSession, err := reopened.Get(sessionBead.ID)
			if err != nil {
				t.Fatalf("Get(session bead): %v", err)
			}
			if claim := gotSession.Metadata[beadmeta.CurrentClaimBeadIDMetadataKey]; claim != "" {
				t.Errorf("closed session %s = %q, want cleared even though the identity keeps its work", beadmeta.CurrentClaimBeadIDMetadataKey, claim)
			}
		})
	}
}

// TestClosedNamedSessionKeptAssignees pins which assignees a manual close of a
// named session row keeps: only the exact forms the re-materialized canonical
// bead serves, never one reached through the bare-name fallback, and nothing
// for a row that is not a still-configured named session.
func TestClosedNamedSessionKeptAssignees(t *testing.T) {
	plain := &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker"}},
		NamedSessions: []config.NamedSession{{Name: "reviewer", Template: "worker"}},
	}
	bound := &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", BindingName: "ops"}},
		NamedSessions: []config.NamedSession{{Name: "reviewer", Template: "worker", BindingName: "ops"}},
	}
	templated := &config.City{
		Workspace:     config.Workspace{Name: "test-city", SessionTemplate: "{{.City}}-{{.Agent}}"},
		Agents:        []config.Agent{{Name: "worker"}},
		NamedSessions: []config.NamedSession{{Name: "reviewer", Template: "worker"}},
	}
	removed := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker"}},
	}
	noBacking := &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		NamedSessions: []config.NamedSession{{Name: "reviewer", Template: "worker"}},
	}
	row := func(named bool, identity, sessionName string) beads.Bead {
		meta := map[string]string{"session_name": sessionName, "template": "worker"}
		if named {
			meta[namedSessionMetadataKey] = "true"
			meta[namedSessionIdentityMetadata] = identity
		}
		return beads.Bead{ID: "tc-1", Type: session.BeadType, Metadata: meta}
	}
	tests := []struct {
		name string
		cfg  *config.City
		bead beads.Bead
		want []string
	}{
		{name: "nil config", cfg: nil, bead: row(true, "reviewer", "reviewer")},
		{name: "unnamed row", cfg: plain, bead: row(false, "", "reviewer")},
		{name: "blank identity", cfg: plain, bead: row(true, "  ", "reviewer")},
		{name: "identity removed from config", cfg: removed, bead: row(true, "reviewer", "reviewer")},
		{name: "backing template missing", cfg: noBacking, bead: row(true, "reviewer", "reviewer")},
		{name: "exact identity", cfg: plain, bead: row(true, "reviewer", "reviewer"), want: []string{"reviewer"}},
		{name: "exact bound identity", cfg: bound, bead: row(true, "ops.reviewer", "ops__reviewer"), want: []string{"ops.reviewer", "ops__reviewer"}},
		{name: "distinct current runtime name", cfg: templated, bead: row(true, "reviewer", "test-city-reviewer"), want: []string{"reviewer", "test-city-reviewer"}},
		{name: "obsolete stored runtime name", cfg: templated, bead: row(true, "reviewer", "old-reviewer"), want: []string{"reviewer", "test-city-reviewer"}},
		{name: "bare name resolving only through the bound fallback", cfg: bound, bead: row(true, "reviewer", "reviewer")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := closedNamedSessionKeptAssignees(tt.bead, tt.cfg, "test-city")
			var keys []string
			for k := range got {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			if !slices.Equal(keys, tt.want) {
				t.Errorf("closedNamedSessionKeptAssignees = %v, want %v", keys, tt.want)
			}
			if _, ok := got[tt.bead.ID]; ok {
				t.Errorf("closedNamedSessionKeptAssignees kept the closed row's own ID %q", tt.bead.ID)
			}
		})
	}
}

// TestCmdSessionCloseKeepsConfiguredNamedRuntimeNameClaims pins the runtime
// name half of gastownhall/gascity#8179: work routed to a named session is
// claimed under its runtime name, which the re-materialized canonical bead
// carries too, so a close keeps it as it keeps the identity's.
func TestCmdSessionCloseKeepsConfiguredNamedRuntimeNameClaims(t *testing.T) {
	cityTOML := strings.Replace(namedCloseCityTOML, `name = "test-city"`, "name = \"test-city\"\nsession_template = \"{{.City}}-{{.Agent}}\"", 1)
	cityDir, store, sessionBead := seedNamedCloseSession(t, cityTOML, "test-city-reviewer")
	runtimeWork := seedCloseClaim(t, store, "test-city-reviewer", "in_progress", map[string]string{beadmeta.RoutedToMetadataKey: "reviewer"})
	identityWork := seedCloseClaim(t, store, "reviewer", "open", map[string]string{beadmeta.RoutedToMetadataKey: ""})
	control := seedCloseClaim(t, store, sessionBead.ID, "in_progress", map[string]string{beadmeta.RoutedToMetadataKey: "worker"})

	reopened := runSessionCloseAndReopen(t, cityDir, sessionBead.ID)

	assertCloseClaimKept(t, reopened, runtimeWork)
	assertCloseClaimKept(t, reopened, identityWork)
	assertCloseClaimReleased(t, reopened, control.ID, beadmeta.RoutedToMetadataKey)
}

// TestCmdSessionCloseReleasesNamedSessionBeadIDClaims pins that the
// configured-identity exception does not reach the closed incarnation's own
// ID: the re-materialized canonical bead gets a new one, so a routeless claim
// on the closed row's ID is released open and unassigned into the seat's
// backing template.
func TestCmdSessionCloseReleasesNamedSessionBeadIDClaims(t *testing.T) {
	cityDir, store, sessionBead := seedNamedCloseSession(t, namedCloseCityTOML, "reviewer")
	claimed := seedCloseClaim(t, store, sessionBead.ID, "in_progress", map[string]string{beadmeta.RoutedToMetadataKey: ""})

	reopened := runSessionCloseAndReopen(t, cityDir, sessionBead.ID)

	assertCloseClaimReleased(t, reopened, claimed.ID, beadmeta.RunTargetMetadataKey)
}

// TestCmdSessionCloseReleasesRemovedNamedIdentityClaims pins that the
// configured-identity exception needs the identity still configured: once
// the named session is removed from config no canonical bead returns to
// serve it, so a manual close releases the identity's work under the
// existing retirement policy, into the backing template that still serves it.
func TestCmdSessionCloseReleasesRemovedNamedIdentityClaims(t *testing.T) {
	cityDir, store, sessionBead := seedNamedCloseSession(t, namedCloseAgentTOML, "reviewer")
	identityWork := seedCloseClaim(t, store, "reviewer", "open", map[string]string{beadmeta.RoutedToMetadataKey: ""})

	reopened := runSessionCloseAndReopen(t, cityDir, sessionBead.ID)

	assertCloseClaimReleased(t, reopened, identityWork.ID, beadmeta.RunTargetMetadataKey)
}

// TestSessionCloseHelpDocumentsWorkRelease pins what `gc session close --help`
// tells an operator about the session's assigned work: the closed
// incarnation's work is released, a still-configured named session's
// identity keeps its own, unserved work stays and is reported stranded, and
// `gc session kill` is the way to keep the same session bead and assignments.
func TestSessionCloseHelpDocumentsWorkRelease(t *testing.T) {
	var discard bytes.Buffer
	long := strings.Join(strings.Fields(newSessionCloseCmd(&discard, &discard).Long), " ")
	for _, want := range []string{
		"Open and in-progress work assigned to the closed session",
		"is released",
		"session's template",
		"configured named identity and its runtime name",
		"session.stranded_claims",
		"gc session kill",
		"same session bead",
	} {
		if !strings.Contains(long, want) {
			t.Errorf("gc session close help is missing %q:\n%s", want, long)
		}
	}
}
