package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// beadCopyExemptFields names every beads.Bead field the equality stage does NOT
// witness, each with the reason it cannot be one. It is the exemption list
// TestBeadCopyDifferenceWitnessesEveryDurableField guards against the struct, so
// a field added to beads.Bead is either compared by beadCopyDifference or listed
// here — never silently unwitnessed.
//
// The reasons matter as much as the names. An exemption is normally a promise that
// a copy which changed this field is still a faithful copy, and two of these five
// are only true because something else witnesses the same state.
// IndefinitelyDeferred is the one exemption that does not carry that promise: the
// destination cannot hold it, so the deferral genuinely does not cross. It is
// exempt because comparing it would refuse every copy of a deferred infra row
// without preserving anything — not because the copy stays faithful. Do not cite
// it as precedent for exempting a field the destination could hold.
var beadCopyExemptFields = map[string]string{
	"Revision": "store-internal optimistic-concurrency token. Each store mints and bumps its own; the destination's row is a fresh create, so its revision is unrelated to the source's by construction.",
	"ClaimFence": "store-internal ownership fence, maintained per store like Revision. " +
		"SQLiteStore.Create clears it explicitly (clearClaimFenceTx), so a copied row always starts at zero.",
	"Needs": "create-time dependency shorthand, deliberately stripped by infraMigrationRow so the destination's Create " +
		"cannot mint dangling cross-boundary edges. The edges it would have produced are witnessed by verifyInfraCopy's DepList comparison.",
	"Dependencies": "create-time dependency shorthand, stripped by infraMigrationRow for the same reason as Needs, " +
		"and witnessed the same way — through the source's own materialized dep rows rather than the create-time field.",
	"IndefinitelyDeferred": "not a property of the copy but of READING the source. It is the read-time normalization of bd's " +
		"richer status vocabulary (beads.normalizedBdReadState), re-derived on every work-store read; the destination stores " +
		"Gas City's three-state status verbatim and has no richer status to normalize. Both sides therefore agree on the " +
		"Status this stage does compare, and the field itself is json:\"-\" while SQLiteStore persists beads through " +
		"bead_json — so it is structurally absent from the destination and comparing it would refuse EVERY copy of a " +
		"deferred infra row, identically on each retry. What does not cross is the deferral itself: the binding reads such " +
		"a row as plainly open.",
}

// infraEqualityFixture is a source row with every durable field populated to a
// value distinguishable from its zero, so a mutation of any one of them is
// detectable. It classifies as coordclass.ClassSessions, which is what makes it
// a legal infra row.
func infraEqualityFixture() beads.Bead {
	created := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	deferred := created.Add(72 * time.Hour)
	priority := 2
	blocked := false
	return beads.Bead{
		ID:           "gcg-41",
		Title:        "session lifecycle",
		Status:       "open",
		Type:         "session",
		Priority:     &priority,
		CreatedAt:    created,
		UpdatedAt:    created.Add(time.Hour),
		Assignee:     "worker-1",
		From:         "dispatcher",
		ParentID:     "gcg-40",
		Ref:          "step-3",
		Needs:        []string{"gcg-39"},
		Description:  "the bead body, which is durable domain state",
		Labels:       []string{"gc:session"},
		Metadata:     beads.StringMap{"gc.session_name": "worker-1"},
		Dependencies: []beads.Dep{{IssueID: "gcg-41", DependsOnID: "gcg-40", Type: "blocks"}},
		Ephemeral:    true,
		NoHistory:    true,
		DeferUntil:   &deferred,
		IsBlocked:    &blocked,
		Revision:     7,
		ClaimFence:   3,
		// Set so the exempt mutation below models the loss that actually
		// happens — a source row carrying the marker against a destination that
		// cannot hold it — rather than the destination inventing one.
		IndefinitelyDeferred: true,
	}
}

// beadCopyFieldMutations is one mutation per witnessed beads.Bead field: the
// exact loss a copy could suffer in that field. Every entry must make
// beadCopyDifference refuse, and every Bead field must appear either here or in
// beadCopyExemptFields.
func beadCopyFieldMutations() map[string]func(beads.Bead) beads.Bead {
	return map[string]func(beads.Bead) beads.Bead{
		"ID":        func(b beads.Bead) beads.Bead { b.ID = "gcg-999"; return b },
		"Title":     func(b beads.Bead) beads.Bead { b.Title = "some other title"; return b },
		"Status":    func(b beads.Bead) beads.Bead { b.Status = "closed"; return b },
		"Type":      func(b beads.Bead) beads.Bead { b.Type = "task"; return b },
		"Priority":  func(b beads.Bead) beads.Bead { b.Priority = nil; return b },
		"CreatedAt": func(b beads.Bead) beads.Bead { b.CreatedAt = b.CreatedAt.Add(48 * time.Hour); return b },
		"UpdatedAt": func(b beads.Bead) beads.Bead { b.UpdatedAt = b.UpdatedAt.Add(48 * time.Hour); return b },
		"Assignee":  func(b beads.Bead) beads.Bead { b.Assignee = ""; return b },
		"From":      func(b beads.Bead) beads.Bead { b.From = ""; return b },
		"ParentID":  func(b beads.Bead) beads.Bead { b.ParentID = ""; return b },
		"Ref":       func(b beads.Bead) beads.Bead { b.Ref = ""; return b },
		"Description": func(b beads.Bead) beads.Bead {
			b.Description = ""
			return b
		},
		"Labels":    func(b beads.Bead) beads.Bead { b.Labels = nil; return b },
		"Metadata":  func(b beads.Bead) beads.Bead { b.Metadata = nil; return b },
		"Ephemeral": func(b beads.Bead) beads.Bead { b.Ephemeral = false; return b },
		"NoHistory": func(b beads.Bead) beads.Bead { b.NoHistory = false; return b },
		"DeferUntil": func(b beads.Bead) beads.Bead {
			b.DeferUntil = nil
			return b
		},
		// Unlike every other entry, this mutation is not a LOSS — it is a value
		// the destination must never carry at all. infraMigrationRow strips it,
		// so any non-nil value on the destination is a readiness projection the
		// binding would serve forever with nothing to correct it.
		"IsBlocked": func(b beads.Bead) beads.Bead {
			blocked := true
			b.IsBlocked = &blocked
			return b
		},
	}
}

