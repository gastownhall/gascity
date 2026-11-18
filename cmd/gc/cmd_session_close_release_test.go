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

// namedCloseSessionTemplate gives a named session's runtime name a city
// prefix, so it differs from the identity and work addressed to each is a
// distinct case.
const namedCloseSessionTemplate = "{{.City}}-{{.Agent}}"

// namedCloseCity is a file-store city holding the active bead of a named
// session `reviewer` (rig-qualified when the fixture declares a rig), backed
// by the agent `worker`. declared controls whether the [[named_session]]
// entry is still configured; the bead records the same identity either way.
type namedCloseCity struct {
	dir      string
	store    beads.Store
	session  beads.Bead
	identity string
	runtime  string
	route    string // a served gc.routed_to
}

func newNamedCloseCity(t *testing.T, rig string, declared bool) *namedCloseCity {
	t.Helper()
	cityDir := t.TempDir()
	dirLine, identity, route := "", "reviewer", "worker"
	var toml strings.Builder
	toml.WriteString("[workspace]\nname = \"test-city\"\nsession_template = \"" + namedCloseSessionTemplate + "\"\n\n[beads]\nprovider = \"file\"\n")
	if rig != "" {
		rigDir := filepath.Join(cityDir, rig)
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}
		toml.WriteString("\n[[rigs]]\nname = \"" + rig + "\"\npath = \"" + rigDir + "\"\n")
		dirLine, identity, route = "dir = \""+rig+"\"\n", rig+"/reviewer", rig+"/worker"
	}
	toml.WriteString("\n[[agent]]\nname = \"worker\"\n" + dirLine + "start_command = \"true\"\nmax_active_sessions = 1\n")
	if declared {
		toml.WriteString("\n[[named_session]]\nname = \"reviewer\"\ntemplate = \"worker\"\n" + dirLine)
	}
	writePhase0InterfaceCity(t, cityDir, toml.String())
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_DIR", t.TempDir())
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	runtimeName := config.NamedSessionRuntimeName("test-city", config.Workspace{SessionTemplate: namedCloseSessionTemplate}, identity)
	if runtimeName == identity {
		t.Fatalf("fixture runtime name %q equals the identity; the runtime-address case would be vacuous", runtimeName)
	}
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	sessionBead, err := store.Create(beads.Bead{
		Title:  "named reviewer incarnation",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name":               runtimeName,
			"template":                   route,
			"state":                      "active",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: identity,
			namedSessionModeMetadata:     "on_demand",
		},
	})
	if err != nil {
		t.Fatalf("Create(session bead): %v", err)
	}
	return &namedCloseCity{dir: cityDir, store: store, session: sessionBead, identity: identity, runtime: runtimeName, route: route}
}

