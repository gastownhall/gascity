package main

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// setRewritingStore is a work store whose batched delete rewrites citations
// as bd's `bd delete <ids...> --force` does: every row linked to any id of the
// batch has its mention of EVERY id of the batch rewritten to
// "[deleted:<id>]", then the batch's rows and edges go.
type setRewritingStore struct{ *beads.MemStore }

func (s setRewritingStore) DeleteBatch(ids []string) error {
	neighbors := map[string]bool{}
	for _, id := range ids {
		for _, dir := range []string{"up", "down"} {
			deps, err := s.DepList(id, dir)
			if err != nil {
				return err
			}
			for _, d := range deps {
				neighbors[d.IssueID], neighbors[d.DependsOnID] = true, true
			}
		}
	}
	for other := range neighbors {
		row, err := s.Get(other)
		if err != nil {
			continue
		}
		text := row.Description
		for _, id := range ids {
			text = regexp.MustCompile(`\b`+regexp.QuoteMeta(id)+`\b`).ReplaceAllString(text, "[deleted:"+id+"]")
		}
		if text != row.Description {
			if err := s.Update(other, beads.UpdateOpts{Description: &text}); err != nil {
				return err
			}
		}
	}
	for _, id := range ids {
		for _, dir := range []string{"up", "down"} {
			deps, _ := s.DepList(id, dir)
			for _, d := range deps {
				_ = s.DepRemove(d.IssueID, d.DependsOnID)
			}
		}
		if err := s.Delete(id); err != nil {
			return err
		}
	}
	return nil
}

// TestTheBatchedClearDetachesEveryRowOfAChunk (#8786, review A): three moved
// rows in one chunk, and one work bead that cites and links all three. bd's
// batched delete rewrites citations across the whole set, so every row of the
// chunk, not only the first, must have no neighbor left when it goes.
func TestTheBatchedClearDetachesEveryRowOfAChunk(t *testing.T) {
	stubInfraControllerPing(t, 0)
	source := setRewritingStore{beads.NewMemStore()}
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) { return source, nil }
	t.Cleanup(func() { openInfraMigrationSource = prev })
	var ids []string
	for _, title := range []string{"first", "second", "third"} {
		ids = append(ids, mustCreateInfraBead(t, source, beads.Bead{Title: title, Type: "task", Metadata: beads.StringMap{"gc.kind": "workflow"}}).ID)
	}
	text := "tracks " + strings.Join(ids, ", ")
	work := mustCreateInfraBead(t, source, beads.Bead{Title: "work", Type: "task", Description: text})
	for _, id := range ids {
		if err := source.DepAdd(work.ID, id, "related"); err != nil {
			t.Fatal(err)
		}
	}
	request := storageTestRequest(t, infraSplitConfig(filepath.Join(t.TempDir(), "store")))
	var log bytes.Buffer
	if report := migrateInfraClasses(t, request.CityPath, request.Cfg, &log); report.Outcome != infraMigrationConverged || report.Clear.Cleared != 3 {
		t.Fatalf("migrate = %s (cleared %d): %s", report.Outcome, report.Clear.Cleared, log.String())
	}
	got, err := source.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != text {
		t.Fatalf("the batched clear rewrote work bead %s:\n got %q\nwant %q", work.ID, got.Description, text)
	}
}

// payloadKeepingSource is a MemStore-backed work store that keeps edge
// payloads: DepMetadata answers them, DepAddWithMetadata writes them, and a
// removed edge loses its payload.
type payloadKeepingSource struct {
	*beads.MemStore
	payloads map[[2]string]string
}

func (s *payloadKeepingSource) DepMetadata(issueID, dependsOnID string) (string, bool, error) { //nolint:unparam // the error is the beads.DepMetadataReader contract
	payload, ok := s.payloads[[2]string{issueID, dependsOnID}]
	return payload, ok, nil
}

func (s *payloadKeepingSource) DepAddWithMetadata(issueID, dependsOnID, depType, metadata string) error {
	if err := s.DepAdd(issueID, dependsOnID, depType); err != nil {
		return err
	}
	if metadata != "" {
		s.payloads[[2]string{issueID, dependsOnID}] = metadata
	}
	return nil
}

