package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// Tier-1 of the three-tier "stores return domain objects" enforcement
// (engdocs/plans/store-domain-objects/spec.md §6). A beads.Bead is the
// SERIALIZED storage form; a domain object (session.Info, mail.Message,
// orders.OrderRun, nudgequeue.NudgeShadow, session.WaitInfo) is the type-safe
// form. De/serialization must happen only at the store edge — so a typed-class
// codec (…FromBead / a raw-bead list export) must never be CALLED in the
// interior (cmd/gc, internal/api, internal/worker, internal/dispatch).
//
// This test is a ratchet, not a hard zero: it pins a per-file census of codec
// call-site counts. Any INCREASE (or a new interior file) fails — a typed-class
// bead is being cracked in business logic; route it through that class's front
// door instead. Any DECREASE also fails — with instructions to ratchet the
// census down in the same PR, so progress is recorded and can never silently
// regress. When a sanctioned edge PR folds in a codec (O5/O6/O8 landing their
// edge work), the failure log prints a regenerated census literal to paste over
// the baseline — an explicit, review-visible ratchet-up, exactly like editing
// frontDoorStoreFreeFiles.
//
// Counting is raw-substring over file content (comments included) — the same
// semantics the sibling guards in frontdoor_di_guard_test.go already use; a
// comment that names a needle trips the guard, by design.
//
// SCAN SCOPE deliberately omits the EDGE packages where the codecs LIVE
// (internal/beads, internal/coordclass, internal/session, internal/orders,
// internal/nudgequeue, internal/mail, internal/extmsg, internal/convoy) — they
// are not under any scan dir — plus the cmd/gc wiring files in
// typedClassCodecEdgeFiles.
//
// EXEMPTION CENSUS (spec §5): class-generic machinery legitimately HOLDS
// typed-class beads.Bead forever and is NOT expected to be bead-free — the
// work/graph business logic (internal/dispatch, sling/convoy: Bead is their
// domain object), the generic event wire (internal/api/event_payloads.go
// BeadEventPayload), the policy-store class router (cmd/gc/bead_policy_store.go,
// which must see raw beads to Classify them), the by-id federation lanes
// (cmd/gc/cmd_beads.go collectBeadsAcrossStores + gc bead show,
// cmd/gc/cmd_convoy_dispatch.go:findBeadAcrossStores), and the doctor_*.go
// diagnostic lanes. Those files STAY IN THIS SCAN because Tier-1 counts CODEC
// CALLS, not beads.Bead occurrences, and Tier-3 (unexporting a codec) requires a
// TRUE zero — e.g. cmd/gc/doctor_session_model.go really does call
// session.ListAllSessionBeads and must migrate before that codec can be
// unexported. Their exemption covers holding raw beads (a Tier-2 concern), not
// calling codecs.
//
// CANDIDATE NEEDLE EXPANSIONS (not in WI-0; fold in with WI-5/WI-6): the session
// package exports more raw-bead surfaces not yet policed here — the
// named_config.go family (Find*NamedSessionBead / NamedSessionResolution*),
// ResolveSessionBeadByExactID, ExactMetadataSessionCandidates*, and
// Manager.GetWithBead / GetWithPersistedResponse. Adding a needle later is a
// one-line change plus a baseline regen.

// codecNeedle is one exact substring counted per interior file, with the class
// it belongs to and a pointer to that class's front door for failure messages.
type codecNeedle struct {
	class     string
	needle    string
	frontDoor string
}

