package main

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
)

// The started-work policy of every release path is one table.
func TestReleasePoliciesTable(t *testing.T) {
	want := map[releaseWhy]releasePolicy{
		releaseKilledSeat:     {statuses: []string{"open", "in_progress"}, startedKeeps: true, lane: laneDirect, seatRoute: true},
		releaseDrainAck:       {statuses: []string{"in_progress"}, lane: laneCarried},
		releaseCloseCascade:   {statuses: []string{"open", "in_progress"}, lane: laneCarried, seatRoute: true},
		releaseStrandedRepair: {statuses: []string{"open", "in_progress"}, lane: laneCarried, seatRoute: true},
		releaseOrphan:         {statuses: []string{"open", "in_progress"}, lane: laneDirect, pool: true},
		releaseDeadAssignee:   {statuses: []string{"open", "in_progress"}, lane: laneDirect, pool: true},
		releaseRetired:        {statuses: []string{"open", "in_progress"}, lane: laneCarried, seatRoute: true},
	}
	if !reflect.DeepEqual(releasePolicies, want) {
		t.Fatalf("releasePolicies = %+v\nwant %+v", releasePolicies, want)
	}
}

// releaseWritesOnlyIn are the production functions that may call a release
// write: the verb's body.
var releaseWritesOnlyIn = []string{"work_release.go:releaseListed"}

// Every release path goes through ReleaseClaims: no other production code
// calls a release write.
func TestReleaseWritesOnlyThroughTheVerb(t *testing.T) {
	root := repoRootForLint(t)
	paths, err := filepath.Glob(filepath.Join(root, "cmd/gc", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var sites []string
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if name := calleeName(call.Fun); name == "ReleaseWorkBead" || name == "releaseOrphanedPoolAssignment" {
						sites = append(sites, filepath.Base(p)+":"+fn.Name.Name)
					}
				}
				return true
			})
		}
	}
	slices.Sort(sites)
	sites = slices.Compact(sites)
	if !slices.Equal(sites, releaseWritesOnlyIn) {
		t.Fatalf("release writes in %v, want only %v: release through ReleaseClaims", sites, releaseWritesOnlyIn)
	}
}

// strandCase is one row of NEW-5's strand table (mc-zndi7.87): a claim no
// lane would serve once released, except "served", the control.
type strandCase struct {
	name     string
	meta     map[string]string
	released bool
}

var strandCases = []strandCase{
	{"served", map[string]string{beadmeta.RoutedToMetadataKey: "repo/other"}, true},
	{"unserved route", map[string]string{beadmeta.RoutedToMetadataKey: "nowhere/lane"}, false},
	{"expanded root, no route", map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow, beadmeta.WorkflowExpandedMetadataKey: "true"}, false},
	{"expanded root, served run_target", map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow, beadmeta.WorkflowExpandedMetadataKey: "true", beadmeta.RunTargetMetadataKey: "repo/worker"}, false},
}

// strandCity is a city serving repo/worker and repo/other, with one seat.
type strandCity struct {
	path  string
	cfg   *config.City
	store *beads.MemStore
	seat  beads.Bead
	claim map[string]beads.Bead // by strandCase name
	rec   *events.Fake
}

func newStrandCity(t *testing.T, status string, seatMeta map[string]string) *strandCity {
	t.Helper()
	c := &strandCity{path: t.TempDir(), store: beads.NewMemStore(), claim: map[string]beads.Bead{}, rec: events.NewFake()}
	c.cfg = &config.City{Workspace: config.Workspace{Name: "strand-town"}, Agents: []config.Agent{persistentWorker(), otherLane()}}
	meta := map[string]string{"session_name": "repo--worker-1", "template": "repo/worker", "pool_managed": "true", "state": "asleep"}
	for k, v := range seatMeta {
		meta[k] = v
	}
	seat, err := c.store.Create(beads.Bead{Title: "seat", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: meta})
	if err != nil {
		t.Fatal(err)
	}
	c.seat = seat
	for _, sc := range strandCases {
		c.claim[sc.name] = createWork(t, c.store, status, seat.ID, sc.meta)
	}
	return c
}