// beadCopyExemptMutations is one mutation per exempt field. Each must compare
// EQUAL: an exemption that quietly stopped being an exemption is as much a
// defect as an unwitnessed field, because it would refuse every faithful copy.
func beadCopyExemptMutations() map[string]func(beads.Bead) beads.Bead {
	return map[string]func(beads.Bead) beads.Bead{
		"Revision":     func(b beads.Bead) beads.Bead { b.Revision = 99; return b },
		"ClaimFence":   func(b beads.Bead) beads.Bead { b.ClaimFence = 0; return b },
		"Needs":        func(b beads.Bead) beads.Bead { b.Needs = nil; return b },
		"Dependencies": func(b beads.Bead) beads.Bead { b.Dependencies = nil; return b },
		// The fixture carries the marker, so clearing it here is the real loss:
		// a destination that cannot hold what the source read produced. This is
		// the mutation that must NOT be refused, or the migration wedges.
		"IndefinitelyDeferred": func(b beads.Bead) beads.Bead { b.IndefinitelyDeferred = false; return b },
	}
}

// TestBeadCopyDifferenceWitnessesEveryDurableField is the field-sync guard, in
// the shape of internal/config's TestAgentFieldSync: every field of beads.Bead
// is either compared by the equality stage or explicitly exempted with a reason.
//
// The name check alone would be satisfied by a comparison that named a field and
// did nothing with it, so each witnessed field also carries a mutation that must
// be REFUSED, and each exempt field a mutation that must be ACCEPTED. That is
// what makes the guard non-vacuous: it fails both when a new field goes
// unwitnessed and when an existing comparison silently stops comparing.
func TestBeadCopyDifferenceWitnessesEveryDurableField(t *testing.T) {
	witnessed := beadCopyFieldMutations()
	exemptMutations := beadCopyExemptMutations()

	var unwitnessed, doubleBooked []string
	for _, field := range reflect.VisibleFields(reflect.TypeOf(beads.Bead{})) {
		_, exempt := beadCopyExemptFields[field.Name]
		_, compared := witnessed[field.Name]
		switch {
		case exempt && compared:
			doubleBooked = append(doubleBooked, field.Name)
		case !exempt && !compared:
			unwitnessed = append(unwitnessed, field.Name)
		}
	}
	sort.Strings(unwitnessed)
	sort.Strings(doubleBooked)
	if len(unwitnessed) > 0 {
		t.Fatalf("beads.Bead field(s) %v are neither compared by beadCopyDifference nor listed in beadCopyExemptFields. "+
			"A copy that dropped them would pass the equality stage and get a convergence marker. "+
			"Compare them, or exempt them with the reason they cannot be witnessed", unwitnessed)
	}
	if len(doubleBooked) > 0 {
		t.Fatalf("beads.Bead field(s) %v are listed as exempt AND carry a witness mutation; the two lists disagree", doubleBooked)
	}
	for name := range beadCopyExemptFields {
		if _, ok := exemptMutations[name]; !ok {
			t.Fatalf("exempt field %q has no mutation proving the exemption is real", name)
		}
	}

	// The comparison is source-row against destination-row, and those are not the
	// same shape: infraMigrationRow adds the provenance stamp and strips the
	// readiness projection. So the baseline for "a faithful copy" is the migrated
	// row, not the source compared against itself — comparing the fixture to
	// itself would assert that a raw source row is a valid destination row, which
	// is exactly what the migration exists to make false.
	base := infraEqualityFixture()
	faithful := infraMigrationRow(base)
	if diff := beadCopyDifference(base, faithful); diff != "" {
		t.Fatalf("a faithful copy of the fixture differs from its source: %s", diff)
	}
	for name, mutate := range witnessed {
		if diff := beadCopyDifference(base, mutate(faithful)); diff == "" {
			t.Errorf("a copy that lost %s compared equal; the equality stage does not witness that field", name)
		}
	}
	for name, mutate := range exemptMutations {
		if diff := beadCopyDifference(base, mutate(faithful)); diff != "" {
			t.Errorf("exempt field %s was compared after all (%s); %s", name, diff, beadCopyExemptFields[name])
		}
	}
}

// TestBeadCopyDifferenceExemptsOnlyTheProvenanceStamp pins the both-directions
// label and metadata comparison and its single exemption.
//
// Subset-only comparison was the hole: a label or metadata key INVENTED on the
// destination is state no reader can attribute to the source, and it passed. The
// one key that legitimately appears only on the destination is the migration's
// own provenance stamp, and it is named from the beadmeta constant rather than
// spelled here so the exemption cannot drift from the stamp.
func TestBeadCopyDifferenceExemptsOnlyTheProvenanceStamp(t *testing.T) {
	base := infraEqualityFixture()

	invented := base
	invented.Labels = append(append([]string{}, base.Labels...), "gc:invented")
	if diff := beadCopyDifference(base, invented); diff == "" {
		t.Error("a label invented on the destination compared equal")
	}

	inventedKey := base
	inventedKey.Metadata = beads.StringMap{"gc.session_name": "worker-1", "gc.invented": "yes"}
	if diff := beadCopyDifference(base, inventedKey); diff == "" {
		t.Error("a metadata key invented on the destination compared equal")
	}

	// An invented key whose value is empty is still invented: absent and
	// present-but-empty are different states, and only one of them is the
	// source's.
	emptyInvention := base
	emptyInvention.Metadata = beads.StringMap{"gc.session_name": "worker-1", "gc.invented": ""}
	if diff := beadCopyDifference(base, emptyInvention); diff == "" {
		t.Error("a metadata key invented on the destination with an empty value compared equal")
	}

	// The destination row shape the migration actually writes — the source row
	// plus the stamp — is the one exemption.
	if diff := beadCopyDifference(base, infraMigrationRow(base)); diff != "" {
		t.Errorf("the migration's own destination row shape broke equality: %s", diff)
	}

	// The stamp is exempt in the forward direction too. A source row that
	// already carries a stamp (a re-migrated city) has it OVERWRITTEN by
	// infraMigrationRow, so comparing it forward would refuse every such copy.
	restamped := base
	restamped.Metadata = beads.StringMap{"gc.session_name": "worker-1", infraMigrationStampKey: "some-older-binding"}
	if diff := beadCopyDifference(restamped, infraMigrationRow(restamped)); diff != "" {
		t.Errorf("a re-stamped source row broke equality: %s", diff)
	}
}