func (s *payloadKeepingSource) DepRemove(issueID, dependsOnID string) error {
	delete(s.payloads, [2]string{issueID, dependsOnID})
	return s.MemStore.DepRemove(issueID, dependsOnID)
}

// TestTheBatchedBackupCarriesAWorkEdgePayloadOntoAMovedRow (review B): a work
// bead waits on a moved row through a waits-for edge carrying a payload. The
// batched backup records the payload on the moved row's dependent, and the
// clear puts the kept edge back carrying it.
func TestTheBatchedBackupCarriesAWorkEdgePayloadOntoAMovedRow(t *testing.T) {
	stubInfraControllerPing(t, 0)
	source := &payloadKeepingSource{MemStore: beads.NewMemStore(), payloads: map[[2]string]string{}}
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) { return source, nil }
	t.Cleanup(func() { openInfraMigrationSource = prev })
	moved := mustCreateInfraBead(t, source, beads.Bead{Title: "graft running", Type: "task", Metadata: beads.StringMap{"gc.kind": "workflow"}})
	work := mustCreateInfraBead(t, source, beads.Bead{Title: "work", Type: "task"})
	const payload = `{"gate":"any-children"}`
	if err := source.DepAddWithMetadata(work.ID, moved.ID, "waits-for", payload); err != nil {
		t.Fatal(err)
	}
	request := storageTestRequest(t, infraSplitConfig(filepath.Join(t.TempDir(), "store")))
	var log bytes.Buffer
	if report := migrateInfraClasses(t, request.CityPath, request.Cfg, &log); report.Outcome != infraMigrationConverged {
		t.Fatalf("migrate = %s: %s", report.Outcome, log.String())
	}
	target := mustResolveInfraTarget(t, request.CityPath, request.Cfg)
	backup, _, err := readInfraRetainedBackup(target)
	if err != nil {
		t.Fatal(err)
	}
	var recorded *infraBackupEdge
	for _, e := range backup {
		for i, d := range e.Dependents {
			if e.Bead.ID == moved.ID && d.IssueID == work.ID {
				recorded = &e.Dependents[i]
			}
		}
	}
	if recorded == nil || !recorded.Carried || recorded.Payload != payload {
		t.Fatalf("the backup records the dependent %s -> %s as %+v, want it carrying %q", work.ID, moved.ID, recorded, payload)
	}
	if got, carried, _ := source.DepMetadata(work.ID, moved.ID); !carried || got != payload {
		t.Fatalf("the kept edge %s -> %s carries (%q, %v) after the clear, want %q", work.ID, moved.ID, got, carried, payload)
	}
}

// TestVerifyInfraCopyRefusesAPayloadOnlyTheBindingCarries (review F): a
// within-infra edge that carries no payload in the work store and one in the
// binding is a difference, as much as a dropped payload is.
func TestVerifyInfraCopyRefusesAPayloadOnlyTheBindingCarries(t *testing.T) {
	city := newEngineCity(t, 60, "memory")
	if report, said := city.migrate(t); report.Outcome != infraMigrationConverged {
		t.Fatalf("outcome = %v: %s", report.Outcome, said)
	}
	var edge [2]string
	for _, d := range city.deps {
		key := [2]string{d.IssueID, d.DependsOnID}
		if _, carries := city.payloads[key]; !carries && d.Type == "blocks" {
			edge = key
			break
		}
	}
	if edge[0] == "" {
		t.Fatal("the fixture has no payloadless within-infra edge")
	}
	binding, err := openInfraDestination(city.target)
	if err != nil {
		t.Fatal(err)
	}
	if err := binding.(beads.DepMetadataWriter).DepAddWithMetadata(edge[0], edge[1], "blocks", `{"gate":"invented"}`); err != nil {
		t.Fatal(err)
	}
	_ = closeBeadStoreHandle(binding)
	restored := newMCShapedInfraCity(t, 60).source()
	_, err = verifyInfraCopy(func() (beads.Store, error) { return openInfraDestination(city.target) }, restored, nil)
	if err == nil || !strings.Contains(err.Error(), edge[0]) || !strings.Contains(err.Error(), "invented") {
		t.Fatalf("verifyInfraCopy = %v, want a refusal naming the payload only the binding carries on %s -> %s", err, edge[0], edge[1])
	}
}

