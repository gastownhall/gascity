package main

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// The cost of the repair's source reads (ga-50tsx).
//
// `gc storage recover-stranded --from-work` reads the source's dep edges three
// times over: once classifying the stranded beads, once planning the topology
// over every bead the binding will hold, and once verifying against a second
// independent read. Done one bead at a time that is a few hundred round trips,
// and against a work store reached over a WAN link that drops handshakes it is
// a few hundred chances to fail — which is why the command could not finish for
// city fdcanary-1786758267 while the beads it had to drain kept the controller
// crash-looping.
//
// The defect is a COST, so these tests assert cost: how many times the repair
// asks the source, and whether it still finishes when asking one bead at a time
// no longer works.

// countingDepSource counts how the repair asks the source for edges and can
// refuse the per-bead question, which is the failure the WAN link produces.
type countingDepSource struct {
	beads.Store
	depListCalls int
	batchCalls   int
	batchAnchors int
	// perBeadErr, when set, fails every per-bead DepList — the link surviving a
	// bounded number of reads but not a few hundred of them.
	perBeadErr error
}

func (s *countingDepSource) DepList(id, direction string) ([]beads.Dep, error) {
	s.depListCalls++
	if s.perBeadErr != nil {
		return nil, s.perBeadErr
	}
	return s.Store.DepList(id, direction)
}

func (s *countingDepSource) DepListBatch(ids []string) (map[string][]beads.Dep, error) {
	s.batchCalls++
	s.batchAnchors += len(ids)
	batch, ok := s.Store.(interface {
		DepListBatch(ids []string) (map[string][]beads.Dep, error)
	})
	if !ok {
		return nil, fmt.Errorf("the fixture's leaf store %T cannot answer a batched dep read", s.Store)
	}
	return batch.DepListBatch(ids)
}

// DepMetadata forwards the edge-payload read the repair makes while it restores
// the topology. It is not counted: the cost these tests pin is the edge LIST.
func (s *countingDepSource) DepMetadata(issueID, dependsOnID string) (string, bool, error) {
	reader, ok := s.Store.(beads.DepMetadataReader)
	if !ok {
		return "", false, fmt.Errorf("the fixture's leaf store %T cannot report dep payloads", s.Store)
	}
	return reader.DepMetadata(issueID, dependsOnID)
}

// countingRecoverySource wraps the fixture's source in the counter and points
// the repair at it.
func countingRecoverySource(t *testing.T, source beads.Store) *countingDepSource {
	t.Helper()
	counted := &countingDepSource{Store: source}
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) { return counted, nil }
	t.Cleanup(func() { openInfraMigrationSource = prev })
	return counted
}

// strandedMessageBeads seeds the shape the live city stranded: warm-up mail the
// binding never received, wired to each other so the run has a topology to
// restore as well as rows to copy.
func strandedMessageBeads(t *testing.T, source beads.Store, n int) []string {
	t.Helper()
	ids := make([]string, 0, n)
	for i := range n {
		b := mustCreateInfraBead(t, source, beads.Bead{Title: fmt.Sprintf("warm-up mail %d", i), Type: "message"})
		ids = append(ids, b.ID)
	}
	for i := 1; i < len(ids); i++ {
		if err := source.DepAdd(ids[i], ids[i-1], "blocks"); err != nil {
			t.Fatalf("seeding the within-infra edge %s -> %s: %v", ids[i], ids[i-1], err)
		}
	}
	return ids
}

