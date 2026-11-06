package main

import (
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// The S4b equivalence proof for the lift (S5-D): planRigImport, now a row
// selection over the shared crossBoundaryPlan, plans exactly what the
// pre-lift planner (rig_import_plan_reference_test.go) planned, on a corpus
// that walks every edge shape both directions can take, the shapes S4b's own
// end-to-end tests never build included: an in-edge from a retained or
// colliding infrastructure row, an edge onto a row the binding holds that the
// rig does not, a conditional-blocks or membership edge onto a blocker held
// elsewhere, an unreadable elsewhere, an absent blocker, an outcome-blocked
// closed blocker, and a dependent the binding already carried.

var equivalenceEdgeTypes = []string{"blocks", "waits-for", "conditional-blocks", "tracks", "parent-child", "related", "discovered-from"}

// equivalenceStatus is one status a corpus row takes: open, closed, or closed
// with a work outcome that does not release dependents.
type equivalenceStatus int

const (
	eqOpen equivalenceStatus = iota
	eqClosed
	eqClosedBlocked
	eqStatusCount
)

func (s equivalenceStatus) apply(b *beads.Bead) {
	switch s {
	case eqOpen:
		b.Status = "open"
	case eqClosed:
		b.Status = "closed"
	case eqClosedBlocked:
		b.Status = "closed"
		b.Metadata[beadmeta.WorkOutcomeMetadataKey] = beadmeta.WorkOutcomeBlocked
	}
}

var equivalenceBase = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

func equivalenceRow(id string, infra bool, s equivalenceStatus, n int) beads.Bead {
	b := beads.Bead{ID: id, Title: id, Type: "task", Metadata: beads.StringMap{}, CreatedAt: equivalenceBase.Add(time.Duration(n) * time.Second)}
	if infra {
		b.Metadata[beadmeta.KindMetadataKey] = beadmeta.KindWorkflow
	}
	s.apply(&b)
	return b
}

// equivalenceCity builds one rig's planning context, row by row.
type equivalenceCity struct {
	ctx  rigPlanContext
	rows []beads.Bead
	down map[string][]beads.Dep
	n    int
}

func newEquivalenceCity(manifested bool) *equivalenceCity {
	c := &equivalenceCity{down: map[string][]beads.Dep{}}
	c.ctx = rigPlanContext{Rig: "gp", CityName: "mc", Held: map[string]beads.Bead{}, Elsewhere: map[string][]beads.Bead{}}
	if manifested {
		c.ctx.Manifest = &infraCopyManifest{IDs: map[string]bool{}, Created: map[string]time.Time{}, Generation: coordclass.CurrentGeneration}
	}
	return c
}

func (c *equivalenceCity) add(id string, infra bool, s equivalenceStatus) beads.Bead {
	c.n++
	b := equivalenceRow(id, infra, s, c.n)
	c.rows = append(c.rows, b)
	return b
}

// retained adds a row an earlier import carried: the manifest records it and
// the binding holds it under this rig's stamp.
func (c *equivalenceCity) retained(id string, s equivalenceStatus) {
	b := c.add(id, true, s)
	if c.ctx.Manifest == nil {
		c.ctx.Manifest = &infraCopyManifest{IDs: map[string]bool{}, Created: map[string]time.Time{}, Generation: coordclass.CurrentGeneration}
	}
	c.ctx.Manifest.IDs[id], c.ctx.Manifest.Created[id] = true, b.CreatedAt
	h := b
	h.Metadata = beads.StringMap{infraMigrationStampKey: rigImportStamp("gp")}
	for k, v := range b.Metadata {
		h.Metadata[k] = v
	}
	c.ctx.Held[id] = h
}

// collided adds an infrastructure row whose id the binding holds as another
// bead.
func (c *equivalenceCity) collided(id string, s equivalenceStatus) {
	c.add(id, true, s)
	c.ctx.Held[id] = beads.Bead{ID: id, Title: "someone else", Status: "open", Metadata: beads.StringMap{}}
}

func (c *equivalenceCity) edge(from, to, kind string) {
	c.down[from] = append(c.down[from], beads.Dep{IssueID: from, DependsOnID: to, Type: kind})
}

func (c *equivalenceCity) build() rigPlanContext {
	snap := rigSnapshot{byID: map[string]beads.Bead{}, down: map[string][]beads.Dep{}}
	snap.rows = append([]beads.Bead(nil), c.rows...)
	sort.Slice(snap.rows, func(i, j int) bool { return snap.rows[i].ID < snap.rows[j].ID })
	for _, b := range snap.rows {
		snap.byID[b.ID] = b
		snap.down[b.ID] = c.down[b.ID]
	}
	ctx := c.ctx
	ctx.Snap = snap
	return ctx
}

// equivalenceTargets are the far ends an edge of a moved row can have.
var equivalenceTargets = []string{"work", "moved", "retained", "collided", "external", "held", "elsewhere", "elsewhere-unread", "absent"}

// equivalenceSingleEdgeCorpus is one context per (direction, edge type, far
// end, both statuses): every shape one boundary edge can take.
func equivalenceSingleEdgeCorpus() []rigPlanContext {
	var out []rigPlanContext
	for _, kind := range equivalenceEdgeTypes {
		for _, target := range equivalenceTargets {
			for ds := equivalenceStatus(0); ds < eqStatusCount; ds++ {
				for ws := equivalenceStatus(0); ws < eqStatusCount; ws++ {
					for _, dir := range []string{"out", "in"} {
						c := newEquivalenceCity(false)
						c.add("gp-d", true, ds)
						far := "gp-x"
						switch target {
						case "work":
							c.add(far, false, ws)
						case "moved":
							c.add(far, true, ws)
						case "retained":
							c.retained(far, ws)
						case "collided":
							c.collided(far, ws)
						case "external":
							far = "external:ga-1"
						case "held":
							far = "mc-held"
							h := equivalenceRow(far, true, ws, 99)
							h.Metadata[infraMigrationStampKey] = "work"
							c.ctx.Held[far] = h
						case "elsewhere":
							far = "ga-other"
							c.ctx.Elsewhere[far] = []beads.Bead{equivalenceRow(far, false, ws, 98)}
						case "elsewhere-unread":
							far = "ga-other"
							c.ctx.ElsewhereUnread = []string{"rig ga: unreachable"}
						case "absent":
							far = "ga-gone"
						}
						switch {
						case dir == "out":
							c.edge("gp-d", far, kind)
						case target == "work" || target == "moved" || target == "retained" || target == "collided":
							c.edge(far, "gp-d", kind)
						default:
							continue
						}
						out = append(out, c.build())
					}
				}
			}
		}
	}
	return out
}

// equivalenceRandomCorpus mixes many rows and edges per context, so the
// planner meets the shapes together: several edges on one row, records to
// sort, refusals in order, a dependent the binding already carried.
func equivalenceRandomCorpus(seeds int) []rigPlanContext {
	var out []rigPlanContext
	for seed := 0; seed < seeds; seed++ {
		r := rand.New(rand.NewSource(int64(seed)))
		c := newEquivalenceCity(r.Intn(3) == 0)
		var ids []string
		for i := 0; i < 3+r.Intn(10); i++ {
			id := fmt.Sprintf("gp-%c%d", 'a'+rune(r.Intn(6)), i)
			s := equivalenceStatus(r.Intn(int(eqStatusCount)))
			switch r.Intn(6) {
			case 0, 1:
				c.add(id, false, s)
			case 2, 3:
				c.add(id, true, s)
			case 4:
				c.retained(id, s)
			default:
				c.collided(id, s)
			}
			ids = append(ids, id)
		}
		far := []string{"external:ga-9", "ga-elsewhere", "ga-gone", "mc-held"}
		h := equivalenceRow("mc-held", true, equivalenceStatus(r.Intn(int(eqStatusCount))), 77)
		h.Metadata[infraMigrationStampKey] = "work"
		c.ctx.Held["mc-held"] = h
		if r.Intn(2) == 0 {
			c.ctx.Elsewhere["ga-elsewhere"] = []beads.Bead{
				equivalenceRow("ga-elsewhere", false, equivalenceStatus(r.Intn(int(eqStatusCount))), 70),
				equivalenceRow("ga-elsewhere", false, equivalenceStatus(r.Intn(int(eqStatusCount))), 71),
			}
		}
		if r.Intn(4) == 0 {
			c.ctx.ElsewhereUnread = []string{"rig ga: unreachable"}
		}
		for i := 0; i < r.Intn(20); i++ {
			from := ids[r.Intn(len(ids))]
			to := ids[r.Intn(len(ids))]
			if r.Intn(3) == 0 {
				to = far[r.Intn(len(far))]
			}
			if to == from {
				continue
			}
			c.edge(from, to, equivalenceEdgeTypes[r.Intn(len(equivalenceEdgeTypes))])
		}
		out = append(out, c.build())
	}
	return out
}

// equivalenceShape names the branch a plan reached, for the coverage check.
func equivalenceShapes(plan rigImportPlan) []string {
	var shapes []string
	for _, recs := range plan.Records {
		for _, r := range recs {
			shapes = append(shapes, r.Dir+":"+r.Action)
			if r.OtherStatus == "absent from every store" {
				shapes = append(shapes, "absent")
			}
		}
	}
	for _, f := range plan.Refusals {
		for _, key := range []string{"membership edge", "encodes only blocks", "cannot hold a cross-store gate", "could not be read", "held in another store"} {
			if strings.Contains(f.Reason, key) {
				shapes = append(shapes, "refused:"+key)
			}
		}
	}
	if len(plan.Gates) > 0 {
		shapes = append(shapes, "gate")
	}
	for _, e := range plan.Copy {
		if strings.HasPrefix(e.DependsOnID, "mc-") {
			shapes = append(shapes, "copied onto held")
		}
	}
	return shapes
}

// TestCrossBoundaryPlanLiftIsEquivalentForTheRigImport is the S4b
// equivalence: the lifted planner and the pre-lift one plan the same, field
// for field, over the whole corpus, and the corpus reaches every branch.
func TestCrossBoundaryPlanLiftIsEquivalentForTheRigImport(t *testing.T) {
	corpus := append(equivalenceSingleEdgeCorpus(), equivalenceRandomCorpus(3000)...)
	seen := map[string]int{}
	for i, ctx := range corpus {
		got, want := planRigImport(ctx), refPlanRigImport(ctx)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("context %d plans differently after the lift:\n got  %+v\n want %+v", i, got, want)
		}
		for _, s := range equivalenceShapes(got) {
			seen[s]++
		}
	}
	for _, shape := range []string{
		"out:" + rigEdgeDropped, "out:" + rigEdgeGated, "out:" + rigEdgeExternal, "in:" + rigEdgeDropped, "absent", "gate", "copied onto held",
		"refused:membership edge", "refused:encodes only blocks", "refused:cannot hold a cross-store gate", "refused:could not be read", "refused:held in another store",
	} {
		if seen[shape] == 0 {
			t.Errorf("the corpus never reaches %q, so the equivalence says nothing about it", shape)
		}
	}
	if len(corpus) < 1000 {
		t.Fatalf("corpus of %d contexts is too small", len(corpus))
	}
}
