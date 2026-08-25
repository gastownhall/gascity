package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// payloadCarryingSource is a work store whose edges carry payloads. It is the
// production shape a Dolt-backed source has now that NativeDoltStore can read
// the dependencies.metadata column, expressed over a MemStore so the migration
// tests keep their in-memory source.
type payloadCarryingSource struct {
	beads.Store
	payloads map[[2]string]string
	// readErr is the failure the store answers with instead of a payload. A
	// read that fails is a third answer alongside "carries one" and "carries
	// nothing", and the migration must treat it as the first: it learned
	// nothing, so it may not proceed.
	readErr error
}

func (s *payloadCarryingSource) DepMetadata(issueID, dependsOnID string) (string, bool, error) {
	if s.readErr != nil {
		return "", false, s.readErr
	}
	payload, ok := s.payloads[[2]string{issueID, dependsOnID}]
	return payload, ok, nil
}

// mutePayloadSource is a work store that cannot be asked about edge payloads.
// The embedded field's static type is the interface, so DepMetadata is not
// promoted even though the MemStore inside implements it.
type mutePayloadSource struct{ beads.Store }

// depMetadataThrough forwards an edge-payload read to the leaf inside a test
// double that embeds beads.Store.
//
// Every migration double needs this: embedding the interface strips
// beads.DepMetadataReader, the migration reads a stripped capability as UNABLE
// TO ANSWER, and the double is refused before the property it exists to prove
// can happen. mutePayloadSource is the one double that deliberately skips it,
// which is the control that keeps the refusal honest.
func depMetadataThrough(leaf beads.Store, issueID, dependsOnID string) (string, bool, error) {
	reader, ok := leaf.(beads.DepMetadataReader)
	if !ok {
		return "", false, fmt.Errorf("reading dependency metadata %s -> %s: test double leaf %T exposes no edge-payload read", issueID, dependsOnID, leaf)
	}
	return reader.DepMetadata(issueID, dependsOnID)
}

// seedInfraEdgeSource returns a MemStore holding two infra beads with an edge
// between them and one work bead the infra source also points at.
func seedInfraEdgeSource(t *testing.T) (store *beads.MemStore, from, to, work beads.Bead) {
	t.Helper()
	store = beads.NewMemStore()
	seed := func(b beads.Bead) beads.Bead {
		created, err := store.Create(b)
		if err != nil {
			t.Fatalf("seeding %s: %v", b.Title, err)
		}
		return created
	}
	from = seed(beads.Bead{Title: "gated step", Type: "session"})
	to = seed(beads.Bead{Title: "the step it waits on", Type: "session"})
	work = seed(beads.Bead{Title: "an ordinary work bead", Type: "task"})
	if err := store.DepAdd(from.ID, to.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd within infra: %v", err)
	}
	if err := store.DepAdd(from.ID, work.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd across the boundary: %v", err)
	}
	return store, from, to, work
}

// migrateFromSource points the migration at one prepared source and runs it
// against a fresh split city, returning the report and what it said.
func migrateFromSource(t *testing.T, source beads.Store) (infraMigrationReport, string) {
	t.Helper()
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) { return source, nil }
	t.Cleanup(func() { openInfraMigrationSource = prev })

	cityPath := t.TempDir()
	cfg := infraSplitConfig(".gc/store")
	var stderr bytes.Buffer
	report := migrateInfraClasses(t, cityPath, cfg, &stderr)
	return report, stderr.String()
}

// TestInfraMigrationRefusesASourceCarryingEdgePayloads is the interim safety
// property: until the copy can CARRY a dependency payload, a source that has
// one is refused rather than copied.
//
// The copy re-adds edges with beads.Dep, which holds the pair and the type and
// nothing else, so a payload on the source has no way across — and on the
// destination the empty carry is not merely absent but destructive, because
// setGraphEdgeMetadataTx clears the pair's sidecar before deciding it has
// nothing to store. Refusing is strictly better than dropping: the source is
// retained and unchanged, so a refused city is exactly where it was.
func TestInfraMigrationRefusesASourceCarryingEdgePayloads(t *testing.T) {
	backing, from, to, _ := seedInfraEdgeSource(t)
	source := &payloadCarryingSource{
		Store:    backing,
		payloads: map[[2]string]string{{from.ID, to.ID}: `{"gate":"waits_for","threshold":3}`},
	}

	report, said := migrateFromSource(t, source)
	if report.Outcome == infraMigrationConverged {
		t.Fatal("the migration converged a city whose source carries edge payloads the copy drops")
	}
	if report.Outcome != infraMigrationUnconverged {
		t.Fatalf("Outcome = %v, want infraMigrationUnconverged", report.Outcome)
	}
	for _, want := range []string{from.ID, to.ID, "payload"} {
		if !strings.Contains(said, want) {
			t.Errorf("the refusal does not mention %q, so an operator cannot find the edge it is about: %s", want, said)
		}
	}
	// Nothing may have been written: the refusal runs before the destination is
	// touched, so a re-run after the payload is carried starts from empty.
	if !report.BindingProvenEmpty {
		t.Errorf("the refusal left content in the binding (probe: %s)", report.BindingProbe)
	}
}

// TestInfraMigrationProceedsWhenNoEdgeCarriesAPayload is the control the
// refusal is worthless without. The same city, the same edges, no payload —
// and it must converge, or the check has simply stopped every migration.
func TestInfraMigrationProceedsWhenNoEdgeCarriesAPayload(t *testing.T) {
	backing, _, _, _ := seedInfraEdgeSource(t)

	report, said := migrateFromSource(t, backing)
	if report.Outcome != infraMigrationConverged {
		t.Fatalf("Outcome = %v, want infraMigrationConverged; stderr: %s", report.Outcome, said)
	}
}