// TestInfraCopyClassDifferenceRefusesAReclassifiedRow pins the classification
// invariant directly. Every input coordclass.Classify reads — type, labels,
// metadata — is also a compared field, so on today's equality stage this check
// is unreachable from a real copy; it is kept because it states the property the
// destination must have in its own terms rather than as a consequence of another
// check, and because "no work bead crosses" is the one thing the infra binding
// must never be wrong about.
func TestInfraCopyClassDifferenceRefusesAReclassifiedRow(t *testing.T) {
	session := infraEqualityFixture()
	if diff := infraCopyClassDifference(session, session); diff != "" {
		t.Fatalf("a faithful session row was refused: %s", diff)
	}

	demoted := session
	demoted.Type = "task"
	demoted.Labels = nil
	demoted.Metadata = nil
	if coordclass.Classify(demoted) != coordclass.ClassWork {
		t.Fatalf("fixture is wrong: the demoted row classifies as %s, want work", coordclass.Classify(demoted))
	}
	if diff := infraCopyClassDifference(session, demoted); diff == "" {
		t.Error("a destination row that classifies as work crossed the class check")
	}

	reclassified := session
	reclassified.Type = "message"
	reclassified.Labels = nil
	if got := coordclass.Classify(reclassified); got != coordclass.ClassMessaging {
		t.Fatalf("fixture is wrong: the reclassified row is %s, want messaging", got)
	}
	if diff := infraCopyClassDifference(session, reclassified); diff == "" {
		t.Error("a destination row that changed infra class crossed the class check")
	}
}

// TestInfraMigrationRowPreservesClass is the reachable half of the
// classification invariant: the migration's own provenance stamp must not move
// a bead between classes. It is the one mutation the equality stage exempts, so
// nothing else would catch a stamp key that happened to be a classification
// input.
func TestInfraMigrationRowPreservesClass(t *testing.T) {
	for _, tc := range []struct {
		name string
		bead beads.Bead
		want coordclass.Class
	}{
		{"wisp", beads.Bead{Title: "wisp root", Type: "molecule", Labels: []string{"gc:wisp"}}, coordclass.ClassGraph},
		{"session", beads.Bead{Title: "session", Type: "session"}, coordclass.ClassSessions},
		{"message", beads.Bead{Title: "mail", Type: "message"}, coordclass.ClassMessaging},
		{"order", beads.Bead{Title: "order run", Type: "task", Labels: []string{"order-tracking"}}, coordclass.ClassOrders},
		{"nudge", beads.Bead{Title: "nudge", Type: "task", Labels: []string{"gc:nudge"}}, coordclass.ClassNudges},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := coordclass.Classify(tc.bead); got != tc.want {
				t.Fatalf("fixture classifies as %s, want %s", got, tc.want)
			}
			if got := coordclass.Classify(infraMigrationRow(tc.bead)); got != tc.want {
				t.Fatalf("the provenance stamp reclassified the bead to %s, want %s", got, tc.want)
			}
		})
	}
}

// infraEqualityDestination opens the deployed SQLite destination at a temporary
// binding root, through the production opener, and returns the opener the
// equality stage takes alongside the writing handle. The destination is never
// stubbed — a fake one is what let this migration pass while writing somewhere
// no runtime binding reads.
func infraEqualityDestination(t *testing.T) (beads.Store, func() (beads.Store, error)) {
	t.Helper()
	cityPath := t.TempDir()
	cfg := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))
	target := mustResolveInfraTarget(t, cityPath, cfg)
	return openMigratedDestination(t, target), func() (beads.Store, error) { return openInfraDestination(target) }
}

// infraStoreOpener hands the equality stage a store that is already open. It is
// for destinations with no close at all (MemStore): a warm handle can only
// diverge from the file when there IS a file, so those tests model nothing by
// reopening.
func infraStoreOpener(store beads.Store) func() (beads.Store, error) {
	return func() (beads.Store, error) { return store, nil }
}

// TestVerifyInfraCopyRefusesADroppedDurableField is the end-to-end proof for the
// fields the equality stage used to ignore. It runs against the deployed SQLite
// destination — the exact store the runtime binding opens — so it proves both
// that the field survives a faithful copy and that a copy which dropped it is
// refused before any marker is written.
func TestVerifyInfraCopyRefusesADroppedDurableField(t *testing.T) {
	deferred := time.Now().Add(96 * time.Hour).UTC().Truncate(time.Second)
	priority := 1
	source := beads.NewMemStore()
	row, err := source.Create(beads.Bead{
		Title:       "session",
		Type:        "session",
		Labels:      []string{"gc:session"},
		Description: "the bead body",
		Priority:    &priority,
		From:        "dispatcher",
		Assignee:    "worker-1",
		Ref:         "step-3",
		NoHistory:   true,
		DeferUntil:  &deferred,
		Metadata:    beads.StringMap{"gc.session_name": "worker-1"},
		// The source row carries the status-based deferral marker the work
		// store produces for a bd-`deferred` row. The destination cannot hold
		// it — it is json:"-" and SQLiteStore persists through bead_json — so
		// the faithful subtest below is what proves the equality stage does not
		// refuse a copy over a field no destination row can ever carry. The
		// drop subtests inherit it and must still refuse for their own field.
		IndefinitelyDeferred: true,
	})
	if err != nil {
		t.Fatalf("seeding the source: %v", err)
	}
	// The marker reaches the source store only because MemStore.Create seeds
	// Status directly rather than going through an explicit transition, which
	// clears it. Assert it on the equality stage's OWN view of the source
	// rather than assume it: if that ever changes, these subtests must fail
	// loudly instead of comparing false against false and proving nothing.
	seeded, err := readInfraSnapshot(source)
	if err != nil {
		t.Fatalf("reading the seeded source: %v", err)
	}
	if len(seeded) != 1 || !seeded[0].IndefinitelyDeferred {
		t.Fatalf("the equality stage's view of the source does not carry the deferral marker: %+v", seeded)
	}

	for _, tc := range []struct {
		field string
		drop  func(beads.Bead) beads.Bead
	}{
		{"Description", func(b beads.Bead) beads.Bead { b.Description = ""; return b }},
		{"Priority", func(b beads.Bead) beads.Bead { b.Priority = nil; return b }},
		{"From", func(b beads.Bead) beads.Bead { b.From = ""; return b }},
		{"NoHistory", func(b beads.Bead) beads.Bead { b.NoHistory = false; return b }},
		{"DeferUntil", func(b beads.Bead) beads.Bead { b.DeferUntil = nil; return b }},
		{"UpdatedAt", func(b beads.Bead) beads.Bead { b.UpdatedAt = b.UpdatedAt.Add(72 * time.Hour); return b }},
	} {
		t.Run(tc.field, func(t *testing.T) {
			destination, reopen := infraEqualityDestination(t)
			creator, ok := destination.(beads.ForeignIDCreator)
			if !ok {
				t.Fatalf("the deployed destination %T cannot preserve ids", destination)
			}
			if _, err := creator.CreateWithForeignID(tc.drop(infraMigrationRow(row))); err != nil {
				t.Fatalf("writing the lossy row: %v", err)
			}
			if _, err := verifyInfraCopy(reopen, source); err == nil {
				t.Fatalf("the equality stage blessed a copy that dropped %s; that copy would get a convergence marker", tc.field)
			}
		})
	}

	t.Run("faithful", func(t *testing.T) {
		destination, reopen := infraEqualityDestination(t)
		creator, ok := destination.(beads.ForeignIDCreator)
		if !ok {
			t.Fatalf("the deployed destination %T cannot preserve ids", destination)
		}
		if _, err := creator.CreateWithForeignID(infraMigrationRow(row)); err != nil {
			t.Fatalf("writing the faithful row: %v", err)
		}
		proven, err := verifyInfraCopy(reopen, source)
		if err != nil {
			t.Fatalf("the equality stage refused a faithful copy through the deployed destination: %v", err)
		}
		if len(proven) != 1 || proven[0] != row.ID {
			t.Fatalf("proven ids = %v, want %v", proven, []string{row.ID})
		}
	})
}

