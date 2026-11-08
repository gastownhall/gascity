package main

// The reverse migration's writes into one destination store (S5 §4.4 R1, R2):
// prepare, copy and verify, each cut into batches the time budget and the
// crash seam can stop between (infra_copy_engine.go), and the detach-and-delete
// a resume, --abort-reverse and the to-rigs binding clear share.
//
// The store is opened and its capabilities looked up in openReverseTarget and
// nowhere else; the rest works on the narrow surfaces it hands out.

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// reverseRowStore is the row and edge surface a destination answers.
type reverseRowStore interface {
	List(query beads.ListQuery) ([]beads.Bead, error)
	DepList(id, direction string) ([]beads.Dep, error)
	DepAdd(issueID, dependsOnID, depType string) error
	DepRemove(issueID, dependsOnID string) error
}

// reverseTarget is one opened destination: a work or rig store.
type reverseTarget struct {
	dest   string
	engine string
	rows   reverseRowStore
	// create writes rows under their own ids, all or nothing per call; nil
	// when the store cannot keep an id.
	create func([]beads.Bead) error
	// batched reports that create commits a whole call at once.
	batched bool
	// unsupported says why the store cannot take the reverse's rows, or "".
	unsupported string
	// carriesPayload reports an edge-payload writer.
	carriesPayload bool
	addEdges       func([]beads.DepWithPayload) error
	deleteIDs      func([]string) error
	down           func(ids []string) (map[string][]beads.Dep, error)
	// payloads reads the payloads on edges, or is nil when the store cannot
	// be asked.
	payloads func(edges map[string][]beads.Dep) (map[beads.DepPair]string, error)
	// form is the store's created form (beads.CreatedFormer), or nil.
	form func(beads.Bead) (beads.Bead, error)
	// acceptsDangling reports whether the store keeps an edge whose target
	// it does not hold (beads.DanglingDepJudge; true without one).
	acceptsDangling func(issueID, dependsOnID string) bool
	close           func()
}

// dropsRef reports whether the store keeps no step ref (its created form
// drops it).
func (t *reverseTarget) dropsRef() bool {
	if t.form == nil {
		return false
	}
	got, err := t.form(beads.Bead{ID: "probe", Title: "probe", Ref: "probe"})
	return err == nil && got.Ref == ""
}

// reverseTargetOpener opens a destination; tests replace it.
var reverseTargetOpener = openReverseTarget

// openReverseTarget opens dest (reverseCityDest, or a rig's import stamp) as
// the window opens it: the city's work store through openInfraMigrationSource
// and a rig's through rigImportStoreOpener. The capabilities are looked up on
// the store under the bead-policy wrapper, which declares every capability
// whatever the store under it can do.
func openReverseTarget(cityPath string, cfg *config.City, dest string) (*reverseTarget, error) {
	var store beads.Store
	var err error
	if rig, ok := strings.CutPrefix(dest, rigImportStamp("")); ok {
		r, declared := rigImportRigNamed(cfg, rig)
		if !declared {
			return nil, fmt.Errorf("this city configures no rig %s", rig)
		}
		store, err = rigImportStoreOpener(cityPath, r)
	} else {
		store, err = openInfraMigrationSource(cityPath)
	}
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", reverseDestNoun(dest), err)
	}
	under, _, _ := unwrapBeadPolicyStore(store)
	t := &reverseTarget{dest: dest, engine: fmt.Sprintf("%T", under), rows: store, close: func() { _ = closeBeadStoreHandle(store) }}
	if batch, ok := beads.As[beads.ForeignIDBatchCreator](under); ok {
		t.create, t.batched = batch.CreateWithForeignIDBatch, true
	} else if one, ok := beads.As[beads.ForeignIDCreator](under); ok {
		t.create = func(rows []beads.Bead) error {
			for _, b := range rows {
				if _, err := one.CreateWithForeignID(b); err != nil {
					return fmt.Errorf("creating %s: %w", b.ID, err)
				}
			}
			return nil
		}
	}
	if probe, ok := beads.As[interface{ ImportSupported() (bool, error) }](under); ok {
		supported, err := probe.ImportSupported()
		switch {
		case err != nil:
			t.unsupported = fmt.Sprintf("asking its exec provider for the optional import op failed: %v", err)
		case !supported:
			t.unsupported = "it is a generic exec store whose provider offers no import op (listed by its capabilities op), so it cannot take a bead under its own id"
		}
	}
	if fidelity, ok := beads.As[beads.ForeignIDCreateFidelity](under); ok && !fidelity.ForeignIDCreateKeepsRows() && t.unsupported == "" {
		t.unsupported = fmt.Sprintf("store %s is a proxied store whose write leaf has no create that keeps a bead's id, status and timestamps (it falls back to a plain create)", t.engine)
	}
	if t.create == nil && t.unsupported == "" {
		t.unsupported = fmt.Sprintf("store %s cannot create a bead under its own id", t.engine)
	}
	writer, carries := beads.As[beads.DepMetadataWriter](under)
	t.carriesPayload = carries
	if batch, ok := beads.As[beads.DepBatchWriter](under); ok {
		t.addEdges = batch.DepAddBatch
	} else {
		t.addEdges = func(edges []beads.DepWithPayload) error {
			for _, e := range edges {
				var err error
				switch {
				case e.Payload == "":
					err = store.DepAdd(e.IssueID, e.DependsOnID, e.Type)
				case carries:
					err = writer.DepAddWithMetadata(e.IssueID, e.DependsOnID, e.Type, e.Payload)
				default:
					err = fmt.Errorf("store %s cannot carry the payload on %s -> %s", t.engine, e.IssueID, e.DependsOnID)
				}
				if err != nil {
					return fmt.Errorf("adding dep %s -> %s: %w", e.IssueID, e.DependsOnID, err)
				}
			}
			return nil
		}
	}
	t.deleteIDs = func(ids []string) error { return deleteInBatches(under, ids) }
	lister, _ := beads.As[beads.DependencyBatchLister](under)
	t.down = func(ids []string) (map[string][]beads.Dep, error) { return readInfraDownEdges(lister, store, ids) }
	if _, blind := beads.DepMetadataAbsent(under); !blind {
		reader, single := beads.As[beads.DepMetadataReader](under)
		batch, many := beads.As[beads.DepMetadataBatchReader](under)
		if single || many {
			t.payloads = func(edges map[string][]beads.Dep) (map[beads.DepPair]string, error) {
				return readInfraPayloads(batch, reader, edges, func(beads.Dep) bool { return true })
			}
		}
	}
	if former, ok := beads.As[beads.CreatedFormer](under); ok {
		t.form = former.CreatedForm
	}
	t.acceptsDangling = func(string, string) bool { return true }
	if judge, ok := beads.As[beads.DanglingDepJudge](under); ok {
		t.acceptsDangling = judge.AcceptsDanglingDep
	}
	return t, nil
}