// typedClassCodecNeedles is the policed set. Some start at zero interior hits
// (their codec exists only in the edge, or arrives with a fold-in PR) and act as
// tripwires; the exact-compare handles a missing census entry natively.
var typedClassCodecNeedles = []codecNeedle{
	// InfoFromPersistedBead( is a PERMANENT-ZERO tripwire: the exported name was
	// UNEXPORTED to infoFromPersistedBead in the W-test-fixture endgame, so it can
	// never re-appear in the interior (the symbol no longer exists — a call would not
	// compile). The lowercase sibling below polices the interior against a
	// locally-redefined / re-leaked codec of the new name.
	{"sessions", "InfoFromPersistedBead(", "session.Store.Get/List (internal/session/info_store.go; exported name UNEXPORTED — permanent-zero tripwire)"},
	{"sessions", "infoFromPersistedBead(", "session.Store.Get/List (internal/session/info_store.go; the unexported codec — interior must route through session.Store, never call it directly)"},
	{"sessions", "SessionInfoFromBead(", "session.Store.Get (internal/session/info_store.go)"},
	{"sessions", "WaitInfoFromBead(", "session.Store.GetWait/ListWaits (internal/session)"},
	{"sessions", "ListAllSessionBeads(", "session.Store.ListAll (internal/session)"},
	{"sessions", "ListSessionWaitBeads(", "session.Store.ListWaits (internal/session)"},
	{"sessions", "PersistedResponseFromBead(", "session.Store.GetPersistedResponse (internal/session/persisted_response.go)"},
	{"sessions", "ListFullFromBeads(", "session.Store.ListAll + Manager.ListFromInfos (internal/session)"},
	// GetWithPersistedResponse( needle RETIRED in WI-7 W-unexport: the
	// raw-cracking Manager.GetWithPersistedResponse was retired long ago, and the
	// surviving same-named worker method (a clean Store.GetPersistedResponse +
	// EnrichInfo composition, NOT a codec crack) was DELETED as dead code — its
	// only reader was gone, and the canonical worker read is
	// worker.sessionRecordViaManager. No interior GetWithPersistedResponse(
	// call site remains, so the tripwire has nothing left to police.
	{"sessions", "GetBeadWithInfo(", "session.Store.GetPersistedResponse (internal/session; the transitional raw+Info single-fetch escape, retired + deleted in WI-6 R4 — all-zero tripwire)"},
	{"sessions", "GetWithBead(", "session.Store.GetPersistedResponse / worker.Factory.SessionByHandle (internal/session, internal/worker; retired in WI-6 W3 — all-zero tripwire)"},
	{"sessions", "SessionByLoadedBead(", "worker.Factory.SessionByRecord (internal/worker; retired in WI-6 W3 — all-zero tripwire)"},
	{"sessions", "ResolveSessionBeadByExactID(", "session.ResolveSessionRecordByExactID (internal/session; worker-boundary use retired in WI-6 W3, the reconciler existence-probe use retired in WI-6 W6 — all-zero tripwire)"},
	{"sessions", "PollerKeyFromBead(", "session.Store poller-key accessor (internal/session/poller_key.go)"},
	{"orders", "RunFromTrackingBead(", "orders.Store.Get/RecentRuns (internal/orders)"},
	{"orders", "MaxSeqFromLabels(", "orders.Store.Cursor (internal/orders)"},
	{"nudges", "DecodeShadow(", "nudgequeue.Store.Find/StaleShadowsBefore (internal/nudgequeue)"},
	{"nudges", ".FindBead(", "nudgequeue.Store.Find (internal/nudgequeue)"},
	{"nudges", ".FindBeadIncludingTerminal(", "nudgequeue.Store.FindIncludingTerminal (internal/nudgequeue)"},
	{"nudges", "StaleCandidatesBefore(", "nudgequeue.Store.StaleShadowsBefore (internal/nudgequeue)"},
	{"messaging", ".ReadMessagesBefore(", "beadmail.SweepReadMessagesBefore/CountReadMessagesBefore (internal/mail/beadmail)"},
	{"messaging", "ReadMessageWispEntries(", "beadmail.PurgeReadMessageWisps (internal/mail/beadmail)"},
}

// typedClassCodecScanDirs are the interior trees walked for codec call sites.
// The edge packages are deliberately absent (the codecs live there).
var typedClassCodecScanDirs = []string{
	"cmd/gc",
	"internal/api",
	"internal/worker",
	"internal/dispatch",
}

