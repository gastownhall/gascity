package main

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// seedSessionBeads populates a Store with the given number of open and
// closed session beads. Open beads carry a fresh session_name and template
// so newSessionBeadSnapshot's identity indexes get exercised the same way
// as in production.
func seedSessionBeads(tb testing.TB, store beads.Store, openCount, closedCount int) {
	tb.Helper()
	for i := 0; i < openCount; i++ {
		bead, err := store.Create(beads.Bead{
			Title:  fmt.Sprintf("open session %d", i),
			Type:   session.BeadType,
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"session_name": fmt.Sprintf("agent-open-%d", i),
				"template":     fmt.Sprintf("template-open-%d", i),
			},
		})
		if err != nil {
			tb.Fatalf("seed open session bead %d: %v", i, err)
		}
		_ = bead
	}
	for i := 0; i < closedCount; i++ {
		bead, err := store.Create(beads.Bead{
			Title:  fmt.Sprintf("closed session %d", i),
			Type:   session.BeadType,
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"session_name": fmt.Sprintf("agent-closed-%d", i),
				"template":     fmt.Sprintf("template-closed-%d", i),
			},
		})
		if err != nil {
			tb.Fatalf("seed closed session bead %d: %v", i, err)
		}
		if err := store.Close(bead.ID); err != nil {
			tb.Fatalf("close session bead %d: %v", i, err)
		}
	}
}

// BenchmarkLoadSessionBeadSnapshot_LargeStore exercises the hot-path
// snapshot loader against a store dominated by closed session beads. After
// the IncludeClosed drop in loadSessionBeadSnapshot, runtime should scale
// with the open count, not the open+closed total.
func BenchmarkLoadSessionBeadSnapshot_LargeStore(b *testing.B) {
	store := beads.NewMemStore()
	seedSessionBeads(b, store, 50, 5000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		snap, err := loadSessionBeadSnapshot(store)
		if err != nil {
			b.Fatal(err)
		}
		if got := len(snap.Open()); got != 50 {
			b.Fatalf("Open()=%d, want 50", got)
		}
	}
}

// BenchmarkLoadSessionBeadSnapshot_OpenOnlyBaseline establishes a control
// for BenchmarkLoadSessionBeadSnapshot_LargeStore: same open count, no
// closed history. The two benchmarks should report comparable ns/op.
func BenchmarkLoadSessionBeadSnapshot_OpenOnlyBaseline(b *testing.B) {
	store := beads.NewMemStore()
	seedSessionBeads(b, store, 50, 0)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		snap, err := loadSessionBeadSnapshot(store)
		if err != nil {
			b.Fatal(err)
		}
		if got := len(snap.Open()); got != 50 {
			b.Fatalf("Open()=%d, want 50", got)
		}
	}
}

// TestLoadSessionBeadSnapshot_IncludesTypedBeadsWithoutLabel guards against
// the regression where canonical configured_named_session beads that have
// lost their gc:session label (observed in production after crashes /
// schema migrations) become invisible to the reconciler. Such beads still
// carry issue_type=session and IsSessionBeadOrRepairable accepts them; the
// snapshot loader must surface them so the reconciler can heal their
// state=awake → state=asleep transition once the runtime is gone. Without
// this, the bead lives forever holding its alias reservation and the pool
// cannot materialize a fresh session for the same template ("alias …
// already belongs to gm-XXXX").
func TestLoadSessionBeadSnapshot_IncludesTypedBeadsWithoutLabel(t *testing.T) {
	store := beads.NewMemStore()
	// Bead with proper Type but NO labels — the production failure mode for
	// canonical configured_named_session beads after a crash.
	if _, err := store.Create(beads.Bead{
		Title:  "beads/reviewer",
		Type:   session.BeadType,
		Labels: nil,
		Metadata: map[string]string{
			"session_name":              "beads--reviewer",
			"template":                  "beads/reviewer",
			"configured_named_session":  "true",
			"configured_named_identity": "beads/reviewer",
			"state":                     "awake",
		},
	}); err != nil {
		t.Fatalf("seed labelless typed session bead: %v", err)
	}
	// Bead with the label set normally — control case to verify the loader
	// still surfaces label-only beads.
	if _, err := store.Create(beads.Bead{
		Title:  "beads/builder",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "s-pool-builder",
			"template":     "beads/builder",
		},
	}); err != nil {
		t.Fatalf("seed labeled typed session bead: %v", err)
	}

	snap, err := loadSessionBeadSnapshot(store)
	if err != nil {
		t.Fatalf("loadSessionBeadSnapshot: %v", err)
	}
	if got := len(snap.Open()); got != 2 {
		t.Fatalf("Open()=%d, want 2 (labelless + labeled session beads)", got)
	}
	if got := snap.FindSessionNameByTemplate("beads/reviewer"); got != "beads--reviewer" {
		t.Errorf("FindSessionNameByTemplate(beads/reviewer)=%q, want beads--reviewer — labelless typed bead must be visible", got)
	}
	if got := snap.FindSessionNameByTemplate("beads/builder"); got != "s-pool-builder" {
		t.Errorf("FindSessionNameByTemplate(beads/builder)=%q, want s-pool-builder", got)
	}
}