// reverseStampQuery lists the rows stamped with stamp, both tiers, closed
// included.
func reverseStampQuery(stamp string) beads.ListQuery {
	return beads.ListQuery{Metadata: map[string]string{beadmeta.InfraReversedFromMetadataKey: stamp}, IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true}
}

// reverseRowDifference compares a destination row with the row the reverse
// wrote, in the form the destination reads it back (beads.CreatedFormer):
// "" when they agree. The readiness projection is the destination's own.
func reverseRowDifference(form func(beads.Bead) (beads.Bead, error), want, got beads.Bead) string {
	if form != nil {
		normalized, err := form(want)
		if err != nil {
			return err.Error()
		}
		want = normalized
	}
	got.IsBlocked = nil
	if diff := beadCopyDifference(want, got); diff != "" {
		return diff
	}
	if wantClass, gotClass := coordclass.Classify(want), coordclass.Classify(got); wantClass != gotClass {
		return fmt.Sprintf("classifies as %s in the store, want %s", gotClass, wantClass)
	}
	return ""
}

// reverseEdgeKey is one edge as a set member: target and type.
func reverseEdgeKey(d beads.Dep) string { return d.DependsOnID + " (" + d.Type + ")" }

// reverseCopyInto copies rows and edges into t: prepare (keep this run's
// earlier rows that still equal the plan, delete the rest), create the rows
// not kept in batches, add the edges not present, then verify against a
// reopened store. It returns the rows it created.
//
// inBinding is every id the binding holds: a stamped row whose binding copy
// is gone may be the only copy left (a to-rigs clear that began), so it is
// refused, never deleted.
func reverseCopyInto(t *reverseTarget, reopen func() (*reverseTarget, error), rows []beads.Bead, edges []beads.DepWithPayload, inBinding map[string]bool, stamp string, clock infraEngineClock) (int, error) {
	phase := "reverse " + t.dest
	if !t.carriesPayload {
		for _, e := range edges {
			if e.Payload != "" {
				return 0, fmt.Errorf("%s cannot write the payload on %s -> %s, so the edge is refused rather than removed and lost; nothing was changed", reverseDestNoun(t.dest), e.IssueID, e.DependsOnID)
			}
		}
	}
	want := make(map[string]beads.Bead, len(rows))
	for _, b := range rows {
		want[b.ID] = b
	}
	planned := map[string]map[string]bool{}
	for _, e := range edges {
		if planned[e.IssueID] == nil {
			planned[e.IssueID] = map[string]bool{}
		}
		planned[e.IssueID][reverseEdgeKey(e.Dep)] = true
	}
	stamped, err := t.rows.List(reverseStampQuery(stamp))
	if err != nil {
		return 0, fmt.Errorf("listing %s: %w", reverseDestNoun(t.dest), err)
	}
	stampedIDs := make([]string, 0, len(stamped))
	for _, b := range stamped {
		stampedIDs = append(stampedIDs, b.ID)
	}
	existing, err := t.down(stampedIDs)
	if err != nil {
		return 0, err
	}
	kept := map[string]bool{}
	var drop, onlyCopy []string
	for _, b := range stamped {
		if b.Metadata[beadmeta.InfraReversedFromMetadataKey] != stamp {
			continue // only this run's rows are ever deleted
		}
		if !inBinding[b.ID] {
			onlyCopy = append(onlyCopy, b.ID)
			continue
		}
		w, ok := want[b.ID]
		keep := ok && reverseRowDifference(t.form, w, b) == ""
		for _, d := range existing[b.ID] {
			keep = keep && planned[b.ID][reverseEdgeKey(d)]
		}
		if keep {
			kept[b.ID] = true
			continue
		}
		drop = append(drop, b.ID)
	}
	if len(onlyCopy) > 0 {
		sort.Strings(onlyCopy)
		return 0, fmt.Errorf("%s holds %d row(s) under this reverse's stamp that the binding no longer holds, so each may be the only copy: %s. Nothing was removed", reverseDestNoun(t.dest), len(onlyCopy), infraStrandedIDList(onlyCopy))
	}
	if err := reverseDeleteRows(t, drop); err != nil {
		return 0, err
	}
	if err := infraEngineCrashPoint(phase + " prepare"); err != nil {
		return 0, err
	}
	var create []beads.Bead
	for _, b := range rows {
		if !kept[b.ID] {
			create = append(create, b)
		}
	}
	err = runInfraBatches(clock, phase+" copying rows", len(create), infraCopyRowBatch, func(start, end int) error {
		return t.create(create[start:end])
	})
	if err != nil {
		return 0, err
	}
	sources := map[string]bool{}
	for _, e := range edges {
		sources[e.IssueID] = true
	}
	present, err := t.down(sortedMapKeys(sources))
	if err != nil {
		return len(create), err
	}
	var add []beads.DepWithPayload
	for _, e := range edges {
		have := ""
		for _, d := range present[e.IssueID] {
			if d.DependsOnID == e.DependsOnID {
				have = d.Type
			}
		}
		switch {
		case have == e.Type && e.Payload == "":
			continue
		case have != "":
			if err := t.rows.DepRemove(e.IssueID, e.DependsOnID); err != nil {
				return len(create), fmt.Errorf("replacing dep %s -> %s: %w", e.IssueID, e.DependsOnID, err)
			}
		}
		add = append(add, e)
	}
	err = runInfraBatches(clock, phase+" copying edges", len(add), infraCopyEdgeBatch, func(start, end int) error {
		return t.addEdges(add[start:end])
	})
	if err != nil {
		return len(create), err
	}
	if err := reverseVerify(reopen, rows, edges, stamp); err != nil {
		return len(create), fmt.Errorf("verifying %s: %w", reverseDestNoun(t.dest), err)
	}
	return len(create), infraEngineCrashPoint(phase + " verify")
}

