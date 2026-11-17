package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/session"
)

// Kills: a per-leg count that is not first-leg-wins (gc-a0's migrated copy on
// the city leg read as a second row), samples that list a lone slot row or more
// than ten IDs, a not-checked or failed leg that reads as a clean census, a
// leg that failed to open or read that does not block under v2, a leg with no
// read-only open that blocks under v2, a legacy not-checked leg that fails the
// sweep, a refusal that warns under legacy or passes under v2, and a v2 refusal
// that an unchecked leg hides behind the lower-bound warning.
func TestV2SessionMigrationCheckCountsPerLeg(t *testing.T) {
	cfg := chatCity("always")
	cfg.Agents = append(cfg.Agents, allocPoolAgent("worker", 3))
	slotRow := func(id string) beads.Bead {
		return poolRow(id, "worker", 2, "creating", "session_name", "worker-2-pool")
	}
	var archived []beads.Bead
	for _, id := range []string{"gc-a0", "gc-a1", "gc-a2", "gc-a3", "gc-a4", "gc-a5", "gc-a6", "gc-a7", "gc-a8", "gc-a9", "gc-aa"} {
		archived = append(archived, poolRow(id, "worker", 1, "archived"))
	}
	binding := censusStore(append(archived, slotRow("gc-s1"))...)
	city := censusStore(poolRow("gc-a0", "worker", 1, "archived"), slotRow("gc-s2"), poolRow("gc-k1", "worker", 3, "active"))
	readErr := errors.New("bd: connection refused")
	proxiedLeg := v2SessionLeg{ref: "rig:fixture", notChecked: "proxied-server Dolt, where bd refuses --readonly", noReadOnlyOpen: true}
	for _, tc := range []struct {
		name    string
		mode    string
		legs    []v2SessionLeg
		status  doctor.CheckStatus
		message string
		details []string
	}{
		{
			name:    "legacy reports",
			legs:    []v2SessionLeg{{ref: "binding:infra", store: binding}, {ref: "city", store: city}},
			status:  doctor.StatusOK,
			message: "14 open session row(s): 11 in a state main does not know, 1 shared pool-slot session name(s), 0 named session(s) claimed by more than one row (legacy runs over these; v2 would refuse to boot)",
			details: []string{
				"leg binding:infra: 12 open session row(s)",
				"leg city: 2 open session row(s)",
				`shared slot name "worker-2-pool": gc-s1 gc-s2`,
				`state "archived": gc-a0 gc-a1 gc-a2 gc-a3 gc-a4 gc-a5 gc-a6 gc-a7 gc-a8 gc-a9`,
			},
		},
		{
			name:   "v2 warns",
			mode:   config.SessionReconcilerV2,
			legs:   []v2SessionLeg{{ref: "binding:infra", store: binding}, {ref: "city", store: city}},
			status: doctor.StatusWarning,
		},
		{
			name:    "clean",
			mode:    config.SessionReconcilerV2,
			legs:    []v2SessionLeg{{ref: "city", store: censusStore(poolRow("gc-k1", "worker", 3, "active"))}},
			status:  doctor.StatusOK,
			message: "1 open session row(s): 0 in a state main does not know, 0 shared pool-slot session name(s), 0 named session(s) claimed by more than one row",
		},
		{
			name:    "no read-only open warns under v2",
			mode:    config.SessionReconcilerV2,
			legs:    []v2SessionLeg{{ref: "city", store: censusStore()}, proxiedLeg},
			status:  doctor.StatusWarning,
			message: "0 open session row(s): 0 in a state main does not know, 0 shared pool-slot session name(s), 0 named session(s) claimed by more than one row; 1 leg(s) not checked, counts are a lower bound",
			details: []string{"leg city: 0 open session row(s)", "leg rig:fixture: not checked (no read-only open): proxied-server Dolt, where bd refuses --readonly"},
		},
		{
			name:    "open failed blocks under v2",
			mode:    config.SessionReconcilerV2,
			legs:    []v2SessionLeg{{ref: "binding", notChecked: "binding \"infra\": no such provider"}, proxiedLeg},
			status:  doctor.StatusError,
			message: "0 open session row(s): 0 in a state main does not know, 0 shared pool-slot session name(s), 0 named session(s) claimed by more than one row; 2 leg(s) not checked, counts are a lower bound",
			details: []string{"leg binding: not checked (open failed): binding \"infra\": no such provider", "leg rig:fixture: not checked (no read-only open): proxied-server Dolt, where bd refuses --readonly"},
		},
		{
			name:    "not checked warns under legacy",
			legs:    []v2SessionLeg{{ref: "city", store: censusStore()}, proxiedLeg, {ref: "binding", notChecked: "binding \"infra\": no such provider"}},
			status:  doctor.StatusWarning,
			message: "0 open session row(s): 0 in a state main does not know, 0 shared pool-slot session name(s), 0 named session(s) claimed by more than one row; 2 leg(s) not checked, counts are a lower bound",
			details: []string{"leg city: 0 open session row(s)", "leg rig:fixture: not checked (no read-only open): proxied-server Dolt, where bd refuses --readonly", "leg binding: not checked (open failed): binding \"infra\": no such provider"},
		},
		{
			name:    "v2 refusal leads an unchecked leg",
			mode:    config.SessionReconcilerV2,
			legs:    []v2SessionLeg{{ref: "binding:infra", store: binding}, proxiedLeg},
			status:  doctor.StatusWarning,
			message: "12 open session row(s): 11 in a state main does not know, 0 shared pool-slot session name(s), 0 named session(s) claimed by more than one row; v2 refuses to boot over them (session reconciler: v2 (set by [daemon] session_reconciler)); 1 leg(s) not checked, counts are a lower bound",
		},
		{
			name:    "failed read blocks under v2",
			mode:    config.SessionReconcilerV2,
			legs:    []v2SessionLeg{{ref: "city", store: censusErrStore{Store: censusStore(), err: readErr}}},
			status:  doctor.StatusError,
			details: []string{"leg city: not checked (read failed after 0 row(s)): listing session beads by type: " + readErr.Error()},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := *cfg
			c.Daemon.SessionReconciler = tc.mode
			check := &v2SessionMigrationCheck{cfg: &c, cityName: "test-city", openLegs: func() []v2SessionLeg { return tc.legs }, lookupEnv: noEnv}
			r := check.Run(&doctor.CheckContext{})
			if r.Status != tc.status || r.Severity != doctor.SeverityBlocking {
				t.Errorf("status = %v severity %v, want %v blocking: %s %q", r.Status, r.Severity, tc.status, r.Message, r.Details)
			}
			if tc.message != "" && r.Message != tc.message {
				t.Errorf("message = %q, want %q", r.Message, tc.message)
			}
			// The last detail names the resolved mode and why.
			resolved := r.Details[len(r.Details)-1]
			wantResolved := "session reconciler: legacy (default fallback: "
			if tc.mode == config.SessionReconcilerV2 {
				wantResolved = "session reconciler: v2 (set by [daemon] session_reconciler)"
			}
			if !strings.HasPrefix(resolved, wantResolved) {
				t.Errorf("resolved-mode detail = %q, want prefix %q", resolved, wantResolved)
			}
			if tc.details != nil && !slices.Equal(r.Details[:len(r.Details)-1], tc.details) {
				t.Errorf("details = %q, want %q", r.Details, tc.details)
			}
		})
	}
}

