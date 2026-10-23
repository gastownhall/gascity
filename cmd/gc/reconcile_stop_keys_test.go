package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/session"
)

// stopKeyOwners are the only production files that may name the stop
// request's keys: the definitions, which hold the session package's one
// reader (ReadStopRequest, V2-18), the C6a accessor and patch builders, E3's
// drain-ack CLI (the request half), and the session field registry, which
// declares those sites. Other writers clear them through
// session.ClearStopRequestPatch, which lives with the definitions.
var stopKeyOwners = []string{"internal/session/stop_request_keys.go", "internal/session/fields.go", "cmd/gc/reconcile_stop_request.go", "cmd/gc/cmd_runtime_drain.go"}

// stopRequestReaders are the production files that may call
// session.ReadStopRequest, the content gate's reader of the stop request
// (V2-18), each passing the reading its process may take: only the
// conversation lane's row read, by the controller's latched mode (its own
// lane host's latch, or, for a pass outside the controller, the mode the
// controller answers, live while it is unknown). The drain reminder's gate
// reads none: the drain notice passes a stop request, so it asks with the
// zero StopRequest.
var stopRequestReaders = []string{"cmd/gc/conversation_lane_view.go"}

// stopReadingChoosers are the production files that may name a
// session.StopRequestReading value (StopRequestsLive, StopRequestsIgnored)
// beside their definition: the one place that turns the controller's
// latched mode into a reading (laneStopReading, controllerStopReading). Any
// other caller takes its reading from there; with laneModeChoosers below,
// none can read the stop request live, or as residue, by its own choice
// (V2-18 ruling 1).
var stopReadingChoosers = []string{"cmd/gc/conversation_lane_view.go", "internal/session/stop_request_keys.go"}

// laneModeChoosers are the production files that may name laneStopReading,
// which turns a reconciler mode into a stop-request reading: its definition
// beside controllerStopReading, which passes the mode the controller
// answers, and the controller's lane host start, which passes its own latch.
// Any other caller could hand it a mode of its own choosing.
var laneModeChoosers = []string{"cmd/gc/conversation_lane_view.go", "cmd/gc/conversation_lane_host.go"}

// Kills a legacy reader of the stop request's controller half (v5 R1's
// rollback rule: legacy ignores the new keys): no production Go file under
// cmd/, internal/ or pkg/ but stopKeyOwners spells a key of either half or
// names its exported constant. And kills a third reader through the content
// gate's (V2-18): no production file but stopRequestReaders references
// session.ReadStopRequest in code, and each listed file still does. And
// kills a reader that picks its own reading: no production file but
// stopReadingChoosers names StopRequestsLive or StopRequestsIgnored in code,
// and each listed file still does; nor one that picks the mode its reading
// follows: no production file but laneModeChoosers names laneStopReading in
// code, and each listed file still does.
func TestStopRequestKeysAreNewKeys(t *testing.T) {
	banned := []string{
		session.DrainIntentReasonKey, session.DrainIntentAtKey, session.DrainIntentIncarnationKey,
		session.DrainAckIncarnationKey, session.DrainAckAtKey,
		"DrainIntentReasonKey", "DrainIntentAtKey", "DrainIntentIncarnationKey", "DrainAckIncarnationKey", "DrainAckAtKey",
	}
	root := repoRootForLint(t)
	scanned, owners := 0, 0
	readers, choosers, modeChoosers := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, dir := range []string{"cmd", "internal", "pkg"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if strings.Contains(string(data), "StopRequests") && namesIdent(t, path, data, "StopRequestsLive", "StopRequestsIgnored") {
				choosers[filepath.ToSlash(rel)] = true
			}
			if strings.Contains(string(data), "laneStopReading") && namesIdent(t, path, data, "laneStopReading") {
				modeChoosers[filepath.ToSlash(rel)] = true
			}
			if slices.Contains(stopKeyOwners, filepath.ToSlash(rel)) {
				owners++
				return nil
			}
			scanned++
			for _, k := range banned {
				if strings.Contains(string(data), k) {
					t.Errorf("%s names the stop-request key %q", rel, k)
				}
			}
			if strings.Contains(string(data), "ReadStopRequest") && namesIdent(t, path, data, "ReadStopRequest") {
				readers[filepath.ToSlash(rel)] = true
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", dir, err)
		}
	}
	if owners != len(stopKeyOwners) || scanned < 1000 {
		t.Fatalf("scanned %d files and %d of %d owners; the scan missed the tree", scanned, owners, len(stopKeyOwners))
	}
	for _, rel := range slices.Sorted(maps.Keys(readers)) {
		if !slices.Contains(stopRequestReaders, rel) {
			t.Errorf("%s calls session.ReadStopRequest, the content gate's stop-request reader; admit it in stopRequestReaders with the reading its process may take", rel)
		}
	}
	for _, rel := range stopRequestReaders {
		if !readers[rel] {
			t.Errorf("%s is admitted in stopRequestReaders but no longer calls session.ReadStopRequest; drop it from the list", rel)
		}
	}
	for _, rel := range slices.Sorted(maps.Keys(choosers)) {
		if !slices.Contains(stopReadingChoosers, rel) {
			t.Errorf("%s names a stop-request reading (StopRequestsLive, StopRequestsIgnored); take it from controllerStopReading or laneStopReading instead (V2-18)", rel)
		}
	}
	for _, rel := range stopReadingChoosers {
		if !choosers[rel] {
			t.Errorf("%s is admitted in stopReadingChoosers but no longer names a stop-request reading; drop it from the list", rel)
		}
	}
	for _, rel := range slices.Sorted(maps.Keys(modeChoosers)) {
		if !slices.Contains(laneModeChoosers, rel) {
			t.Errorf("%s names laneStopReading, choosing the mode a stop request is read by; take the reading from controllerStopReading instead (V2-18)", rel)
		}
	}
	for _, rel := range laneModeChoosers {
		if !modeChoosers[rel] {
			t.Errorf("%s is admitted in laneModeChoosers but no longer names laneStopReading; drop it from the list", rel)
		}
	}
}