// TestInfraDepDifferenceWitnessesEveryDepField is the field-sync guard for the
// EDGE model, the sibling of the bead one. The bead guard was added because an
// unwitnessed field is a silent loss; beads.Dep is just as capable of growing
// one, and nothing was watching it.
//
// It does NOT cover the edge payload, and that is the point of saying so here:
// the payload is not a field of beads.Dep at all — it is a sidecar the source
// carries in its dependencies.metadata column and the destination keeps in kv.
// No reflection over this struct could ever see it, so growing this table can
// never close it. It is witnessed next door by infraEdgePayloadDifference, which
// asks both stores about each pair rather than comparing two slices; see
// TestInfraEdgePayloadDifferenceComparesPayloadsBothWays.
func TestInfraDepDifferenceWitnessesEveryDepField(t *testing.T) {
	compared := map[string]func(beads.Dep) beads.Dep{
		"DependsOnID": func(d beads.Dep) beads.Dep { d.DependsOnID = "gcg-998"; return d },
		"Type":        func(d beads.Dep) beads.Dep { d.Type = "tracks"; return d },
	}
	// IssueID is structurally determined rather than compared: verifyInfraCopy
	// asks both stores DepList(want.ID, "down"), so every edge on either side
	// already has that bead as its IssueID. Comparing it would assert a property
	// of the query, not of the copy.
	depExempt := map[string]string{"IssueID": "fixed by the DepList(id, \"down\") query on both sides"}

	var unwitnessed []string
	for _, field := range reflect.VisibleFields(reflect.TypeOf(beads.Dep{})) {
		_, ok := compared[field.Name]
		_, exempt := depExempt[field.Name]
		if !ok && !exempt {
			unwitnessed = append(unwitnessed, field.Name)
		}
	}
	sort.Strings(unwitnessed)
	if len(unwitnessed) > 0 {
		t.Fatalf("beads.Dep field(s) %v are not witnessed by infraDepDifference. "+
			"A copy that changed them would pass the equality stage and get a convergence marker. "+
			"Compare them, or state here why they cannot be", unwitnessed)
	}

	infraIDs := map[string]bool{"gcg-1": true, "gcg-2": true}
	base := []beads.Dep{{IssueID: "gcg-1", DependsOnID: "gcg-2", Type: "blocks"}}
	if diff := infraDepDifference("gcg-1", base, base, infraIDs); diff != "" {
		t.Fatalf("an edge set differs from itself: %s", diff)
	}
	for name, mutate := range compared {
		mutated := []beads.Dep{mutate(base[0])}
		if diff := infraDepDifference("gcg-1", base, mutated, infraIDs); diff == "" {
			t.Errorf("a copy that changed Dep.%s compared equal; the edge comparison does not witness that field", name)
		}
	}
}

// TestVerifyInfraCopyStripsTheReadinessProjection pins the one field whose
// invariant is about the DESTINATION alone rather than about equality: the
// source may carry is_blocked, and the destination must not.
//
// The exemption this replaces read "denormalized ready-work projection each
// store recomputes from its own dep rows". That is true of bd and FALSE of the
// destination: internal/beads' SQLiteStore never mentions IsBlocked — it
// round-trips the field through bead_json and serves back whatever was written —
// while CachingStore.cachedBeadReady prefers a non-nil IsBlocked over
// dependency-derived readiness, and the API fronts session and mail beads with a
// CachingStore. So a copied value is authoritative and permanent.
//
// Copying it FAITHFULLY would be wrong too, which is why this is a strip rather
// than a comparison. bd computed the source's value over the whole graph
// including edges to work beads; the binding holds only within-infra edges. A
// session bead blocked at copy time by a work bead would be frozen blocked
// forever, with nothing in the binding able to unblock it.
func TestVerifyInfraCopyStripsTheReadinessProjection(t *testing.T) {
	source := beads.NewMemStore()
	blocked := true
	row, err := source.Create(beads.Bead{
		Title:     "session",
		Type:      "session",
		Labels:    []string{"gc:session"},
		IsBlocked: &blocked,
	})
	if err != nil {
		t.Fatalf("seeding the source: %v", err)
	}
	if row.IsBlocked == nil || !*row.IsBlocked {
		t.Fatalf("the source row does not carry is_blocked=true; this test would prove nothing")
	}

	t.Run("a copy that carried it over is refused", func(t *testing.T) {
		destination, reopen := infraEqualityDestination(t)
		creator, ok := destination.(beads.ForeignIDCreator)
		if !ok {
			t.Fatalf("the deployed destination %T cannot preserve ids", destination)
		}
		carried := infraMigrationRow(row)
		carried.IsBlocked = &blocked
		if _, err := creator.CreateWithForeignID(carried); err != nil {
			t.Fatalf("writing the row that kept is_blocked: %v", err)
		}
		if _, err := verifyInfraCopy(reopen, source); err == nil {
			t.Fatal("the equality stage blessed a copy carrying is_blocked; the binding would serve that projection forever")
		}
	})

	t.Run("the stripped copy is accepted and reads back nil", func(t *testing.T) {
		destination, reopen := infraEqualityDestination(t)
		creator, ok := destination.(beads.ForeignIDCreator)
		if !ok {
			t.Fatalf("the deployed destination %T cannot preserve ids", destination)
		}
		if _, err := creator.CreateWithForeignID(infraMigrationRow(row)); err != nil {
			t.Fatalf("writing the stripped row: %v", err)
		}
		if _, err := verifyInfraCopy(reopen, source); err != nil {
			t.Fatalf("the equality stage refused the stripped copy: %v", err)
		}
		// Read it back through a fresh handle: the strip has to survive the
		// round trip, not just the in-memory value handed to Create.
		fresh, err := reopen()
		if err != nil {
			t.Fatalf("reopening: %v", err)
		}
		defer func() { _ = closeBeadStoreHandle(fresh) }()
		got, err := fresh.Get(row.ID)
		if err != nil {
			t.Fatalf("reading back: %v", err)
		}
		if got.IsBlocked != nil {
			t.Fatalf("the destination serves is_blocked=%v; nil is what makes readiness fall back to the witnessed dep rows", *got.IsBlocked)
		}
	})
}

