package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/session"
)

// The parity ledger for the legacy session pass (ga-zwddym6, from the audit
// ga-bw7yr7a): every step the legacy pass performs and v2 does not, and the
// tests that fail the build when a new one appears with no v2 counterpart.
//
// v2 either performs a step (an arm in rowArms), refuses it (a
// v2LatchRefusals entry in reconcile_latch.go) or records the difference: a
// parityAccepted, parityFindings or parityUnported entry in
// reconcile_session_differential_test.go, each of which demands a twin-world
// fixture, or a parityLegacyOnly row here, which has none. Three inventories
// keep that true:
//
//   - the parityLegacyOnly rows stay current (shape, legacy symbol still
//     declared, no v2 witness yet);
//   - every session field the legacy pass writes and no v2 site builds carries
//     a marker in the registry (internal/session/fields_legacy_only.go), whose
//     ledger and latch references this file checks;
//   - every event type a legacy-pass file emits has a v2 emitter or a cover.

// parityLegacyRow is one legacy-only step the differential fixtures cannot hold.
type parityLegacyRow struct {
	ID string // PAR-...
	// Legacy are the legacy symbols in cmd/gc, matched by declaration name
	// whatever the receiver. A symbol that is no longer declared means the
	// legacy step is gone and the row must go.
	Legacy []string
	// Witness are symbols whose appearance in a non-test v2 file (reconcile_*.go
	// or allocator*.go) proves a v2 counterpart exists. They are chosen so the
	// obvious fix trips them, so a fix PR deletes its row; that is the only way
	// its tests pass.
	Witness     []string
	V2Status    string // one line: what v2 does today
	Disposition string // "open" or "accepted"
	Owner       string // the bead that closes an open row
	Reason      string // an accepted row's reason, with its residual
}

const (
	legacyOnlyOpen     = "open"
	legacyOnlyAccepted = "accepted"
)

// parityLegacyOnly is every legacy-only step the differential fixtures cannot
// hold. An open row is a gap with an owner bead; its fix deletes the row
// because the fix trips the row's witness. An accepted row is a ruling with
// its residual stated. The witness scan reads identifiers in non-test v2
// files, so a fix that reimplements a step under new names does not trip it:
// the owner bead's review catches that, not this table.
var parityLegacyOnly = []parityLegacyRow{
	{
		ID: "PAR-DRAINDL", Legacy: []string{"retirePoolSlotAtDrainDeadline"},
		Witness:     []string{"poolSlotDrainRetireDeadline", "poolSlotDrainAgePastDeadline"},
		V2Status:    "no v2 arm or effect retires a pool seat at its drain deadline; ga-8epipf3 found the v2 sim already correct for the case it tried, by a path no arm owns",
		Disposition: legacyOnlyOpen, Owner: "ga-8epipf3",
	},
	{
		ID: "PAR-NAMEDGONE", Legacy: []string{"bumpSuspendDeferral", "retireRemovedConfiguredNamedSessionBead"},
		Witness:     []string{"namedSuspendConfirmTicks", "RetireNamedSessionPatch"},
		V2Status:    "no confirm window before draining a named session whose spec is absent, and no retire of a removed configured named session bead",
		Disposition: legacyOnlyOpen, Owner: "ga-48o9dqp",
	},
	{
		ID: "PAR-PINS", Legacy: []string{"newWarmClaimTriggerResolver"},
		Witness:     []string{"newWarmClaimTriggerResolver"},
		V2Status:    "v2 pool launch does not resolve trigger-bead opt_* pins from a rig store",
		Disposition: legacyOnlyOpen, Owner: "ga-ampl0ro",
	},
	{
		ID: "PAR-WARMNUDGE", Legacy: []string{"deliverWarmBindClaimNudge"},
		Witness:     []string{"deliverWarmBindClaimNudge", "markWarmBindBinding"},
		V2Status:    "v2 never delivers the warm-bind claim nudge, so a warm seat bound to a trigger bead is not told to claim it",
		Disposition: legacyOnlyOpen, Owner: "ga-ampl0ro",
	},
	{
		ID: "PAR-WAKEREF", Legacy: []string{"emitSessionWakeRefused"},
		Witness:     []string{"emitSessionWakeRefused", "SessionWakeRefused"},
		V2Status:    "v2 emits no session.wake_refused event; v2 writes wake_refused_event_at only as a cleared value, so the registry does not mark that key legacy-only and this row is its only record",
		Disposition: legacyOnlyOpen, Owner: "ga-h0zfmce",
	},
	{
		ID: "PAR-PROVHEALTH", Legacy: []string{"recordRedSkip", "recordGreenTick", "emitProviderHealthGateAlert"},
		Witness:     []string{"recordRedSkip", "recordGreenTick", "emitProviderHealthGateAlert", "ProviderHealthGateAlert"},
		V2Status:    "v2 keeps no provider-health episode bookkeeping and raises no provider_health_gate alert",
		Disposition: legacyOnlyOpen, Owner: "ga-73fsd2f",
	},
	{
		ID: "PAR-PREBOOT", Legacy: []string{"reapPreBootSessionBeads"},
		Witness:     []string{"reapPreBootSessionBeads"},
		V2Status:    "v2 relies on ServerConfirmedDead reaching ConfirmedDead and arm A9 to close a bead whose runtime died with the host",
		Disposition: legacyOnlyAccepted,
		Reason:      "arm A9 covers a runtime whose death the server confirms; residual: after a host reboot where server death is unconfirmable, an alias-holding orphan bead can persist, and the acp and subprocess runtimes were not traced",
	},
	{
		ID: "PAR-DRAINLOG", Legacy: []string{"sweepDrainSkips"},
		Witness:     []string{"sweepDrainSkips"},
		V2Status:    "v2 does not dedupe the drain-skip log line",
		Disposition: legacyOnlyAccepted,
		Reason:      "a log dedupe with no effect on session state; residual: a repeated log line",
	},
	{
		ID: "PAR-FORCEKILL", Legacy: []string{"escalateWedgedDrainAckStopPending"},
		Witness:     []string{"escalateWedgedDrainAckStopPending", "terminateDrainAckRuntimeByProcessTable", "queueDrainAckForcedTermination", "AsProcessTableScanner"},
		V2Status:    "v2 only alerts through escalateStop (session.drain_stop_escalated at 30 minutes, then every 15) and never kills the runtime",
		Disposition: legacyOnlyAccepted,
		Reason:      "ga-zwddym6 builds no force path; residual: a drain-acked seat whose runtime survives its stop stays parked holding its pool slot name until an operator acts, the one accepted row that can strand a pool seat; the open question on CONTRACT section 12.2 item 1 stays with the operator or pm",
	},
}