// typedClassCodecEdgeFiles are cmd/gc wiring/adapter files excluded from the
// scan because calling a codec is their legitimate job (composition roots /
// per-class front-door constructors). nudge_beads.go used to be listed here,
// but the nudges class closeout (WI-1) left it needle-free wiring — the nudge
// store-open seam, the per-call front-door constructor, and the flock-callable
// write adapters, none of which decode a bead — so it is no longer excluded and
// is now policed like any interior file: a needle reintroduced there fails the
// build.
var typedClassCodecEdgeFiles = map[string]bool{
	"cmd/gc/class_store.go":       true,
	"cmd/gc/cli_session_store.go": true,
	"cmd/gc/providers.go":         true,
	// internal/api/client_waits.go is the /v0/waits wire-serialization edge: its
	// legacy rungs (ListWaitsViaBeads / GetWaitViaBead) project raw beads via
	// WaitInfoFromBead during the rolling-deploy deprecation window. Excluded so
	// that codec call keeps the interior at zero; removed with the legacy rungs
	// when the window closes.
	"internal/api/client_waits.go": true,
}

// typedClassCodecCensus is the checked-in baseline: needle -> slash-normalized
// repo-relative path -> occurrence count. Zero-hit needles and zero-hit files
// carry no entry. Regenerate by running this test with the map empty and pasting
// the emitted literal.
var typedClassCodecCensus = map[string]map[string]int{
	// InfoFromPersistedBead( / infoFromPersistedBead( are both PERMANENT INTERIOR ZEROs
	// across all four scan dirs (no census entry). The interior was zeroed during the
	// domain-object migration; the W-test-fixture endgame then migrated every EXTERNAL
	// _test.go caller (cmd/gc, internal/api, internal/worker) onto real store test
	// doubles (internal/session/sessiontest: SeedBead / Info / Store; the package-main
	// seedSessionInfo type-stamp seeder for makeBead corpora; session.Info struct
	// literals for degraded/empty-id fixtures) and UNEXPORTED the codec
	// (InfoFromPersistedBead → infoFromPersistedBead). The exported name no longer
	// exists, so its needle is a can-never-fire tripwire; the lowercase needle guards
	// against a re-leaked / locally-redefined codec. internal/session's own white-box
	// tests renamed WITH the codec and are not in any scan dir.
	"ListAllSessionBeads(": {
		// WI-7 W-delete zeroed session_bead_snapshot (1→0, the raw-half load edge flipped
		// to ListAllForReconcile) and doctor_session_model (1→0, doctor issues its own two
		// raw store.List legs inline rather than calling the policed helper — its §5
		// exemption covers HOLDING raw beads, not calling the codec). session_beads STAYS
		// at 1 — the honest endgame floor: loadSessionBeads feeds the still-raw sync
		// internals + internal/mail/beadmail's same-module compile dependency. Full sync
		// typing is a separate out-of-budget W-sync wave (see tickfeed-design §3
		// W-unexport).
		"cmd/gc/session_beads.go": 1,
	},
	// ResolveSessionBeadByExactID( is now all-zero in the interior: the
	// worker-boundary resolve+construct site moved to ResolveSessionRecordByExactID
	// + SessionByRecord in WI-6 W3, and the reconciler's attached-config-drift
	// EXISTENCE probe (session_reconciler.go, which discards the record and reads
	// only the error) moved onto the same typed twin in WI-6 W6. The raw codec now
	// has no interior consumer, so it is a pure tripwire.
	//
	// PollerKeyFromBead( is now all-zero in the interior too: WI-6 R5 moved
	// cmd_wait's waitNudgePollerKey onto PollerKeyFromInfo (fed by FindInfoByID), so
	// the last interior caller is gone. The needle stays policed as a tripwire until
	// the WI-7 unexport (pollerKeyFromBead) lands.
	"RunFromTrackingBead(": {
		"internal/api/huma_handlers_orders.go": 1,
	},
	"MaxSeqFromLabels(": {
		"cmd/gc/cmd_order.go":                  1,
		"internal/api/huma_handlers_orders.go": 1,
	},
}

