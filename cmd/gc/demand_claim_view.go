package main

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// A rig pool's default probe also reads the city legs (vp-s37 / vp-cl4), so a
// row routed to the pool but delivered to the city store wakes it. On a
// federated-ready city a seat reads and claims through its own rig leg alone
// (hookStoresScopeToPrimary), so its claim CAS, run in that leg's directory,
// resolves the id in the pool's own store first. When that store holds its own
// copy of the id — the owning store of a bead the city holds only a stale
// replica of — the CAS answers for that copy, not the city row the probe
// counted. A closed or already-held copy refuses every seat the city row
// spawns, and the city row stays ready, so demand and claim never converge
// (#5857). A city that relocates nothing also hands a rig seat its city leg,
// where the city copy itself can be claimed; gc puts no id in two stores
// there, so such a copy is a stale replica, and leaving it uncounted spawns
// no seat for it.
//
// The claim view is the pool's own store, asked the claim's question: a row a
// cross-store leg counts is demand only when the claim view has no copy of the
// id (the work genuinely lives elsewhere) or holds it open and unassigned (the
// claim can win it). The view is read by id the way the claim resolves the id
// (readDemandClaimView), so the probe is the claim's own view, not a separate
// rule about where a bead lives.
//
// Asking reads another store, and a read by id costs bd work per id. A demand
// pass asks only about the rows inside each template's claim window
// (demandClaimViewWindow), so what it asks a view is bounded by the view's
// templates, not by the routed backlog, and it asks each view about all of
// them in one read by id. It asks through its demandReads (ClaimViews):
// legacy reads every view live and waits at most
// demandClaimViewDeadline (askDemandClaimViewsLive); the v2 pass, which does
// no remote I/O (CONTRACT v5 R4), reads no view at all and serves the verdicts
// the external-reads lane recorded off the pass (recordDemandClaimView). A
// question the view cannot answer (its read failed or did not end in time; on
// v2, the lane's recording of the view is missing, stale or failed) is an
// error: its row is not counted and its template reads partial, which retains
// and blocks creates rather than under-counting demand. A row a fresh,
// error-free recording has no verdict for reached the pass after the lane last
// read the view's legs. It counts, as it does with no claim view, until the
// lane's next recording judges it (v2ClaimView).

// demandClaimViewWindow is how many of a template's rows, in the canonical
// ready order, a pass asks their claim views about: the claim window, the
// rows a seat's claim tier reads (config.RoutedReadyClaimWindow). A seat
// placed now claims from those rows, so their verdicts decide whether it can
// win work. A cross-store row ranked behind them counts as a row with no
// claim view does, and is asked about once claims advance the window to it.
const demandClaimViewWindow = config.RoutedReadyClaimWindow

// demandClaimViewDeadline bounds how long a legacy demand pass waits for its
// claim views, as crossStoreGateDeadline bounds its cross-store gate. A pass
// reads each view once, and the read takes no context: one still in flight
// when the deadline passes runs on, and its answer is dropped.
const demandClaimViewDeadline = 10 * time.Second

var (
	// errDemandClaimViewDeadline answers a question a legacy pass stopped
	// waiting for at demandClaimViewDeadline.
	errDemandClaimViewDeadline = errors.New("claim view did not answer within its deadline")
	// errDemandClaimViewUnanswered is a question a pass's reads returned no
	// verdict for.
	errDemandClaimViewUnanswered = errors.New("claim view question was not answered")
)

// crossStoreDemandLegs marks a pool's cross-store work-ledger legs with the
// store a seat of that pool resolves their ids in when it claims: the pool's
// own store. A class-binding leg stays unmarked: its rows are coordination-class
// rows no work ledger holds on a split city, so the claim resolves them in the
// binding they were read from, and asking the pool's ledger would be a misroute.
func crossStoreDemandLegs(legs []defaultScaleCheckTarget, own defaultScaleCheckTarget) []defaultScaleCheckTarget {
	out := make([]defaultScaleCheckTarget, len(legs))
	for i, leg := range legs {
		if !leg.classBinding {
			leg.claimView = own.store
			leg.claimViewKey = own.storeKey
		}
		out[i] = leg
	}
	return out
}