func (c *strandCity) sw() *SeatWork {
	sw := testSeatWork(c.path, c.cfg, c.store, nil)
	sw.alerts = &unservedClaimAlerts{rec: c.rec}
	return sw
}

func (c *strandCity) info() session.Info {
	b, _ := c.store.Get(c.seat.ID)
	return seedSessionInfo(b)
}

// assertStrandTable checks every row: the control released, every strand
// kept assigned to the seat with its status.
func (c *strandCity) assertStrandTable(t *testing.T, status string, rows ...strandCase) {
	t.Helper()
	if len(rows) == 0 {
		rows = strandCases
	}
	for _, sc := range rows {
		got, err := c.store.Get(c.claim[sc.name].ID)
		if err != nil {
			t.Fatal(err)
		}
		if sc.released && (got.Status != "open" || got.Assignee != "") {
			t.Errorf("%s: status=%q assignee=%q, want released", sc.name, got.Status, got.Assignee)
		}
		if !sc.released && (got.Status != status || got.Assignee != c.seat.ID) {
			t.Errorf("%s: status=%q assignee=%q, want kept %s by %s", sc.name, got.Status, got.Assignee, status, c.seat.ID)
		}
	}
}

// unservedIDs are the claims a seat keeps, sorted.
func (c *strandCity) unservedIDs() []string {
	var ids []string
	for _, sc := range strandCases {
		if !sc.released {
			ids = append(ids, c.claim[sc.name].ID)
		}
	}
	slices.Sort(ids)
	return ids
}

// assertAlerted checks one session.unserved_claims naming the kept claims.
func (c *strandCity) assertAlerted(t *testing.T, reason string, n int) {
	t.Helper()
	var got []api.SessionUnservedClaimsPayload
	for _, e := range c.rec.Events {
		if e.Type == events.SessionUnservedClaims {
			var p api.SessionUnservedClaimsPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatal(err)
			}
			got = append(got, p)
		}
	}
	if len(got) != n {
		t.Fatalf("session.unserved_claims events = %d, want %d", len(got), n)
	}
	if n > 0 && (got[0].Reason != reason || got[0].Refusal != "unserved-claims" || !slices.Equal(got[0].WorkBeadIDs, c.unservedIDs())) {
		t.Fatalf("alert = %+v, want reason %q and beads %v", got[0], reason, c.unservedIDs())
	}
}

// NEW-5, every row on every path: each path releases the served claim and
// keeps every claim no lane would serve.
func TestReleasePathsKeepEveryStrand(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	t.Run("killed seat", func(t *testing.T) {
		c := newStrandCity(t, "open", map[string]string{"sleep_reason": string(session.SleepReasonKilled), "slept_at": now.Add(-time.Minute).Format(time.RFC3339)})
		releaseUnexecutedClaimsOnKill(c.sw(), c.store, now, c.info(), io.Discard)
		c.assertStrandTable(t, "open")
		c.assertAlerted(t, "killed", 1)
	})
	t.Run("drain-ack", func(t *testing.T) {
		c := newStrandCity(t, "in_progress", nil)
		releaseUnexecutedClaimsOnDrainAck(testWorkLegs(c.path, c.cfg, c.store, nil), c.seat, time.Minute, io.Discard)
		c.assertStrandTable(t, "in_progress")
	})
	t.Run("stranded repair", func(t *testing.T) {
		c := newStrandCity(t, "in_progress", map[string]string{strandedEventEmittedKey: now.Add(-strandedRepairConfirmGrace - time.Minute).Format(time.RFC3339)})
		var stderr bytes.Buffer
		if repairStrandedPoolWorkerBead(c.sw(), c.store, c.info(), &clock.Fake{Time: now}, &stderr) {
			t.Fatal("the repair closed a seat holding claims no lane serves")
		}
		c.assertStrandTable(t, "in_progress")
		c.assertAlerted(t, strandedRepairCloseReason, 1)
		if !strings.Contains(stderr.String(), "unserved-claims") {
			t.Fatalf("stderr = %q, want the unserved-claims refusal", stderr.String())
		}
	})
	t.Run("confirmed orphan", func(t *testing.T) {
		c := newStrandCity(t, "open", nil)
		var assigned []beads.Bead
		for _, sc := range strandCases {
			assigned = append(assigned, c.claim[sc.name])
		}
		releaseConfirmedOrphanSessionWork(c.cfg, c.store, nil, assigned, nil, c.info())
		c.assertStrandTable(t, "open")
	})
	t.Run("dead assignee", func(t *testing.T) {
		c := newStrandCity(t, "open", nil)
		var assigned []beads.Bead
		for _, sc := range strandCases {
			assigned = append(assigned, c.claim[sc.name])
		}
		// No open session holds the claims: their assignee is dead.
		if err := c.store.Close(c.seat.ID); err != nil {
			t.Fatal(err)
		}
		releaseOrphanedPoolAssignments(c.store, beads.SessionStore{Store: c.store}, c.cfg, c.path, nil, assigned, nil, nil, nil, nil, nil)
		c.assertStrandTable(t, "open")
	})
	t.Run("retired session", func(t *testing.T) {
		c := newStrandCity(t, "in_progress", nil)
		unclaimWorkAssignedToRetiredSessionBead(testWorkLegs(c.path, c.cfg, c.store, nil), c.store, c.seat, "", io.Discard)
		c.assertStrandTable(t, "in_progress")
	})
}

