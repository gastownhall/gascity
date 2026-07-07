package main

import (
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// TestSessionSnapshotInfoEquivalence is the byte-identical oracle for P3 of
// NONWORK-BEAD-FIELDDOOR-PLAN.md. The snapshot grows typed session.Info
// accessors (OpenInfos / FindInfoByID / FindInfoByTemplate /
// FindInfoByNamedIdentity) ADDITIVELY alongside the existing raw-bead methods.
//
// Each Info accessor must return exactly InfoFromPersistedBead(b) for the same
// bead b the corresponding bead method returns. Proving that here keeps the P4
// consumer migration safe: a consumer can swap Open()/Find*Bead for the Info
// form without any change in meaning. Any divergence is a real lockstep or
// codec bug.
func TestSessionSnapshotInfoEquivalence(t *testing.T) {
	beadsIn := []beads.Bead{
		{
			ID:     "ga-pool",
			Type:   session.BeadType,
			Title:  "worker",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":     "frontend/worker",
				"agent_name":   "frontend/worker-1",
				"pool_managed": "true",
				"pool_slot":    "1",
				"state":        "awake",
				"session_name": "worker-ga-pool",
			},
		},
		{
			ID:     "ga-named",
			Type:   session.BeadType,
			Title:  "mayor",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":                  "mayor",
				"configured_named_session":  "true",
				"configured_named_identity": "mayor",
				"session_name":              "mayor-session",
				"state":                     "active",
			},
		},
		{
			ID:     "ga-common",
			Type:   session.BeadType,
			Title:  "deacon",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":     "deacon",
				"common_name":  "the-deacon",
				"session_name": "deacon-session",
			},
		},
		{
			ID:     "ga-other-template",
			Type:   session.BeadType,
			Title:  "polecat",
			Labels: []string{session.LabelSession, "agent:polecat"},
			Metadata: map[string]string{
				"template":     "polecat",
				"session_name": "polecat-session",
			},
		},
		{
			ID:     "ga-empty-sn",
			Type:   session.BeadType,
			Title:  "no-session-name",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template": "scribe",
			},
		},
		{
			ID:     "ga-closed",
			Type:   session.BeadType,
			Title:  "closed",
			Status: "closed",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":                  "worker",
				"configured_named_identity": "ghost",
				"session_name":              "ghost-session",
			},
		},
	}

	snap := newSessionBeadSnapshot(beadsIn)

	// OpenInfos mirrors Open one-for-one (closed bead filtered out of both).
	open := snap.Open()
	openInfos := snap.OpenInfos()
	if len(openInfos) != len(open) {
		t.Fatalf("len(OpenInfos) = %d, want len(Open) = %d", len(openInfos), len(open))
	}
	for i := range open {
		want := session.InfoFromPersistedBead(open[i])
		if !reflect.DeepEqual(openInfos[i], want) {
			t.Errorf("OpenInfos[%d] = %#v, want InfoFromPersistedBead(Open()[%d]) = %#v",
				i, openInfos[i], i, want)
		}
	}

	// The closed bead must not leak into the typed view either.
	for i, info := range openInfos {
		if info.ID == "ga-closed" {
			t.Errorf("OpenInfos[%d] surfaced the closed bead ga-closed", i)
		}
	}

	// FindInfoByID agrees with FindByID on hits and misses.
	for _, id := range []string{"ga-pool", "ga-named", "ga-common", "ga-empty-sn", "ga-closed", "missing", ""} {
		bead, ok := snap.FindByID(id)
		info, ok2 := snap.FindInfoByID(id)
		if ok != ok2 {
			t.Errorf("FindByID(%q) ok=%v but FindInfoByID ok=%v", id, ok, ok2)
			continue
		}
		if ok {
			want := session.InfoFromPersistedBead(bead)
			if !reflect.DeepEqual(info, want) {
				t.Errorf("FindInfoByID(%q) = %#v, want %#v", id, info, want)
			}
		}
	}

	// FindInfoByTemplate agrees with FindSessionBeadByTemplate. Covers an
	// agent-name index hit, a template-hint hit, a common-name hit, and a miss.
	for _, template := range []string{"frontend/worker-1", "mayor", "the-deacon", "deacon", "polecat", "nope", ""} {
		bead, ok := snap.FindSessionBeadByTemplate(template)
		info, ok2 := snap.FindInfoByTemplate(template)
		if ok != ok2 {
			t.Errorf("FindSessionBeadByTemplate(%q) ok=%v but FindInfoByTemplate ok=%v", template, ok, ok2)
			continue
		}
		if ok {
			want := session.InfoFromPersistedBead(bead)
			if !reflect.DeepEqual(info, want) {
				t.Errorf("FindInfoByTemplate(%q) = %#v, want %#v", template, info, want)
			}
		}
	}

	// FindInfoByNamedIdentity agrees with FindSessionBeadByNamedIdentity. The
	// closed bead's identity (ghost) must miss in both.
	for _, identity := range []string{"mayor", "ghost", "absent", ""} {
		bead, ok := snap.FindSessionBeadByNamedIdentity(identity)
		info, ok2 := snap.FindInfoByNamedIdentity(identity)
		if ok != ok2 {
			t.Errorf("FindSessionBeadByNamedIdentity(%q) ok=%v but FindInfoByNamedIdentity ok=%v", identity, ok, ok2)
			continue
		}
		if ok {
			want := session.InfoFromPersistedBead(bead)
			if !reflect.DeepEqual(info, want) {
				t.Errorf("FindInfoByNamedIdentity(%q) = %#v, want %#v", identity, info, want)
			}
		}
	}

	// add() must keep open and openInfos in lockstep.
	snap.add(beads.Bead{
		ID:     "ga-added",
		Type:   session.BeadType,
		Title:  "added",
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"template":     "added-template",
			"session_name": "added-session",
		},
	})
	open = snap.Open()
	openInfos = snap.OpenInfos()
	if len(openInfos) != len(open) {
		t.Fatalf("after add: len(OpenInfos) = %d, want len(Open) = %d", len(openInfos), len(open))
	}
	for i := range open {
		want := session.InfoFromPersistedBead(open[i])
		if !reflect.DeepEqual(openInfos[i], want) {
			t.Errorf("after add: OpenInfos[%d] diverged from InfoFromPersistedBead(Open()[%d])", i, i)
		}
	}
	addedBead, ok := snap.FindByID("ga-added")
	addedInfo, ok2 := snap.FindInfoByID("ga-added")
	if !ok || !ok2 {
		t.Fatalf("after add: FindByID ok=%v FindInfoByID ok=%v, want both true", ok, ok2)
	}
	if !reflect.DeepEqual(addedInfo, session.InfoFromPersistedBead(addedBead)) {
		t.Errorf("after add: FindInfoByID(ga-added) diverged from InfoFromPersistedBead")
	}
}