// TestInfraMigrationIgnoresAPayloadOnACrossBoundaryEdge pins the refusal's
// scope. An edge from an infra bead into work is not re-added by the copy at
// all — it stays metadata linkage resolved by the owning-store read on each
// side — so its payload is not something the copy drops, and refusing on it
// would block cities over an edge the migration never touches.
func TestInfraMigrationIgnoresAPayloadOnACrossBoundaryEdge(t *testing.T) {
	backing, from, _, work := seedInfraEdgeSource(t)
	source := &payloadCarryingSource{
		Store:    backing,
		payloads: map[[2]string]string{{from.ID, work.ID}: `{"gate":"waits_for"}`},
	}

	report, said := migrateFromSource(t, source)
	if report.Outcome != infraMigrationConverged {
		t.Fatalf("Outcome = %v, want infraMigrationConverged; a payload on an edge the copy does not carry blocked the migration: %s", report.Outcome, said)
	}
}

// TestInfraMigrationRefusesASourceThatCannotReportEdgePayloads pins the
// fail-closed half. A source this build cannot ask is not a source it may
// assume is clean — that assumption is exactly what the drop was.
func TestInfraMigrationRefusesASourceThatCannotReportEdgePayloads(t *testing.T) {
	backing, _, _, _ := seedInfraEdgeSource(t)

	report, said := migrateFromSource(t, mutePayloadSource{Store: backing})
	if report.Outcome != infraMigrationUnconverged {
		t.Fatalf("Outcome = %v, want infraMigrationUnconverged; stderr: %s", report.Outcome, said)
	}
	if !strings.Contains(said, "payload") {
		t.Errorf("the refusal does not say what it could not read: %s", said)
	}
}

// TestInfraSourceEdgePayloadRefusalIsSilentOnAnEmptyPayload pins the rule at
// the boundary the engines disagree on. Dolt renders an absent payload as the
// empty JSON object; a reader that called that a payload would refuse every
// city, which is the same as having no check at all.
func TestInfraSourceEdgePayloadRefusalIsSilentOnAnEmptyPayload(t *testing.T) {
	backing, from, to, _ := seedInfraEdgeSource(t)
	rows, err := readInfraSnapshot(backing)
	if err != nil {
		t.Fatalf("readInfraSnapshot: %v", err)
	}

	for _, empty := range []string{"", "{}", "  ", "null"} {
		source := &payloadCarryingSource{
			Store:    backing,
			payloads: map[[2]string]string{{from.ID, to.ID}: empty},
		}
		if err := infraSourceEdgePayloadRefusal(source, rows); err != nil {
			t.Errorf("an edge whose payload is %q was refused: %v", empty, err)
		}
	}
	carrying := &payloadCarryingSource{
		Store:    backing,
		payloads: map[[2]string]string{{from.ID, to.ID}: `{"gate":"waits_for"}`},
	}
	if err := infraSourceEdgePayloadRefusal(carrying, rows); err == nil {
		t.Error("a real payload was not refused")
	}
}

// TestInfraMigrationRefusesWhenTheEdgePayloadReadFails covers the third answer.
// A read that errors is not a read that found nothing: the migration learned
// nothing about the edge, so proceeding would drop a payload it never saw.
func TestInfraMigrationRefusesWhenTheEdgePayloadReadFails(t *testing.T) {
	backing, _, _, _ := seedInfraEdgeSource(t)
	source := &payloadCarryingSource{Store: backing, readErr: errors.New("the dependencies table is unreadable")}

	report, said := migrateFromSource(t, source)
	if report.Outcome != infraMigrationUnconverged {
		t.Fatalf("Outcome = %v, want infraMigrationUnconverged; a failed payload read was treated as an empty one: %s", report.Outcome, said)
	}
	if !strings.Contains(said, "the dependencies table is unreadable") {
		t.Errorf("the refusal does not carry the read failure, so an operator cannot tell why it stopped: %s", said)
	}
}

// TestPolicyWrappedStoreAnswersTheEdgePayloadRead pins the forwarding the
// production migration source depends on.
//
// openInfraMigrationSource hands back a policy-wrapped store, and beadPolicyStore
// embeds the beads.Store interface — which strips every optional capability
// discovered by type-assertion. Without an explicit forward the migration reads
// the wrapper as UNABLE TO ANSWER and refuses every city, including ones whose
// leaf store answers fine. The assertion below is the whole subject: the wrapper
// must both satisfy the reader AND return the leaf's answer, not an error.
func TestPolicyWrappedStoreAnswersTheEdgePayloadRead(t *testing.T) {
	backing, from, to, _ := seedInfraEdgeSource(t)
	store := wrapStoreWithBeadPolicies(backing, &config.City{})

	reader, ok := store.(beads.DepMetadataReader)
	if !ok {
		t.Fatalf("policy-wrapped store %T does not satisfy beads.DepMetadataReader: "+
			"the migration will read every city as unanswerable and refuse it", store)
	}
	payload, carried, err := reader.DepMetadata(from.ID, to.ID)
	if err != nil {
		t.Fatalf("reading the payload through the policy wrapper: %v", err)
	}
	if carried || payload != "" {
		t.Fatalf("DepMetadata = (%q, %v), want (\"\", false): the MemStore leaf holds no edge payloads", payload, carried)
	}
}