// TestPolicyWrappedStoreForwardsTheBatchedPayloadRead (review S2): the
// bead-policy wrapper every production open returns forwards the batched
// payload read, so the engine reads a SQLite or native work store's payloads
// in batches without peeling the wrapper; over a store without it, the
// wrapper reports ErrDepMetadataUnsupported and the engine reads per edge.
func TestPolicyWrappedStoreForwardsTheBatchedPayloadRead(t *testing.T) {
	city := newMCShapedInfraCity(t, 60)
	wrapped := city.sqliteSource(t)
	reader, ok := beads.As[beads.DepMetadataBatchReader](wrapped)
	if !ok {
		t.Fatal("the policy-wrapped SQLite store has no batched payload read")
	}
	ids := make([]string, 0, len(city.rows))
	for _, b := range city.rows {
		ids = append(ids, b.ID)
	}
	got, err := reader.DepMetadataBatch(ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(city.payloads) {
		t.Fatalf("the forwarded batch read %d payloads, want the fixture's %d", len(got), len(city.payloads))
	}
	plain := wrapStoreWithBeadPolicies(beads.NewMemStore(), nil)
	if over, ok := beads.As[beads.DepMetadataBatchReader](plain); !ok {
		t.Fatal("the policy wrapper does not advertise the forward")
	} else if _, err := over.DepMetadataBatch([]string{"x"}); !errors.Is(err, beads.ErrDepMetadataUnsupported) {
		t.Fatalf("the forward over a store without the batch = %v, want ErrDepMetadataUnsupported", err)
	}
}

// batchListFailingSource lists edges one row at a time and fails to list them
// in batches, so a reader that took the batched path says so.
type batchListFailingSource struct{ *beads.MemStore }

var errBatchedEdgeListFailed = errors.New("the batched edge list failed in this test")

func (batchListFailingSource) DepListBatch([]string) (map[string][]beads.Dep, error) {
	return nil, errBatchedEdgeListFailed
}

// TestPreflightReadsTheEdgesTheWayTheCopyWill (review S1): the preflight hands
// the edge-payload refusal the same id set the migration does, so a work store
// that lists edges in batches is read in batches in the rehearsal too, and a
// batched read that fails in the window fails in the preflight first.
func TestPreflightReadsTheEdgesTheWayTheCopyWill(t *testing.T) {
	request, _, _ := preflightReadyCity(t, 0)
	source := batchListFailingSource{beads.NewMemStore()}
	mustCreateInfraBead(t, source, beads.Bead{Title: "a session", Type: "session"})
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) { return source, nil }
	t.Cleanup(func() { openInfraMigrationSource = prev })
	var stdout, stderr bytes.Buffer
	if code := doStoragePreflight(request, &stdout, &stderr); code == 0 {
		t.Fatalf("preflight cleared a city whose batched edge read fails: %q", stdout.String())
	}
	if said := stdout.String() + stderr.String(); !strings.Contains(said, errBatchedEdgeListFailed.Error()) {
		t.Fatalf("preflight blocks without naming the batched read that failed: %q", said)
	}
}

// listHidingStore is a work store whose List leaves one row out, as a native
// or bd list leaves out a row whose metadata does not parse.
type listHidingStore struct {
	*beads.MemStore
	hidden string
}

func (s listHidingStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	rows, err := s.MemStore.List(query)
	out := rows[:0]
	for _, b := range rows {
		if b.ID != s.hidden {
			out = append(out, b)
		}
	}
	return out, err
}

func (s listHidingStore) DeleteBatch(ids []string) error {
	for _, id := range ids {
		if err := s.Delete(id); err != nil {
			return err
		}
	}
	return nil
}