// TestVerifyInfraCopyRefusesAnInventedDependencyEdge pins the reverse direction
// of the dep comparison. The forward direction — every source edge present on
// the destination — was already checked; an edge the copy INVENTED was not, and
// a fabricated blocks edge silently changes what the destination reports ready.
func TestVerifyInfraCopyRefusesAnInventedDependencyEdge(t *testing.T) {
	source := beads.NewMemStore()
	seed := func(title string) beads.Bead {
		b, err := source.Create(beads.Bead{Title: title, Type: "session", Labels: []string{"gc:session"}})
		if err != nil {
			t.Fatalf("seeding %s: %v", title, err)
		}
		return b
	}
	a, b, c := seed("a"), seed("b"), seed("c")
	if err := source.DepAdd(a.ID, b.ID, "blocks"); err != nil {
		t.Fatal(err)
	}

	rows, err := readInfraSnapshot(source)
	if err != nil {
		t.Fatal(err)
	}
	destination, reopen := infraEqualityDestination(t)
	if _, err := importInfraSnapshot(destination, source, rows); err != nil {
		t.Fatalf("importing: %v", err)
	}
	if _, err := verifyInfraCopy(reopen, source); err != nil {
		t.Fatalf("the equality stage refused a faithful copy: %v", err)
	}

	if err := destination.DepAdd(a.ID, c.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyInfraCopy(reopen, source); err == nil {
		t.Fatal("the equality stage blessed a destination holding a dep edge the work store does not have")
	}
}

// TestInfraDepDifferenceComparesEdgesBothWays pins the dep comparison directly,
// including the two cases the store shapes make awkward: a dep type the
// destination normalized from empty, and a cross-boundary edge into work that is
// deliberately not re-added.
func TestInfraDepDifferenceComparesEdgesBothWays(t *testing.T) {
	infra := map[string]bool{"gcg-2": true, "gcg-3": true}
	dep := func(dependsOn, kind string) beads.Dep {
		return beads.Dep{IssueID: "gcg-1", DependsOnID: dependsOn, Type: kind}
	}

	if diff := infraDepDifference("gcg-1", []beads.Dep{dep("gcg-2", "blocks")}, []beads.Dep{dep("gcg-2", "blocks")}, infra); diff != "" {
		t.Errorf("a faithful edge was refused: %s", diff)
	}
	if diff := infraDepDifference("gcg-1", []beads.Dep{dep("gcg-2", "blocks")}, nil, infra); diff == "" {
		t.Error("a missing edge compared equal")
	}
	if diff := infraDepDifference("gcg-1", nil, []beads.Dep{dep("gcg-2", "blocks")}, infra); diff == "" {
		t.Error("an invented edge compared equal")
	}
	if diff := infraDepDifference("gcg-1", []beads.Dep{dep("gcg-2", "tracks")}, []beads.Dep{dep("gcg-2", "blocks")}, infra); diff == "" {
		t.Error("an edge whose type the copy changed compared equal")
	}
	// The destination normalizes an empty dep type to its own default, so an
	// empty source type is not evidence of a mangled copy.
	if diff := infraDepDifference("gcg-1", []beads.Dep{dep("gcg-2", "")}, []beads.Dep{dep("gcg-2", "blocks")}, infra); diff != "" {
		t.Errorf("a normalized empty dep type was refused: %s", diff)
	}
	// Cross-boundary edges into work are metadata linkage, resolved by the
	// owning-store read on each side. They are not re-added and their absence is
	// not a defect.
	if diff := infraDepDifference("gcg-1", []beads.Dep{dep("gc-9", "blocks")}, nil, infra); diff != "" {
		t.Errorf("a cross-boundary edge into work was treated as missing: %s", diff)
	}
	// But an edge into work on the DESTINATION is invented: nothing in this
	// migration writes one.
	if diff := infraDepDifference("gcg-1", nil, []beads.Dep{dep("gc-9", "blocks")}, infra); diff == "" {
		t.Error("a cross-boundary edge invented on the destination compared equal")
	}
}

// TestVerifyInfraCopyRefusesAStrippedEdgePayload proves the equality stage
// ACTUALLY ASKS about payloads, which no other test in this slice can show.
//
// Every other one drives the copy, and the copy carries payloads correctly — so
// deleting the payload comparison out of verifyInfraCopy leaves all of them
// green. The gap is only visible against a destination that received a faithful
// copy and then lost the payload, which is exactly the state a future
// regression in the writer would produce.
//
// Stripping is done by re-adding the edge with a bare DepAdd: on this
// destination that is not a no-op, because setGraphEdgeMetadataTx clears the
// pair's sidecar before deciding it has nothing to store. That destructive
// re-add is the regression itself, reproduced.
func TestVerifyInfraCopyRefusesAStrippedEdgePayload(t *testing.T) {
	backing, from, to, _ := seedInfraEdgeSource(t)
	const payload = `{"gate":"waits_for","threshold":3}`
	source := &payloadCarryingSource{
		Store:    backing,
		payloads: map[[2]string]string{{from.ID, to.ID}: payload},
	}

	rows, err := readInfraSnapshot(source)
	if err != nil {
		t.Fatalf("readInfraSnapshot: %v", err)
	}
	destination, reopen := infraEqualityDestination(t)
	if _, err := importInfraSnapshot(destination, source, rows); err != nil {
		t.Fatalf("importing: %v", err)
	}
	if _, err := verifyInfraCopy(reopen, source); err != nil {
		t.Fatalf("the equality stage refused a faithful copy: %v", err)
	}

	if err := destination.DepAdd(from.ID, to.ID, "blocks"); err != nil {
		t.Fatalf("stripping the payload: %v", err)
	}
	if _, err := verifyInfraCopy(reopen, source); err == nil {
		t.Fatal("the equality stage blessed a destination that holds every edge and none of their payloads; a copy that lost every formula gate would get a convergence marker")
	}
}

// TestInfraEdgePayloadDifferenceComparesPayloadsBothWays is the edge-payload
// half of the equality stage, and the reason the carry is safe rather than
// merely present.
//
// A copy that moved every edge and dropped every gate produces an identical
// beads.Dep set on both sides, so infraDepDifference clears it and the marker
// lands on a binding that has quietly lost its formula gating. Only this
// comparison sees that, and it sees it by asking each store about each pair
// rather than by comparing two slices.
//
// Both directions, and the four cases the engines make possible:
// dropped, invented, changed, and the pair of empty spellings that must NOT be
// read as a difference — Dolt renders an absent payload as "{}" while SQLite
// stores nothing, and a comparison that called those unequal would refuse every
// Dolt-sourced city.
func TestInfraEdgePayloadDifferenceComparesPayloadsBothWays(t *testing.T) {
	backing, from, to, work := seedInfraEdgeSource(t)
	infra := map[string]bool{from.ID: true, to.ID: true}
	deps := []beads.Dep{
		{IssueID: from.ID, DependsOnID: to.ID, Type: "blocks"},
		{IssueID: from.ID, DependsOnID: work.ID, Type: "blocks"},
	}
	const gate = `{"gate":"waits_for","threshold":3}`
	carrying := func(payload string) beads.Store {
		return &payloadCarryingSource{Store: backing, payloads: map[[2]string]string{{from.ID, to.ID}: payload}}
	}
	difference := func(t *testing.T, source, destination beads.Store) string {
		t.Helper()
		diff, err := infraEdgePayloadDifference(from.ID, deps, deps, infra, source, destination)
		if err != nil {
			t.Fatalf("comparing edge payloads: %v", err)
		}
		return diff
	}

	if diff := difference(t, carrying(gate), carrying(gate)); diff != "" {
		t.Errorf("a faithfully carried payload was refused: %s", diff)
	}
	if diff := difference(t, carrying(gate), backing); diff == "" {
		t.Error("a destination that dropped the payload compared equal; the equality stage would bless a copy that lost every formula gate")
	}
	if diff := difference(t, backing, carrying(gate)); diff == "" {
		t.Error("a destination that invented a payload compared equal")
	}
	if diff := difference(t, carrying(gate), carrying(`{"gate":"waits_for","threshold":9}`)); diff == "" {
		t.Error("a destination whose payload differs in content compared equal")
	}
	// The engines' two spellings of "carries nothing" are the same answer.
	if diff := difference(t, carrying("{}"), backing); diff != "" {
		t.Errorf("a Dolt-sourced empty payload was read as a difference against a destination holding nothing: %s", diff)
	}

	// A source that cannot be asked is not a source that answered "nothing" —
	// the same rule the refusal upstream enforces, restated here because this
	// stage reads the stores directly and could quietly reach the opposite
	// conclusion.
	if _, err := infraEdgePayloadDifference(from.ID, deps, deps, infra, mutePayloadSource{Store: backing}, carrying(gate)); err == nil {
		t.Error("the equality stage treated an unanswerable source as one carrying nothing")
	}

	// An edge only the DESTINATION holds. Every case above hands the same slice
	// as both sides, so the walk over the destination's own deps is never the
	// thing that finds anything and could be deleted with the suite still green.
	//
	// Through verifyInfraCopy this is unreachable — infraDepDifference runs
	// first and refuses any structural asymmetry before payloads are compared —
	// which is exactly why it is pinned on the function directly. The comparator
	// is a general one, the doc above it claims both directions, and the next
	// caller need not run a structural pass first.
	// The source's only edge leaves infra, so the wantDeps walk — which filters
	// to within-infra targets — contributes no pair at all. Every pair here comes
	// from the destination's own list, which is what makes this the one case that
	// dies if that walk is deleted.
	sourceDeps := []beads.Dep{{IssueID: from.ID, DependsOnID: work.ID, Type: "blocks"}}
	onlyOnDestination := []beads.Dep{
		{IssueID: from.ID, DependsOnID: work.ID, Type: "blocks"},
		{IssueID: from.ID, DependsOnID: to.ID, Type: "blocks"},
	}
	diff, err := infraEdgePayloadDifference(from.ID, sourceDeps, onlyOnDestination, infra, backing, carrying(gate))
	if err != nil {
		t.Fatalf("comparing an edge only the destination holds: %v", err)
	}
	if diff == "" {
		t.Error("a payload on an edge only the destination holds compared equal; the walk over the destination's deps finds nothing")
	}
}

// countingInfraSource records how many times the equality stage re-read the
// work store.
type countingInfraSource struct {
	beads.Store
	lists int
}

func (s *countingInfraSource) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.lists++
	return s.Store.List(query)
}

