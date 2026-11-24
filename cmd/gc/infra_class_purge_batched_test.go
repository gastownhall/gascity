package main

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// purgeBatchProbe records the batched edge reads a purge run makes, and the
// per-row dependents reads ("up") a batched run must not need.
type purgeBatchProbe struct {
	mu      sync.Mutex
	batches [][]string
	ups     int
}

func (p *purgeBatchProbe) recordUp() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ups++
}

// upReads is how many per-row dependents reads the run made.
func (p *purgeBatchProbe) upReads() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ups
}

func (p *purgeBatchProbe) record(ids []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.batches = append(p.batches, slices.Clone(ids))
}

// readAnchorOutside reports whether a batch read an anchor outside selected:
// the inbound-edge read (infraPurgeInboundEdgesInBatches) is the one batch
// that reads every work-store row, not only the selected ones.
func (p *purgeBatchProbe) readAnchorOutside(selected map[string]bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, batch := range p.batches {
		for _, id := range batch {
			if !selected[id] {
				return true
			}
		}
	}
	return false
}

// purgeBatchingStore is a work store that answers batched edge reads and
// records them.
type purgeBatchingStore struct {
	*beads.FileStore
	probe *purgeBatchProbe
}

func (s purgeBatchingStore) DepListBatch(ids []string) (map[string][]beads.Dep, error) {
	s.probe.record(ids)
	return s.FileStore.DepListBatch(ids)
}

func (s purgeBatchingStore) DepList(id, direction string) ([]beads.Dep, error) {
	if direction == "up" {
		s.probe.recordUp()
	}
	return s.FileStore.DepList(id, direction)
}

// purgePerRowStore is a work store whose batched edge read reports itself
// unsupported, so the purge reads every row's edges on its own: the path
// every purge took before T6a, when the work handle hid the batch.
type purgePerRowStore struct {
	*beads.FileStore
	probe *purgeBatchProbe
}

func (s purgePerRowStore) DepListBatch(ids []string) (map[string][]beads.Dep, error) { //nolint:unparam // implements beads.DependencyBatchLister; its whole answer is the refusal
	s.probe.record(ids)
	return nil, beads.ErrDepListBatchUnsupported
}

// purgeTierEdgeFixture is a converged city with 4 stranded wisp-tier messages
// and edges of every tier shape a purge reads: from an issue-tier work bead
// into a message, from a message to the work bead, from a non-selected
// wisp-tier row into a message, between two messages, from a message to that
// wisp-tier row, and one edge the purge must leave alone.
func purgeTierEdgeFixture(t *testing.T) (purgeCity, map[string]bool) {
	t.Helper()
	city := newSmallPurgeCity(t, 4)
	wispTask := beads.Bead{ID: "mc-wisp-task", Title: "a wisp-tier step", Status: "open", Type: "task", Ephemeral: true, CreatedAt: mustParseTime(t, "2026-08-20T00:00:00Z")}
	editPurgeWorkStore(t, city.workPath, []beads.Bead{wispTask})
	workID := purgeWorkBeadID(t, city)
	work := openPurgeWorkStore(t, city.workPath)
	for _, edge := range [][3]string{
		{workID, "mc-wisp-0001", "blocks"},          // issue tier into a message
		{"mc-wisp-0002", workID, "relates-to"},      // a message out to the issue tier
		{wispTask.ID, "mc-wisp-0003", "blocks"},     // a non-selected wisp row into a message
		{"mc-wisp-0001", "mc-wisp-0004", "blocks"},  // message to message
		{"mc-wisp-0004", wispTask.ID, "relates-to"}, // a message out to the wisp row
		{workID, wispTask.ID, "blocks"},             // untouched by the purge
	} {
		if err := work.DepAdd(edge[0], edge[1], edge[2]); err != nil {
			t.Fatalf("adding %v: %v", edge, err)
		}
	}
	selected := map[string]bool{}
	for i := 1; i <= 4; i++ {
		selected[fmt.Sprintf("mc-wisp-%04d", i)] = true
	}
	return city, selected
}

// purgeOutcome is what a purge run left: the work store's rows and edges, and
// its backup with each far end named by id alone (the fixture's work bead is
// minted at run time, so its created_at differs between two fixtures).
type purgeOutcome struct {
	rows, edges, backup []string
	// inbound is how many inbound edges the backup holds per backed-up row.
	inbound map[string]int
	workID  string
}

func readPurgeOutcome(t *testing.T, city purgeCity) purgeOutcome {
	t.Helper()
	out := readPurgeOutcomeFrom(t, city, openPurgeWorkStore(t, city.workPath), func(id string) string { return id })
	out.workID = purgeWorkBeadID(t, city)
	return out
}