// reverseVerify proves a destination against a reopened store: it holds
// exactly the planned rows under this run's stamp, each equal to the row
// written; each planned row's edges are exactly the planned ones; each
// planned edge from a row the store already held is there; and every planned
// payload reads back.
func reverseVerify(reopen func() (*reverseTarget, error), rows []beads.Bead, edges []beads.DepWithPayload, stamp string) error {
	t, err := reopen()
	if err != nil {
		return err
	}
	defer t.close()
	got, err := t.rows.List(reverseStampQuery(stamp))
	if err != nil {
		return err
	}
	held := make(map[string]beads.Bead, len(got))
	for _, b := range got {
		held[b.ID] = b
	}
	want := make(map[string]bool, len(rows))
	for _, b := range rows {
		want[b.ID] = true
		g, ok := held[b.ID]
		if !ok {
			return fmt.Errorf("the store does not hold %s", b.ID)
		}
		if diff := reverseRowDifference(t.form, b, g); diff != "" {
			return fmt.Errorf("%s differs: %s", b.ID, diff)
		}
	}
	for id := range held {
		if !want[id] {
			return fmt.Errorf("the store holds %s under this reverse's stamp, which the plan does not", id)
		}
	}
	wantEdges := map[string][]string{}
	sources := map[string]bool{}
	for _, b := range rows {
		sources[b.ID] = true
		wantEdges[b.ID] = nil
	}
	for _, e := range edges {
		sources[e.IssueID] = true
		wantEdges[e.IssueID] = append(wantEdges[e.IssueID], reverseEdgeKey(e.Dep))
	}
	down, err := t.down(sortedMapKeys(sources))
	if err != nil {
		return err
	}
	for _, id := range sortedMapKeys(sources) {
		have := make([]string, 0, len(down[id]))
		for _, d := range down[id] {
			have = append(have, reverseEdgeKey(d))
		}
		if want[id] {
			if diff := stringSetDifference("dependency of "+id, wantEdges[id], have); diff != "" {
				return errors.New(diff)
			}
			continue
		}
		haveSet := setOf(have)
		for _, e := range wantEdges[id] {
			if !haveSet[e] {
				return fmt.Errorf("dependency of %s %q missing", id, e)
			}
		}
	}
	if t.payloads == nil {
		return nil
	}
	payloads, err := t.payloads(down)
	if err != nil {
		return err
	}
	for _, e := range edges {
		if got := payloads[beads.DepPair{IssueID: e.IssueID, DependsOnID: e.DependsOnID}]; got != e.Payload {
			return fmt.Errorf("dep %s -> %s carries %q in the store, want %q", e.IssueID, e.DependsOnID, got, e.Payload)
		}
	}
	return nil
}

