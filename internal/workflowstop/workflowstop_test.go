package workflowstop

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// ledger is a Getter over a fixed set of beads, answering beads.ErrNotFound
// for any other id, as a store does for a bead it does not hold.
type ledger map[string]beads.Bead

func (l ledger) Get(id string) (beads.Bead, error) {
	b, ok := l[id]
	if !ok {
		return beads.Bead{}, fmt.Errorf("bead %s: %w", id, beads.ErrNotFound)
	}
	return b, nil
}

// failingLedger fails every read with err.
type failingLedger struct{ err error }

func (f failingLedger) Get(string) (beads.Bead, error) { return beads.Bead{}, f.err }

// bead is an id with status and metadata key/value pairs.
func bead(id, status string, meta ...string) beads.Bead {
	m := make(map[string]string, len(meta)/2)
	for i := 0; i+1 < len(meta); i += 2 {
		m[meta[i]] = meta[i+1]
	}
	return beads.Bead{ID: id, Status: status, Metadata: m}
}

// TestStoppedWorkflowAncestor pins the ancestry walk where it lives: a step's
// workflow has stopped when the nearest root on its chain is closed (settled)
// or carries cancel intent (gc.outcome=canceled, or gc.cancel_requested on a
// root still open), and a chain that is live, broken by an absent link, or
// cyclic has not. A drain item root climbs to the run above it through its
// drain control.
func TestStoppedWorkflowAncestor(t *testing.T) {
	step := bead("s-1", "open", beadmeta.RootBeadIDMetadataKey, "r-1")
	for _, tc := range []struct {
		name         string
		ledger       ledger
		gated        beads.Bead
		wantRoot     string
		wantCanceled bool
	}{
		{
			name:   "open root",
			ledger: ledger{"r-1": bead("r-1", "open")},
			gated:  step,
		},
		{
			name:     "closed root",
			ledger:   ledger{"r-1": bead("r-1", "closed", beadmeta.OutcomeMetadataKey, beadmeta.OutcomePass)},
			gated:    step,
			wantRoot: "r-1",
		},
		{
			name:         "canceled root",
			ledger:       ledger{"r-1": bead("r-1", "closed", beadmeta.OutcomeMetadataKey, beadmeta.OutcomeCanceled)},
			gated:        step,
			wantRoot:     "r-1",
			wantCanceled: true,
		},
		{
			name:         "cancel-requested root still open",
			ledger:       ledger{"r-1": bead("r-1", "open", beadmeta.CancelRequestedMetadataKey, "2026-11-28T00:00:00Z")},
			gated:        step,
			wantRoot:     "r-1",
			wantCanceled: true,
		},
		{
			name:   "absent root",
			ledger: ledger{},
			gated:  step,
		},
		{
			name:   "bead naming no root",
			ledger: ledger{},
			gated:  bead("s-1", "open"),
		},
		{
			name: "drain item root under a canceled run",
			ledger: ledger{
				"item": bead("item", "open", beadmeta.DrainControlIDMetadataKey, "ctl"),
				"ctl":  bead("ctl", "open", beadmeta.RootBeadIDMetadataKey, "run"),
				"run":  bead("run", "open", beadmeta.CancelRequestedMetadataKey, "2026-11-28T00:00:00Z"),
			},
			gated:        bead("s-1", "open", beadmeta.RootBeadIDMetadataKey, "item"),
			wantRoot:     "run",
			wantCanceled: true,
		},
		{
			name: "drain item root handed out whole, its run closed",
			ledger: ledger{
				"ctl": bead("ctl", "open", beadmeta.RootBeadIDMetadataKey, "run"),
				"run": bead("run", "closed"),
			},
			gated:    bead("item", "open", beadmeta.DrainControlIDMetadataKey, "ctl"),
			wantRoot: "run",
		},
		{
			name: "drain control naming no root ends the chain",
			ledger: ledger{
				"item": bead("item", "open", beadmeta.DrainControlIDMetadataKey, "ctl"),
				"ctl":  bead("ctl", "open"),
			},
			gated: bead("s-1", "open", beadmeta.RootBeadIDMetadataKey, "item"),
		},
		{
			name: "cycle",
			ledger: ledger{
				"item": bead("item", "open", beadmeta.DrainControlIDMetadataKey, "ctl"),
				"ctl":  bead("ctl", "open", beadmeta.RootBeadIDMetadataKey, "item"),
			},
			gated: bead("s-1", "open", beadmeta.RootBeadIDMetadataKey, "item"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stop, err := StoppedWorkflowAncestor(tc.ledger, tc.gated)
			if err != nil {
				t.Fatalf("StoppedWorkflowAncestor: %v", err)
			}
			if stop.RootID != tc.wantRoot || stop.Canceled != tc.wantCanceled || stop.Stopped() != (tc.wantRoot != "") {
				t.Fatalf("stop = %+v (stopped %t), want root %q canceled %t", stop, stop.Stopped(), tc.wantRoot, tc.wantCanceled)
			}
		})
	}
}

// TestStoppedWorkflowAncestorReturnsReadErrors pins that only a missing link
// ends the walk quietly: any other failed read is returned, naming the link,
// because an unreadable ancestry is not evidence either way.
func TestStoppedWorkflowAncestorReturnsReadErrors(t *testing.T) {
	readErr := errors.New("ledger unavailable")
	_, err := StoppedWorkflowAncestor(failingLedger{err: readErr}, bead("s-1", "open", beadmeta.RootBeadIDMetadataKey, "r-1"))
	if !errors.Is(err, readErr) || !strings.Contains(err.Error(), "r-1") {
		t.Fatalf("err = %v, want the read error naming r-1", err)
	}
}

// TestStopOutcome pins the outcome a bead stranded by a stop closes with:
// canceled under a cancellation, skipped otherwise.
func TestStopOutcome(t *testing.T) {
	if got := (Stop{RootID: "r-1", Canceled: true}).Outcome(); got != beadmeta.OutcomeCanceled {
		t.Fatalf("canceled stop outcome = %q, want %q", got, beadmeta.OutcomeCanceled)
	}
	if got := (Stop{RootID: "r-1"}).Outcome(); got != beadmeta.OutcomeSkipped {
		t.Fatalf("settled stop outcome = %q, want %q", got, beadmeta.OutcomeSkipped)
	}
	if (Stop{}).Stopped() {
		t.Fatal("the zero Stop reports stopped; want a live chain")
	}
}