// scanCodecCensusAt walks scanDirs under root and counts each needle per
// non-test .go file, skipping testdata/node_modules dirs and the edgeFiles set.
// It is pure with respect to root, which lets the synthetic self-test drive it
// against a temp tree. filesPerDir reports how many files each scan dir
// contributed (a dir that scans zero files signals a rename).
func scanCodecCensusAt(root string, scanDirs []string, edgeFiles map[string]bool, needles []codecNeedle) (census map[string]map[string]int, filesPerDir map[string]int, err error) {
	census = map[string]map[string]int{}
	filesPerDir = map[string]int{}
	for _, dir := range scanDirs {
		abs := filepath.Join(root, filepath.FromSlash(dir))
		walkErr := filepath.WalkDir(abs, func(path string, d fs.DirEntry, e error) error {
			if e != nil {
				return e
			}
			if d.IsDir() {
				if name := d.Name(); name == "testdata" || name == "node_modules" {
					return filepath.SkipDir
				}
				return nil
			}
			name := d.Name()
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				return nil
			}
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return relErr
			}
			relSlash := filepath.ToSlash(rel)
			if edgeFiles[relSlash] {
				return nil
			}
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			filesPerDir[dir]++
			content := string(data)
			for _, n := range needles {
				if c := strings.Count(content, n.needle); c > 0 {
					if census[n.needle] == nil {
						census[n.needle] = map[string]int{}
					}
					census[n.needle][relSlash] = c
				}
			}
			return nil
		})
		if walkErr != nil {
			return nil, nil, walkErr
		}
	}
	return census, filesPerDir, nil
}

// diffCodecCensus compares got against the baseline want and returns ordered,
// human-actionable failure strings (empty when they match exactly).
func diffCodecCensus(needles []codecNeedle, got, want map[string]map[string]int) []string {
	var findings []string
	for _, n := range needles {
		g := got[n.needle]
		w := want[n.needle]
		files := map[string]bool{}
		for f := range g {
			files[f] = true
		}
		for f := range w {
			files[f] = true
		}
		sorted := make([]string, 0, len(files))
		for f := range files {
			sorted = append(sorted, f)
		}
		sort.Strings(sorted)
		for _, f := range sorted {
			gc, wc := g[f], w[f]
			switch {
			case gc > wc:
				findings = append(findings, fmt.Sprintf(
					"%s: %d×%q (baseline %d) — a typed-class bead is being cracked/read raw in the interior; route it through the %s class front door (%s). If this is a sanctioned edge fold-in, ratchet typedClassCodecCensus UP in this same PR so review sees the debt.",
					f, gc, n.needle, wc, n.class, n.frontDoor))
			case gc < wc:
				findings = append(findings, fmt.Sprintf(
					"%s: %d×%q (baseline %d) — progress: ratchet typedClassCodecCensus DOWN in this PR so the win is recorded and cannot silently regress.",
					f, gc, n.needle, wc))
			}
		}
	}
	return findings
}

// formatCodecCensusLiteral renders got as a gofmt-shaped Go map literal for
// pasting over typedClassCodecCensus.
func formatCodecCensusLiteral(needles []codecNeedle, got map[string]map[string]int) string {
	var b strings.Builder
	b.WriteString("var typedClassCodecCensus = map[string]map[string]int{\n")
	for _, n := range needles {
		files := got[n.needle]
		if len(files) == 0 {
			continue
		}
		b.WriteString(fmt.Sprintf("\t%q: {\n", n.needle))
		keys := make([]string, 0, len(files))
		for f := range files {
			keys = append(keys, f)
		}
		sort.Strings(keys)
		for _, f := range keys {
			b.WriteString(fmt.Sprintf("\t\t%q: %d,\n", f, files[f]))
		}
		b.WriteString("\t},\n")
	}
	b.WriteString("}\n")
	return b.String()
}

