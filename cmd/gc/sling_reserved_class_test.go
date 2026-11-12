package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
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
		"gcnq-abcdef0123456789", // auxiliary nudge-queue namespace, outside the mint-prefix map
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
	for _, prefix := range config.AllReservedClassPrefixes() {
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
// config.AllReservedClassPrefixes, including the double-dash wisp shape whose
// negative-int64 suffix the sling.BeadPrefix heuristic is not relied on for.
func TestReservedClassPrefixForID(t *testing.T) {
	for _, tc := range []struct {
		id, want string
	}{
		{"gcg--9223372036854775645", "gcg"},
		{"gcg-1", "gcg"},
		{"gcm-1", "gcm"},
		{"gcg", "gcg"},
		{"gcnq-abcdef0123456789", "gcnq"},
		{"gcnq", "gcnq"},
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

// splitCityWorkflowRoot writes a workflow root into a split city's graph
// binding, as gc formula cook does for a v2 formula there, and returns it.
func splitCityWorkflowRoot(t *testing.T, cityPath string) (beads.Store, beads.Bead) {
	t.Helper()
	graph, relocated := cliStorageRoutes(cityPath).storeFor(coordclass.ClassGraph)
	if !relocated {
		t.Fatal("graph class is not relocated; the fixture is not a split city")
	}
	root, err := graph.Create(beads.Bead{
		Title: "pancakes",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:            "workflow",
			beadmeta.FormulaContractMetadataKey: "graph.v2",
		},
	})
	if err != nil {
		t.Fatalf("creating the workflow root in the binding: %v", err)
	}
	if _, ok := reservedClassPrefixForID(root.ID); !ok {
		t.Fatalf("the binding minted %q, outside the reserved namespace; the fixture proves nothing", root.ID)
	}
	return graph, root
}

// TestCmdSlingNamesACoordinationBeadTheBindingHolds: on a split city, every
// new city, a v2 gc formula cook mints the workflow in the city's class
// binding under a reserved id. gc sling routes work-ledger beads and cannot
// route one the binding holds (#5974), so it refuses by name, naming the
// launch that works, instead of reporting the bead missing and suggesting
// --force, which cannot route it either. Nothing is written.
func TestCmdSlingNamesACoordinationBeadTheBindingHolds(t *testing.T) {
	cityPath := oneShotCLICity(t, filepath.Join(t.TempDir(), "store"))
	writeSplitSlingFormulaCity(t, cityPath)
	graph, root := splitCityWorkflowRoot(t, cityPath)

	for _, force := range []bool{false, true} {
		var stdout, stderr bytes.Buffer
		code := cmdSling(
			[]string{"worker", root.ID},
			false, false, force, // isFormula, doNudge, force
			"", nil, "",
			false, false, false, "", // noConvoy, owned, reassign, onFormula
			false, false, false, // noFormula, fromStdin, dryRun
			"", "",
			&stdout, &stderr,
		)
		if code == 0 {
			t.Fatalf("force=%t: gc sling %s exited 0, want a refusal\nstdout: %s", force, root.ID, stdout.String())
		}
		msg := stderr.String()
		for _, want := range []string{root.ID, "infrastructure store", "--formula"} {
			if !strings.Contains(msg, want) {
				t.Errorf("force=%t: refusal %q does not mention %q", force, msg, want)
			}
		}
		if strings.Contains(msg, "not found") || strings.Contains(msg, "use --force") {
			t.Errorf("force=%t: refusal %q reports the bead missing or points at --force; the binding holds it", force, msg)
		}
	}
	if got := slingJSONErrorCode(t, "worker", root.ID); got != "bead_not_slingable" {
		t.Errorf("gc sling --json %s: error code %q, want the terminal bead_not_slingable", root.ID, got)
	}

	got, err := graph.Get(root.ID)
	if err != nil {
		t.Fatalf("reading the root back: %v", err)
	}
	if routed := got.Metadata[beadmeta.RoutedToMetadataKey]; routed != "" {
		t.Errorf("the refused sling routed %s to %q", root.ID, routed)
	}
	all, err := graph.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatalf("listing the binding: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("the binding holds %d bead(s) after the refused slings, want only the root: %+v", len(all), all)
	}
	work, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		t.Fatalf("opening the work store: %v", err)
	}
	if left, err := work.List(beads.ListQuery{AllowScan: true, IncludeClosed: true}); err != nil {
		t.Fatalf("listing the work store: %v", err)
	} else if len(left) != 0 {
		t.Errorf("the refused slings wrote %d bead(s) to the work store: %+v", len(left), left)
	}
}

// TestSlingCoordinationBeadRefusalLeavesEveryOtherShapeAlone: the refusal is
// only for an id the binding holds and the store sling acts on does not. A
// work store may hold an id inside the class namespace (the prefix is only an
// advisory there), and such a bead still routes; an id no store holds keeps
// the missing-bead path; a city that relocates nothing has no binding to ask.
func TestSlingCoordinationBeadRefusalLeavesEveryOtherShapeAlone(t *testing.T) {
	cityPath := oneShotCLICity(t, filepath.Join(t.TempDir(), "store"))
	_, root := splitCityWorkflowRoot(t, cityPath)

	if refusal, probeErr := slingCoordinationBeadRefusal(cityPath, root.ID, beads.NewMemStore()); refusal == nil || probeErr != nil {
		t.Fatalf("a root only the binding holds: refusal=%v probe=%v, want a refusal", refusal, probeErr)
	}
	twin := beads.NewMemStoreFrom(0, []beads.Bead{{ID: root.ID, Title: "work-resident", Type: "task"}}, nil)
	if refusal, probeErr := slingCoordinationBeadRefusal(cityPath, root.ID, twin); refusal != nil || probeErr != nil {
		t.Errorf("a reserved-namespace id the work store holds: refusal=%v probe=%v, want it left to route", refusal, probeErr)
	}
	if refusal, probeErr := slingCoordinationBeadRefusal(cityPath, "gcg-424242", beads.NewMemStore()); refusal != nil || probeErr != nil {
		t.Errorf("an id no store holds: refusal=%v probe=%v, want it left to the missing-bead path", refusal, probeErr)
	}
	if refusal, probeErr := slingCoordinationBeadRefusal(cityPath, "gc-1", beads.NewMemStore()); refusal != nil || probeErr != nil {
		t.Errorf("a work-prefixed id: refusal=%v probe=%v, want it left alone", refusal, probeErr)
	}

	single := oneShotCLICity(t, "")
	if refusal, probeErr := slingCoordinationBeadRefusal(single, root.ID, beads.NewMemStore()); refusal != nil || probeErr != nil {
		t.Errorf("a city that relocates nothing: refusal=%v probe=%v for %s, want it left alone", refusal, probeErr, root.ID)
	}
}

// TestSlingCoordinationBeadRefusalReportsAnUnreadableBindingAsAProbeFailure:
// a binding that cannot be read (a busy SQLite store) says nothing about
// whether it holds the id, so when work does not hold it either the sling
// reports a read failure, which cmdSling maps to source_bead_probe_failed and
// a --json caller may retry, never the terminal bead_not_slingable refusal.
// When work holds the id the bead routes as before.
func TestSlingCoordinationBeadRefusalReportsAnUnreadableBindingAsAProbeFailure(t *testing.T) {
	cityPath := t.TempDir()
	routes := splitRoutes(faultingGetStore{beads.NewMemStore()})
	registerResidencyRoutes(cityPath, routes, func() beads.Store { return beads.NewMemStore() })
	t.Cleanup(func() { unregisterResidencyRoutes(cityPath, routes) })

	refusal, probeErr := slingCoordinationBeadRefusal(cityPath, "gcg-7", beads.NewMemStore())
	if refusal != nil {
		t.Errorf("an unreadable binding was reported as a refusal: %v", refusal)
	}
	if probeErr == nil || !strings.Contains(probeErr.Error(), "gcg-7") || !strings.Contains(probeErr.Error(), "could not be read") {
		t.Fatalf("probe failure = %v, want a read failure naming gcg-7", probeErr)
	}
	twin := beads.NewMemStoreFrom(0, []beads.Bead{{ID: "gcg-7", Title: "work-resident", Type: "task"}}, nil)
	if refusal, probeErr := slingCoordinationBeadRefusal(cityPath, "gcg-7", twin); refusal != nil || probeErr != nil {
		t.Errorf("an id the work store holds, beside an unreadable binding: refusal=%v probe=%v, want it left to route", refusal, probeErr)
	}
}

// TestCmdSlingReportsAnUnreadableBindingAsARetryableProbeFailure: the code a
// gc sling --json caller sees decides whether it retries. A binding that
// cannot be read, beside a work store that does not hold the id, is the
// retryable source_bead_probe_failed, never the terminal bead_not_slingable.
func TestCmdSlingReportsAnUnreadableBindingAsARetryableProbeFailure(t *testing.T) {
	cityPath := oneShotCLICity(t, filepath.Join(t.TempDir(), "store"))
	writeSplitSlingFormulaCity(t, cityPath)
	routes := splitRoutes(faultingGetStore{beads.NewMemStore()})
	registerResidencyRoutes(cityPath, routes, func() beads.Store { return beads.NewMemStore() })
	t.Cleanup(func() { unregisterResidencyRoutes(cityPath, routes) })

	if got := slingJSONErrorCode(t, "worker", "gcg-7"); got != "source_bead_probe_failed" {
		t.Errorf("gc sling --json gcg-7 over an unreadable binding: error code %q, want the retryable source_bead_probe_failed", got)
	}
}

// slingJSONErrorCode runs gc sling --json target id on the test's city and
// returns the code of the JSON error it prints, failing the test when the
// sling succeeds or prints none.
func slingJSONErrorCode(t *testing.T, target, id string) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := cmdSlingWithJSON(
		[]string{target, id},
		false, false, false, // isFormula, doNudge, force
		"", nil, "",
		false, false, false, "", // noConvoy, owned, reassign, onFormula
		false, false, false, // noFormula, fromStdin, dryRun
		"", "",
		true, // jsonOutput
		&stdout, &stderr,
	)
	if code == 0 {
		t.Fatalf("gc sling --json %s %s exited 0, want a refusal\nstdout: %s", target, id, stdout.String())
	}
	var out cliJSONErrorOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("gc sling --json %s %s printed no JSON error: %v\nstdout: %s\nstderr: %s", target, id, err, stdout.String(), stderr.String())
	}
	return out.Error.Code
}