// TestTheClearNamesADependentTheListLeavesOut (review S4): the batched backup
// reads a moved row's dependents from the down edges of every row the work
// store lists, so a dependent the list leaves out is one it cannot record.
// The clear refuses the row, and the refusal names that dependent and why, so
// a resume that refuses again says what to repair.
func TestTheClearNamesADependentTheListLeavesOut(t *testing.T) {
	stubInfraControllerPing(t, 0)
	mem := beads.NewMemStore()
	moved := mustCreateInfraBead(t, mem, beads.Bead{Title: "graft running", Type: "task", Metadata: beads.StringMap{"gc.kind": "workflow"}})
	unparsed := mustCreateInfraBead(t, mem, beads.Bead{Title: "work whose metadata does not parse", Type: "task"})
	if err := mem.DepAdd(unparsed.ID, moved.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	source := listHidingStore{MemStore: mem, hidden: unparsed.ID}
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) { return source, nil }
	t.Cleanup(func() { openInfraMigrationSource = prev })
	request := storageTestRequest(t, infraSplitConfig(filepath.Join(t.TempDir(), "store")))
	var log bytes.Buffer
	report := migrateInfraClasses(t, request.CityPath, request.Cfg, &log)
	if report.Outcome != infraMigrationRetained {
		t.Fatalf("migrate = %s, want the clear refused over the unlisted dependent: %s", report.Outcome, log.String())
	}
	advice := infraMigrationOperatorAdvice(report, "gc storage migrate")
	if !strings.Contains(advice, unparsed.ID) || !strings.Contains(advice, "does not return "+unparsed.ID) || !strings.Contains(advice, "metadata does not parse") {
		t.Fatalf("the refusal does not name the dependent %s the list leaves out, and why: %s", unparsed.ID, advice)
	}
	if _, err := mem.Get(moved.ID); err != nil {
		t.Fatalf("the clear removed %s over a dependent it could not record: %v", moved.ID, err)
	}
	if deps, err := mem.DepList(unparsed.ID, "down"); err != nil || len(deps) != 1 {
		t.Fatalf("%s deps = %+v (%v); the unrecorded edge must survive the refusal", unparsed.ID, deps, err)
	}
}

// TestStorageMigrateRefusesATimeBudgetTheRigPathsIgnore (review S7): the rig
// import and the abandon do not check the budget, so an explicit
// --time-budget with either is refused, not silently dropped; the default
// budget, unstated, does not stand in their way.
func TestStorageMigrateRefusesATimeBudgetTheRigPathsIgnore(t *testing.T) {
	surface, err := parseOperatorCommandSpelling(storageMigrationCommand)
	if err != nil {
		t.Fatal(err)
	}
	budget := "--" + storageTimeBudgetFlag + "=5m"
	for _, args := range [][]string{
		{"--" + storageFromRigsFlag, budget},
		{"--" + storageFromRigsFlag, "--" + storageRigFlag, "r", "--" + storageAbandonRigStoreFlag, budget},
		{"--" + storageAbandonRigStoreFlag, budget, "--" + storageFromRigsFlag, "--" + storageRigFlag, "r"},
	} {
		var stdout, stderr bytes.Buffer
		cmd := newStorageMigrateCmd(surface, &stdout, &stderr)
		cmd.SetArgs(args)
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		if err := cmd.Execute(); err == nil {
			t.Fatalf("migrate %v succeeded; an explicit budget the rig paths ignore must be refused", args)
		}
		if !strings.Contains(stderr.String(), "--"+storageTimeBudgetFlag) || !strings.Contains(stderr.String(), "refused rather than ignored") {
			t.Fatalf("migrate %v: the refusal does not name the budget: %s", args, stderr.String())
		}
	}
	// Unstated, the default passes this check and the run stops at the next
	// one (--rig without --from-rigs), before any city is resolved.
	var stdout, stderr bytes.Buffer
	cmd := newStorageMigrateCmd(surface, &stdout, &stderr)
	cmd.SetArgs([]string{"--" + surface.Flag, "--" + storageRigFlag, "r", "--" + storageAbandonRigStoreFlag})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	_ = cmd.Execute()
	if strings.Contains(stderr.String(), "refused rather than ignored") || !strings.Contains(stderr.String(), "means nothing without it") {
		t.Fatalf("the default budget, unstated, was refused with the abandon: %s", stderr.String())
	}
}

// batchRemovingStore is a setRewritingStore that removes edges in batches and
// counts how the clear removed them, and the largest batch it was sent.
type batchRemovingStore struct {
	setRewritingStore
	batches, single, largest *int
}