// TestVerifyInfraCopyReReadsTheSourceAtVerificationTime pins the behavior that
// turns a mid-copy write into a refusal instead of a silent strand: the source
// is enumerated again at verification time, not taken from the snapshot the copy
// was derived from.
//
// Pinned two ways, because either alone is weak. The re-read must HAPPEN (a
// verification that never touched the source could not see a late write at all),
// and it must MATTER (a bead added after the copy is a refusal).
func TestVerifyInfraCopyReReadsTheSourceAtVerificationTime(t *testing.T) {
	backing := beads.NewMemStore()
	source := &countingInfraSource{Store: backing}
	early, err := backing.Create(beads.Bead{Title: "copied", Type: "session", Labels: []string{"gc:session"}})
	if err != nil {
		t.Fatal(err)
	}

	rows, err := readInfraSnapshot(source)
	if err != nil {
		t.Fatal(err)
	}
	destination, reopen := infraEqualityDestination(t)
	if _, err := importInfraSnapshot(destination, source, rows); err != nil {
		t.Fatalf("importing: %v", err)
	}

	before := source.lists
	if _, err := verifyInfraCopy(reopen, source); err != nil {
		t.Fatalf("the equality stage refused a faithful copy: %v", err)
	}
	if source.lists == before {
		t.Fatal("the equality stage did not re-read the work store; it compared against the copy's own snapshot")
	}

	late, err := backing.Create(beads.Bead{Title: "arrived after the copy", Type: "session", Labels: []string{"gc:session"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifyInfraCopy(reopen, source); err == nil {
		t.Fatalf("the equality stage blessed a copy that stranded %s (%s)", late.ID, late.Title)
	}
	if _, err := destination.Get(early.ID); err != nil {
		t.Fatalf("the equality stage mutated the destination: %v", err)
	}
}

// unlinkingInfraSource removes the destination database at the exact moment the
// equality stage re-reads the work store — the first thing verifyInfraCopy does,
// and therefore the last moment before it opens the destination for itself.
//
// It is the only way to tell the two proofs apart. A warm SQLite handle keeps
// reading the unlinked inode and answers exactly as it did before, so a copy
// verified through the writing connection still looks perfect; a verification
// that opens the path finds nothing there. Nothing else distinguishes them,
// because for this store a warm handle and a fresh one agree on every byte.
type unlinkingInfraSource struct {
	beads.Store
	t        *testing.T
	database string
	lists    int
	unlinked bool
}

func (s *unlinkingInfraSource) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.lists++
	// Call one is the copy's read; call two is the equality stage's re-read.
	if s.lists == 2 && !s.unlinked {
		s.unlinked = true
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if err := os.Remove(s.database + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
				s.t.Fatalf("unlinking %s: %v", s.database+suffix, err)
			}
		}
	}
	return s.Store.List(query)
}

// DepMetadata forwards the leaf's edge-payload read. The subject here is which
// bytes the equality stage reads, not what an edge carries, and a double that
// stays silent about the capability is refused as unanswerable before the
// unlink this test turns on can ever happen.
func (s *unlinkingInfraSource) DepMetadata(issueID, dependsOnID string) (string, bool, error) {
	return depMetadataThrough(s.Store, issueID, dependsOnID)
}

// TestEnsureInfraClassMigratedProvesEqualityAgainstTheReopenedDatabase pins the
// stage boundary the marker's meaning rests on: what convergence attests to is
// the database the runtime binding will open, not the connection this migration
// wrote through.
//
// The distinction is invisible until the two disagree, so the test makes them
// disagree in the only way a POSIX filesystem allows — the database is unlinked
// after the copy and before the proof. A verification holding the writing handle
// reads the unlinked inode, sees a flawless copy, and writes a marker over a
// path that no longer holds a database. One that opens the path sees nothing and
// refuses.
func TestEnsureInfraClassMigratedProvesEqualityAgainstTheReopenedDatabase(t *testing.T) {
	cityPath := t.TempDir()
	cfg := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))
	target := mustResolveInfraTarget(t, cityPath, cfg)

	backing := beads.NewMemStore()
	if _, err := backing.Create(beads.Bead{Title: "session", Type: "session", Labels: []string{"gc:session"}}); err != nil {
		t.Fatal(err)
	}
	source := &unlinkingInfraSource{Store: backing, t: t, database: target.Database}
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) { return source, nil }
	t.Cleanup(func() { openInfraMigrationSource = prev })

	var log strings.Builder
	got := migrateInfraClasses(t, cityPath, cfg, &log)
	if !source.unlinked {
		t.Fatal("the destination database was never unlinked; this test proved nothing")
	}
	if got.Outcome != infraMigrationUnconverged {
		t.Fatalf("outcome = %v, want unconverged: the equality stage was satisfied by a database that is no longer on disk, "+
			"so it read the writing handle rather than the bytes the runtime binding opens; log: %s", got.Outcome, log.String())
	}
	assertNoConvergenceMarker(t, target,
		"the copy was proven only against the writing connection, not against the bytes on disk")
}

