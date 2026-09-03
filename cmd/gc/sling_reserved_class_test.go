package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// TestResolveInlineBeadAction_ReservedClassIDRefused proves gc sling refuses to
// turn an id in a reserved coordination-class namespace into inline bead text.
//
// These are the ids that actually reproduced the defect: a reserved-class id
// that the CLI's bead-ID heuristic does NOT accept (looksLikeBeadID needs a
// single dash and a <=8-char suffix, so the wisp shape's double dash and a
// descriptive multi-dash id both fail it) used to fall through to
// store.Create() and mint a task bead whose TITLE was the id string.
// `gc sling <target> gcg--9223372036854775645` printed
// `Created gc-1 — "gcg--9223372036854775645"` and exited 0, so a caller routing
// a workflow step silently got a brand-new duplicate instead (westlands
// cr-ahjgr: five such calls, five duplicate beads).
func TestResolveInlineBeadAction_ReservedClassIDRefused(t *testing.T) {
	cfg := &config.City{}
	for _, id := range []string{
		"gcg--9223372036854775645", // the reported wisp shape: negative int64 suffix
		"gcm--1",
		"gcg-workflow-finalize", // descriptive multi-dash suffix
		"gcs-abcdefghijkl",      // single dash, but suffix longer than the heuristic allows
	} {
		t.Run(id, func(t *testing.T) {
			// Precondition: this id really does degrade to inline text — that is
			// what made it dangerous, and it is what the refusal guards.
			if !looksLikeInlineText(cfg, id) {
				t.Fatalf("precondition: %q no longer degrades to inline text", id)
			}
			store := beads.NewMemStore()
			for _, dryRun := range []bool{false, true} {
				create, preview, err := resolveInlineBeadAction(cfg, id, dryRun, store)
				if err == nil {
					t.Fatalf("dryRun=%v: got no error, want refusal (create=%v preview=%v)", dryRun, create, preview)
				}
				if create || preview {
					t.Fatalf("dryRun=%v: refusal must not create or preview (create=%v preview=%v)", dryRun, create, preview)
				}
				if !strings.Contains(err.Error(), "coordination-class") {
					t.Fatalf("dryRun=%v: error should name the coordination-class namespace: %v", dryRun, err)
				}
			}
			if open, lerr := store.ListOpen(); lerr != nil {
				t.Fatalf("listing store: %v", lerr)
			} else if len(open) != 0 {
				t.Fatalf("store must be empty after a refusal, got %d bead(s)", len(open))
			}
		})
	}
}

// TestResolveInlineBeadAction_ReservedClassIDNeverMinted is the invariant behind
// the fix, stated over every reserved class prefix and both id shapes: whatever
// path a reserved-class id takes, gc sling must never mint a bead from it.
// Short-suffix ids (e.g. "gcg-1") take the bead-ID fast path and are routed;
// inline-text-shaped ids are refused. Neither may return create=true.
func TestResolveInlineBeadAction_ReservedClassIDNeverMinted(t *testing.T) {
	cfg := &config.City{}
	for _, prefix := range config.ReservedClassPrefixes() {
		for _, suffix := range []string{"-1", "1", "-9223372036854775645", "workflow-finalize", "abcdefghijkl"} {
			id := prefix + "-" + suffix
			t.Run(id, func(t *testing.T) {
				store := beads.NewMemStore()
				create, _, err := resolveInlineBeadAction(cfg, id, false, store)
				if create {
					t.Fatalf("%q must never be minted as inline text (err=%v)", id, err)
				}
				if open, lerr := store.ListOpen(); lerr == nil && len(open) != 0 {
					t.Fatalf("%q left %d bead(s) in the store", id, len(open))
				}
			})
		}
	}
}

// TestResolveInlineBeadAction_WorkResidentReservedIDStillRoutes pins the reason
// the refusal sits AFTER the store probe rather than before it. A reserved class
// prefix is only an ADVISORY on work stores (config.ReservedPrefixWarnings
// warns; config.ValidateRigs does not reject), so a work store can legitimately
// hold an id inside the class namespace. When the probe finds it, sling must
// route it as it always did — a prefix check placed ahead of the probe would
// refuse these and break routing for beads that really are there.
func TestResolveInlineBeadAction_WorkResidentReservedIDStillRoutes(t *testing.T) {
	const id = "gcg-42"
	store := beads.NewMemStore()
	store.HonorExplicitIDs = true
	if _, err := store.Create(beads.Bead{ID: id, Title: "a real bead that lives in the work ledger"}); err != nil {
		t.Fatalf("seeding work-resident reserved-prefix bead: %v", err)
	}
	create, preview, err := resolveInlineBeadAction(&config.City{}, id, false, store)
	if err != nil {
		t.Fatalf("work-resident %s must route, not refuse: %v", id, err)
	}
	if create || preview {
		t.Fatalf("work-resident %s must route (create=%v preview=%v)", id, create, preview)
	}
}

// TestResolveInlineBeadAction_OrdinaryInlineTextUnaffected pins that the
// refusal is narrow: ordinary inline text, including prose that merely mentions
// a class prefix, still creates a bead. isBeadIDCandidate rejects anything with
// whitespace, so a real title can never reach the refusal.
func TestResolveInlineBeadAction_OrdinaryInlineTextUnaffected(t *testing.T) {
	for _, text := range []string{
		"fix the dispatcher backoff",
		"gcg- something went wrong", // has whitespace: not an id candidate
		"investigate gcg beads",
	} {
		t.Run(text, func(t *testing.T) {
			create, _, err := resolveInlineBeadAction(&config.City{}, text, false, beads.NewMemStore())
			if err != nil {
				t.Fatalf("ordinary inline text must not be refused: %v", err)
			}
			if !create {
				t.Fatalf("ordinary inline text should create a bead, got create=false")
			}
		})
	}
}

// TestReservedClassPrefixForID pins the namespace predicate against
// config.ReservedClassPrefixes, including the double-dash wisp shape whose
// negative-int64 suffix the sling.BeadPrefix heuristic is not relied on for.
func TestReservedClassPrefixForID(t *testing.T) {
	for _, tc := range []struct {
		id, want string
	}{
		{"gcg--9223372036854775645", "gcg"},
		{"gcg-1", "gcg"},
		{"gcm-1", "gcm"},
		{"gcg", "gcg"},
		{"gc-1", ""},     // the ordinary work prefix is not reserved
		{"gcgx-1", ""},   // longer prefix, different namespace
		{"cr-ahjgr", ""}, // a work-ledger id
		{"", ""},
	} {
		got, ok := reservedClassPrefixForID(tc.id)
		if tc.want == "" {
			if ok {
				t.Errorf("reservedClassPrefixForID(%q) = (%q, true), want not reserved", tc.id, got)
			}
			continue
		}
		if !ok || got != tc.want {
			t.Errorf("reservedClassPrefixForID(%q) = (%q, %v), want (%q, true)", tc.id, got, ok, tc.want)
		}
	}
}
