package main

import (
	"bytes"
	"errors"
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
	_, err = verifyInfraCopy(func() (beads.Store, error) { return openInfraDestination(city.target) }, restored)
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
