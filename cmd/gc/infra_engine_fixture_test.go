package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// mcShapedInfraCity is a work store shaped like maintainer-city's (S5 §3.5:
// 41,786 infrastructure rows, 12,558 not closed, 19,060 dep rows), scaled to
// rows infrastructure rows: messages (half of them wisp-tier), sessions,
// order-tracking rows and formula graphs, with one work bead per ten. Graph
// steps hang off their root by parent-child and chain by blocks; every fifth
// step waits on the previous one through a waits_for gate whose payload the
// source reports. Work beads track a graph step and block on a session, so
// the copy and the clear both meet cross-store edges.
type mcShapedInfraCity struct {
	backing  *beads.MemStore
	rows     []beads.Bead
	deps     []beads.Dep
	payloads map[[2]string]string
	infra    int
	edges    int
}

// sqliteSource is the same city as a real SQLite work store under the
// bead-policy wrapper every production open hands out: indexed reads, a
// payload sidecar, batched lists and deletes. It is written with the per-row
// APIs alone, so the fixture builds on a tree without the batch capabilities.
func (c mcShapedInfraCity) sqliteSource(tb testing.TB) beads.Store {
	tb.Helper()
	opened, err := beads.OpenSQLiteStore(tb.TempDir())
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = closeBeadStoreHandle(opened) })
	if batch, ok := opened.(interface{ CreateWithForeignIDBatch([]beads.Bead) error }); ok {
		for start := 0; start < len(c.rows); start += 1000 {
			if err := batch.CreateWithForeignIDBatch(c.rows[start:min(start+1000, len(c.rows))]); err != nil {
				tb.Fatal(err)
			}
		}
	} else {
		creator := opened.(beads.ForeignIDCreator)
		for _, b := range c.rows {
			if _, err := creator.CreateWithForeignID(b); err != nil {
				tb.Fatal(err)
			}
		}
	}
	writer := opened.(beads.DepMetadataWriter)
	for _, d := range c.deps {
		if err := writer.DepAddWithMetadata(d.IssueID, d.DependsOnID, d.Type, c.payloads[[2]string{d.IssueID, d.DependsOnID}]); err != nil {
			tb.Fatal(err)
		}
	}
	// Borrowed: the migration closes every handle it opens, and the test hands
	// it this one store for each of them.
	return borrowedStoreWithBeadPolicies(opened, &config.City{})
}

// source is the work store as the migration opens it: the edges' payloads
// readable, one at a time and in batches.
func (c mcShapedInfraCity) source() beads.Store {
	return &payloadCarryingSource{Store: c.backing, payloads: c.payloads}
}

func newMCShapedInfraCity(tb testing.TB, infraRows int) mcShapedInfraCity {
	tb.Helper()
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	rows := make([]beads.Bead, 0, infraRows+infraRows/10)
	var deps []beads.Dep
	payloads := map[[2]string]string{}
	status := func(i int) string {
		if i%10 < 3 {
			return "open"
		}
		return "closed"
	}
	add := func(b beads.Bead) {
		b.CreatedAt = base.Add(time.Duration(len(rows)) * time.Second)
		b.UpdatedAt = b.CreatedAt
		rows = append(rows, b)
	}
	var sessions, steps []string
	var root string
	for i := 0; i < infraRows; i++ {
		id := fmt.Sprintf("mc-%06d", i)
		switch {
		case i%20 < 10: // messages, half wisp-tier
			add(beads.Bead{ID: id, Title: "context cycle", Type: "message", Status: status(i), Ephemeral: i%2 == 0, Assignee: "mayor", From: "deacon"})
		case i%20 < 14: // sessions
			add(beads.Bead{ID: id, Title: "session", Type: "session", Status: status(i), Labels: []string{"gc:session"}, Metadata: beads.StringMap{"session_id": id}})
			sessions = append(sessions, id)
		case i%20 < 15: // order-tracking
			add(beads.Bead{ID: id, Title: "order run", Type: "task", Status: status(i), Labels: []string{"order-tracking"}})
		default: // graph: a root every sixth row, then its steps
			if root == "" || i%6 == 0 {
				add(beads.Bead{ID: id, Title: "workflow root", Type: "molecule", Status: status(i), Metadata: beads.StringMap{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}})
				root, steps = id, nil
				continue
			}
			add(beads.Bead{ID: id, Title: "step", Type: "task", Status: status(i), Metadata: beads.StringMap{beadmeta.RootBeadIDMetadataKey: root}})
			deps = append(deps, beads.Dep{IssueID: id, DependsOnID: root, Type: "parent-child"})
			if len(steps) > 0 {
				prev := steps[len(steps)-1]
				if len(steps)%5 == 0 {
					deps = append(deps, beads.Dep{IssueID: id, DependsOnID: prev, Type: "waits-for"})
					payloads[[2]string{id, prev}] = `{"gate":"any-children"}`
				} else {
					deps = append(deps, beads.Dep{IssueID: id, DependsOnID: prev, Type: "blocks"})
				}
			}
			steps = append(steps, id)
		}
	}
	for i := 0; i < infraRows/10; i++ {
		id := fmt.Sprintf("mc-w%05d", i)
		add(beads.Bead{ID: id, Title: "work", Type: "task", Status: status(i)})
		if len(steps) > 0 && i%3 == 0 {
			deps = append(deps, beads.Dep{IssueID: id, DependsOnID: steps[i%len(steps)], Type: "tracks"})
		}
		if len(sessions) > 0 && i%3 == 1 {
			deps = append(deps, beads.Dep{IssueID: id, DependsOnID: sessions[i%len(sessions)], Type: "related"})
		}
	}
	return mcShapedInfraCity{backing: beads.NewMemStoreFrom(len(rows), rows, deps), rows: rows, deps: deps, payloads: payloads, infra: infraRows, edges: len(deps)}
}