// Kills: a bd read without --readonly, and any subcommand but list reaching
// bd (bd opens `sql` and most writers writable, on PostgreSQL even under
// --readonly). The argv pinned is the census's exact bd invocation.
func TestV2SessionMigrationCheckNeverOpensWritable(t *testing.T) {
	var argv []string
	run := func(_, name string, args ...string) ([]byte, error) {
		argv = append(argv, name+" "+strings.Join(args, " "))
		return []byte("[]"), nil
	}
	cityPath := t.TempDir()
	leg := v2SessionLeg{ref: "city", store: beads.NewBdStoreWithPrefix(cityPath, readOnlyBdRunner(run), "gc")}
	if _, err := sessionFrontDoor(leg.store).ListAll(session.ListAllOptions{Live: true}); err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	want := []string{
		"bd --readonly list --json --type=session --include-infra --include-gates --limit 0",
		"bd --readonly list --json --label=gc:session --include-infra --include-gates --limit 0",
	}
	if !slices.Equal(argv, want) {
		t.Errorf("bd argv = %q, want %q", argv, want)
	}
	argv = nil
	refused := readOnlyBdRunner(run)
	for _, args := range [][]string{{"sql", "select 1"}, {"update", "gc-1"}, {"show", "gc-1"}, {}} {
		if _, err := refused(cityPath, "bd", args...); !errors.Is(err, errV2SessionNotReadOnly) {
			t.Errorf("bd %q: err = %v, want the refusal", args, err)
		}
	}
	if _, err := refused(cityPath, "dolt", "list"); !errors.Is(err, errV2SessionNotReadOnly) {
		t.Errorf("dolt list: err = %v, want the refusal", err)
	}
	if len(argv) != 0 {
		t.Errorf("refused invocations reached the runner: %q", argv)
	}
}