var (
	parIDShape   = regexp.MustCompile(`^PAR-[A-Z0-9]+$`)
	parOwnerBead = regexp.MustCompile(`^(mc|ga)-[a-z0-9]+(\.[0-9]+)*$`)
)

// legacyOnlyShapeFindings is the table's shape rules: a PAR id, unique; legacy
// symbols and witnesses named; a status; an open row owned by a bead and an
// accepted one carrying a reason.
func legacyOnlyShapeFindings(rows []parityLegacyRow) []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range rows {
		if !parIDShape.MatchString(r.ID) {
			out = append(out, fmt.Sprintf("%q: id must look like PAR-NAME", r.ID))
		}
		if seen[r.ID] {
			out = append(out, fmt.Sprintf("%s: duplicate id", r.ID))
		}
		seen[r.ID] = true
		if len(r.Legacy) == 0 || len(r.Witness) == 0 {
			out = append(out, fmt.Sprintf("%s: needs legacy symbols and v2 witness symbols", r.ID))
		}
		if strings.TrimSpace(r.V2Status) == "" {
			out = append(out, fmt.Sprintf("%s: no v2 status", r.ID))
		}
		switch r.Disposition {
		case legacyOnlyOpen:
			if !parOwnerBead.MatchString(r.Owner) {
				out = append(out, fmt.Sprintf("%s: open row's owner %q is not a bead id", r.ID, r.Owner))
			}
		case legacyOnlyAccepted:
			if strings.TrimSpace(r.Reason) == "" {
				out = append(out, fmt.Sprintf("%s: accepted row has no reason", r.ID))
			}
		default:
			out = append(out, fmt.Sprintf("%s: disposition %q is neither open nor accepted", r.ID, r.Disposition))
		}
	}
	return out
}