// demandClaimView is the store a template's seats claim a cross-store leg's
// rows in, and its store key ("rig:<name>"), which names the view in errors
// and keys its verdicts, in a pass and in the external-reads recording.
type demandClaimView struct {
	reader beads.StoreReader
	key    string
}

// demandClaimViewKey is one id asked of one claim view, named by its store
// key.
type demandClaimViewKey struct {
	view string
	id   string
}

// demandClaimViewQuestion is one row a pass asks a claim view about.
type demandClaimViewQuestion struct {
	view demandClaimView
	id   string
}

func (q demandClaimViewQuestion) key() demandClaimViewKey {
	return demandClaimViewKey{view: q.view.key, id: q.id}
}

// demandClaimViewVerdict is a claim view's answer for one id: whether a seat's
// claim can win it, or the error that left the answer unknown, in which case
// Claimable is false.
type demandClaimViewVerdict struct {
	Claimable bool
	Err       error
}

// demandClaimViews is one demand pass's claim-view verdicts, and what the pass
// shadowed with them, for its trace.
type demandClaimViews struct {
	verdicts map[demandClaimViewKey]demandClaimViewVerdict
	// probed and shadowed count the cross-store rows the pass asked about and
	// the ones it did not count; shadowedIDs names the latter.
	probed      int
	shadowed    int
	shadowedIDs []string
}

// askDemandClaimViews asks reads one pass's questions in one batch, each
// (view, id) once.
func askDemandClaimViews(reads demandReads, questions []demandClaimViewQuestion) *demandClaimViews {
	views := &demandClaimViews{}
	if unique := uniqueDemandClaimViewQuestions(questions); len(unique) > 0 {
		views.verdicts = reads.ClaimViews(unique)
	}
	return views
}

// uniqueDemandClaimViewQuestions is questions with each (view, id) once, in
// the order first asked.
func uniqueDemandClaimViewQuestions(questions []demandClaimViewQuestion) []demandClaimViewQuestion {
	seen := make(map[demandClaimViewKey]bool, len(questions))
	out := make([]demandClaimViewQuestion, 0, len(questions))
	for _, q := range questions {
		if key := q.key(); !seen[key] {
			seen[key] = true
			out = append(out, q)
		}
	}
	return out
}

// claimable reports whether a seat claiming id in view can win it. A non-nil
// error means the view's answer is unknown; the row is then not claimable
// either, and the caller reports the template partial.
func (v *demandClaimViews) claimable(view demandClaimView, id string) (bool, error) {
	verdict, ok := v.verdicts[demandClaimViewKey{view: view.key, id: id}]
	if !ok {
		verdict = demandClaimViewVerdict{Err: errDemandClaimViewUnanswered}
	}
	v.probed++
	if !verdict.Claimable {
		v.shadowed++
		v.shadowedIDs = append(v.shadowedIDs, id)
	}
	return verdict.Claimable, verdict.Err
}

// readDemandClaimView reads the claim view's own copies of ids in one read by
// id: beads.GetMany, the read the cross-store gate judges blockers with. It
// reads the view's live handle, as the claim does, never a cache: one exact
// batch read per chunk of ids where the handle has one (bd's is one bd show
// for the chunk, native Dolt's a keyed listing), otherwise one Get per id, a
// few at once. An id the read does not answer exactly is one the view does not
// hold, as Get's not-found and its id collision are, and that is the claim's
// own answer: bd show and bd update --claim resolve an id through the same
// code (bd's resolveAndGetFromStore), which reads the wisps table as well as
// the issues table, so an id bd show cannot resolve the claim cannot resolve
// in that store either; native Dolt's batch lists both planes. A read that
// panics is its error.
func readDemandClaimView(view beads.StoreReader, ids []string) (map[string]beads.Bead, error) {
	return guardedRead(func() (map[string]beads.Bead, error) { return beads.GetMany(view, ids) })
}