// Kills: a bd leg that is read through a writable open (a non-bd provider,
// proxied-server Dolt, an unverified backend such as doltlite), and a refusal
// of the two verified backends.
func TestV2SessionBdRefusal(t *testing.T) {
	for _, tc := range []struct {
		provider, backend string
		proxied           bool
		want              string
	}{
		{provider: "bd", backend: "dolt"},
		{provider: "", backend: ""},
		{provider: "exec:/x/gc-beads-bd.sh", backend: "dolt"},
		{provider: "bd", backend: "postgres"},
		{provider: "file", backend: "dolt", want: `provider "file" is not read through bd`},
		{provider: "exec:/x/custom", backend: "dolt", want: `provider "exec:/x/custom" is not read through bd`},
		{provider: "bd", backend: "dolt", proxied: true, want: "proxied-server Dolt, where bd refuses --readonly"},
		{provider: "bd", backend: "doltlite", want: `backend "doltlite" has no verified bd --readonly open`},
	} {
		if got := v2SessionBdRefusal(tc.provider, tc.proxied, tc.backend); got != tc.want {
			t.Errorf("v2SessionBdRefusal(%q, %v, %q) = %q, want %q", tc.provider, tc.proxied, tc.backend, got, tc.want)
		}
	}
}

// Kills: a sessions binding opened writable (a write succeeds, or the database
// file changes across the read), a binding read from somewhere other than the
// database the runtime opens, and a binding with no database yet reported as a
// leg (it holds no rows) instead of skipped.
func TestV2SessionMigrationBindingLegIsReadOnly(t *testing.T) {
	cityPath := t.TempDir()
	cfg := infraSplitConfig(".gc/store")
	target, ok, err := resolveInfraBindingTarget(cityPath, cfg)
	if err != nil || !ok {
		t.Fatalf("resolveInfraBindingTarget = %v, %v", ok, err)
	}
	if legs := openV2SessionLegs(cityPath, cfg); len(legs) == 0 || strings.HasPrefix(legs[0].ref, "binding") {
		t.Fatalf("legs without a binding database = %+v, want no binding leg", legs)
	}
	prefix, err := infraBindingIDPrefix()
	if err != nil {
		t.Fatal(err)
	}
	writer, err := beads.OpenSQLiteStore(target.Dir, beads.WithSQLiteStoreIDPrefix(prefix))
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	if _, err := writer.Create(beads.Bead{Title: "s", Type: session.BeadType, Labels: []string{session.LabelSession}, Metadata: map[string]string{"state": "archived"}}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := closeBeadStoreHandle(writer); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	before, err := os.ReadFile(target.Database)
	if err != nil {
		t.Fatal(err)
	}

	legs := openV2SessionLegs(cityPath, cfg)
	if len(legs) == 0 || legs[0].ref != "binding:infra" || legs[0].store == nil {
		t.Fatalf("legs = %+v, want the binding first", legs)
	}
	rows, err := sessionFrontDoor(legs[0].store).ListAll(session.ListAllOptions{Live: true})
	if err != nil || len(rows) != 1 || rows[0].MetadataState != "archived" {
		t.Fatalf("binding rows = %+v, %v; want the one archived row", rows, err)
	}
	if _, err := legs[0].store.Create(beads.Bead{Title: "w", Type: session.BeadType}); err == nil {
		t.Error("the binding leg accepted a write")
	}
	if err := closeBeadStoreHandle(legs[0].store); err != nil {
		t.Fatalf("close leg: %v", err)
	}
	after, err := os.ReadFile(target.Database)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("the binding database changed across a read-only census")
	}
}

