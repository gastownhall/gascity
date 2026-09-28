package main

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
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

// TestInfraMigrationCarriesAnEdgePayloadToTheBinding is the property this file
// was written to demand and could not yet assert: a source edge carrying a
// waits_for gate arrives in the binding still carrying it.
//
// Until the carry existed the migration refused such a city — better than
// dropping the payload, since the source is retained and a refused city is
// exactly where it was, but it meant no city whose formulas use waits_for gates
// could cut over at all. What makes the carry safe rather than merely present
// is that the equality stage now witnesses the payload too: a copy that moved
// the edge and dropped the gate is refused before the marker, not blessed by it.
func TestInfraMigrationCarriesAnEdgePayloadToTheBinding(t *testing.T) {
	backing, from, to, _ := seedInfraEdgeSource(t)
	const payload = `{"gate":"waits_for","threshold":3}`
	source := &payloadCarryingSource{
		Store:    backing,
		payloads: map[[2]string]string{{from.ID, to.ID}: payload},
	}

	report, said := migrateFromSource(t, source)
	if report.Outcome != infraMigrationConverged {
		t.Fatalf("Outcome = %v, want infraMigrationConverged; the copy still cannot carry an edge payload: %s", report.Outcome, said)
	}

	binding := openMigratedBinding(t, report)
	got, carried, err := binding.DepMetadata(from.ID, to.ID)
	if err != nil {
		t.Fatalf("reading the payload back out of the binding: %v", err)
	}
	if !carried || got != payload {
		t.Fatalf("the binding's edge %s -> %s carries (%q, %v), want (%q, true): the migration converged having dropped the gate",
			from.ID, to.ID, got, carried, payload)
	}
}

// TestInfraMigrationLeavesAPayloadlessEdgeCarryingNothing is the other half of
// the carry, and the one a writer is most likely to get wrong.
//
// setGraphEdgeMetadataTx stores whatever it is handed, so a copy that passed
// through an engine's rendering of "no payload" — Dolt hands back "{}" — would
// write that literally and the binding would read back a payload the source
// never had. Absent and present-but-empty are exactly the two the binding's own
// adoption witness insists must stay distinguishable.
func TestInfraMigrationLeavesAPayloadlessEdgeCarryingNothing(t *testing.T) {
	backing, from, to, _ := seedInfraEdgeSource(t)
	for _, empty := range []string{"", "{}", "  ", "null"} {
		t.Run(fmt.Sprintf("source says %q", empty), func(t *testing.T) {
			source := &payloadCarryingSource{
				Store:    backing,
				payloads: map[[2]string]string{{from.ID, to.ID}: empty},
			}
			report, said := migrateFromSource(t, source)
			if report.Outcome != infraMigrationConverged {
				t.Fatalf("Outcome = %v, want infraMigrationConverged: %s", report.Outcome, said)
			}
			binding := openMigratedBinding(t, report)
			got, carried, err := binding.DepMetadata(from.ID, to.ID)
			if err != nil {
				t.Fatalf("reading the payload back out of the binding: %v", err)
			}
			if carried || got != "" {
				t.Fatalf("the binding's edge carries (%q, %v), want (\"\", false): the copy stored an engine's spelling of no payload as a payload", got, carried)
			}
		})
	}
}

// TestInfraCopyDepEdgeRefusesADestinationThatCannotCarryAPayload pins the
// fail-closed half of the carry, mirroring the source-side rule: a destination
// that cannot hold the payload is refused rather than written to without it.
//
// It also pins the narrowness of that refusal. A store with no writer is a
// perfectly good destination for an edge that carries nothing, and refusing it
// unconditionally would reject every destination in this tree apart from
// SQLite — including the MemStore every other migration test uses.
func TestInfraCopyDepEdgeRefusesADestinationThatCannotCarryAPayload(t *testing.T) {
	backing, from, to, _ := seedInfraEdgeSource(t)
	destination := beads.NewMemStore()
	if _, ok := beads.Store(destination).(beads.DepMetadataWriter); ok {
		t.Fatal("MemStore now carries edge payloads, so it is no longer the store this test needs")
	}

	carrying := &payloadCarryingSource{
		Store:    backing,
		payloads: map[[2]string]string{{from.ID, to.ID}: `{"gate":"waits_for"}`},
	}
	err := infraCopyDepEdge(destination, carrying, from.ID, to.ID, "blocks")
	if err == nil {
		t.Fatal("a destination that cannot carry a payload accepted an edge that has one, dropping it silently")
	}
	if !strings.Contains(err.Error(), from.ID) || !strings.Contains(err.Error(), "payload") {
		t.Errorf("the refusal does not name the edge and what was lost: %v", err)
	}

	// The control: the same destination, the same edge, no payload. It must be
	// written, or the refusal above is indistinguishable from a blanket one.
	if err := infraCopyDepEdge(destination, backing, from.ID, to.ID, "blocks"); err != nil {
		t.Fatalf("a payloadless edge was refused by a destination that has nothing to lose: %v", err)
	}
	deps, err := destination.DepList(from.ID, "down")
	if err != nil {
		t.Fatalf("DepList: %v", err)
	}
	if len(deps) != 1 || deps[0].DependsOnID != to.ID {
		t.Fatalf("the payloadless edge did not land: %+v", deps)
	}
}