// TestStorageRecoverStrandedReadsSourceEdgesInOneBatchNotOnePerBead is the
// defect as a number.
//
// Red before the batch: 20 source DepList calls for this fixture's 8 beads (one
// probe, 4 classifying the stranded rows, 7 planning the topology over every
// resident bead, 8 verifying) and no batched read at all. The live city's 231
// source beads made that ~460 round trips over the Tailscale link, each one
// able to burn the store's whole 90-second read-retry budget before the walk
// moved on and refused the bead — which is the "wedge" the operator saw.
func TestStorageRecoverStrandedReadsSourceEdgesInOneBatchNotOnePerBead(t *testing.T) {
	cityPath, cfg, source, target := convergedRecoveryCity(t)
	stranded := strandedMessageBeads(t, source, 4)
	counted := countingRecoverySource(t, source)

	var stdout, stderr bytes.Buffer
	if code := runStrandedRecovery(t, cityPath, cfg, &stdout, &stderr); code != 0 {
		t.Fatalf("recovery exited %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}

	if counted.batchCalls != 1 {
		t.Errorf("the repair made %d batched dep read(s), want exactly 1 for the whole run", counted.batchCalls)
	}
	// The probe newSourceDepReader runs once to decide whether this adapter can
	// list relations at all is the only per-bead read left. Anything beyond it is
	// a walk that still scales with the bead count.
	if counted.depListCalls > 1 {
		t.Errorf("the repair made %d per-bead source DepList call(s), want at most the single capability probe: the walk still costs one round trip per bead", counted.depListCalls)
	}
	if counted.batchAnchors < len(stranded) {
		t.Errorf("the batch asked about %d anchor(s), fewer than the %d stranded beads: it cannot have covered the walk", counted.batchAnchors, len(stranded))
	}

	// Cheaper is worthless if it is also wrong: the rows and the topology must
	// still cross.
	if got := strandedIDs(t, cityPath, target); len(got) != 0 {
		t.Errorf("the boot guard still reads %d stranded bead(s): %v", len(got), got)
	}
	for i := 1; i < len(stranded); i++ {
		if got := bindingEdges(t, target, stranded[i]); !slices.Contains(got, stranded[i-1]) {
			t.Errorf("the edge %s -> %s did not cross: binding deps = %v", stranded[i], stranded[i-1], got)
		}
	}
}

// TestStorageRecoverStrandedDrainsWhenPerBeadSourceReadsFail is the operational
// claim behind the fix: the repair is a DEGRADED-INCIDENT command, so it has to
// make progress on the link it is called for rather than on a perfect one.
//
// The link here survives the batch and refuses every per-bead read. Before the
// batch existed there was nothing but per-bead reads, so every stranded bead
// became "dependency topology unreadable", the run moved nothing and exited 1 —
// a walk whose only outcome after hours of retrying was a refusal.
func TestStorageRecoverStrandedDrainsWhenPerBeadSourceReadsFail(t *testing.T) {
	cityPath, cfg, source, target := convergedRecoveryCity(t)
	stranded := strandedMessageBeads(t, source, 4)
	counted := countingRecoverySource(t, source)
	counted.perBeadErr = errors.New("dial tcp 100.109.51.65:3306: i/o timeout")

	var stdout, stderr bytes.Buffer
	code := runStrandedRecovery(t, cityPath, cfg, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("recovery exited %d over a link that refuses per-bead reads, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if got := strandedIDs(t, cityPath, target); len(got) != 0 {
		t.Errorf("the boot guard still reads %d stranded bead(s) after the run: %v", len(got), got)
	}
	if !strings.Contains(stdout.String(), fmt.Sprintf("copied: %d bead(s)", len(stranded))) {
		t.Errorf("the report does not say the stranded beads were copied: %s", stdout.String())
	}
	for i := 1; i < len(stranded); i++ {
		if got := bindingEdges(t, target, stranded[i]); !slices.Contains(got, stranded[i-1]) {
			t.Errorf("the edge %s -> %s did not cross: binding deps = %v", stranded[i], stranded[i-1], got)
		}
	}
}

// TestStorageRecoverStrandedRefusesWhenTheBatchedSourceReadFails is the other
// half of the same claim: with the walk now resting on ONE read, that read
// failing must stop the run and name the link.
//
// The failure it prevents is the worse one — a batched read that returned a
// partial answer with no error would present every bead it lost as edge-free
// and copy it that way.
func TestStorageRecoverStrandedRefusesWhenTheBatchedSourceReadFails(t *testing.T) {
	cityPath, cfg, source, target := convergedRecoveryCity(t)
	strandedMessageBeads(t, source, 4)
	before := strandedIDs(t, cityPath, target)
	counted := countingRecoverySource(t, source)
	counted.Store = refusingBatchStore{Store: source}

	var stdout, stderr bytes.Buffer
	if code := runStrandedRecovery(t, cityPath, cfg, &stdout, &stderr); code == 0 {
		t.Fatalf("recovery exited 0 with the source's batched edge read failing; stdout: %s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "i/o timeout") {
		t.Errorf("the refusal does not name what failed, so an operator cannot tell a broken link from an empty city: %s", stderr.String())
	}
	if got := strandedIDs(t, cityPath, target); len(got) != len(before) {
		t.Errorf("the refused run changed the stranded set from %d to %d bead(s); it must move nothing", len(before), len(got))
	}
}

// refusingBatchStore answers every batched dep read with the link error.
type refusingBatchStore struct{ beads.Store }

func (s refusingBatchStore) DepListBatch([]string) (map[string][]beads.Dep, error) {
	return nil, errors.New("dial tcp 100.109.51.65:3306: i/o timeout")
}