func (s batchRemovingStore) DepRemoveBatch(pairs []beads.DepPair) error {
	*s.batches++
	if s.largest != nil {
		*s.largest = max(*s.largest, len(pairs))
	}
	for _, p := range pairs {
		if err := s.setRewritingStore.DepRemove(p.IssueID, p.DependsOnID); err != nil {
			return err
		}
	}
	return nil
}

func (s batchRemovingStore) DepRemove(issueID, dependsOnID string) error {
	*s.single++
	return s.setRewritingStore.DepRemove(issueID, dependsOnID)
}

// TestTheClearRemovesAChunksEdgesInOneBatch (review S3): a work store that
// removes edges in batches has a chunk's edges removed in one call, not one
// commit per edge, and so does the policy wrapper every production open
// returns over one (review X1); over a wrapper whose store cannot, the clear
// removes them edge by edge. Either way no work bead's text is rewritten.
func TestTheClearRemovesAChunksEdgesInOneBatch(t *testing.T) {
	for _, tc := range []struct {
		name    string
		batched bool
		wrapped bool
	}{{"batched", true, false}, {"policy wrapper over a batching store", true, true}, {"wrapper over a store without it", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			stubInfraControllerPing(t, 0)
			rewriting := setRewritingStore{beads.NewMemStore()}
			var batches, single int
			var source beads.Store = rewriting
			if tc.batched {
				source = batchRemovingStore{setRewritingStore: rewriting, batches: &batches, single: &single}
			}
			if tc.wrapped {
				source = wrapStoreWithBeadPolicies(source, nil)
			}
			prev := openInfraMigrationSource
			openInfraMigrationSource = func(string) (beads.Store, error) { return source, nil }
			t.Cleanup(func() { openInfraMigrationSource = prev })
			var ids []string
			for i := 0; i < 4; i++ {
				ids = append(ids, mustCreateInfraBead(t, rewriting, beads.Bead{Title: "graft", Type: "task", Metadata: beads.StringMap{"gc.kind": "workflow"}}).ID)
			}
			for i := 1; i < len(ids); i++ {
				if err := rewriting.DepAdd(ids[i], ids[i-1], "blocks"); err != nil {
					t.Fatal(err)
				}
			}
			text := "tracks " + strings.Join(ids, ", ")
			work := mustCreateInfraBead(t, rewriting, beads.Bead{Title: "work", Type: "task", Description: text})
			for _, id := range ids {
				if err := rewriting.DepAdd(work.ID, id, "related"); err != nil {
					t.Fatal(err)
				}
			}
			request := storageTestRequest(t, infraSplitConfig(filepath.Join(t.TempDir(), "store")))
			var log bytes.Buffer
			if report := migrateInfraClasses(t, request.CityPath, request.Cfg, &log); report.Outcome != infraMigrationConverged || report.Clear.Cleared != len(ids) {
				t.Fatalf("migrate = %s (cleared %d): %s", report.Outcome, report.Clear.Cleared, log.String())
			}
			if tc.batched && (batches == 0 || single != 0) {
				t.Fatalf("the clear removed edges in %d batch(es) and %d single call(s), want batches only", batches, single)
			}
			if got, err := rewriting.Get(work.ID); err != nil || got.Description != text {
				t.Fatalf("the clear rewrote work bead %s: %q (%v), want %q", work.ID, got.Description, err, text)
			}
		})
	}
}