// legacyOnlyStaleFindings checks the two sides of every row against the tree:
// each legacy symbol is still declared in a non-test file, and no witness
// appears in a v2 file.
func legacyOnlyStaleFindings(rows []parityLegacyRow, files map[string]*ast.File) []string {
	declared := map[string]bool{}
	v2Idents := map[string]string{}
	for name, f := range files {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok {
				declared[fd.Name.Name] = true
			}
		}
		if !isV2SourceFile(name) {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok {
				v2Idents[id.Name] = name
			}
			return true
		})
	}
	var out []string
	for _, r := range rows {
		for _, s := range r.Legacy {
			if !declared[s] {
				out = append(out, fmt.Sprintf("%s: legacy symbol %s is no longer declared in a non-test cmd/gc file; the legacy step is gone, delete the row", r.ID, s))
			}
		}
		for _, w := range r.Witness {
			if file, ok := v2Idents[w]; ok {
				out = append(out, fmt.Sprintf("%s: witness %s appears in %s; v2 has a counterpart, delete the row", r.ID, w, file))
			}
		}
	}
	return out
}

// isV2SourceFile is whether a cmd/gc file belongs to the v2 reconciler:
// reconcile_*.go and allocator*.go.
func isV2SourceFile(name string) bool {
	return strings.HasPrefix(name, "reconcile_") || strings.HasPrefix(name, "allocator")
}

var (
	gcSourceOnce  sync.Once
	gcSourceFiles map[string]*ast.File
	gcSourceErr   error
)

// nonTestGCFiles parses every non-test Go file of cmd/gc, keyed by base name.
func nonTestGCFiles(t *testing.T) map[string]*ast.File {
	t.Helper()
	gcSourceOnce.Do(func() {
		_, currentFile, _, ok := runtime.Caller(0)
		if !ok {
			gcSourceErr = fmt.Errorf("runtime.Caller failed")
			return
		}
		dir := gcCallerDir(currentFile)
		entries, err := os.ReadDir(dir)
		if err != nil {
			gcSourceErr = err
			return
		}
		gcSourceFiles = map[string]*ast.File{}
		fset := token.NewFileSet()
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
			if err != nil {
				gcSourceErr = err
				return
			}
			gcSourceFiles[name] = f
		}
	})
	if gcSourceErr != nil {
		t.Fatalf("reading cmd/gc sources: %v", gcSourceErr)
	}
	return gcSourceFiles
}

func parseSource(t *testing.T, name, src string) *ast.File {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), name, src, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", name, err)
	}
	return f
}

// TestParityLegacyOnlyTable checks the ledger rows' shape and that neither
// side of any row is stale.
func TestParityLegacyOnlyTable(t *testing.T) {
	for _, f := range append(legacyOnlyShapeFindings(parityLegacyOnly), legacyOnlyStaleFindings(parityLegacyOnly, nonTestGCFiles(t))...) {
		t.Error(f)
	}
}