// readPurgeOutcomeFrom is readPurgeOutcome over the work store store, with
// every bead id written as label names it, so two fixtures that minted
// different ids for the same rows compare.
func readPurgeOutcomeFrom(t *testing.T, city purgeCity, store beads.Store, label func(string) string) purgeOutcome {
	t.Helper()
	out := purgeOutcome{inbound: map[string]int{}}
	rows, err := store.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		t.Fatalf("listing the work store: %v", err)
	}
	for _, b := range rows {
		out.rows = append(out.rows, label(b.ID))
		deps, err := store.DepList(b.ID, "down")
		if err != nil {
			t.Fatalf("listing the edges of %s: %v", b.ID, err)
		}
		for _, d := range deps {
			out.edges = append(out.edges, fmt.Sprintf("%s -[%s]-> %s", label(d.IssueID), d.Type, label(d.DependsOnID)))
		}
	}
	files := purgeBackupFiles(t, city)
	if len(files) != 1 {
		t.Fatalf("backups = %v, want one", files)
	}
	records, err := readInfraPurgeBackup(files[0])
	if err != nil {
		t.Fatal(err)
	}
	edgeList := func(edges []infraBackupEdge) []string {
		var list []string
		for _, e := range edges {
			list = append(list, fmt.Sprintf("%s -[%s]-> %s payload=%q carried=%v", label(e.IssueID), e.Type, label(e.DependsOnID), e.Payload, e.Carried))
		}
		sort.Strings(list) // the backup orders by id, and labels reorder ids
		return list
	}
	for _, record := range records {
		entry := record.Entry
		id := label(entry.Bead.ID)
		out.inbound[id] = len(entry.Dependents)
		var ends []string
		for _, key := range entry.Endpoints {
			ends = append(ends, label(key.ID))
		}
		sort.Strings(ends)
		out.backup = append(out.backup, fmt.Sprintf("%s class=%s deps=%v dependents=%v endpoints=%v", id, entry.Class, edgeList(entry.Deps), edgeList(entry.Dependents), ends))
	}
	for _, list := range [][]string{out.rows, out.edges, out.backup} {
		sort.Strings(list)
	}
	return out
}