// TestInfraEngineDropsAStampedRowTheSourceDeletedSincePausing (review S8,
// mutant N): a row an earlier, interrupted run copied that the work store has
// deleted since is not kept by the resume. The binding ends holding exactly
// the work store's infrastructure rows, and nothing else is copied again, no
// row and no edge.
func TestInfraEngineDropsAStampedRowTheSourceDeletedSincePausing(t *testing.T) {
	smallEngineBatches(t)
	city := newEngineCity(t, 300, "sqlite")
	prev := infraEngineCrashPoint
	infraEngineCrashPoint = func(p string) error {
		if p == "verify" {
			return errors.New("killed before the manifest")
		}
		return nil
	}
	if report, _ := city.migrate(t); report.Outcome == infraMigrationConverged {
		t.Fatal("the kill did not stop the run")
	}
	infraEngineCrashPoint = prev

	var gone string
	for _, b := range city.rows {
		if !isInfra(b) {
			continue
		}
		for _, d := range city.deps {
			if d.IssueID == b.ID || d.DependsOnID == b.ID {
				gone = b.ID
			}
		}
		if gone != "" {
			break
		}
	}
	if gone == "" {
		t.Fatal("the fixture has no infrastructure row with an edge")
	}
	deps := city.deps[:0]
	for _, d := range city.deps {
		if d.IssueID == gone || d.DependsOnID == gone {
			if err := city.source.DepRemove(d.IssueID, d.DependsOnID); err != nil {
				t.Fatal(err)
			}
			delete(city.payloads, [2]string{d.IssueID, d.DependsOnID})
			continue
		}
		deps = append(deps, d)
	}
	city.deps = deps
	if err := city.source.Delete(gone); err != nil {
		t.Fatal(err)
	}
	rows := city.rows[:0]
	for _, b := range city.rows {
		if b.ID != gone {
			rows = append(rows, b)
		}
	}
	city.rows = rows

	var points []string
	infraEngineCrashPoint = func(p string) error { points = append(points, p); return nil }
	report, said := city.migrate(t)
	infraEngineCrashPoint = prev
	if report.Outcome != infraMigrationConverged {
		t.Fatalf("the run after the source deleted %s = %v: %s", gone, report.Outcome, said)
	}
	// Every edge between two kept rows already carries its type and payload,
	// so the resume writes none of them again.
	for _, p := range points {
		if strings.HasPrefix(p, "copying edges") {
			t.Fatalf("the resume wrote edges again (%s), though every edge between kept rows was already right", p)
		}
	}
	if m := infraCopiedCount.FindStringSubmatch(said); m == nil || m[1] != "0" {
		t.Fatalf("the run copied %v rows, want none: every row it kept still equals the work store: %s", m, said)
	}
	infra := 0
	for _, b := range city.rows {
		if isInfra(b) {
			infra++
		}
	}
	if want := fmt.Sprintf("kept %d of %d stamped row(s)", infra, infra+1); !strings.Contains(said, want) {
		t.Fatalf("the resume does not say %q: %s", want, said)
	}
	city.assertNotLossy(t)
}

// edgeCountingMuteSource is a work store that cannot be asked for payloads (it
// carries only the Store interface) and counts its edge reads.
type edgeCountingMuteSource struct {
	beads.Store
	reads *int
}

func (s edgeCountingMuteSource) DepList(id, direction string) ([]beads.Dep, error) {
	*s.reads++
	return s.Store.DepList(id, direction)
}

// TestThePlanRefusesAnUnaskableSourceBeforeReadingItsEdges (review NIT): a
// work store that cannot report payloads is refused before the walk over its
// edges, which on an MC-sized city is the expensive part of the plan.
func TestThePlanRefusesAnUnaskableSourceBeforeReadingItsEdges(t *testing.T) {
	mem := beads.NewMemStore()
	row := mustCreateInfraBead(t, mem, beads.Bead{Title: "a session", Type: "session"})
	reads := 0
	source := edgeCountingMuteSource{Store: mem, reads: &reads}
	_, err := infraSourceEdgePayloadRefusal(source, []beads.Bead{row}, nil)
	if !errors.Is(err, beads.ErrDepMetadataUnsupported) {
		t.Fatalf("infraSourceEdgePayloadRefusal = %v, want ErrDepMetadataUnsupported", err)
	}
	if reads != 0 {
		t.Fatalf("the plan read edges %d time(s) before refusing a source it cannot ask", reads)
	}
}

