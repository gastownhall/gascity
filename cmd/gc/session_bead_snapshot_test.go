package main

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
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
		if got := len(snap.OpenInfos()); got != 50 {
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
		if got := len(snap.OpenInfos()); got != 50 {
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
	if got := len(snap.OpenInfos()); got != 2 {
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
	if got := len(snap.OpenInfos()); got != 1 {
		t.Fatalf("Open()=%d, want 1 — bead matching both queries must dedup", got)
	}
}

// TestSessionBeadSnapshotFromReconcileRowsIndexPrecedence is the LOAD-BEARING,
// PERMANENT index-precedence characterization of newSessionBeadSnapshotFromReconcileRows
// — the reconciler-tick constructor and (via the test helper) the reference for every
// snapshot built from beads. It is the WI-7 W-delete successor to the raw-vs-Info
// constructor-equivalence pin: the raw constructor retired with the snapshot's raw
// half, so instead of comparing two constructors this asserts the exact index maps
// directly. An index-map precedence bug strands named sessions invisibly — a leaked
// pool bead beats the canonical named bead, or a label-lost typed bead never indexes —
// so this is where such a divergence is caught. The 12-branch corpus is preserved from
// the retired pin; only the reference constructor changed.
func TestSessionBeadSnapshotFromReconcileRowsIndexPrecedence(t *testing.T) {
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

	snap := newSessionBeadSnapshotFromReconcileRows(session.ReconcileRowsFromBeads(corpus))

	// The EXACT index maps every precedence branch of the corpus must produce.
	// Hand-derived from the documented rules: canonical named beats leaked pool at
	// the agent+template index (ga-named-mayor over ga-leaked-mayor, which clears its
	// agentName and indexes nothing); a slotted pool bead's agentName is rewritten to
	// its stamped qualified instance (frontend/worker-2) and it skips the template
	// index; agent: label fallback (labeled/one); a label-lost typed bead still
	// indexes by template (beads/reviewer -> ga-labellost); no-session_name indexes
	// nothing; the later canonical bead overrides the earlier non-canonical one at
	// both indices (dup-agent/dup -> ga-dup-canonical); the closed bead is excluded.
	wantBeadIDByAgentName := map[string]string{
		"mayor":             "ga-named-mayor",
		"frontend/worker-2": "ga-pool-slot",
		"recon/scout":       "ga-scout",
		"labeled/one":       "ga-labelagent",
		"dup-agent":         "ga-dup-canonical",
	}
	wantSessionNameByAgentName := map[string]string{
		"mayor":             "mayor",
		"frontend/worker-2": "s-worker-2",
		"recon/scout":       "s-scout",
		"labeled/one":       "s-labeled",
		"dup-agent":         "s-dup-canonical",
	}
	wantBeadIDByTemplateHint := map[string]string{
		"mayor":          "ga-named-mayor",
		"scout":          "ga-scout",
		"scout-common":   "ga-scout",
		"labeled":        "ga-labelagent",
		"beads/reviewer": "ga-labellost",
		"dup":            "ga-dup-canonical",
	}
	wantSessionNameByTemplateHint := map[string]string{
		"mayor":          "mayor",
		"scout":          "s-scout",
		"scout-common":   "s-scout",
		"labeled":        "s-labeled",
		"beads/reviewer": "beads--reviewer",
		"dup":            "s-dup-canonical",
	}

	if !reflect.DeepEqual(snap.beadIDByAgentName, wantBeadIDByAgentName) {
		t.Errorf("beadIDByAgentName:\n got=%v\nwant=%v", snap.beadIDByAgentName, wantBeadIDByAgentName)
	}
	if !reflect.DeepEqual(snap.sessionNameByAgentName, wantSessionNameByAgentName) {
		t.Errorf("sessionNameByAgentName:\n got=%v\nwant=%v", snap.sessionNameByAgentName, wantSessionNameByAgentName)
	}
	if !reflect.DeepEqual(snap.beadIDByTemplateHint, wantBeadIDByTemplateHint) {
		t.Errorf("beadIDByTemplateHint:\n got=%v\nwant=%v", snap.beadIDByTemplateHint, wantBeadIDByTemplateHint)
	}
	if !reflect.DeepEqual(snap.sessionNameByTemplateHint, wantSessionNameByTemplateHint) {
		t.Errorf("sessionNameByTemplateHint:\n got=%v\nwant=%v", snap.sessionNameByTemplateHint, wantSessionNameByTemplateHint)
	}
	// The closed bead (ga-closed) is excluded; the 9 open beads remain in openInfos.
	if got := len(snap.openInfos); got != 9 {
		t.Fatalf("openInfos length = %d, want 9 (10 beads minus the closed one)", got)
	}

	// The canonical-wins precedence is the headline invariant: the leaked pool bead
	// must NOT strand the canonical named session.
	if got := snap.FindSessionNameByTemplate("mayor"); got != "mayor" {
		t.Fatalf("FindSessionNameByTemplate(mayor)=%q, want mayor (canonical must win over the leaked pool bead)", got)
	}
}

// TestSessionBeadSnapshotFromInfosTypedLookups pins that an Info-built snapshot
// answers the typed FindInfo* lookups from openInfos + the index maps. Against the
// pre-fix code — where findInfoByIDLocked / FindInfoByNamedIdentity scanned a
// then-existing raw slice — every assertion below returned (Info{}, false), silently
// stranding a Get-projection sweep built on this constructor.
func TestSessionBeadSnapshotFromInfosTypedLookups(t *testing.T) {
	seed := sessiontest.SeedBead(t, beads.Bead{
		ID:     "ga-named-reviewer",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"template":                  "beads/reviewer",
			"agent_name":                "beads/reviewer",
			"configured_named_identity": "beads/reviewer",
			"session_name":              "beads--reviewer",
		},
	})
	snap := newSessionBeadSnapshotFromInfos([]session.Info{seed})

	if got, ok := snap.FindInfoByID("ga-named-reviewer"); !ok || got.ID != "ga-named-reviewer" {
		t.Errorf("FindInfoByID = (%+v, %v), want the seeded info", got, ok)
	}
	if got, ok := snap.FindInfoByTemplate("beads/reviewer"); !ok || got.ID != "ga-named-reviewer" {
		t.Errorf("FindInfoByTemplate = (%+v, %v), want the seeded info", got, ok)
	}
	if got, ok := snap.FindInfoByNamedIdentity("beads/reviewer"); !ok || got.ID != "ga-named-reviewer" {
		t.Errorf("FindInfoByNamedIdentity = (%+v, %v), want the seeded info", got, ok)
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
	info, ok := snapshot.FindInfoByTemplate("cashmaster/refinery")
	if !ok {
		t.Fatal("FindInfoByTemplate(canonical singleton pool bead) = false")
	}
	if info.ID != "refinery-session" {
		t.Fatalf("FindInfoByTemplate ID = %q, want refinery-session", info.ID)
	}
}

// TestSessionBeadSnapshotFingerprintReflectsRawMetadata pins the config-change cache
// key across the W-delete raw-half deletion: sessionBeadSnapshotFingerprint returns the
// snapshot's stored field, computed at construction from the raw beads via
// session.SetFingerprint over the OPEN set — so it reflects EVERY metadata key,
// including ones session.Info drops. This is what makes the fingerprint survivable when
// the raw half is gone: it is a field, not a recomputation. A regression that dropped
// the field (empty string) or recomputed from Info (dropping unprojected keys) fails
// the change-detection assertion below.
func TestSessionBeadSnapshotFingerprintReflectsRawMetadata(t *testing.T) {
	beadWith := func(tag string) beads.Bead {
		return beads.Bead{
			ID:     "ga-fp",
			Type:   session.BeadType,
			Status: "open",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"session_name":            "fp",
				"state":                   "active",
				"bespoke_unprojected_tag": tag, // a key session.Info does NOT project
			},
		}
	}

	snapV1 := newSessionBeadSnapshot([]beads.Bead{beadWith("v1")})
	// The getter returns exactly SetFingerprint over the open beads.
	if got, want := sessionBeadSnapshotFingerprint(snapV1), session.SetFingerprint([]beads.Bead{beadWith("v1")}); got != want {
		t.Fatalf("fingerprint = %q, want SetFingerprint(open) %q", got, want)
	}
	if sessionBeadSnapshotFingerprint(snapV1) == "" {
		t.Fatal("fingerprint is empty on a non-empty snapshot")
	}

	// Changing ONLY an unprojected metadata key must change the fingerprint — proof it
	// reflects raw metadata, not the (lossy) Info projection.
	snapV2 := newSessionBeadSnapshot([]beads.Bead{beadWith("v2")})
	if sessionBeadSnapshotFingerprint(snapV1) == sessionBeadSnapshotFingerprint(snapV2) {
		t.Fatal("fingerprint ignored an unprojected metadata change; config-change detection would miss it")
	}

	// nil snapshot is empty, not a panic.
	if got := sessionBeadSnapshotFingerprint(nil); got != "" {
		t.Fatalf("nil snapshot fingerprint = %q, want empty", got)
	}
}