// namesIdent reports whether src's code, comments aside, names one of names:
// as a selector's Sel (session.ReadStopRequest), a bare reference from the
// declaring package, or the declaration itself.
func namesIdent(t *testing.T, path string, src []byte, names ...string) bool {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, src, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && slices.Contains(names, id.Name) {
			found = true
		}
		return !found
	})
	return found
}

// TestNamesIdentSkipsComments (V2-18) pins the guard's reading of a source:
// a name in a comment is no reference, and one in code is.
func TestNamesIdentSkipsComments(t *testing.T) {
	commented := []byte("package main\n\n// session.StopRequestsLive is named here only.\nfunc f() {}\n")
	coded := []byte("package main\n\nimport \"github.com/gastownhall/gascity/internal/session\"\n\nvar r = session.StopRequestsLive\n")
	if namesIdent(t, "commented.go", commented, "StopRequestsLive") {
		t.Fatal("a comment counted as a reference")
	}
	if !namesIdent(t, "coded.go", coded, "StopRequestsLive", "StopRequestsIgnored") {
		t.Fatal("a reference in code was missed")
	}
}

// stopKeyIndexers are the names of the functions in src whose bodies index
// a map by a key of the stop request's two halves: by its constant's name
// or by its spelling.
func stopKeyIndexers(t *testing.T, path string, src any) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, src, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	names := []string{"DrainIntentReasonKey", "DrainIntentAtKey", "DrainIntentIncarnationKey", "DrainAckIncarnationKey", "DrainAckAtKey"}
	spellings := []string{session.DrainIntentReasonKey, session.DrainIntentAtKey, session.DrainIntentIncarnationKey, session.DrainAckIncarnationKey, session.DrainAckAtKey}
	var out []string
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		reads := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			ix, ok := n.(*ast.IndexExpr)
			if !ok {
				return true
			}
			switch k := ix.Index.(type) {
			case *ast.Ident:
				reads = reads || slices.Contains(names, k.Name)
			case *ast.SelectorExpr:
				reads = reads || slices.Contains(names, k.Sel.Name)
			case *ast.BasicLit:
				if v, err := strconv.Unquote(k.Value); err == nil && k.Kind == token.STRING {
					reads = reads || slices.Contains(spellings, v)
				}
			}
			return true
		})
		if reads {
			out = append(out, fn.Name.Name)
		}
	}
	return out
}

// TestSessionReadsTheStopRequestOnlyInReadStopRequest (V2-18). Kills a
// second reader of the stop request in the session package: in the files
// that may name its keys there, only ReadStopRequest indexes a row's
// metadata by one. A seeded source with two more readers, one by constant
// and one by spelling, reports both.
func TestSessionReadsTheStopRequestOnlyInReadStopRequest(t *testing.T) {
	seeded := `package session

func byConstant(meta map[string]string) bool { return meta[DrainIntentReasonKey] != "" }

func bySpelling(meta map[string]string) bool { return meta["drain_ack_incarnation"] != "" }

func clean(meta map[string]string) bool { return meta["generation"] != "" }
`
	if got := stopKeyIndexers(t, "seeded.go", seeded); !slices.Equal(got, []string{"byConstant", "bySpelling"}) {
		t.Fatalf("the seeded source's readers = %v, want [byConstant bySpelling]", got)
	}
	root := repoRootForLint(t)
	var got []string
	for _, rel := range []string{"internal/session/stop_request_keys.go", "internal/session/fields.go"} {
		path := filepath.Join(root, rel)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", rel, err)
		}
		got = append(got, stopKeyIndexers(t, path, data)...)
	}
	if !slices.Equal(got, []string{"ReadStopRequest"}) {
		t.Fatalf("the session package's readers of the stop request = %v, want only [ReadStopRequest]", got)
	}
}