// TestPurgeStrandedBatchedEdgeReadMatchesThePerRowRead pins the purge's
// batched edge reads, which T6a turned on (typed-stores T6 plan, R2 side
// effect): the purge's work store is a WorkStore handle, and DepListBatchFor
// now sees through it, so the run reads its rows' edges and every inbound
// edge in batches (newSourceDepReader, infraPurgeInboundEdgesInBatches)
// instead of one read per row. The batched run must take that path and leave
// exactly what the per-row run leaves on the same fixture: the deleted rows,
// the edges kept, and the backup, wisp-tier edges and edges across the tiers
// included.
func TestPurgeStrandedBatchedEdgeReadMatchesThePerRowRead(t *testing.T) {
	run := func(wrap func(*beads.FileStore, *purgeBatchProbe) beads.Store) (purgeOutcome, *purgeBatchProbe, map[string]bool) {
		city, selected := purgeTierEdgeFixture(t)
		probe := &purgeBatchProbe{}
		openPurgeWorkStoreAs(t, city, func(store *beads.FileStore) beads.Store { return wrap(store, probe) })
		var stdout, stderr bytes.Buffer
		if code := runStrandedPurge(t, city, confirmedPurge(t, city, len(selected)), &stdout, &stderr); code != 0 {
			t.Fatalf("purge exited %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
		}
		return readPurgeOutcome(t, city), probe, selected
	}
	batched, batchProbe, selected := run(func(s *beads.FileStore, p *purgeBatchProbe) beads.Store { return purgeBatchingStore{s, p} })
	perRow, perRowProbe, _ := run(func(s *beads.FileStore, p *purgeBatchProbe) beads.Store { return purgePerRowStore{s, p} })

	if !batchProbe.readAnchorOutside(selected) {
		t.Errorf("the batched run never read the inbound edges in batches (batches %v): infraPurgeInboundEdgesInBatches did not run", batchProbe.batches)
	}
	if n := batchProbe.upReads(); n != 0 {
		t.Errorf("the batched run made %d per-row dependents read(s): the batch did not answer", n)
	}
	if len(perRowProbe.batches) == 0 {
		t.Error("the per-row run never asked for a batch; it does not prove the fallback")
	}
	for _, id := range batched.rows {
		if selected[id] {
			t.Errorf("the purge left the selected row %s", id)
		}
	}
	if kept := batched.workID + " -[blocks]-> mc-wisp-task"; !slices.Contains(batched.edges, kept) {
		t.Errorf("the purge removed the edge %s it must leave alone: %v", kept, batched.edges)
	}
	if !slices.Equal(batched.rows, perRow.rows) {
		t.Errorf("rows left: batched %v, per-row %v", batched.rows, perRow.rows)
	}
	if !slices.Equal(batched.edges, perRow.edges) {
		t.Errorf("edges left: batched %v, per-row %v", batched.edges, perRow.edges)
	}
	if !slices.Equal(batched.backup, perRow.backup) {
		t.Errorf("backup: batched\n  %v\nper-row\n  %v", batched.backup, perRow.backup)
	}
	// The inbound edges from the issue tier and from the wisp-tier row, and
	// the one between two messages, are each in the backup.
	for id, want := range map[string]int{"mc-wisp-0001": 1, "mc-wisp-0003": 1, "mc-wisp-0004": 1} {
		if got := batched.inbound[id]; got != want {
			t.Errorf("the backup of %s holds %d inbound edge(s), want %d", id, got, want)
		}
	}
}

// TestPurgeStrandedReadsInboundEdgesFromEveryBatchChunk: the inbound read
// asks infraPurgeBatchChunk rows at a time, so a city with more rows than one
// chunk has inbound edges in every chunk, and every one of them must reach
// the backup, or a restore cannot put it back.
func TestPurgeStrandedReadsInboundEdgesFromEveryBatchChunk(t *testing.T) {
	n := 3 * infraPurgeBatchChunk
	city := newSmallPurgeCity(t, n)
	work := openPurgeWorkStore(t, city.workPath)
	reused := mcPurgeRows / 2 // mcAutoHandoffMail's row under mcReusedID
	edges := 0
	for i := 5; i < n; i += 37 {
		if i == reused || i+1 == reused {
			continue
		}
		if err := work.DepAdd(fmt.Sprintf("mc-wisp-%04d", i), fmt.Sprintf("mc-wisp-%04d", i+1), "blocks"); err != nil {
			t.Fatal(err)
		}
		edges++
	}
	// The work bead and the session make the last chunk one row: give it an
	// edge too. A store that omits edge-free anchors answers an edge-free
	// chunk with nothing, and the purge then reads every row on its own.
	if err := work.DepAdd(fmt.Sprintf("mc-wisp-%04d", n), fmt.Sprintf("mc-wisp-%04d", n-1), "blocks"); err != nil {
		t.Fatal(err)
	}
	edges++
	probe := &purgeBatchProbe{}
	openPurgeWorkStoreAs(t, city, func(store *beads.FileStore) beads.Store { return purgeBatchingStore{store, probe} })
	var stdout, stderr bytes.Buffer
	if code := runStrandedPurge(t, city, confirmedPurge(t, city, n), &stdout, &stderr); code != 0 {
		t.Fatalf("purge exited %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	out := readPurgeOutcome(t, city)
	total := 0
	for _, k := range out.inbound {
		total += k
	}
	if total != edges {
		t.Errorf("the backup holds %d inbound edge(s), want all %d, from every chunk", total, edges)
	}
	if got := probe.upReads(); got != 0 {
		t.Errorf("the run made %d per-row dependents read(s): a batch did not answer", got)
	}
}

// purgeFailingInboundStore answers the selected rows' batch and fails the
// inbound read, the batch over every work-store row.
type purgeFailingInboundStore struct {
	*beads.FileStore
	selected map[string]bool
}

func (s purgeFailingInboundStore) DepListBatch(ids []string) (map[string][]beads.Dep, error) {
	for _, id := range ids {
		if !s.selected[id] {
			return nil, errors.New("the link dropped mid-batch")
		}
	}
	return s.FileStore.DepListBatch(ids)
}

// TestPurgeStrandedRefusesWhenTheInboundBatchReadFails: a failed batch is not
// "no edges", so the run refuses before it writes a backup or removes a row.
func TestPurgeStrandedRefusesWhenTheInboundBatchReadFails(t *testing.T) {
	city, selected := purgeTierEdgeFixture(t)
	openPurgeWorkStoreAs(t, city, func(store *beads.FileStore) beads.Store {
		return purgeFailingInboundStore{store, selected}
	})
	var stdout, stderr bytes.Buffer
	if code := runStrandedPurge(t, city, confirmedPurge(t, city, len(selected)), &stdout, &stderr); code == 0 {
		t.Fatalf("a purge whose inbound batch read failed exited 0; stdout: %s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "the link dropped mid-batch") {
		t.Errorf("the refusal does not carry the read's error: %s", stderr.String())
	}
	if files := purgeBackupFiles(t, city); len(files) != 0 {
		t.Errorf("the refused run wrote a backup: %v", files)
	}
	if got := countPurgeRowsOfType(purgeWorkRows(t, city), "message"); got != len(selected) {
		t.Errorf("the refused run removed rows: %d message rows left, want %d", got, len(selected))
	}
}