// A city born split on gc init's default work store: the sessions and graph
// classes on the sqlite-beads binding, so an unset session_reconciler resolves
// to v2, and the city and rig scopes on bd's proxied-server Dolt, which bd
// will not open --readonly. Those legs go unchecked because of this check's
// read path, not because anything is wrong with the city. Kills: grading a
// leg whose backend has no read-only open as a blocking failure, which made
// gc doctor exit 1 on every such city.
func TestV2SessionMigrationCheckDoesNotFailADefaultSplitCity(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(t.TempDir(), "r1")
	for _, scope := range []string{cityPath, rigPath} {
		if err := os.MkdirAll(filepath.Join(scope, ".beads"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(scopeMetadataJSONPath(scope), []byte(`{"backend":"dolt","dolt_mode":"proxied-server"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := v2DefaultCity(func(c *config.City) {
		c.Workspace.Name = "demo"
		c.Rigs = []config.Rig{{Name: "r1", Path: rigPath}}
	})
	// The binding database exists, as after the first boot, and holds no row.
	target, ok, err := resolveInfraBindingTarget(cityPath, cfg)
	if err != nil || !ok {
		t.Fatalf("resolveInfraBindingTarget = %v, %v", ok, err)
	}
	prefix, err := infraBindingIDPrefix()
	if err != nil {
		t.Fatal(err)
	}
	writer, err := beads.OpenSQLiteStore(target.Dir, beads.WithSQLiteStoreIDPrefix(prefix))
	if err != nil {
		t.Fatalf("open binding: %v", err)
	}
	if err := closeBeadStoreHandle(writer); err != nil {
		t.Fatalf("close binding: %v", err)
	}

	check := newV2SessionMigrationCheck(cfg, cityPath)
	check.lookupEnv = noEnv
	d := &doctor.Doctor{}
	d.Register(check)
	report := d.RunCollect(&doctor.CheckContext{CityPath: cityPath}, false)
	r := report.Results[0]
	if report.BlockingFailed != 0 || r.Status != doctor.StatusWarning {
		t.Fatalf("status %v, blocking_failed %d, want a warning that does not fail gc doctor: %s %q", r.Status, report.BlockingFailed, r.Message, r.Details)
	}
	want := []string{
		"leg binding:infra: 0 open session row(s)",
		"leg city: not checked (no read-only open): proxied-server Dolt, where bd refuses --readonly",
		"leg rig:r1: not checked (no read-only open): proxied-server Dolt, where bd refuses --readonly",
		"session reconciler: v2 (default: session_reconciler is unset)",
	}
	if !slices.Equal(r.Details, want) {
		t.Errorf("details = %q, want %q", r.Details, want)
	}
	if !strings.HasSuffix(r.Message, "; 2 leg(s) not checked, counts are a lower bound") {
		t.Errorf("message = %q, want the lower-bound note", r.Message)
	}
}

// Kills: a scope leg whose metadata cannot be read graded as a backend with no
// read-only open (it would only warn under v2), even when its provider is also
// one bd does not read, and a proxied-server scope graded as a fault (it would
// block every default split city).
func TestV2SessionScopeLegGradesItsGap(t *testing.T) {
	t.Setenv("GC_BEADS", "")
	t.Setenv("GC_BEADS_BACKEND", "")
	const proxied = "proxied-server Dolt, where bd refuses --readonly"
	for _, tc := range []struct {
		name           string
		cityTOML       string
		metadata       string // "" makes metadata.json a directory, which no read opens
		noReadOnlyOpen bool
		notChecked     string // "" wants the metadata read error, not a refusal
	}{
		{name: "proxied-server", metadata: `{"backend":"dolt","dolt_mode":"proxied-server"}`, noReadOnlyOpen: true, notChecked: proxied},
		{name: "unreadable metadata"},
		{name: "unreadable metadata under a provider bd does not read", cityTOML: "[beads]\nprovider = \"exec:/opt/custom-beads-store\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			if tc.cityTOML != "" {
				if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(tc.cityTOML), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			scope := t.TempDir()
			if err := os.MkdirAll(filepath.Join(scope, ".beads"), 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.metadata == "" {
				if err := os.Mkdir(scopeMetadataJSONPath(scope), 0o755); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(scopeMetadataJSONPath(scope), []byte(tc.metadata), 0o644); err != nil {
				t.Fatal(err)
			}
			leg := openV2SessionScopeLeg(cityPath, &config.City{}, "rig:r1", scope, func(string, string, ...string) ([]byte, error) {
				t.Fatal("a not-checked leg ran bd")
				return nil, nil
			})
			if leg.store != nil || leg.notChecked == "" || leg.noReadOnlyOpen != tc.noReadOnlyOpen {
				t.Fatalf("leg = %+v, want not checked with noReadOnlyOpen %v", leg, tc.noReadOnlyOpen)
			}
			if tc.notChecked != "" && leg.notChecked != tc.notChecked {
				t.Errorf("notChecked = %q, want %q", leg.notChecked, tc.notChecked)
			}
			if tc.notChecked == "" && !strings.Contains(leg.notChecked, "metadata.json") {
				t.Errorf("notChecked = %q, want the metadata read error", leg.notChecked)
			}
		})
	}
}