// TestParityLegacyOnlyTableCatches mutates the table the ways its rules exist
// to catch and checks each mutant fails with its reason.
func TestParityLegacyOnlyTableCatches(t *testing.T) {
	files := map[string]*ast.File{
		"legacy_pass.go":       parseSource(t, "legacy_pass.go", "package main\ntype d struct{}\nfunc (*d) legacyStep() {}\nfunc plainStep() {}\n"),
		"reconcile_v2_step.go": parseSource(t, "reconcile_v2_step.go", "package main\nfunc armV2() { witnessed() }\n"),
		"not_v2_witness.go":    parseSource(t, "not_v2_witness.go", "package main\nfunc other() { elsewhere() }\n"),
	}
	good := parityLegacyRow{ID: "PAR-X", Legacy: []string{"legacyStep"}, Witness: []string{"neverCalled"}, V2Status: "none", Disposition: legacyOnlyOpen, Owner: "ga-abc123"}
	mut := func(fn func(*parityLegacyRow)) []parityLegacyRow {
		r := good
		fn(&r)
		return []parityLegacyRow{r}
	}
	if got := append(legacyOnlyShapeFindings([]parityLegacyRow{good}), legacyOnlyStaleFindings([]parityLegacyRow{good}, files)...); len(got) != 0 {
		t.Fatalf("the good row fails: %v", got)
	}
	for _, tc := range []struct {
		name string
		rows []parityLegacyRow
		want string
	}{
		{"no PAR prefix", mut(func(r *parityLegacyRow) { r.ID = "DRAINDL" }), "id must look like PAR-NAME"},
		{"duplicate id", append([]parityLegacyRow{good}, good), "duplicate id"},
		{"no witness", mut(func(r *parityLegacyRow) { r.Witness = nil }), "needs legacy symbols and v2 witness symbols"},
		{"no status", mut(func(r *parityLegacyRow) { r.V2Status = " " }), "no v2 status"},
		{"malformed owner", mut(func(r *parityLegacyRow) { r.Owner = "the C5b follow-up PR" }), "is not a bead id"},
		{"accepted without a reason", mut(func(r *parityLegacyRow) { r.Disposition, r.Owner = legacyOnlyAccepted, "" }), "accepted row has no reason"},
		{"unknown disposition", mut(func(r *parityLegacyRow) { r.Disposition = "later" }), "neither open nor accepted"},
		{"legacy symbol gone", mut(func(r *parityLegacyRow) { r.Legacy = []string{"nonexistentStep"} }), "legacy symbol nonexistentStep is no longer declared"},
		{"witness in a v2 file", mut(func(r *parityLegacyRow) { r.Witness = []string{"witnessed"} }), "witness witnessed appears in reconcile_v2_step.go"},
	} {
		got := strings.Join(append(legacyOnlyShapeFindings(tc.rows), legacyOnlyStaleFindings(tc.rows, files)...), "\n")
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: mutant passed or failed otherwise; want %q in:\n%s", tc.name, tc.want, got)
		}
	}
	// A witness that only a non-v2 file uses is no counterpart; a method is
	// matched by its declaration name.
	row := parityLegacyRow{ID: "PAR-Y", Legacy: []string{"legacyStep", "plainStep"}, Witness: []string{"elsewhere"}, V2Status: "none", Disposition: legacyOnlyOpen, Owner: "mc-1"}
	if got := legacyOnlyStaleFindings([]parityLegacyRow{row}, files); len(got) != 0 {
		t.Errorf("a method symbol or a non-v2 witness use is reported stale: %v", got)
	}
}

// latchLiteral is the shape of a PAR id.
var (
	latchLiteral = regexp.MustCompile(`^PAR-[A-Z0-9]+(-[A-Z0-9]+)*$`)
)

// latchPARIDs is every PAR id a v2LatchRefusals entry names: a string literal
// of that shape in reconcile_latch.go.
func latchPARIDs(files map[string]*ast.File) map[string]bool {
	ids := map[string]bool{}
	f := files["reconcile_latch.go"]
	if f == nil {
		return ids
	}
	ast.Inspect(f, func(n ast.Node) bool {
		if bl, ok := n.(*ast.BasicLit); ok && bl.Kind == token.STRING {
			if s := strings.Trim(bl.Value, "\"`"); latchLiteral.MatchString(s) {
				ids[s] = true
			}
		}
		return true
	})
	return ids
}

// ledgerIDs is every id the ledger answers to: a parityLegacyOnly row's PAR id
// or a parityFindings name.
func ledgerIDs() map[string]bool {
	ids := map[string]bool{}
	for _, r := range parityLegacyOnly {
		ids[r.ID] = true
	}
	for name := range parityFindings {
		ids[name] = true
	}
	return ids
}

// coverFindings checks one marker's reference where the registry package
// cannot: a ledger reference names a ledger row, a latch reference names a
// PAR id v2LatchRefusals carries. Other kinds are checked by shape alone.
func coverFindings(subject, marker string, ledger, latch map[string]bool) []string {
	kind, ref, _ := strings.Cut(marker, ":")
	switch kind {
	case "ledger":
		if !ledger[ref] {
			return []string{fmt.Sprintf("%s: marker %q names no ledger row (a parityLegacyOnly id or a parityFindings entry)", subject, marker)}
		}
	case "latch":
		if !latch[ref] {
			return []string{fmt.Sprintf("%s: marker %q names no v2LatchRefusals PAR id", subject, marker)}
		}
	}
	return nil
}

// TestLegacyOnlyRegistryMarkersResolve checks every registry marker that
// refers to the ledger or the latch refers to something that exists.
func TestLegacyOnlyRegistryMarkersResolve(t *testing.T) {
	ledger, latch := ledgerIDs(), latchPARIDs(nonTestGCFiles(t))
	for key, marker := range session.LegacyOnlyMarkers() {
		for _, f := range coverFindings(key, marker, ledger, latch) {
			t.Error(f)
		}
	}
}