// idSuffix returns the numeric suffix of a store-minted id.
func idSuffix(t *testing.T, id string) int {
	t.Helper()
	idx := strings.LastIndex(id, "-")
	if idx < 0 {
		t.Fatalf("id %q has no numeric suffix", id)
	}
	n, err := strconv.Atoi(id[idx+1:])
	if err != nil {
		t.Fatalf("id %q has no numeric suffix: %v", id, err)
	}
	return n
}

// TestInfraDestinationCannotReMintAnImportedID pins both legs of the id
// allocator against the imported slice. A re-minted id is not a copy defect —
// the copy is long finished — it is a collision the city discovers later, when
// two different beads answer to one id.
//
// Leg one is in-memory: normalizeCreate raises the sequence floor for every
// foreign id it writes, so the handle that just imported cannot reissue one.
// Leg two survives a restart: recoverSequence scans the imported rows when the
// database is opened again, so a fresh process cannot reissue one either. They
// fail independently — the second is the one a boot migration actually depends
// on, because the runtime opens its own handle.
func TestInfraDestinationCannotReMintAnImportedID(t *testing.T) {
	prefix, ok := config.ReservedClassPrefix(config.BeadClassGraph)
	if !ok || prefix == "" {
		t.Fatal("no reserved id prefix is registered for the graph class")
	}
	imported := prefix + "-500"

	cityPath := t.TempDir()
	cfg := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))
	target := mustResolveInfraTarget(t, cityPath, cfg)

	destination, err := openInfraDestination(target)
	if err != nil {
		t.Fatalf("opening the destination: %v", err)
	}
	creator, ok := destination.(beads.ForeignIDCreator)
	if !ok {
		t.Fatalf("the deployed destination %T cannot preserve ids", destination)
	}
	if _, err := creator.CreateWithForeignID(beads.Bead{ID: imported, Title: "imported", Type: "session"}); err != nil {
		t.Fatalf("importing %s: %v", imported, err)
	}
	warm, err := destination.Create(beads.Bead{Title: "minted by the importing handle", Type: "session"})
	if err != nil {
		t.Fatalf("minting from the importing handle: %v", err)
	}
	if got := idSuffix(t, warm.ID); got <= 500 {
		t.Fatalf("the importing handle minted %s, which can collide with the imported %s", warm.ID, imported)
	}
	if err := closeBeadStoreHandle(destination); err != nil {
		t.Fatalf("closing the destination: %v", err)
	}

	reopened, err := openInfraDestination(target)
	if err != nil {
		t.Fatalf("reopening the destination: %v", err)
	}
	t.Cleanup(func() { _ = closeBeadStoreHandle(reopened) })
	cold, err := reopened.Create(beads.Bead{Title: "minted after a restart", Type: "session"})
	if err != nil {
		t.Fatalf("minting after a restart: %v", err)
	}
	if got := idSuffix(t, cold.ID); got <= idSuffix(t, warm.ID) {
		t.Fatalf("a reopened destination minted %s, which collides with an id already in the database (%s, %s)", cold.ID, imported, warm.ID)
	}
	for _, id := range []string{imported, warm.ID} {
		if cold.ID == id {
			t.Fatalf("a reopened destination re-minted %s", id)
		}
	}
}