// demandClaimViewVerdictOf is id's verdict from a read of its claim view that
// returned held, the view's copies by id, and err: claimable when the view
// holds no copy of id or holds it open and unassigned, unknown when the read
// failed.
func demandClaimViewVerdictOf(held map[string]beads.Bead, err error, id string) demandClaimViewVerdict {
	if err != nil {
		return demandClaimViewVerdict{Err: err}
	}
	b, ok := held[id]
	if !ok {
		return demandClaimViewVerdict{Claimable: true}
	}
	return demandClaimViewVerdict{Claimable: demandClaimViewAdmits(b)}
}

// demandClaimViewAdmits reports whether the claim view's own copy of a row is
// one the claim CAS accepts: open and unassigned. The CAS tests the status the
// store records (bd's claim updates only a row whose status is open), and a
// bd-backed read folds blocked, deferred, pinned and hooked to open, so the
// copy's raw status must be open too (infraRawStatus; a store that reports no
// raw status leaves the folded one).
func demandClaimViewAdmits(b beads.Bead) bool {
	return strings.TrimSpace(b.Status) == "open" &&
		strings.TrimSpace(infraRawStatus(b)) == "open" &&
		strings.TrimSpace(b.Assignee) == ""
}

// demandClaimViewBatch is one claim view and the ids a pass asks it about.
type demandClaimViewBatch struct {
	view demandClaimView
	ids  []string
}

// demandClaimViewBatches groups questions by view: each view once, in the
// order first asked, with its ids in the order asked.
func demandClaimViewBatches(questions []demandClaimViewQuestion) []demandClaimViewBatch {
	var batches []demandClaimViewBatch
	byView := make(map[string]int)
	for _, q := range questions {
		i, ok := byView[q.view.key]
		if !ok {
			i = len(batches)
			byView[q.view.key] = i
			batches = append(batches, demandClaimViewBatch{view: q.view})
		}
		batches[i].ids = append(batches[i].ids, q.id)
	}
	return batches
}

// askDemandClaimViewsLive reads each view live once for all of its questions,
// the views at once, and waits at most deadline. A view whose read has not
// ended by then answers each of its questions with errDemandClaimViewDeadline:
// its read runs on and its answer is dropped.
func askDemandClaimViewsLive(questions []demandClaimViewQuestion, deadline time.Duration) map[demandClaimViewKey]demandClaimViewVerdict {
	questions = uniqueDemandClaimViewQuestions(questions)
	batches := demandClaimViewBatches(questions)
	type answer struct {
		batch demandClaimViewBatch
		held  map[string]beads.Bead
		err   error
	}
	// Room for every view's answer, so a read that ends after the pass
	// stopped waiting never blocks its goroutine.
	answers := make(chan answer, len(batches))
	for _, batch := range batches {
		go func() {
			held, err := readDemandClaimView(batch.view.reader, batch.ids)
			answers <- answer{batch: batch, held: held, err: err}
		}()
	}
	verdicts := make(map[demandClaimViewKey]demandClaimViewVerdict, len(questions))
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	for range batches {
		select {
		case a := <-answers:
			for _, id := range a.batch.ids {
				verdicts[demandClaimViewKey{view: a.batch.view.key, id: id}] = demandClaimViewVerdictOf(a.held, a.err, id)
			}
		case <-timer.C:
			late := fmt.Errorf("%w (%s)", errDemandClaimViewDeadline, deadline)
			for _, q := range questions {
				if _, ok := verdicts[q.key()]; !ok {
					verdicts[q.key()] = demandClaimViewVerdict{Err: late}
				}
			}
			return verdicts
		}
	}
	return verdicts
}