// openMigratedBinding reopens the binding a completed migration wrote and asks
// it for its edge-payload read, for a test that needs to see what landed rather
// than what the report says landed.
func openMigratedBinding(t *testing.T, report infraMigrationReport) beads.DepMetadataReader {
	t.Helper()
	if report.Target.Database == "" {
		t.Fatal("the report names no database, so there is nothing to read back")
	}
	opened := openMigratedDestination(t, report.Target)
	reader, ok := opened.(beads.DepMetadataReader)
	if !ok {
		t.Fatalf("the binding opened as %T, which cannot be asked about edge payloads", opened)
	}
	return reader
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
	assertRefusedBeforeTheDestinationWasTouched(t, report, said)
}

// assertRefusedBeforeTheDestinationWasTouched pins WHERE a source-side edge
// refusal happens, which the outcome alone cannot say.
//
// Both refusals below would still report infraMigrationUnconverged if the check
// moved from infraSourceEdgePayloadRefusal into the import — infraCopyDepEdge
// reaches the same fault a moment later, and fail() maps any pre-marker failure
// to the same outcome. The difference is the operator's: a refusal taken before
// the destination is opened leaves a binding a revert cannot abandon anything
// from, and BindingProvenEmpty is the only positive evidence of that in the
// report. It is also what keeps the preflight parity honest, since the rehearsal
// opens no destination and can only mirror a check that runs before one exists.
func assertRefusedBeforeTheDestinationWasTouched(t *testing.T, report infraMigrationReport, said string) {
	t.Helper()
	if report.BindingProbe != nil {
		t.Fatalf("the binding probe could not run (%v), so nothing here proves the refusal came before the copy: %s", report.BindingProbe, said)
	}
	if !report.BindingProvenEmpty {
		t.Fatalf("BindingProvenEmpty = false: the refusal landed after the destination was written to, so a revert can now abandon rows: %s", said)
	}
}

// TestInfraSourceEdgePayloadRefusalPassesAnySourceItCanRead pins what the
// refusal narrowed to once the copy could carry a payload: it asks whether the
// source can be READ, not what the read returned. A payload is now carried, so
// finding one is no longer a reason to stop.
//
// The empty spellings are still enumerated because they are the ones most
// likely to be mistaken for a fault by a future edit — Dolt renders an absent
// payload as the empty JSON object — and a check that refused on those would
// refuse every city, which is the same as having no check at all.
func TestInfraSourceEdgePayloadRefusalPassesAnySourceItCanRead(t *testing.T) {
	backing, from, to, _ := seedInfraEdgeSource(t)
	rows, err := readInfraSnapshot(backing)
	if err != nil {
		t.Fatalf("readInfraSnapshot: %v", err)
	}

	for _, tc := range []struct {
		payload      string
		wantCarrying bool
	}{
		{payload: "", wantCarrying: false},
		{payload: "{}", wantCarrying: false},
		{payload: "  ", wantCarrying: false},
		{payload: "null", wantCarrying: false},
		{payload: `{"gate":"waits_for"}`, wantCarrying: true},
	} {
		source := &payloadCarryingSource{
			Store:    backing,
			payloads: map[[2]string]string{{from.ID, to.ID}: tc.payload},
		}
		carrying, err := infraSourceEdgePayloadRefusal(source, rows)
		if err != nil {
			t.Errorf("an edge whose payload is %q was refused, but the copy carries payloads now: %v", tc.payload, err)
			continue
		}
		// The same walk answers whether anything is carried, and that answer
		// decides whether the destination must be able to carry one. An
		// engine's spelling of "no payload" reported as carried would demand a
		// writer of every destination, which is the blanket refusal this
		// segment removed.
		if carrying != tc.wantCarrying {
			t.Errorf("an edge whose payload is %q reports carrying=%v, want %v", tc.payload, carrying, tc.wantCarrying)
		}
	}

	// The control. Only an unreadable source stops the migration here, or the
	// loop above is passing because the check does nothing.
	if _, err := infraSourceEdgePayloadRefusal(mutePayloadSource{Store: backing}, rows); err == nil {
		t.Error("a source that cannot be asked about edge payloads was cleared")
	}
}