// TestReadStopRequestAgreesWithActiveStop (V2-18). Kills a drift between the
// content gate's reader and the reconciler's: on rows that claim a live
// runtime, are not stop-pending and carry no ack half, where only the
// controller half decides, session.ReadStopRequest read live answers as
// activeStop does (stopRequestStands).
func TestReadStopRequestAgreesWithActiveStop(t *testing.T) {
	at := laneT0.Format(time.RFC3339)
	half := func(gen, reason, incarnation string) map[string]string {
		return map[string]string{"generation": gen, session.DrainIntentReasonKey: reason, session.DrainIntentAtKey: at, session.DrainIntentIncarnationKey: incarnation}
	}
	without := func(m map[string]string, key string) map[string]string {
		delete(m, key)
		return m
	}
	type row struct {
		name string
		meta map[string]string
	}
	var rows []row
	for _, gen := range []int64{0, 1, 7} {
		for _, reason := range []string{drainIdle, drainOrphaned, drainConfigDrift, drainSuspended} {
			begun := map[string]string{"generation": strconv.FormatInt(gen, 10)}
			maps.Copy(begun, stopBeginPatch(censusRow{Incarnation: gen}, reason, laneT0))
			rows = append(rows, row{"begun " + reason + " at " + strconv.FormatInt(gen, 10), begun})
			canceled := maps.Clone(begun)
			maps.Copy(canceled, stopCancelPatch())
			rows = append(rows, row{"canceled " + reason + " at " + strconv.FormatInt(gen, 10), canceled})
		}
	}
	other := map[string]string{"generation": "3"}
	maps.Copy(other, stopBeginPatch(censusRow{Incarnation: 2}, drainIdle, laneT0))
	rows = append(rows,
		row{"begun for generation 2 on a generation-3 row", other},
		row{"no keys", map[string]string{"generation": "3"}},
		row{"empty generation", half("", drainIdle, "0")},
		row{"absent generation", without(half("", drainIdle, "0"), "generation")},
		row{"malformed generation", half("x", drainIdle, "0")},
		row{"padded", half(" 3 ", " idle ", " 3 ")},
		row{"generation 03", half("03", drainIdle, "3")},
		row{"incarnation 03", half("3", drainIdle, "03")},
		row{"out-of-range generation", half("9223372036854775808", drainIdle, "9223372036854775807")},
		row{"blank reason", half("3", "  ", "3")},
		row{"no time", without(half("3", drainIdle, "3"), session.DrainIntentAtKey)},
		row{"no reason", without(half("3", drainIdle, "3"), session.DrainIntentReasonKey)},
		row{"no incarnation", without(half("3", drainIdle, "3"), session.DrainIntentIncarnationKey)},
	)
	requested := 0
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			meta := map[string]string{"template": "worker", "session_name": "s-gc-1", "state": "active"}
			maps.Copy(meta, r.meta)
			info := sessionInfoFromBead(sessionRow("gc-1", flattenMeta(meta)...))
			if !sessionBeadClaimsLiveRuntime(info) || isDrainAckStopPendingInfo(info) {
				t.Fatalf("the row does not exercise the controller half alone: claims %v, stop-pending %v", sessionBeadClaimsLiveRuntime(info), isDrainAckStopPendingInfo(info))
			}
			got := session.ReadStopRequest(meta, session.StopRequestsLive).Requested()
			if want := stopRequestStands(info, meta); got != want {
				t.Fatalf("ReadStopRequest = %v, activeStop = %v on %v", got, want, meta)
			}
			if got {
				requested++
			}
		})
	}
	if requested == 0 || requested == len(rows) {
		t.Fatalf("%d of %d rows request a stop; the table exercises one answer only", requested, len(rows))
	}
}

// flattenMeta is meta as sessionRow's key, value list, in key order.
func flattenMeta(meta map[string]string) []string {
	out := make([]string, 0, 2*len(meta))
	for _, k := range slices.Sorted(maps.Keys(meta)) {
		out = append(out, k, meta[k])
	}
	return out
}