// TestLoadSessionBeadSnapshot_DeduplicatesAcrossQueries verifies a bead that
// matches BOTH the Type and Label queries is included exactly once.
func TestLoadSessionBeadSnapshot_DeduplicatesAcrossQueries(t *testing.T) {
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "dual-match",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "s-dual",
			"template":     "dual-match",
		},
	}); err != nil {
		t.Fatalf("seed dual-match bead: %v", err)
	}
	snap, err := loadSessionBeadSnapshot(store)
	if err != nil {
		t.Fatalf("loadSessionBeadSnapshot: %v", err)
	}
	if got := len(snap.Open()); got != 1 {
		t.Fatalf("Open()=%d, want 1 — bead matching both queries must dedup", got)
	}
}

// TestSessionBeadSnapshotConstructorInfoEquivalence is the LOAD-BEARING pin for
// WI-6 W4: newSessionBeadSnapshotFromInfos (the typed front-door constructor)
// must build byte-identical index maps to newSessionBeadSnapshot (the raw-bead
// constructor whose precedence is the reference) across a corpus that exercises
// every precedence branch. An index-map precedence bug strands named sessions
// invisibly — a leaked pool bead beats the canonical named bead, or a label-lost
// typed bead never indexes — so this comparison, not a downstream behavior test,
// is where such a divergence is caught.
func TestSessionBeadSnapshotConstructorInfoEquivalence(t *testing.T) {
	corpus := []beads.Bead{
		// Canonical configured_named bead for template "mayor": must win the
		// agent AND template index over the leaked pool bead below.
		{
			ID:     "ga-named-mayor",
			Type:   session.BeadType,
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":                  "mayor",
				"agent_name":                "mayor",
				"configured_named_identity": "mayor",
				"session_name":              "mayor",
			},
		},
		// Leaked pool-style bead for the same template "mayor" (agent_name ==
		// template, pool-managed, no slot, non-canonical): agentName clears and
		// the whole entry is skipped, so it must NOT overwrite the canonical
		// index above.
		{
			ID:     "ga-leaked-mayor",
			Type:   session.BeadType,
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":     "mayor",
				"agent_name":   "mayor",
				"pool_managed": "true",
				"session_name": "s-leaked-mayor",
			},
		},
		// Pool-managed bead with a slot: stampedPoolQualifiedIdentity rewrites
		// agentName to the qualified instance ("frontend/worker-2").
		{
			ID:     "ga-pool-slot",
			Type:   session.BeadType,
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":     "frontend/worker",
				"agent_name":   "frontend/worker",
				"pool_managed": "true",
				"pool_slot":    "2",
				"session_name": "s-worker-2",
			},
		},
		// Non-pool bead with a distinct agent_name and a common_name: indexes by
		// agent_name, by template, and by common_name hint.
		{
			ID:     "ga-scout",
			Type:   session.BeadType,
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":     "scout",
				"agent_name":   "recon/scout",
				"common_name":  "scout-common",
				"session_name": "s-scout",
			},
		},
		// Agent-label fallback (no agent_name metadata): sessionBeadAgentName
		// reads the agent: label.
		{
			ID:     "ga-labelagent",
			Type:   session.BeadType,
			Labels: []string{session.LabelSession, "agent:labeled/one"},
			Metadata: map[string]string{
				"template":     "labeled",
				"session_name": "s-labeled",
			},
		},
		// Type-only bead that lost its gc:session label after a crash: must still
		// index (the reconciler-stranding regression this whole path guards).
		{
			ID:     "ga-labellost",
			Type:   session.BeadType,
			Labels: nil,
			Metadata: map[string]string{
				"template":                  "beads/reviewer",
				"configured_named_identity": "beads/reviewer",
				"session_name":              "beads--reviewer",
			},
		},
		// Bead with no session_name: appears in openInfos but indexes nothing.
		{
			ID:     "ga-noname",
			Type:   session.BeadType,
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template": "nameless",
			},
		},
		// Canonical-override pair. Bead A (non-canonical, non-pool) indexes both
		// agent "dup-agent" and template "dup" FIRST. Bead B (canonical, same
		// agent/template, later in order) MUST override both entries — this is
		// the `!exists || isCanonicalNamed` precedence branch. Drop the
		// `|| isCanonicalNamed` from the Info constructor and these entries stop
		// overriding, diverging from the raw constructor and failing this test.
		{
			ID:     "ga-dup-first",
			Type:   session.BeadType,
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":     "dup",
				"agent_name":   "dup-agent",
				"session_name": "s-dup-first",
			},
		},
		{
			ID:     "ga-dup-canonical",
			Type:   session.BeadType,
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":                  "dup",
				"agent_name":                "dup-agent",
				"configured_named_identity": "dup-agent",
				"session_name":              "s-dup-canonical",
			},
		},
		// Closed bead: excluded from openInfos and every index.
		{
			ID:     "ga-closed",
			Type:   session.BeadType,
			Status: "closed",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":     "gone",
				"agent_name":   "gone",
				"session_name": "s-gone",
			},
		},
	}

	beadSnap := newSessionBeadSnapshot(corpus)

	infos := make([]session.Info, 0, len(corpus))
	for _, b := range corpus {
		infos = append(infos, session.InfoFromPersistedBead(b))
	}
	infoSnap := newSessionBeadSnapshotFromInfos(infos)

	if !reflect.DeepEqual(infoSnap.beadIDByAgentName, beadSnap.beadIDByAgentName) {
		t.Errorf("beadIDByAgentName mismatch:\ninfo=%v\nbead=%v", infoSnap.beadIDByAgentName, beadSnap.beadIDByAgentName)
	}
	if !reflect.DeepEqual(infoSnap.beadIDByTemplateHint, beadSnap.beadIDByTemplateHint) {
		t.Errorf("beadIDByTemplateHint mismatch:\ninfo=%v\nbead=%v", infoSnap.beadIDByTemplateHint, beadSnap.beadIDByTemplateHint)
	}
	if !reflect.DeepEqual(infoSnap.sessionNameByAgentName, beadSnap.sessionNameByAgentName) {
		t.Errorf("sessionNameByAgentName mismatch:\ninfo=%v\nbead=%v", infoSnap.sessionNameByAgentName, beadSnap.sessionNameByAgentName)
	}
	if !reflect.DeepEqual(infoSnap.sessionNameByTemplateHint, beadSnap.sessionNameByTemplateHint) {
		t.Errorf("sessionNameByTemplateHint mismatch:\ninfo=%v\nbead=%v", infoSnap.sessionNameByTemplateHint, beadSnap.sessionNameByTemplateHint)
	}
	if len(infoSnap.openInfos) != len(beadSnap.openInfos) {
		t.Fatalf("openInfos length: info=%d bead=%d", len(infoSnap.openInfos), len(beadSnap.openInfos))
	}
	for i := range infoSnap.openInfos {
		if !reflect.DeepEqual(infoSnap.openInfos[i], beadSnap.openInfos[i]) {
			t.Errorf("openInfos[%d] mismatch:\ninfo=%+v\nbead=%+v", i, infoSnap.openInfos[i], beadSnap.openInfos[i])
		}
	}

	// Guard the corpus actually exercises the canonical-override precedence: the
	// canonical bead (ga-dup-canonical) must win over the earlier non-canonical
	// bead (ga-dup-first) at BOTH the agent and template index. If it ever stops
	// (both constructors would agree on the wrong answer, still matching above),
	// this fails loudly.
	if got := beadSnap.sessionNameByAgentName["dup-agent"]; got != "s-dup-canonical" {
		t.Fatalf("corpus no longer exercises agent canonical-override: sessionNameByAgentName[dup-agent]=%q, want s-dup-canonical", got)
	}
	if got := beadSnap.sessionNameByTemplateHint["dup"]; got != "s-dup-canonical" {
		t.Fatalf("corpus no longer exercises template canonical-override: sessionNameByTemplateHint[dup]=%q, want s-dup-canonical", got)
	}
	if got := beadSnap.FindSessionNameByTemplate("mayor"); got != "mayor" {
		t.Fatalf("corpus no longer exercises canonical-wins precedence: FindSessionNameByTemplate(mayor)=%q, want mayor", got)
	}
}

func TestSessionBeadSnapshotIndexesCanonicalSingletonPoolManagedBead(t *testing.T) {
	snapshot := newSessionBeadSnapshot([]beads.Bead{{
		ID:     "refinery-session",
		Title:  "cashmaster/refinery",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel, "agent:cashmaster/refinery"},
		Metadata: map[string]string{
			"template":             "cashmaster/refinery",
			"agent_name":           "cashmaster/refinery",
			"session_name":         "s-canonical-refinery",
			poolManagedMetadataKey: boolMetadata(true),
		},
	}})

	if got := snapshot.FindSessionNameByTemplate("cashmaster/refinery"); got != "s-canonical-refinery" {
		t.Fatalf("FindSessionNameByTemplate(canonical singleton pool bead) = %q, want s-canonical-refinery", got)
	}
	bead, ok := snapshot.FindSessionBeadByTemplate("cashmaster/refinery")
	if !ok {
		t.Fatal("FindSessionBeadByTemplate(canonical singleton pool bead) = false")
	}
	if bead.ID != "refinery-session" {
		t.Fatalf("FindSessionBeadByTemplate ID = %q, want refinery-session", bead.ID)
	}
}