// TestTheClearDetachesAnEdgeFromALaterChunkBeforeTheDelete (review S3): an
// edge onto a moved row from a moved row in a later chunk is recorded in the
// later row's own entry, not in the first's, so the first chunk finds it live
// and removes it before its delete. A delete that followed it would rewrite
// the later row's text, which a paused or killed clear leaves in the work
// store until the next run.
func TestTheClearDetachesAnEdgeFromALaterChunkBeforeTheDelete(t *testing.T) {
	stubInfraControllerPing(t, 0)
	prevChunk := infraClearChunk
	infraClearChunk = 1
	t.Cleanup(func() { infraClearChunk = prevChunk })
	source := setRewritingStore{beads.NewMemStore()}
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) { return source, nil }
	t.Cleanup(func() { openInfraMigrationSource = prev })
	first := mustCreateInfraBead(t, source, beads.Bead{Title: "graft root", Type: "task", Metadata: beads.StringMap{"gc.kind": "workflow"}})
	text := "follows " + first.ID
	later := mustCreateInfraBead(t, source, beads.Bead{Title: "graft step", Type: "task", Description: text, Metadata: beads.StringMap{"gc.kind": "workflow"}})
	if err := source.DepAdd(later.ID, first.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	prevBefore := infraClearBeforeDelete
	infraClearBeforeDelete = func(id string) error {
		if id == later.ID {
			return errors.New("killed between chunks")
		}
		return nil
	}
	request := storageTestRequest(t, infraSplitConfig(filepath.Join(t.TempDir(), "store")))
	var log bytes.Buffer
	report := migrateInfraClasses(t, request.CityPath, request.Cfg, &log)
	infraClearBeforeDelete = prevBefore
	if report.Outcome != infraMigrationRetained {
		t.Fatalf("migrate = %s, want the kill to leave the clear pending: %s", report.Outcome, log.String())
	}
	if _, err := source.Get(first.ID); err == nil {
		t.Fatalf("precondition: %s should go in the first chunk", first.ID)
	}
	got, err := source.Get(later.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != text {
		t.Fatalf("the first chunk's delete rewrote %s, still in the work store:\n got %q\nwant %q", later.ID, got.Description, text)
	}
	log.Reset()
	if report := migrateInfraClasses(t, request.CityPath, request.Cfg, &log); report.Outcome != infraMigrationConverged {
		t.Fatalf("resume = %s: %s", report.Outcome, log.String())
	}
}

// TestTheClearSubBatchesADenseChunksEdges (round 3 MUST-1): a molecule-shaped
// chunk, a thousand pour steps each blocking on the two before it, has about
// two thousand edges. One DepRemoveBatch of them all ran past native Dolt's
// operation budget on a loaded host and failed the clear after the marker on
// every run; no call may carry more than infraDetachEdgeBatch.
func TestTheClearSubBatchesADenseChunksEdges(t *testing.T) {
	stubInfraControllerPing(t, 0)
	rewriting := setRewritingStore{beads.NewMemStore()}
	var batches, single, largest int
	source := batchRemovingStore{setRewritingStore: rewriting, batches: &batches, single: &single, largest: &largest}
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) { return source, nil }
	t.Cleanup(func() { openInfraMigrationSource = prev })
	const steps = 1000
	ids := make([]string, 0, steps)
	edges := 0
	for i := 0; i < steps; i++ {
		ids = append(ids, mustCreateInfraBead(t, rewriting, beads.Bead{Title: fmt.Sprintf("step %d", i), Type: "task", Metadata: beads.StringMap{"gc.kind": "workflow"}}).ID)
		for back := 1; back <= 2 && i-back >= 0; back++ {
			if err := rewriting.DepAdd(ids[i], ids[i-back], "blocks"); err != nil {
				t.Fatal(err)
			}
			edges++
		}
	}
	if steps > infraClearChunk {
		t.Fatalf("the fixture spans %d chunks; it must be one dense chunk", (steps+infraClearChunk-1)/infraClearChunk)
	}
	request := storageTestRequest(t, infraSplitConfig(filepath.Join(t.TempDir(), "store")))
	var log bytes.Buffer
	if report := migrateInfraClasses(t, request.CityPath, request.Cfg, &log); report.Outcome != infraMigrationConverged || report.Clear.Cleared != steps {
		t.Fatalf("migrate = %s (cleared %d): %s", report.Outcome, report.Clear.Cleared, log.String())
	}
	// The cap is the literal the timing was measured for, not the variable:
	// raising the variable past it must fail here.
	if largest > 250 {
		t.Fatalf("one DepRemoveBatch carried %d edges, over the cap of 250", largest)
	}
	if want := (edges + infraDetachEdgeBatch - 1) / infraDetachEdgeBatch; batches < want || single != 0 {
		t.Fatalf("the %d edges went in %d batch(es) and %d single call(s), want at least %d batches and no single calls", edges, batches, single, want)
	}
}