// TestCoverFindingsCatches is the negative test for the reference check.
func TestCoverFindingsCatches(t *testing.T) {
	ledger, latch := map[string]bool{"PAR-OK": true}, map[string]bool{"PAR-AGE": true}
	for _, tc := range []struct{ marker, want string }{
		{"ledger:PAR-GONE", "names no ledger row"},
		{"latch:PAR-GONE", "names no v2LatchRefusals PAR id"},
	} {
		if got := strings.Join(coverFindings("k", tc.marker, ledger, latch), ""); !strings.Contains(got, tc.want) {
			t.Errorf("%s: want %q, got %q", tc.marker, tc.want, got)
		}
	}
	for _, ok := range []string{"ledger:PAR-OK", "latch:PAR-AGE", "accepted:a reason"} {
		if got := coverFindings("k", ok, ledger, latch); len(got) != 0 {
			t.Errorf("%s reported: %v", ok, got)
		}
	}
}

// Event inventory. A legacy-pass file's event emission is an
// events.Event literal whose Type is an events.* constant; a constant only
// read in a filter is not an emission. The scan sees literals only: an
// emission that passes the type as a function argument or through a variable
// is not seen, which is why a legacy-pass file whose literal Type is not an
// events.* selector fails instead of passing silently.

const (
	fileLegacy  = "legacy-pass" // emitted only by the legacy session pass
	fileOutside = "outside"     // the CLI, API, orders, mail, storage and the other lanes; or shared with v2
)

// eventFileClass classifies every cmd/gc file that constructs an events.Event
// with a Type, other than the v2 files (reconcile_*.go, allocator*.go), which
// are v2 by name. A new emitting file nobody classified fails
// TestLegacyPassEventsHaveACounterpart.
//
// The scan reads literals only, and the legacy-pass class is by file: a helper
// a legacy-pass file shares with other callers is not reachability-analyzed, so
// the class is coarse. An event whose Type travels through a variable is seen
// only when its file is classified outside.
var eventFileClass = map[string]string{
	"api_state.go":                          fileOutside,
	"bd_inventory_trust.go":                 fileOutside,
	"bead_worktree_reaper.go":               fileOutside,
	"beads_blocked_repair.go":               fileOutside,
	"cache_notification.go":                 fileOutside,
	"city_runtime.go":                       fileOutside,
	"cityinit_impl.go":                      fileOutside,
	"claim_loop_guard.go":                   fileOutside,
	"claim_stall_guard.go":                  fileOutside,
	"class_store_emit.go":                   fileOutside,
	"cmd_convoy.go":                         fileOutside,
	"cmd_convoy_dispatch.go":                fileOutside,
	"cmd_event_emit.go":                     fileOutside,
	"cmd_handoff.go":                        fileOutside,
	"cmd_hook_claim.go":                     fileOutside,
	"cmd_mail.go":                           fileOutside,
	"cmd_runtime_drain.go":                  fileOutside,
	"cmd_supervisor.go":                     fileOutside,
	"cmd_suspend.go":                        fileOutside,
	"config_drift_episode.go":               fileOutside,
	"control_dispatcher_scope_gap_event.go": fileOutside,
	"controller.go":                         fileOutside,
	"convergence_store.go":                  fileOutside,
	"cross_store_gate_lane.go":              fileOutside,
	"dead_assignee_event.go":                fileOutside,
	"demand_divergence.go":                  fileOutside,
	"doctor_bd_inventory.go":                fileOutside,
	"dolt_project_id.go":                    fileOutside,
	"drain_ack_escalation.go":               fileLegacy,
	"drain_fence_unavailable.go":            fileOutside,
	"endpoint_capacity.go":                  fileOutside,
	"execution_backstop.go":                 fileOutside,
	"fs_pressure.go":                        fileOutside,
	"hook_session_claim.go":                 fileOutside,
	"molecule_autoclose.go":                 fileOutside,
	"order_dispatch.go":                     fileOutside,
	"order_outcome.go":                      fileOutside,
	"pending_interaction_lane.go":           fileOutside,
	"program_status_shadow.go":              fileOutside,
	"provider_health_gate.go":               fileLegacy,
	"provider_swap.go":                      fileOutside,
	"rig_import.go":                         fileOutside,
	"rig_import_census.go":                  fileOutside,
	"runtime_ownerless_reap.go":             fileOutside,
	"session_lifecycle_parallel.go":         fileOutside,
	"session_list_events.go":                fileOutside,
	"session_pool_drain_deadline.go":        fileLegacy,
	"session_reconciler.go":                 fileLegacy,
	"storage_boot.go":                       fileOutside,
	"store_rollout.go":                      fileOutside,
	"work_ladder_lane.go":                   fileOutside,
	"work_query_failure.go":                 fileOutside,
	"work_release.go":                       fileOutside,
}