// v2ClaimView answers one claim-view question, the view named by its store
// key, from the external-reads lane's recording, reading no store. A view the
// recording does not hold, or holds stale, answers with a partial result, and
// a row the recording has no verdict for takes the error of the lane's read
// of the view when that read failed. A fresh, error-free recording with no
// verdict for the row has not asked the view about it yet: the rows a pass
// counts and these verdicts are separate reads, so a row can reach the pass
// first (an exact leg's cache serves it at once, and a lane-fed leg's read
// can publish before the view's slower one). Such a row is claimable, as a
// pass with no claim view counts it, until the lane's next recording judges
// it; reading it partial would block the pool's creates on a healthy city.
func v2ClaimView(rec *externalReadsRecording, now time.Time, viewKey, id string) demandClaimViewVerdict {
	s, err := rec.lookupKey(claimViewSourceKey(viewKey), now)
	if err != nil {
		return demandClaimViewVerdict{Err: &beads.PartialResultError{Op: "v2 demand claim view", Err: err}}
	}
	if verdict, ok := s.ClaimView[id]; ok {
		return verdict
	}
	if s.Err != nil {
		return demandClaimViewVerdict{Err: s.Err}
	}
	return demandClaimViewVerdict{Claimable: true}
}

// demandClaimViewLegs is one claim view and the cross-store legs whose rows
// are asked of it: what the external-reads lane asks the view each pass.
type demandClaimViewLegs struct {
	view demandClaimView
	legs []demandClaimViewLeg
}

// demandClaimViewLeg is one cross-store leg and the templates that read it
// through the view.
type demandClaimViewLeg struct {
	reader    beads.StoreReader
	templates map[string]struct{}
}

// demandClaimViewSets groups the cross-store legs of targets, a default
// probe's, by the claim view their rows are asked of: each view and each of
// its legs once, in target order.
func demandClaimViewSets(targets []defaultScaleCheckTarget) []demandClaimViewLegs {
	var sets []demandClaimViewLegs
	setOf := make(map[string]int)
	for _, target := range targets {
		template := strings.TrimSpace(target.template)
		if target.claimView == nil || target.store == nil || template == "" {
			continue
		}
		i, ok := setOf[target.claimViewKey]
		if !ok {
			i = len(sets)
			setOf[target.claimViewKey] = i
			sets = append(sets, demandClaimViewLegs{view: demandClaimView{reader: target.claimView, key: target.claimViewKey}})
		}
		sets[i].addLeg(target.store, template)
	}
	return sets
}

// addLeg adds template to the templates that read leg through the view.
func (s *demandClaimViewLegs) addLeg(leg beads.StoreReader, template string) {
	key := beads.PhysicalStoreKey(leg)
	i := slices.IndexFunc(s.legs, func(l demandClaimViewLeg) bool { return beads.PhysicalStoreKey(l.reader) == key })
	if i < 0 {
		s.legs = append(s.legs, demandClaimViewLeg{reader: leg, templates: make(map[string]struct{})})
		i = len(s.legs) - 1
	}
	s.legs[i].templates[template] = struct{}{}
}

// recordDemandClaimView asks set's view about the rows of its legs' live Ready
// that a pass can ask it about (demandClaimWindowIDs), all of them in one read
// of the view, and returns the verdicts by id, with the errors of the legs'
// Ready reads and of the view's read joined. It runs in the external-reads
// lane, off the pass, which serves these verdicts for the rows it counts
// (v2DemandReads.ClaimViews). ready reads a leg's Ready once per lane pass for
// every view its rows are asked of (laneLegReady). A row of a leg whose Ready
// failed gets no verdict, and the pass reads it partial; a failed read of the
// view answers every row with its error.
func recordDemandClaimView(cfg *config.City, set demandClaimViewLegs, ready func(beads.StoreReader) ([]beads.Bead, error)) (map[string]demandClaimViewVerdict, error) {
	var ids []string
	asked := make(map[string]bool)
	var errs []error
	for _, leg := range set.legs {
		rows, err := ready(leg.reader)
		if err != nil {
			errs = append(errs, fmt.Errorf("claim view %s: reading a cross-store leg's Ready: %w", set.view.key, err))
		}
		for _, id := range demandClaimWindowIDs(cfg, rows, leg.templates) {
			if !asked[id] {
				asked[id] = true
				ids = append(ids, id)
			}
		}
	}
	held, err := readDemandClaimView(set.view.reader, ids)
	if err != nil {
		errs = append(errs, fmt.Errorf("claim view %s: reading its copies of %d cross-store rows: %w", set.view.key, len(ids), err))
	}
	verdicts := make(map[string]demandClaimViewVerdict, len(ids))
	for _, id := range ids {
		verdicts[id] = demandClaimViewVerdictOf(held, err, id)
	}
	return verdicts, errors.Join(errs...)
}