func TestTypedClassCodecCensusRatchet(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(currentFile))) // cmd/gc -> cmd -> repo root

	// Typo protection: every census key must be a policed needle.
	needleSet := map[string]bool{}
	for _, n := range typedClassCodecNeedles {
		needleSet[n.needle] = true
	}
	for needle := range typedClassCodecCensus {
		if !needleSet[needle] {
			t.Errorf("typedClassCodecCensus has entry for unknown needle %q — add it to typedClassCodecNeedles or remove the entry", needle)
		}
	}

	got, filesPerDir, err := scanCodecCensusAt(repoRoot, typedClassCodecScanDirs, typedClassCodecEdgeFiles, typedClassCodecNeedles)
	if err != nil {
		t.Fatalf("scanning codec census: %v", err)
	}
	for _, dir := range typedClassCodecScanDirs {
		if filesPerDir[dir] == 0 {
			t.Fatalf("scan dir %q contributed zero files — was it renamed or moved?", dir)
		}
	}

	findings := diffCodecCensus(typedClassCodecNeedles, got, typedClassCodecCensus)
	if len(findings) > 0 {
		for _, f := range findings {
			t.Error(f)
		}
		t.Logf("regenerated census (paste over typedClassCodecCensus):\n%s", formatCodecCensusLiteral(typedClassCodecNeedles, got))
	}
}

func TestTypedClassCodecCensusDiffMechanics(t *testing.T) {
	needles := []codecNeedle{{"sessions", "InfoFromPersistedBead(", "fd"}}
	cases := []struct {
		name         string
		got, want    map[string]map[string]int
		wantFindings int
	}{
		{"equal", m("a.go", 2), m("a.go", 2), 0},
		{"increase", m("a.go", 3), m("a.go", 2), 1},
		{"new-file", m("b.go", 1), map[string]map[string]int{}, 1},
		{"decrease", m("a.go", 1), m("a.go", 2), 1},
		{"file-vanished", map[string]map[string]int{}, m("a.go", 2), 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := len(diffCodecCensus(needles, tc.got, tc.want)); got != tc.wantFindings {
				t.Errorf("findings = %d, want %d", got, tc.wantFindings)
			}
		})
	}
}

// m is a one-file/one-needle census literal for the diff-mechanics table.
func m(file string, n int) map[string]map[string]int {
	return map[string]map[string]int{"InfoFromPersistedBead(": {file: n}}
}

func TestTypedClassCodecScannerCountsSyntheticNeedle(t *testing.T) {
	root := t.TempDir()
	mkfile := func(rel, body string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	needle := "session.InfoFromPersistedBead(b)"
	mkfile("cmd/gc/interior.go", "package main\nvar _ = "+needle+"\n")      // counted
	mkfile("cmd/gc/interior_test.go", "package main\nvar _ = "+needle+"\n") // skipped: _test.go
	mkfile("cmd/gc/testdata/fixture.go", "package x\nvar _ = "+needle+"\n") // skipped: testdata
	mkfile("cmd/gc/class_store.go", "package main\nvar _ = "+needle+"\n")   // skipped: edge file

	needles := []codecNeedle{{"sessions", "InfoFromPersistedBead(", "fd"}}
	census, filesPerDir, err := scanCodecCensusAt(root, []string{"cmd/gc"}, typedClassCodecEdgeFiles, needles)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if filesPerDir["cmd/gc"] != 1 {
		t.Errorf("scanned files in cmd/gc = %d, want 1 (only interior.go)", filesPerDir["cmd/gc"])
	}
	if got := census["InfoFromPersistedBead("]["cmd/gc/interior.go"]; got != 1 {
		t.Errorf("interior.go count = %d, want 1", got)
	}
	if _, seen := census["InfoFromPersistedBead("]["cmd/gc/class_store.go"]; seen {
		t.Error("class_store.go (edge file) must not be scanned")
	}
}