// claim seeds a work bead assigned to assignee with status and gc.routed_to
// route, and returns it as stored.
func (c *namedCloseCity) claim(t *testing.T, assignee, status, route string) beads.Bead {
	t.Helper()
	w, err := c.store.Create(beads.Bead{
		Title:    "work for " + assignee,
		Type:     "task",
		Assignee: assignee,
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: route},
	})
	if err != nil {
		t.Fatalf("Create(work bead): %v", err)
	}
	if status != "open" {
		if err := c.store.Update(w.ID, beads.UpdateOpts{Status: &status}); err != nil {
			t.Fatalf("mark work %s: %v", status, err)
		}
	}
	got, err := c.store.Get(w.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// close runs `gc session close` on the session bead by ID and checks it
// closed.
func (c *namedCloseCity) close(t *testing.T) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := cmdSessionClose([]string{c.session.ID}, "", &stdout, &stderr, false); code != 0 {
		t.Fatalf("cmdSessionClose = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if got := c.get(t, c.session.ID); got.Status != "closed" {
		t.Fatalf("session bead status = %q, want closed", got.Status)
	}
}

// get reads id back through a freshly opened city store.
func (c *namedCloseCity) get(t *testing.T, id string) beads.Bead {
	t.Helper()
	reopened, err := openCityStoreAt(c.dir)
	if err != nil {
		t.Fatalf("reopen city store: %v", err)
	}
	got, err := reopened.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return got
}

// assertUntouched checks the close left want's claim as it was: same
// assignee, status and route, and no stranded stamp.
func (c *namedCloseCity) assertUntouched(t *testing.T, want beads.Bead) {
	t.Helper()
	got := c.get(t, want.ID)
	if got.Assignee != want.Assignee || got.Status != want.Status {
		t.Errorf("work addressed to %q = (assignee %q, status %q), want (%q, %q) left to the still-configured identity", want.Assignee, got.Assignee, got.Status, want.Assignee, want.Status)
	}
	if g, w := got.Metadata[beadmeta.RoutedToMetadataKey], want.Metadata[beadmeta.RoutedToMetadataKey]; g != w {
		t.Errorf("work addressed to %q gc.routed_to = %q, want %q", want.Assignee, g, w)
	}
	if s := got.Metadata[beadmeta.StrandedMetadataKey]; s != "" {
		t.Errorf("work addressed to %q stamped stranded (%q) by the close, want unstamped", want.Assignee, s)
	}
}

// assertReleased checks the close released want's claim: assignee cleared,
// status open, and its route kept.
func (c *namedCloseCity) assertReleased(t *testing.T, want beads.Bead) {
	t.Helper()
	got := c.get(t, want.ID)
	if got.Assignee != "" || got.Status != "open" {
		t.Errorf("work addressed to %q = (assignee %q, status %q), want released to (\"\", open)", want.Assignee, got.Assignee, got.Status)
	}
	if g, w := got.Metadata[beadmeta.RoutedToMetadataKey], want.Metadata[beadmeta.RoutedToMetadataKey]; g != w {
		t.Errorf("released work gc.routed_to = %q, want its route %q kept", g, w)
	}
}

// strandedAlertsNaming returns the city's session.stranded_claims events
// for the session that name any of ids.
func (c *namedCloseCity) strandedAlertsNaming(t *testing.T, ids ...string) []events.Event {
	t.Helper()
	evs, err := events.ReadAll(filepath.Join(c.dir, ".gc", "events.jsonl"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("reading city events: %v", err)
	}
	var hits []events.Event
	for _, e := range evs {
		if e.Type != events.SessionStrandedClaims || e.Subject != c.session.ID {
			continue
		}
		if slices.ContainsFunc(ids, func(id string) bool { return strings.Contains(string(e.Payload), id) }) {
			hits = append(hits, e)
		}
	}
	return hits
}

// namedCloseRigCases are the identity shapes each named-session close test
// runs: a city-scoped identity and a rig-qualified one.
var namedCloseRigCases = []struct{ name, rig string }{
	{"city identity", ""},
	{"rig-qualified identity", "myrig"},
}

// TestCmdSessionCloseOnConfiguredNamedSessionPreservesIdentityAssignedWork
// pins ga-o8xf41: closing the current bead of a named session whose identity
// is still configured must not release work addressed to the identity. The
// controller re-materializes the identity, so that work still has an owner;
// a claim no lane serves is not the close's to strand either.
func TestCmdSessionCloseOnConfiguredNamedSessionPreservesIdentityAssignedWork(t *testing.T) {
	for _, tc := range namedCloseRigCases {
		t.Run(tc.name, func(t *testing.T) {
			c := newNamedCloseCity(t, tc.rig, true)
			queued := c.claim(t, c.identity, "open", c.route)
			held := c.claim(t, c.identity, "in_progress", c.route)
			unserved := c.claim(t, c.identity, "in_progress", "nowhere/lane")

			c.close(t)

			for _, w := range []beads.Bead{queued, held, unserved} {
				c.assertUntouched(t, w)
			}
			if hits := c.strandedAlertsNaming(t, queued.ID, held.ID, unserved.ID); len(hits) != 0 {
				t.Errorf("session.stranded_claims = %+v, want none naming identity-addressed work", hits)
			}
		})
	}
}

// TestConfiguredNamedSessionStableAddresses pins which beads' stable
// addresses `gc session close` leaves claimed: only a named-session bead
// whose identity still resolves to a configured spec with its backing agent.
func TestConfiguredNamedSessionStableAddresses(t *testing.T) {
	configured := func() *config.City {
		return &config.City{
			Workspace:     config.Workspace{Name: "test-city", SessionTemplate: namedCloseSessionTemplate},
			Agents:        []config.Agent{{Name: "worker"}},
			NamedSessions: []config.NamedSession{{Name: "reviewer", Template: "worker"}},
		}
	}
	named := func(meta map[string]string) beads.Bead {
		m := map[string]string{
			"session_name":               "test-city-reviewer",
			"template":                   "worker",
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "reviewer",
		}
		for k, v := range meta {
			m[k] = v
		}
		return beads.Bead{ID: "gc-1", Metadata: m}
	}
	for _, tc := range []struct {
		name string
		bead beads.Bead
		cfg  func() *config.City
		want []string
	}{
		{"still-configured identity", named(nil), configured, []string{"reviewer", "test-city-reviewer"}},
		{"runtime name equals the identity", named(nil), func() *config.City {
			cfg := configured()
			cfg.Workspace.SessionTemplate = ""
			return cfg
		}, []string{"reviewer"}},
		{"city name from the directory", named(nil), func() *config.City {
			cfg := configured()
			cfg.Workspace.Name = ""
			return cfg
		}, []string{"reviewer", "dir-city-reviewer"}},
		{"named-session flag absent", named(map[string]string{namedSessionMetadataKey: ""}), configured, nil},
		{"empty identity", named(map[string]string{namedSessionIdentityMetadata: ""}), configured, nil},
		{"synthetic ID-only snapshot", beads.Bead{ID: "gc-1"}, configured, nil},
		{"nil config", named(nil), func() *config.City { return nil }, nil},
		{"identity removed from config", named(nil), func() *config.City {
			cfg := configured()
			cfg.NamedSessions = nil
			return cfg
		}, nil},
		{"backing template missing", named(nil), func() *config.City {
			cfg := configured()
			cfg.Agents = nil
			return cfg
		}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg()
			got := configuredNamedSessionStableAddresses(tc.bead, cfg, loadedCityName(cfg, "/cities/dir-city"))
			if !slices.Equal(got, tc.want) {
				t.Errorf("configuredNamedSessionStableAddresses = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCmdSessionCloseOnConfiguredNamedSessionPreservesRuntimeAssignedWork:
// the runtime name of a still-configured named session is as stable as its
// identity (the controller re-materializes the identity under it), so work
// addressed to the runtime name stays claimed through the close too.
func TestCmdSessionCloseOnConfiguredNamedSessionPreservesRuntimeAssignedWork(t *testing.T) {
	for _, tc := range namedCloseRigCases {
		t.Run(tc.name, func(t *testing.T) {
			c := newNamedCloseCity(t, tc.rig, true)
			queued := c.claim(t, c.runtime, "open", c.route)
			held := c.claim(t, c.runtime, "in_progress", c.route)

			c.close(t)

			c.assertUntouched(t, queued)
			c.assertUntouched(t, held)
		})
	}
}

// TestCmdSessionCloseOnConfiguredNamedSessionReleasesBeadIDAssignedWork: the
// preserved stable addresses narrow the close's release, never empty it. A
// claim addressed to the dying bead's own ID is still released into its route
// while the identity's work stays claimed.
func TestCmdSessionCloseOnConfiguredNamedSessionReleasesBeadIDAssignedWork(t *testing.T) {
	for _, tc := range namedCloseRigCases {
		t.Run(tc.name, func(t *testing.T) {
			c := newNamedCloseCity(t, tc.rig, true)
			own := c.claim(t, c.session.ID, "in_progress", c.route)
			identity := c.claim(t, c.identity, "in_progress", c.route)

			c.close(t)

			c.assertReleased(t, own)
			c.assertUntouched(t, identity)
		})
	}
}

// TestCmdSessionCloseOnRemovedNamedSessionReleasesIdentityAssignedWork: once
// the identity is gone from config no session will serve its addresses
// again, so the close releases identity- and runtime-addressed work as it
// always has. The bead still records the identity; only config decides.
func TestCmdSessionCloseOnRemovedNamedSessionReleasesIdentityAssignedWork(t *testing.T) {
	for _, tc := range namedCloseRigCases {
		t.Run(tc.name, func(t *testing.T) {
			c := newNamedCloseCity(t, tc.rig, false)
			identity := c.claim(t, c.identity, "in_progress", c.route)
			runtimeAddressed := c.claim(t, c.runtime, "in_progress", c.route)
			own := c.claim(t, c.session.ID, "in_progress", c.route)

			c.close(t)

			for _, w := range []beads.Bead{identity, runtimeAddressed, own} {
				c.assertReleased(t, w)
			}
		})
	}
}