// legacyEventCover maps an events.* constant a legacy-pass file emits and no
// v2 file does to what covers it: "ledger:<id>", "latch:<PAR id>",
// "accepted:<reason>" or "ruling-pending:<bead>: <note>", as the registry's
// markers read.
var legacyEventCover = map[string]string{
	"SessionMaxAgeKilled":                   "latch:PAR-AGE",
	"SessionUnknownState":                   "ledger:unknown-state-diagnostic",
	"SessionWakeRefused":                    "ledger:PAR-WAKEREF",
	"SessionPoolSlotRetiredAtDrainDeadline": "ledger:PAR-DRAINDL",
	"ProviderHealthGateAlert":               "ledger:PAR-PROVHEALTH",
	"SessionResetStalled":                   "ruling-pending:ga-a8dp0uk8: awaiting the architect's per-item ruling on session.reset_stalled",
}

// eventEmission is what one file's literals say.
type eventEmission struct {
	types      map[string]bool // events.* constants
	unresolved int             // literals whose Type is not an events.* selector
}

// isEventsEvent reports whether e names events.Event.
func isEventsEvent(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Event" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "events"
}

// scanEventEmissions finds, per file, the Type of every events.Event literal,
// the elided elements of a []events.Event literal included.
func scanEventEmissions(files map[string]*ast.File) map[string]*eventEmission {
	out := map[string]*eventEmission{}
	for name, f := range files {
		record := func(cl *ast.CompositeLit) {
			for _, e := range cl.Elts {
				kv, ok := e.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if k, ok := kv.Key.(*ast.Ident); !ok || k.Name != "Type" {
					continue
				}
				em := out[name]
				if em == nil {
					em = &eventEmission{types: map[string]bool{}}
					out[name] = em
				}
				if v, ok := kv.Value.(*ast.SelectorExpr); ok {
					if pkg, ok := v.X.(*ast.Ident); ok && pkg.Name == "events" {
						em.types[v.Sel.Name] = true
						continue
					}
				}
				em.unresolved++
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			cl, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			switch t := cl.Type.(type) {
			case *ast.SelectorExpr:
				if isEventsEvent(t) {
					record(cl)
				}
			case *ast.ArrayType:
				if isEventsEvent(t.Elt) {
					for _, e := range cl.Elts {
						if el, ok := e.(*ast.CompositeLit); ok && el.Type == nil {
							record(el)
						}
					}
				}
			}
			return true
		})
	}
	return out
}

