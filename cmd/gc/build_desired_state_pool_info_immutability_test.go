package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// staleSingletonPoolBead builds a non-expanding singleton pool session bead whose
// agent_name/label carry a stale -N identity slot that normalization must collapse
// to the canonical name. The alias is already canonical so normalization takes the
// plain apply() path (no city alias-lock filesystem dance).
func staleSingletonPoolBead(t *testing.T, store beads.Store) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Title:  "cashmaster/refinery-1",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel, "agent:cashmaster/refinery-1", "template:cashmaster/refinery"},
		Metadata: map[string]string{
			"template":             "cashmaster/refinery",
			"agent_name":           "cashmaster/refinery-1",
			"alias":                "cashmaster/refinery",
			"session_name":         "s-refinery-stale",
			"state":                "awake",
			poolManagedMetadataKey: boolMetadata(true),
			"pool_slot":            "1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func staleSingletonAgent() config.Agent {
	return config.Agent{
		Name:              "refinery",
		Dir:               "cashmaster",
		StartCommand:      "true",
		MaxActiveSessions: intPtr(1),
	}
}

// TestNormalizeNonExpandingPoolSessionInfoCopiesCallerLabelsBeforeAddOnlyAppend
// restores the spare-capacity backing-array immutability guard removed in the
// store-domain-objects migration (council finding 8). Normalization appends the
// canonical agent label to the returned Info; it must first COPY the caller's label
// slice, never append into its backing array. The caller passes a slice with spare
// capacity, so an add-only append that aliased it would write the canonical label
// into the caller's spare slot — mutating input the caller still owns.
func TestNormalizeNonExpandingPoolSessionInfoCopiesCallerLabelsBeforeAddOnlyAppend(t *testing.T) {
	store := beads.NewMemStore()
	bead := staleSingletonPoolBead(t, store)

	info, err := sessionFrontDoor(store).Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// Caller input: a label slice with SPARE CAPACITY (len 2, cap 4). Retained for
	// backing-array inspection after the call.
	labels := make([]string, 2, 4)
	labels[0] = sessionBeadLabel
	labels[1] = "agent:cashmaster/refinery-1"
	info.Labels = labels

	cfgAgent := staleSingletonAgent()
	var stderr bytes.Buffer
	bp := newAgentBuildParams("test-city", t.TempDir(), &config.City{Workspace: config.Workspace{Name: "test-city"}}, runtime.NewFake(), time.Now().UTC(), store, &stderr)

	folded, err := normalizeNonExpandingPoolSessionInfo(bp, &cfgAgent, info)
	if err != nil {
		t.Fatalf("normalizeNonExpandingPoolSessionInfo: %v", err)
	}

	// Behavior preserved: the returned Info carries the canonical agent label.
	if !containsString(folded.Labels, "agent:cashmaster/refinery") {
		t.Fatalf("folded labels = %#v, want canonical agent label after normalization", folded.Labels)
	}
	// Immutability: the caller's retained slice must still have spare capacity, and
	// the append slot in its backing array must never have been written.
	if cap(labels) <= len(labels) {
		t.Fatalf("caller labels capacity = %d, want spare capacity to exercise add-only append", cap(labels))
	}
	expanded := labels[:cap(labels)]
	if got := expanded[len(labels)]; got != "" {
		t.Fatalf("caller labels backing array was mutated at the append slot: %q", got)
	}
	// Caller input preserved: still stale, never rewritten to canonical.
	if containsString(labels, "agent:cashmaster/refinery") {
		t.Fatalf("caller labels = %#v, must not be mutated to the canonical label", labels)
	}
	if labels[1] != "agent:cashmaster/refinery-1" {
		t.Fatalf("caller labels[1] = %q, want the original stale label preserved", labels[1])
	}
}

// TestNormalizeNonExpandingPoolSessionInfoDoesNotMutateCallerInput restores the
// caller-input immutability guard removed in the migration (council finding 8):
// normalization returns a normalized COPY (folded Info + durable store write) while
// leaving the caller's Info and its label slice untouched, so a REUSED snapshot row
// keeps its original identity for the rest of the build.
func TestNormalizeNonExpandingPoolSessionInfoDoesNotMutateCallerInput(t *testing.T) {
	store := beads.NewMemStore()
	bead := staleSingletonPoolBead(t, store)

	info, err := sessionFrontDoor(store).Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	labels := []string{sessionBeadLabel, "agent:cashmaster/refinery-1", "template:cashmaster/refinery"}
	info.Labels = labels
	callerAgentName := info.AgentName

	cfgAgent := staleSingletonAgent()
	var stderr bytes.Buffer
	bp := newAgentBuildParams("test-city", t.TempDir(), &config.City{Workspace: config.Workspace{Name: "test-city"}}, runtime.NewFake(), time.Now().UTC(), store, &stderr)

	folded, err := normalizeNonExpandingPoolSessionInfo(bp, &cfgAgent, info)
	if err != nil {
		t.Fatalf("normalizeNonExpandingPoolSessionInfo: %v", err)
	}

	// The returned copy is normalized to the canonical identity.
	if folded.AgentName != "cashmaster/refinery" {
		t.Fatalf("folded AgentName = %q, want canonical", folded.AgentName)
	}
	if !containsString(folded.Labels, "agent:cashmaster/refinery") || containsString(folded.Labels, "agent:cashmaster/refinery-1") {
		t.Fatalf("folded labels = %#v, want canonical label swapped in, stale label removed", folded.Labels)
	}
	// The caller's Info and its label slice are untouched (normalization worked on a
	// copy, never aliased/mutated the caller's input).
	if info.AgentName != callerAgentName {
		t.Fatalf("caller AgentName mutated to %q, want %q", info.AgentName, callerAgentName)
	}
	if !containsString(labels, "agent:cashmaster/refinery-1") || containsString(labels, "agent:cashmaster/refinery") {
		t.Fatalf("caller labels = %#v, want the stale label preserved and no canonical label added", labels)
	}
	// The durable row DID normalize (behavior end-to-end).
	stored, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get stored: %v", err)
	}
	if !containsString(stored.Labels, "agent:cashmaster/refinery") {
		t.Fatalf("stored labels = %#v, want canonical label after normalization", stored.Labels)
	}
	if got := stored.Metadata["agent_name"]; got != "cashmaster/refinery" {
		t.Fatalf("stored agent_name = %q, want canonical", got)
	}
}