// reverseDeleteRows removes ids from t with every edge that joins one of them
// to a row that stays, first: a store's delete rewrites a surviving
// neighbor's text that names a deleted id, and finds the neighbors by their
// edges (rigImportClear). The edges onto the ids are found from the down edges
// of every row the store holds, read in batches.
func reverseDeleteRows(t *reverseTarget, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	sort.Strings(ids)
	going := setOf(ids)
	all, err := t.rows.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		return fmt.Errorf("listing %s: %w", reverseDestNoun(t.dest), err)
	}
	every := make([]string, 0, len(all))
	for _, b := range all {
		every = append(every, b.ID)
	}
	down, err := t.down(every)
	if err != nil {
		return err
	}
	for _, from := range sortedMapKeys(down) {
		for _, d := range down[from] {
			if going[from] == going[d.DependsOnID] || strings.HasPrefix(d.DependsOnID, "external:") {
				continue
			}
			if err := t.rows.DepRemove(from, d.DependsOnID); err != nil && !errors.Is(err, beads.ErrNotFound) {
				return fmt.Errorf("removing dep %s -> %s: %w", from, d.DependsOnID, err)
			}
		}
	}
	if err := t.deleteIDs(ids); err != nil {
		return fmt.Errorf("removing %d row(s) from %s: %w", len(ids), reverseDestNoun(t.dest), err)
	}
	return nil
}

// reverseEdgesOnto reads, from every row t holds that ids does not name, the
// edges onto one of ids, with their payloads: the edges the store held onto a
// planned id before the reverse wrote anything (an earlier clear re-added
// W -> D with D only in the binding), which a delete of D would take with it.
func reverseEdgesOnto(t *reverseTarget, ids map[string]bool) ([]reverseEdge, error) {
	all, err := t.rows.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", reverseDestNoun(t.dest), err)
	}
	from := make([]string, 0, len(all))
	for _, b := range all {
		if !ids[b.ID] {
			from = append(from, b.ID)
		}
	}
	down, err := t.down(from)
	if err != nil {
		return nil, err
	}
	onto := map[string][]beads.Dep{}
	for id, deps := range down {
		for _, d := range deps {
			if ids[d.DependsOnID] {
				onto[id] = append(onto[id], d)
			}
		}
	}
	payloads := map[beads.DepPair]string{}
	if t.payloads != nil && len(onto) > 0 {
		if payloads, err = t.payloads(onto); err != nil {
			return nil, err
		}
	}
	var out []reverseEdge
	for _, id := range sortedMapKeys(onto) {
		for _, d := range onto[id] {
			out = append(out, reverseEdge{From: id, To: d.DependsOnID, Type: d.Type, Payload: payloads[beads.DepPair{IssueID: id, DependsOnID: d.DependsOnID}]})
		}
	}
	return out, nil
}

// reverseEdge is one edge as the journal records it.
type reverseEdge struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Type    string `json:"type"`
	Payload string `json:"payload,omitempty"`
}

func (e reverseEdge) dep() beads.DepWithPayload {
	return beads.DepWithPayload{Dep: beads.Dep{IssueID: e.From, DependsOnID: e.To, Type: e.Type}, Payload: e.Payload}
}