// demandClaimWindowIDs is the ids of the rows of one cross-store leg's Ready
// that a pass can ask the claim view about: for each of templates, the first
// demandClaimViewWindow of the rows the leg serves it, in the canonical ready
// order. A pass ranks a template's rows over all of its legs, and merging in
// the other legs only moves this leg's rows back, so the template's window
// holds none of the leg's rows ranked behind these. rows are shared and left
// unmodified.
func demandClaimWindowIDs(cfg *config.City, rows []beads.Bead, templates map[string]struct{}) []string {
	byTemplate := make(map[string][]beads.Bead)
	var order []string
	for _, b := range rows {
		template, servable := demandServableForTemplates(cfg, b, templates)
		if !servable {
			continue
		}
		if _, seen := byTemplate[template]; !seen {
			order = append(order, template)
		}
		byTemplate[template] = append(byTemplate[template], b)
	}
	var ids []string
	for _, template := range order {
		served := byTemplate[template]
		beads.SortBeadsReadyOrder(served)
		for _, b := range served[:min(len(served), demandClaimViewWindow)] {
			ids = append(ids, b.ID)
		}
	}
	return ids
}

// laneLegReady is one external-reads pass's live Ready of each cross-store
// leg, the unfiltered read legacyDemandReads.ReadyAll makes, read once however
// many claim views ask about the leg's rows.
type laneLegReady struct {
	mu   sync.Mutex
	memo map[string]*readyDemandEntry
}

func newLaneLegReady() *laneLegReady {
	return &laneLegReady{memo: make(map[string]*readyDemandEntry)}
}

// read returns leg's live Ready, reading it on the pass's first call. The rows
// are shared: callers must not edit them. A read that panics is its error.
func (r *laneLegReady) read(leg beads.StoreReader) ([]beads.Bead, error) {
	key := beads.PhysicalStoreKey(leg)
	r.mu.Lock()
	e := r.memo[key]
	if e == nil {
		e = &readyDemandEntry{}
		r.memo[key] = e
	}
	r.mu.Unlock()
	e.once.Do(func() {
		e.rows, e.err = guardedRead(func() ([]beads.Bead, error) {
			return beads.LiveFor(leg).Ready(beads.ReadyQuery{TierMode: beads.TierBoth})
		})
	})
	return e.rows, e.err
}

// trace records the pass's claim-view probes on the demand trace, so a
// standing stale replica is visible to gc trace without a line on every tick.
func (v *demandClaimViews) trace(cache *readyDemandCache) {
	if v.probed == 0 {
		return
	}
	pass, point := cache.readTrace()
	if pass == nil {
		return
	}
	ids := slices.Clone(v.shadowedIDs)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	pass.cycle.RecordControllerOperation(TraceSiteDemandSnapshot, TraceReasonRetained, TraceOutcomeComplete, "demand_snapshot.claim_view", 0, map[string]any{
		"point":    point,
		"probed":   v.probed,
		"shadowed": v.shadowed,
		"ids":      ids,
	})
}

// demandClaimViewError names the template, the row and the claim view a failed
// probe asked.
func demandClaimViewError(template, id, viewKey string, err error) error {
	return fmt.Errorf("default scale_check %s: claim view %s of cross-store row %s: %w", template, viewKey, id, err)
}
