package config

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// Regression for review finding 7 on PR #4322.
//
// The routed tier fetches a bounded window (20 rows). If bd chooses which rows
// land in that window, the window can exclude the row the policy actually ranks
// first: bd's `hybrid` puts every bead created in the last 48h ahead of older
// ones and discards the band of the older rows (priority -> 999). With a
// saturated window of fresh P2s, an aged P0 never gets fetched, and the Go-side
// re-sort cannot rescue a row it never received. Under sustained fresh arrivals
// the aged P0 starves — the exact inversion the change exists to prevent.
//
// These tests run the REAL jq program the query embeds, against the failure
// shape, so they assert ordering behavior rather than a query string.

type windowBead struct {
	ID        string `json:"id"`
	Priority  *int   `json:"priority,omitempty"`
	CreatedAt string `json:"created_at"`
}

// runWindowJQ applies the policy's canonical window program to rows and returns
// the selected ids, in order.
func runWindowJQ(t *testing.T, policy beads.AdmissionPolicy, rows []windowBead, limit int) []string {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skipf("jq not installed: %v", err)
	}
	input, err := json.Marshal(rows)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	program := fmt.Sprintf(`sort_by(%s) | .[:%d] | [.[].id]`, policy.JQSortKey(), limit)
	cmd := exec.Command("jq", "-c", program)
	cmd.Stdin = strings.NewReader(string(input))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("jq %q: %v", program, err)
	}
	var ids []string
	if err := json.Unmarshal(out, &ids); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	return ids
}

// agedP0Backlog is finding 7's scenario: one P0 well past bd's 48h recency
// cutoff, buried behind more fresh P2s than the window can hold.
func agedP0Backlog(freshCount int) []windowBead {
	now := time.Now().UTC()
	p0, p2 := 0, 2
	rows := []windowBead{{
		ID:        "aged-p0",
		Priority:  &p0,
		CreatedAt: now.Add(-30 * 24 * time.Hour).Format(time.RFC3339),
	}}
	for i := 0; i < freshCount; i++ {
		rows = append(rows, windowBead{
			ID:        fmt.Sprintf("fresh-p2-%02d", i),
			Priority:  &p2,
			CreatedAt: now.Add(-time.Duration(i) * time.Minute).Format(time.RFC3339),
		})
	}
	return rows
}

func TestPriorityFIFOWindowKeepsAgedP0OverSaturatingFreshWork(t *testing.T) {
	ids := runWindowJQ(t, beads.PolicyPriorityFIFO, agedP0Backlog(40), 20)

	if len(ids) != 20 {
		t.Fatalf("window returned %d rows, want 20", len(ids))
	}
	if ids[0] != "aged-p0" {
		t.Fatalf("priority_fifo window head = %q, want aged-p0: an aged P0 was pushed out by fresh P2s", ids[0])
	}
}

func TestFIFOWindowKeepsOldestRegardlessOfPriority(t *testing.T) {
	ids := runWindowJQ(t, beads.PolicyFIFO, agedP0Backlog(40), 20)

	if ids[0] != "aged-p0" {
		t.Fatalf("fifo window head = %q, want aged-p0 (it is the oldest)", ids[0])
	}
	// Under fifo the aged bead wins on age alone, so prove priority is ignored:
	// a fresh P0 must NOT jump the queue.
	now := time.Now().UTC()
	p0, p2 := 0, 2
	rows := []windowBead{
		{ID: "fresh-p0", Priority: &p0, CreatedAt: now.Format(time.RFC3339)},
		{ID: "old-p2", Priority: &p2, CreatedAt: now.Add(-time.Hour).Format(time.RFC3339)},
	}
	if got := runWindowJQ(t, beads.PolicyFIFO, rows, 20); got[0] != "old-p2" {
		t.Fatalf("fifo head = %q, want old-p2: fifo must ignore priority", got[0])
	}
}

// The jq window and the Go comparator are two spellings of one policy. If they
// disagree on any input the admission paths disagree with each other.
func TestWindowJQAgreesWithGoComparator(t *testing.T) {
	rows := agedP0Backlog(25)
	for _, policy := range []beads.AdmissionPolicy{beads.PolicyPriorityFIFO, beads.PolicyFIFO} {
		jqIDs := runWindowJQ(t, policy, rows, len(rows))

		goBeads := make([]beads.Bead, 0, len(rows))
		for _, r := range rows {
			created, err := time.Parse(time.RFC3339, r.CreatedAt)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			goBeads = append(goBeads, beads.Bead{ID: r.ID, Priority: r.Priority, CreatedAt: created})
		}
		less := beads.LessFunc(policy)
		sortBeadsBy(goBeads, less)

		for i := range goBeads {
			if goBeads[i].ID != jqIDs[i] {
				t.Fatalf("%s: position %d: Go comparator says %q, jq window says %q",
					policy, i, goBeads[i].ID, jqIDs[i])
			}
		}
	}
}

func sortBeadsBy(items []beads.Bead, less func(a, b beads.Bead) bool) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && less(items[j], items[j-1]); j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}

// The query must not hand window selection back to bd.
func TestRoutedTierDoesNotDelegateWindowingToBd(t *testing.T) {
	for _, policy := range []string{"", "priority_fifo", "fifo"} {
		agent := &Agent{Name: "polecat", SchedulingPolicy: policy}
		q := agent.EffectiveRoutedPoolQuery()

		if strings.Contains(q, "--sort hybrid") {
			t.Errorf("policy %q: routed tier still asks bd for hybrid order", policy)
		}
		if !strings.Contains(q, "--limit 0") {
			t.Errorf("policy %q: routed tier must fetch unbounded so jq picks the window: %s", policy, q)
		}
		want := "sort_by(" + beads.AdmissionPolicy(policy).JQSortKey() + ")"
		if !strings.Contains(q, want) {
			t.Errorf("policy %q: routed tier does not order by %s before windowing", policy, want)
		}
	}
}
