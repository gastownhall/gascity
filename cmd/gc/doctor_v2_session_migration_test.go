package main

import (
	"bytes"
	"errors"
	"os"
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
// than ten IDs, a not-checked or failed leg that reads as a clean census or
// does not block under v2, a legacy not-checked leg that fails the sweep,
// and a refusal that warns under legacy or passes under v2.
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
			name:   "not checked blocks under v2",
			mode:   config.SessionReconcilerV2,
			legs:   []v2SessionLeg{{ref: "rig:fixture", notChecked: "proxied-server Dolt, where bd refuses --readonly"}},
			status: doctor.StatusError,
		},
		{
			name:    "not checked warns under legacy",
			legs:    []v2SessionLeg{{ref: "city", store: censusStore()}, {ref: "rig:fixture", notChecked: "proxied-server Dolt, where bd refuses --readonly"}},
			status:  doctor.StatusWarning,
			message: "0 open session row(s): 0 in a state main does not know, 0 shared pool-slot session name(s), 0 named session(s) claimed by more than one row; 1 leg(s) not checked, counts are a lower bound",
			details: []string{"leg city: 0 open session row(s)", "leg rig:fixture: not checked (no read-only open): proxied-server Dolt, where bd refuses --readonly"},
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
			check := &v2SessionMigrationCheck{cfg: &c, cityName: "test-city", openLegs: func() []v2SessionLeg { return tc.legs }}
			r := check.Run(&doctor.CheckContext{})
			if r.Status != tc.status || r.Severity != doctor.SeverityBlocking {
				t.Errorf("status = %v severity %v, want %v blocking: %s %q", r.Status, r.Severity, tc.status, r.Message, r.Details)
			}
			if tc.message != "" && r.Message != tc.message {
				t.Errorf("message = %q, want %q", r.Message, tc.message)
			}
			if tc.details != nil && !slices.Equal(r.Details, tc.details) {
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