// eventInventoryFindings is the event rules: every emitting file classified
// once (v2 files by name); a legacy-pass file's literals all resolve; every
// type a legacy-pass file emits has a v2 emitter or a cover, and a cover goes
// when the type gains a v2 emitter or loses its legacy one.
func eventInventoryFindings(emissions map[string]*eventEmission, class, cover map[string]string, ledger, latch map[string]bool) []string {
	var out []string
	names := make([]string, 0, len(emissions))
	for name := range emissions {
		names = append(names, name)
	}
	sort.Strings(names)
	v2Types, legacyTypes := map[string]bool{}, map[string]string{}
	for _, name := range names {
		em := emissions[name]
		c, classified := class[name]
		switch {
		case isV2SourceFile(name):
			if classified {
				out = append(out, fmt.Sprintf("%s: a v2 file is classified by its name, not in eventFileClass", name))
			}
			for t := range em.types {
				v2Types[t] = true
			}
			continue
		case !classified:
			out = append(out, fmt.Sprintf("%s: emits events but is in no class; add it to eventFileClass as %s or %s", name, fileLegacy, fileOutside))
			continue
		case c != fileLegacy && c != fileOutside:
			out = append(out, fmt.Sprintf("%s: class %q is neither %s nor %s", name, c, fileLegacy, fileOutside))
			continue
		}
		if c != fileLegacy {
			continue
		}
		if em.unresolved > 0 {
			out = append(out, fmt.Sprintf("%s: %d events.Event literal(s) whose Type is not an events.* constant; the scan cannot name what this legacy-pass file emits", name, em.unresolved))
		}
		for t := range em.types {
			legacyTypes[t] = name
		}
	}
	for name := range class {
		if emissions[name] == nil {
			out = append(out, fmt.Sprintf("%s: classified in eventFileClass but constructs no events.Event with a Type; delete the entry", name))
		}
	}
	covered := make([]string, 0, len(cover))
	for t := range cover {
		covered = append(covered, t)
	}
	sort.Strings(covered)
	for _, t := range covered {
		switch file, legacyEmits := legacyTypes[t]; {
		case !legacyEmits:
			out = append(out, fmt.Sprintf("events.%s: has a cover but no legacy-pass file emits it; delete the cover", t))
		case v2Types[t]:
			out = append(out, fmt.Sprintf("events.%s: covered, but a v2 file now emits it too (legacy: %s); delete the cover", t, file))
		default:
			out = append(out, coverFindings("events."+t, cover[t], ledger, latch)...)
			if err := validMarker(cover[t]); err != "" {
				out = append(out, fmt.Sprintf("events.%s: %s", t, err))
			}
		}
	}
	legacyList := make([]string, 0, len(legacyTypes))
	for t := range legacyTypes {
		legacyList = append(legacyList, t)
	}
	sort.Strings(legacyList)
	for _, t := range legacyList {
		if !v2Types[t] && cover[t] == "" {
			out = append(out, fmt.Sprintf("events.%s: emitted by legacy-pass file %s, no v2 file emits it and no ledger row covers it; give it a v2 emitter or a legacyEventCover entry", t, legacyTypes[t]))
		}
	}
	return out
}

// validMarker is the shape rule for the marker kinds the registry shares.
func validMarker(m string) string {
	kind, rest, _ := strings.Cut(m, ":")
	switch kind {
	case "ledger", "latch":
		if strings.TrimSpace(rest) == "" {
			return fmt.Sprintf("marker %q names nothing", m)
		}
	case "accepted":
		if strings.TrimSpace(rest) == "" {
			return fmt.Sprintf("marker %q has no reason", m)
		}
	case "ruling-pending":
		bead, note, _ := strings.Cut(rest, ": ")
		if !parOwnerBead.MatchString(strings.TrimSpace(bead)) || strings.TrimSpace(note) == "" {
			return fmt.Sprintf("marker %q needs a bead id and a note", m)
		}
	default:
		return fmt.Sprintf("marker %q has no known kind", m)
	}
	return ""
}

// TestLegacyPassEventsHaveACounterpart is inventory test (b): every event type
// the legacy session pass emits is emitted by v2 or covered by the ledger.
func TestLegacyPassEventsHaveACounterpart(t *testing.T) {
	for _, f := range eventInventoryFindings(scanEventEmissions(nonTestGCFiles(t)), eventFileClass, legacyEventCover, ledgerIDs(), latchPARIDs(nonTestGCFiles(t))) {
		t.Error(f)
	}
}