// TestABatchedClearKilledBetweenChunksKeepsTheEdgesIntoRowsItRemoved (round 3
// X7): on a store that deletes in batches, a clear killed after its first
// chunk resumes from the whole session the note names. The kept edge from a
// work bead into a row the first chunk removed is re-added only after the
// delete loop, so a session that dropped that row would resume without it.
func TestABatchedClearKilledBetweenChunksKeepsTheEdgesIntoRowsItRemoved(t *testing.T) {
	stubInfraControllerPing(t, 0)
	prevChunk := infraClearChunk
	infraClearChunk = 1
	t.Cleanup(func() { infraClearChunk = prevChunk })
	source := setRewritingStore{beads.NewMemStore()}
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) { return source, nil }
	t.Cleanup(func() { openInfraMigrationSource = prev })
	first := mustCreateInfraBead(t, source, beads.Bead{Title: "graft running", Type: "task", Metadata: beads.StringMap{"gc.kind": "workflow"}})
	second := mustCreateInfraBead(t, source, beads.Bead{Title: "graft queued", Type: "task", Metadata: beads.StringMap{"gc.kind": "workflow"}})
	work := mustCreateInfraBead(t, source, beads.Bead{Title: "work waiting on the running graft", Type: "task"})
	if err := source.DepAdd(work.ID, first.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	prevBefore := infraClearBeforeDelete
	infraClearBeforeDelete = func(id string) error {
		if id == second.ID {
			return errors.New("killed between chunks")
		}
		return nil
	}
	request := storageTestRequest(t, infraSplitConfig(filepath.Join(t.TempDir(), "store")))
	var log bytes.Buffer
	report := migrateInfraClasses(t, request.CityPath, request.Cfg, &log)
	infraClearBeforeDelete = prevBefore
	if report.Outcome != infraMigrationRetained {
		t.Fatalf("migrate = %s, want the kill to leave the clear pending: %s", report.Outcome, log.String())
	}
	if _, err := source.Get(first.ID); err == nil {
		t.Fatalf("precondition: %s should go in the first chunk", first.ID)
	}
	log.Reset()
	if report := migrateInfraClasses(t, request.CityPath, request.Cfg, &log); report.Outcome != infraMigrationConverged {
		t.Fatalf("resume = %s: %s", report.Outcome, log.String())
	}
	deps, err := source.DepList(work.ID, "down")
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 || deps[0].DependsOnID != first.ID || deps[0].Type != "blocks" {
		t.Fatalf("%s deps after the resumed clear = %+v; the kept edge into %s, removed by the first run, must be back", work.ID, deps, first.ID)
	}
}

// TestPrepareRefusesABindingThatCannotDeleteInBatches (round 3 NIT): the
// per-row delete of an earlier attempt's rows is gone, as the copy's per-row
// writes are; a binding that cannot delete in batches is refused by name with
// its rows left as they were, not cleared by an untested path.
func TestPrepareRefusesABindingThatCannotDeleteInBatches(t *testing.T) {
	binding := beads.NewMemStore()
	if _, ok := beads.As[beads.BatchDeleter](beads.Store(binding)); ok {
		t.Fatal("MemStore now deletes in batches, so it is no longer the binding this test needs")
	}
	stamped := mustCreateInfraBead(t, binding, infraMigrationRow(beads.Bead{Title: "an earlier attempt's row", Type: "session"}))
	_, err := prepareInfraDestination(binding, nil)
	if err == nil || !strings.Contains(err.Error(), "cannot delete in batches") {
		t.Fatalf("prepareInfraDestination = %v, want the binding refused for lacking batched deletes", err)
	}
	if _, err := binding.Get(stamped.ID); err != nil {
		t.Fatalf("the refused prepare removed %s: %v", stamped.ID, err)
	}
}
