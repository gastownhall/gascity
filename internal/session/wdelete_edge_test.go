package session

import (
	"fmt"
	"hash/fnv"
	"io"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// inlineSetFingerprint is a verbatim copy of the pre-migration
// cmd/gc.sessionBeadSnapshotFingerprint hash body (ID + Status + Assignee + ALL
// sorted metadata keys, beads sorted by ID). It is the golden reference the
// edge SetFingerprint must reproduce byte-for-byte: config-change caching
// keys off this value, so a byte drift silently re-runs or skips demand rebuilds.
func inlineSetFingerprint(beadsIn []beads.Bead) string {
	open := make([]beads.Bead, len(beadsIn))
	copy(open, beadsIn)
	sort.Slice(open, func(i, j int) bool { return open[i].ID < open[j].ID })
	h := fnv.New64a()
	for _, bead := range open {
		_, _ = io.WriteString(h, bead.ID)
		_, _ = io.WriteString(h, "\x00")
		_, _ = io.WriteString(h, bead.Status)
		_, _ = io.WriteString(h, "\x00")
		_, _ = io.WriteString(h, bead.Assignee)
		_, _ = io.WriteString(h, "\x00")
		keys := make([]string, 0, len(bead.Metadata))
		for key := range bead.Metadata {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			_, _ = io.WriteString(h, key)
			_, _ = io.WriteString(h, "\x00")
			_, _ = io.WriteString(h, bead.Metadata[key])
			_, _ = io.WriteString(h, "\x00")
		}
	}
	return fmt.Sprintf("%x", h.Sum64())
}

func fingerprintCorpus() []beads.Bead {
	at := func(sec int) time.Time { return time.Date(2026, 4, 1, 0, 0, sec, 0, time.UTC) }
	return []beads.Bead{
		// Out-of-ID-order so the internal sort is exercised. Diverse metadata,
		// including a key session.Info does NOT project (a bespoke tag), so a
		// naive Info-derived fingerprint would drop it.
		{
			ID: "s-b", Type: BeadType, Status: "open", Assignee: "gm-2", CreatedAt: at(2),
			Metadata: map[string]string{"session_name": "beta", "state": "active", "bespoke_unprojected_tag": "v1"},
		},
		{
			ID: "s-a", Type: BeadType, Status: "open", Assignee: "", CreatedAt: at(1),
			Metadata: map[string]string{"session_name": "alpha", "state": "asleep"},
		},
		{
			ID: "s-c", Type: BeadType, Status: "open", Assignee: "gm-9", CreatedAt: at(3),
			Metadata: map[string]string{"template": "worker"},
		},
	}
}

// TestSetFingerprintMatchesInlineHash pins SetFingerprint byte-for-byte
// against the pre-migration inline hash (the config-change cache key), and proves it
// reflects EVERY metadata key — including ones Info drops. A mutation that changes the
// byte layout, drops a metadata key, or stops sorting fails here.
func TestSetFingerprintMatchesInlineHash(t *testing.T) {
	corpus := fingerprintCorpus()
	if got, want := SetFingerprint(corpus), inlineSetFingerprint(corpus); got != want {
		t.Fatalf("SetFingerprint = %q, want inline golden %q", got, want)
	}

	// Order-independence: shuffling the input must not change the fingerprint (the
	// internal ID sort makes it set-shaped).
	reordered := []beads.Bead{corpus[2], corpus[0], corpus[1]}
	if SetFingerprint(reordered) != SetFingerprint(corpus) {
		t.Fatal("SetFingerprint is order-dependent; the internal ID sort regressed")
	}

	// Sensitivity to an UNPROJECTED metadata key: two sets differing only in a key
	// Info drops must hash differently. This is the reason the fingerprint cannot be
	// computed from Info — a regression to Info-only hashing collapses these.
	mutated := fingerprintCorpus()
	mutated[0].Metadata["bespoke_unprojected_tag"] = "v2"
	if SetFingerprint(mutated) == SetFingerprint(corpus) {
		t.Fatal("SetFingerprint ignored an unprojected metadata key; it must hash ALL keys")
	}
}

// TestListAllForReconcileWithFingerprintMatchesSet pins the paired edge method: its
// fingerprint equals SetFingerprint over the same union rows, and its rows are
// row-for-row identical to ListAllForReconcile.
func TestListAllForReconcileWithFingerprintMatchesSet(t *testing.T) {
	corpus := listAllCorpus()
	mem := beads.NewMemStoreFrom(len(corpus), corpus, nil)
	front := NewStore(beads.SessionStore{Store: mem})

	rows, fingerprint, err := front.ListAllForReconcileWithFingerprint(ListAllOptions{})
	if err != nil {
		t.Fatalf("ListAllForReconcileWithFingerprint: %v", err)
	}
	plain, err := front.ListAllForReconcile(ListAllOptions{})
	if err != nil {
		t.Fatalf("ListAllForReconcile: %v", err)
	}
	if !reflect.DeepEqual(rows, plain) {
		t.Fatalf("rows diverge from ListAllForReconcile:\nwith=%+v\nplain=%+v", rows, plain)
	}

	// The fingerprint must be SetFingerprint over the raw union rows (the set
	// the snapshot projects), computed here independently through the raw union.
	rawUnion, err := ListAllSessionBeads(mem, beads.ListQuery{})
	if err != nil {
		t.Fatalf("ListAllSessionBeads: %v", err)
	}
	if want := SetFingerprint(rawUnion); fingerprint != want {
		t.Fatalf("fingerprint = %q, want SetFingerprint(union) %q", fingerprint, want)
	}
	if fingerprint == "" {
		t.Fatal("fingerprint is empty on a non-empty union")
	}
}

// TestReconcileRowsFromBeadsProjectsEachRow pins the in-memory row projection: each
// row carries InfoFromPersistedBead + CircuitStateFromMetadata of its bead, in input
// order, with no union/dedupe/filter applied.
func TestReconcileRowsFromBeadsProjectsEachRow(t *testing.T) {
	in := []beads.Bead{
		{
			ID: "s-open", Type: BeadType, Status: "open", Labels: []string{LabelSession},
			Metadata: map[string]string{"session_name": "one", SessionCircuitStateMetadataKey: SessionCircuitStateOpen},
		},
		// A non-session bead is NOT filtered out (unlike the store union) — the input
		// is taken as-is, row for row.
		{ID: "s-task", Type: "task", Status: "open", Metadata: map[string]string{}},
	}
	rows := ReconcileRowsFromBeads(in)
	if len(rows) != len(in) {
		t.Fatalf("ReconcileRowsFromBeads len = %d, want %d (no filtering)", len(rows), len(in))
	}
	for i, b := range in {
		if !reflect.DeepEqual(rows[i].Info, infoFromPersistedBead(b)) {
			t.Errorf("row %d Info mismatch", i)
		}
		if !reflect.DeepEqual(rows[i].Circuit, CircuitStateFromMetadata(b.Metadata)) {
			t.Errorf("row %d Circuit mismatch", i)
		}
	}
}

// TestListLabeledSessionInfosUnfilteredContract pins the city-stop sleep-reason
// lister: it returns the Info of every OPEN gc:session-labeled bead WITHOUT the
// IsSessionBeadOrRepairable narrowing (so a damaged non-"session"-typed labeled bead
// is still returned) and excludes closed beads.
func TestListLabeledSessionInfosUnfilteredContract(t *testing.T) {
	corpus := []beads.Bead{
		{
			ID: "s-open", Type: BeadType, Status: "open", Labels: []string{LabelSession},
			Metadata: map[string]string{"session_name": "open", "state": "active"},
		},
		// gc:session label but a non-empty non-"session" type: Store.List drops this
		// via IsSessionBeadOrRepairable; the unfiltered lister keeps it.
		{
			ID: "s-damaged", Type: "task", Status: "open", Labels: []string{LabelSession},
			Metadata: map[string]string{"session_name": "damaged", "state": "active"},
		},
		{
			ID: "s-closed", Type: BeadType, Status: "closed", Labels: []string{LabelSession},
			Metadata: map[string]string{"session_name": "closed"},
		},
	}
	mem := beads.NewMemStoreFrom(len(corpus), corpus, nil)
	front := NewStore(beads.SessionStore{Store: mem})

	infos, err := front.ListLabeledSessionInfosUnfiltered()
	if err != nil {
		t.Fatalf("ListLabeledSessionInfosUnfiltered: %v", err)
	}
	got := map[string]bool{}
	for _, in := range infos {
		got[in.ID] = true
	}
	if !got["s-open"] {
		t.Error("missing the healthy open session")
	}
	if !got["s-damaged"] {
		t.Error("dropped the damaged gc:session-labeled non-session-typed bead — the sweep must still mark it")
	}
	if got["s-closed"] {
		t.Error("included a closed bead — the lister must be closed-excluded")
	}

	// Contrast with the filtered Store.List, which DROPS the damaged bead — proving
	// the unfiltered lister is materially different (not a redundant wrapper).
	filtered, err := front.List("", "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, in := range filtered {
		if in.ID == "s-damaged" {
			t.Fatal("Store.List unexpectedly kept the damaged bead; the unfiltered lister's rationale is gone")
		}
	}
}