// TestInfraMigrationRefusesADestinationThatCannotCarryBeforeItClearsIt is the
// placement assertion for the destination half of the payload rule (ga-l88kc).
//
// infraCopyDepEdge already refuses such a destination, so the outcome alone
// proves nothing: both placements report infraMigrationUnconverged. The
// difference is what the city looks like afterwards. prepareInfraDestination
// DELETES the rows an earlier interrupted attempt stamped, and it runs between
// the two, so a refusal taken inside the import destroys that partial state on
// its way to the same refusal — for a city the copy was never going to finish.
// The stamped row surviving is the evidence, and the control below is what
// keeps the refusal from being a blanket one.
func TestInfraMigrationRefusesADestinationThatCannotCarryBeforeItClearsIt(t *testing.T) {
	backing, from, to, _ := seedInfraEdgeSource(t)
	rows, err := readInfraSnapshot(backing)
	if err != nil {
		t.Fatalf("readInfraSnapshot: %v", err)
	}
	carrying := &payloadCarryingSource{
		Store:    backing,
		payloads: map[[2]string]string{{from.ID, to.ID}: `{"gate":"waits_for"}`},
	}
	payloadCarried, err := infraSourceEdgePayloadRefusal(carrying, rows)
	if err != nil {
		t.Fatalf("the source refusal rejected the fixture: %v", err)
	}
	if !payloadCarried {
		t.Fatal("the fixture's edge does not read as carrying a payload, so nothing below is being tested")
	}

	destination := beads.NewMemStore()
	if _, ok := beads.Store(destination).(beads.DepMetadataWriter); ok {
		t.Fatal("MemStore now carries edge payloads, so it is no longer the store this test needs")
	}
	stamped, err := destination.Create(beads.Bead{
		Title:    "a row an interrupted earlier attempt stamped",
		Type:     "session",
		Metadata: map[string]string{infraMigrationStampKey: "1"},
	})
	if err != nil {
		t.Fatalf("seeding the interrupted attempt: %v", err)
	}

	if err := infraDestinationEdgePayloadRefusal(destination, payloadCarried); err == nil {
		t.Fatal("a destination that cannot carry a payload was cleared while the source holds one")
	}

	// Nothing above may have run the preparer. If the refusal had been left to
	// infraCopyDepEdge, this row would be gone by the time the copy reached it.
	if _, err := destination.Get(stamped.ID); err != nil {
		t.Errorf("the stamped row from the earlier attempt is gone (%v): the refusal landed after the destination was cleared", err)
	}

	// The control: the same destination, a source carrying nothing. It must be
	// accepted, or the refusal is a blanket one that rejects every destination
	// in this tree apart from SQLite.
	payloadless, err := infraSourceEdgePayloadRefusal(backing, rows)
	if err != nil {
		t.Fatalf("the source refusal rejected the payloadless control: %v", err)
	}
	if err := infraDestinationEdgePayloadRefusal(destination, payloadless); err != nil {
		t.Errorf("a destination with nothing to lose was refused: %v", err)
	}
}

// TestTheDestinationPayloadRefusalRunsBeforeTheDestinationIsCleared is the
// placement half of ga-l88kc, and it is a source-order assertion because the
// behavior cannot express it.
//
// The production destination is whatever openInfraDestination returns, which is
// always a *beads.SQLiteStore and always a DepMetadataWriter, so no city can be
// driven through runInfraClassMigration into the refusal — the check is a fence
// against a future destination engine, not against today's. What CAN regress
// today is where the fence sits: moved below prepareInfraDestination it still
// refuses the same city, having first deleted the rows an interrupted earlier
// attempt stamped. That ordering is the whole of the segment's refuse-before-
// touch rule, so it is pinned where it lives.
func TestTheDestinationPayloadRefusalRunsBeforeTheDestinationIsCleared(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "infra_class_migrate.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing infra_class_migrate.go: %v", err)
	}
	var body *ast.BlockStmt
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == "runInfraClassMigration" && fn.Recv == nil {
			body = fn.Body
			break
		}
	}
	if body == nil {
		t.Fatal("runInfraClassMigration is gone from infra_class_migrate.go, so this guard is watching nothing")
	}

	positions := map[string]token.Pos{}
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		ident, ok := call.Fun.(*ast.Ident)
		if !ok {
			return true
		}
		if _, seen := positions[ident.Name]; !seen {
			positions[ident.Name] = call.Pos()
		}
		return true
	})
	for _, name := range []string{"infraSourceEdgePayloadRefusal", "infraDestinationEdgePayloadRefusal", "prepareInfraDestination"} {
		if _, ok := positions[name]; !ok {
			t.Fatalf("runInfraClassMigration no longer calls %s, so the ordering this guards is not the ordering that runs", name)
		}
	}
	if positions["infraDestinationEdgePayloadRefusal"] > positions["prepareInfraDestination"] {
		t.Errorf("runInfraClassMigration asserts the destination can carry a payload at %s, after prepareInfraDestination at %s clears the stamped rows of an earlier attempt",
			fset.Position(positions["infraDestinationEdgePayloadRefusal"]), fset.Position(positions["prepareInfraDestination"]))
	}
	if positions["infraSourceEdgePayloadRefusal"] > positions["infraDestinationEdgePayloadRefusal"] {
		t.Errorf("the source refusal runs after the destination refusal, so the destination is asked to carry a payload nobody has established is readable")
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
	assertRefusedBeforeTheDestinationWasTouched(t, report, said)
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