// gcgSourceStore returns a work store that mints ids under the graph class's
// reserved prefix, which is what a migrated city's retained source actually
// holds. MemStore mints its own "gc-" ids and cannot stand in here: the
// destination's sequence recovery only scans its OWN prefix, so a source that
// never produces one would make the collision test vacuous.
func gcgSourceStore(t *testing.T) beads.Store {
	t.Helper()
	prefix, ok := config.ReservedClassPrefix(config.BeadClassGraph)
	if !ok || prefix == "" {
		t.Fatal("no reserved id prefix is registered for the graph class")
	}
	store, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix(prefix))
	if err != nil {
		t.Fatalf("opening the source store: %v", err)
	}
	t.Cleanup(func() { _ = closeBeadStoreHandle(store) })
	return store
}

// TestEnsureInfraClassMigratedLeavesAnAllocatorThatCannotCollide is the boot-path
// form of the same property: after a converged migration, the handle the RUNTIME
// opens must not reissue an id the migration imported.
//
// It pins the outcome rather than either leg. A third mechanism stands behind
// them — Create's mintUniqueIDTx re-scans for the maximum suffix inside its own
// transaction and skips an id that already exists — so this test still passes
// with either named leg disabled. That is the right shape for a boot-path test
// (the city is safe if ANY of the three holds) and the wrong shape for proving
// the two the acceptance criteria name, which is what
// TestInfraDestinationCannotReMintAnImportedID does instead.
func TestEnsureInfraClassMigratedLeavesAnAllocatorThatCannotCollide(t *testing.T) {
	cityPath := t.TempDir()
	source := gcgSourceStore(t)
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) { return source, nil }
	t.Cleanup(func() { openInfraMigrationSource = prev })

	var highest string
	for i := 0; i < 3; i++ {
		b, err := source.Create(beads.Bead{Title: fmt.Sprintf("session %d", i), Type: "session", Labels: []string{"gc:session"}})
		if err != nil {
			t.Fatalf("seeding the source: %v", err)
		}
		highest = b.ID
	}

	cfg := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))
	var log strings.Builder
	if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("migration outcome = %v, want converged; log: %s", got.Outcome, log.String())
	}

	target := mustResolveInfraTarget(t, cityPath, cfg)
	runtime := openMigratedDestination(t, target)
	minted, err := runtime.Create(beads.Bead{Title: "minted after the cutover", Type: "session"})
	if err != nil {
		t.Fatalf("minting from the runtime handle: %v", err)
	}
	if got := idSuffix(t, minted.ID); got <= idSuffix(t, highest) {
		t.Fatalf("the runtime handle minted %s, colliding with the imported slice whose highest id is %s", minted.ID, highest)
	}
}

// TestInfraSnapshotLeavesSyntheticConvoysInTheWorkStore is the blast-radius pin
// for reclassifying synthetic convoys as WORK.
//
// readInfraSnapshot is the one selector shared by the copy and by the boot-time
// containment re-check, so a bead it returns is both carried into the binding
// and, if it is ever absent from the binding, reported as STRANDED — which
// refuses the boot. Synthetic convoys are minted in the work store and stay
// there, so returning them would make every drain a city runs after cutover mint
// a fresh bead the binding never receives, and the next boot would count them as
// stranded writes and refuse to serve.
//
// The wisp row is the other half and is what makes this a boundary rather than a
// blanket exemption. Wisp roots stay GRAPH class, so a city whose binding is
// missing them still reports them stranded exactly as it does today.
func TestInfraSnapshotLeavesSyntheticConvoysInTheWorkStore(t *testing.T) {
	source := beads.NewMemStore()
	seed := func(b beads.Bead) beads.Bead {
		created, err := source.Create(b)
		if err != nil {
			t.Fatalf("seeding %s: %v", b.Title, err)
		}
		return created
	}

	inputConvoy := seed(beads.Bead{
		Title:    "input convoy for a graph.v2 pour",
		Type:     "convoy",
		Metadata: map[string]string{beadmeta.SyntheticMetadataKey: "true"},
	})
	unitConvoy := seed(beads.Bead{
		Title:    "drain unit 0",
		Type:     "convoy",
		Metadata: map[string]string{beadmeta.SyntheticKindMetadataKey: "drain-unit-convoy"},
	})
	userConvoy := seed(beads.Bead{Title: "a human convoy", Type: "convoy"})
	member := seed(beads.Bead{Title: "a work bead", Type: "task"})

	// The maintainer-city shape: wisp roots carrying each of the three markers
	// the wisp arm matches. All three stay graph class.
	wispByKind := seed(beads.Bead{Title: "wisp root", Type: "molecule", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWisp}})
	wispByType := seed(beads.Bead{Title: "wisp by type", Type: beadmeta.KindWisp})
	wispByLabel := seed(beads.Bead{Title: "wisp by label", Type: "task", Labels: []string{"gc:wisp"}})
	sessionBead := seed(beads.Bead{Title: "a session", Type: "session"})

	rows, err := readInfraSnapshot(source)
	if err != nil {
		t.Fatalf("readInfraSnapshot: %v", err)
	}
	carried := make(map[string]bool, len(rows))
	for _, row := range rows {
		carried[row.ID] = true
	}

	for _, tc := range []struct {
		bead beads.Bead
		why  string
	}{
		{inputConvoy, "a graph.v2 input convoy is minted in the work store alongside the members it tracks"},
		{unitConvoy, "a drain-unit convoy is minted in the work store alongside the member it tracks"},
		{userConvoy, "a user convoy has always been work"},
		{member, "an ordinary work bead"},
	} {
		if carried[tc.bead.ID] {
			t.Errorf("readInfraSnapshot carried %s (%s): %s; a work bead the copy carries is one the boot-time containment check will report stranded when the binding does not have it",
				tc.bead.ID, tc.bead.Title, tc.why)
		}
	}

	for _, b := range []beads.Bead{wispByKind, wispByType, wispByLabel, sessionBead} {
		if !carried[b.ID] {
			t.Errorf("readInfraSnapshot dropped %s (%s); the synthetic-convoy reclassification must not widen past convoys", b.ID, b.Title)
		}
	}
	if got := len(rows); got != 4 {
		t.Fatalf("snapshot carried %d rows, want exactly the 4 infra beads; ids=%v", got, carried)
	}
}