// TestEventInventoryCatches is the negative test: each mutant of a synthetic
// tree fails with its reason.
func TestEventInventoryCatches(t *testing.T) {
	src := func(typ string) string {
		return "package main\nimport \"x/events\"\nfunc f() { _ = events.Event{Type: " + typ + "} }\n"
	}
	tree := func(extra map[string]string) map[string]*eventEmission {
		files := map[string]*ast.File{
			"legacy_pass.go":   parseSource(t, "legacy_pass.go", src("events.OnlyLegacy")),
			"shared.go":        parseSource(t, "shared.go", src("events.Shared")),
			"reconcile_arm.go": parseSource(t, "reconcile_arm.go", src("events.Shared")),
		}
		for name, s := range extra {
			files[name] = parseSource(t, name, s)
		}
		return scanEventEmissions(files)
	}
	class := func(extra map[string]string) map[string]string {
		c := map[string]string{"legacy_pass.go": fileLegacy, "shared.go": fileOutside}
		for k, v := range extra {
			c[k] = v
		}
		return c
	}
	ledger, latch := map[string]bool{"PAR-OK": true}, map[string]bool{"PAR-AGE": true}
	cover := func(m string) map[string]string { return map[string]string{"OnlyLegacy": m} }
	find := func(em map[string]*eventEmission, c, cv map[string]string) string {
		return strings.Join(eventInventoryFindings(em, c, cv, ledger, latch), "\n")
	}
	if got := find(tree(nil), class(nil), cover("ledger:PAR-OK")); got != "" {
		t.Fatalf("the good tree fails:\n%s", got)
	}
	for _, tc := range []struct {
		name, got, want string
	}{
		{"unclassified emitting file", find(tree(map[string]string{"new_emitter.go": src("events.Brand")}), class(nil), cover("ledger:PAR-OK")), "new_emitter.go: emits events but is in no class"},
		{"legacy type with no v2 emitter and no cover", find(tree(nil), class(nil), nil), "events.OnlyLegacy: emitted by legacy-pass file legacy_pass.go"},
		{"cover names no ledger row", find(tree(nil), class(nil), cover("ledger:PAR-GONE")), "names no ledger row"},
		{"cover of malformed shape", find(tree(nil), class(nil), cover("ruling-pending:later")), "needs a bead id and a note"},
		{"stale cover: v2 now emits it", find(tree(map[string]string{"reconcile_new.go": src("events.OnlyLegacy")}), class(nil), cover("ledger:PAR-OK")), "a v2 file now emits it too"},
		{"stale cover: legacy no longer emits it", find(tree(nil), class(nil), map[string]string{"OnlyLegacy": "ledger:PAR-OK", "Gone": "ledger:PAR-OK"}), "events.Gone: has a cover but no legacy-pass file emits it"},
		{"unresolvable legacy literal", find(tree(map[string]string{"legacy_var.go": src("kind")}), class(map[string]string{"legacy_var.go": fileLegacy}), cover("ledger:PAR-OK")), "legacy_var.go: 1 events.Event literal(s) whose Type is not an events.* constant"},
		{"classified file that emits nothing", find(tree(nil), class(map[string]string{"quiet.go": fileOutside}), cover("ledger:PAR-OK")), "quiet.go: classified in eventFileClass but constructs no events.Event"},
		{"v2 file in the table", find(tree(nil), class(map[string]string{"reconcile_arm.go": fileOutside}), cover("ledger:PAR-OK")), "a v2 file is classified by its name"},
	} {
		if !strings.Contains(tc.got, tc.want) {
			t.Errorf("%s: mutant passed or failed otherwise; want %q in:\n%s", tc.name, tc.want, tc.got)
		}
	}
	// A constant only read in a filter is not an emission.
	read := scanEventEmissions(map[string]*ast.File{"filter.go": parseSource(t, "filter.go", "package main\nimport \"x/events\"\nfunc f() { _ = events.Filter{Type: events.OnlyLegacy} }\n")})
	if len(read) != 0 {
		t.Errorf("a filter's Type is scanned as an emission: %v", read)
	}
}

// TestScanEventEmissionsReadsLiteralShapes pins what the scan sees: a plain
// literal, the elided elements of a slice, a pointer-free nested one, and a
// Type that is no events.* selector, which it counts as unresolved.
func TestScanEventEmissionsReadsLiteralShapes(t *testing.T) {
	const src = `package main
import "events"
func f(x string) {
	_ = events.Event{Type: events.SessionWoke}
	_ = []events.Event{{Type: events.SessionStranded}, {Type: events.SessionUpdated, Actor: "gc"}}
	_ = events.Event{Type: x}
	_ = struct{ Type string }{Type: events.NotAnEvent}
}
`
	f, err := parser.ParseFile(token.NewFileSet(), "shapes.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	em := scanEventEmissions(map[string]*ast.File{"shapes.go": f})["shapes.go"]
	if em == nil {
		t.Fatal("the scan saw no events in shapes.go")
	}
	for _, want := range []string{"SessionWoke", "SessionStranded", "SessionUpdated"} {
		if !em.types[want] {
			t.Errorf("events.%s not found; saw %v", want, em.types)
		}
	}
	if em.types["NotAnEvent"] || len(em.types) != 3 {
		t.Errorf("a non-events.Event literal counted: %v", em.types)
	}
	if em.unresolved != 1 {
		t.Errorf("unresolved = %d, want 1 (Type: x)", em.unresolved)
	}
}