// The close cascade's close refuses over claims no lane serves (O5), alerting
// once per episode; with only served claims it closes and releases them.
func TestCloseRefusesUnservedClaimsAndAlertsOncePerEpisode(t *testing.T) {
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	c := newStrandCity(t, "open", nil)
	sw := c.sw()
	for i := 0; i < 2; i++ {
		var stderr bytes.Buffer
		if closeBead(c.store, sw, c.info(), "orphaned", now, &stderr) {
			t.Fatal("the close landed over claims no lane serves")
		}
		if !strings.Contains(stderr.String(), "unserved-claims") {
			t.Fatalf("stderr = %q, want the unserved-claims refusal", stderr.String())
		}
	}
	c.assertStrandTable(t, "open", strandCases[1:]...)
	if got, _ := c.store.Get(c.claim["served"].ID); got.Assignee != c.seat.ID {
		t.Fatal("a refused close released the served claim")
	}
	c.assertAlerted(t, "orphaned", 1)

	// The episode ends when no unserved claim is left: the close lands, the
	// served claim is released, and a later episode alerts again.
	for _, sc := range strandCases[1:] {
		closeWork(t, c.store, c.claim[sc.name])
	}
	if !closeBead(c.store, testSeatWork(c.path, c.cfg, c.store, nil), c.info(), "orphaned", now, io.Discard) {
		t.Fatal("the close refused with only a served claim")
	}
	c.assertStrandTable(t, "open", strandCases[0])
	sw.alerts.refuse(releaseSeatOfBead(c.seat), "orphaned", c.unservedIDs())
	c.assertAlerted(t, "orphaned", 1) // same set, same episode: no repeat
	sw.alerts.closed(c.seat.ID)
	sw.alerts.refuse(releaseSeatOfBead(c.seat), "orphaned", c.unservedIDs())
	c.assertAlerted(t, "orphaned", 2)
}

// Route recovery restores a workflow root's run_target only while the root is
// unexpanded: an expanded root is neither demanded nor claimable (#5900).
func TestCarriedPoolRouteSkipsAnExpandedRoot(t *testing.T) {
	root := beads.Bead{Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow, beadmeta.RunTargetMetadataKey: "repo/worker"}}
	if got := carriedPoolRoute(root); got != "repo/worker" {
		t.Fatalf("unexpanded root's carried route = %q, want repo/worker", got)
	}
	root.Metadata[beadmeta.WorkflowExpandedMetadataKey] = "true"
	if got := carriedPoolRoute(root); got != "" {
		t.Fatalf("expanded root's carried route = %q, want none: recovery would resurrect it", got)
	}
}
