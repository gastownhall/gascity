package storebinding

// The queue's cost model: a target's delivery verbs read only that target's
// records, so their request count and the rows the store hands back are a
// property of the target, not of the queue or of the rest of the store. These
// tests count requests and returned rows through a recording store rather than
// timing anything; the index those keyed reads plan through on SQLite is
// pinned in internal/beads (TestSQLiteListMetadataQueriesDriveOffMetadataIndex).
// They also pin the quarantine that keeps one undecodable record from failing
// deliveries to everyone else.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

// claimingOpRecorder is an OpRecordingStore that also forwards (and counts)
// the two-argument Claim the queue's single-winner claim needs; the recorder
// deliberately does not forward it on its own.
type claimingOpRecorder struct {
	*beadstest.OpRecordingStore
	claimer beadsAssignmentClaimer
	claims  atomic.Int64
	// rows counts the beads every List handed back: the part of a read's
	// cost a remote store charges for.
	rows atomic.Int64
}

func (s *claimingOpRecorder) Claim(id, assignee string) (beads.Bead, bool, error) {
	s.claims.Add(1)
	return s.claimer.Claim(id, assignee)
}

func (s *claimingOpRecorder) List(query beads.ListQuery) ([]beads.Bead, error) {
	records, err := s.OpRecordingStore.List(query)
	s.rows.Add(int64(len(records)))
	return records, err
}

func (s *claimingOpRecorder) reset() {
	s.Reset()
	s.claims.Store(0)
	s.rows.Store(0)
}

// newRecordedNudgeQueue opens a SQLite-backed queue behind a recorder.
func newRecordedNudgeQueue(t *testing.T) (NudgeQueue, *claimingOpRecorder, beads.Store) {
	t.Helper()
	backing := openNudgeQueueStore(t, t.TempDir())
	claimer, ok := backing.(beadsAssignmentClaimer)
	if !ok {
		t.Fatalf("store %T carries no Claim", backing)
	}
	recorder := &claimingOpRecorder{OpRecordingStore: beadstest.NewOpRecordingStore(backing), claimer: claimer}
	return newNudgeQueue(t, recorder), recorder, backing
}

// seedUnrelatedNudges writes n live queue records addressed to other queue
// keys straight into the backing store, so the seeding is not recorded.
func seedUnrelatedNudges(t *testing.T, store beads.Store, n int, now time.Time) {
	t.Helper()
	for i := 0; i < n; i++ {
		item := testNudgeItem(fmt.Sprintf("n-unrelated-%d", i), "", now)
		item.Agent = fmt.Sprintf("beta/agent-%d", i%37)
		if _, err := store.Create(nudgeQueueRecord(item, nudgeQueueStateLive, nil)); err != nil {
			t.Fatalf("seeding unrelated nudge %d: %v", i, err)
		}
	}
}

// nudgeQueueRequestProfile is the shape of what one target's delivery verbs
// asked the store: op kinds in order, with each List's label and metadata key.
func nudgeQueueRequestProfile(ops []beadstest.RecordedOp) []string {
	profile := make([]string, 0, len(ops))
	for _, op := range ops {
		entry := op.Kind
		if op.Kind == beadstest.OpList {
			entry += fmt.Sprintf("(%s %v)", op.Query.Label, op.Query.Metadata)
		}
		profile = append(profile, entry)
	}
	return profile
}

// targetVerbCost is what one target's ListFor and ClaimDue asked the store.
type targetVerbCost struct {
	listForOps, claimOps   []beadstest.RecordedOp
	listForRows, claimRows int64
	claims                 int64
}

// targetQueueKeys is the deployed shape of a target's key set: the current
// queue key plus an alias the same agent was addressed by before.
var targetQueueKeys = []string{testNudgeAgent, "alpha/former"}

// runTargetDeliveryVerbs enqueues two nudges for the target, then lists and
// claims for it; it returns what the list and the claim each cost.
func runTargetDeliveryVerbs(t *testing.T, queue NudgeQueue, recorder *claimingOpRecorder, now time.Time) targetVerbCost {
	t.Helper()
	for _, item := range []nudgequeue.Item{testNudgeItem("n-mine", "", now), testNudgeItem("n-pinned", "session-live", now)} {
		if err := queue.Enqueue(item); err != nil {
			t.Fatalf("Enqueue(%s): %v", item.ID, err)
		}
	}
	var cost targetVerbCost
	target := ClaimTarget{QueueKeys: targetQueueKeys, SessionID: "session-live"}
	recorder.reset()
	pending, _, _, err := queue.ListFor(target, now)
	if err != nil {
		t.Fatalf("ListFor: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("ListFor pending = %v, want the target's two nudges", nudgeItemIDs(pending))
	}
	cost.listForOps, cost.listForRows = recorder.Ops(), recorder.rows.Load()
	recorder.reset()
	claimed, err := queue.ClaimDue(target, now)
	if err != nil {
		t.Fatalf("ClaimDue: %v", err)
	}
	if len(claimed) != 2 {
		t.Fatalf("ClaimDue = %v, want the target's two nudges", nudgeItemIDs(claimed))
	}
	cost.claimOps, cost.claimRows, cost.claims = recorder.Ops(), recorder.rows.Load(), recorder.claims.Load()
	return cost
}

// seedSameKeyNeighbors writes the beads another family keeps under the same
// "agent:<key>" label and "agent" metadata as the target's queue records: a
// session bead, an open nudge shadow, and a closed one — shadow history is
// closed but never deleted, so a heavily nudged target accumulates it.
func seedSameKeyNeighbors(t *testing.T, store beads.Store, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		closedShadow, err := store.Create(beads.Bead{
			Title:    fmt.Sprintf("nudge:closed-shadow-%d", i),
			Type:     nudgeQueueBeadType,
			Labels:   []string{"gc:nudge", nudgeQueueAgentLabelPrefix + testNudgeAgent},
			Metadata: map[string]string{nudgeMetaAgent: testNudgeAgent, nudgeMetaNudgeID: fmt.Sprintf("closed-shadow-%d", i)},
		})
		if err != nil {
			t.Fatalf("seeding a closed same-key shadow bead: %v", err)
		}
		if err := store.Close(closedShadow.ID); err != nil {
			t.Fatalf("closing a same-key shadow bead: %v", err)
		}
		if _, err := store.Create(beads.Bead{
			Title:    fmt.Sprintf("session %d", i),
			Type:     "session",
			Labels:   []string{"gc:session", nudgeQueueAgentLabelPrefix + testNudgeAgent},
			Metadata: map[string]string{nudgeMetaAgent: testNudgeAgent},
		}); err != nil {
			t.Fatalf("seeding a same-key session bead: %v", err)
		}
		if _, err := store.Create(beads.Bead{
			Title:    fmt.Sprintf("nudge:shadow-%d", i),
			Type:     nudgeQueueBeadType,
			Labels:   []string{"gc:nudge", nudgeQueueAgentLabelPrefix + testNudgeAgent},
			Metadata: map[string]string{nudgeMetaAgent: testNudgeAgent, nudgeMetaNudgeID: fmt.Sprintf("shadow-%d", i)},
		}); err != nil {
			t.Fatalf("seeding a same-key shadow bead: %v", err)
		}
	}
}

// assertKeyedTargetReads requires every List in ops to be one keyed read —
// the queue family, the queue's bead type, and exactly one queue key in
// metadata — and at most one such read per distinct key.
func assertKeyedTargetReads(t *testing.T, verb string, ops []beadstest.RecordedOp) {
	t.Helper()
	seen := map[string]int{}
	for _, op := range ops {
		if op.Kind != beadstest.OpList {
			continue
		}
		query := op.Query
		key, keyed := query.Metadata[nudgeMetaQueueKey]
		if query.Label != nudgeQueueLabel || query.Type != nudgeQueueBeadType || !keyed || len(query.Metadata) != 1 {
			t.Errorf("%s listed %+v; want the queue family keyed by %q metadata (caller %v)", verb, query, nudgeMetaQueueKey, op.Caller)
			continue
		}
		if query.IncludeClosed || query.Status == "closed" {
			t.Errorf("%s listed retired records: %+v", verb, query)
		}
		seen[key]++
	}
	for key, count := range seen {
		if count > 1 {
			t.Errorf("%s read queue key %q %d times, want at most once", verb, key, count)
		}
		if !containsNudgeID(targetQueueKeys, key) {
			t.Errorf("%s read queue key %q, which the target does not carry", verb, key)
		}
	}
}

// TestBeadsNudgeQueueTargetReadsAreKeyed pins the cost model: ListFor and
// ClaimDue for one target issue at most one keyed List per queue key and never
// the family-wide list, and neither the requests nor the rows the store hands
// back change with a thousand unrelated queue records, or with the session
// beads and open or closed nudge shadows that share the target's identity.
func TestBeadsNudgeQueueTargetReadsAreKeyed(t *testing.T) {
	now := time.Now().UTC()

	quietQueue, quietRecorder, _ := newRecordedNudgeQueue(t)
	quiet := runTargetDeliveryVerbs(t, quietQueue, quietRecorder, now)

	busyQueue, busyRecorder, busyBacking := newRecordedNudgeQueue(t)
	seedUnrelatedNudges(t, busyBacking, 1000, now)
	seedSameKeyNeighbors(t, busyBacking, 25)
	busy := runTargetDeliveryVerbs(t, busyQueue, busyRecorder, now)

	assertKeyedTargetReads(t, "ListFor", busy.listForOps)
	assertKeyedTargetReads(t, "ClaimDue", busy.claimOps)
	for _, pair := range []struct {
		verb        string
		quiet, busy []beadstest.RecordedOp
	}{{"ListFor", quiet.listForOps, busy.listForOps}, {"ClaimDue", quiet.claimOps, busy.claimOps}} {
		q, b := nudgeQueueRequestProfile(pair.quiet), nudgeQueueRequestProfile(pair.busy)
		if strings.Join(q, ",") != strings.Join(b, ",") {
			t.Errorf("%s requests in a busy store = %v, want the same as in a quiet one = %v", pair.verb, b, q)
		}
	}
	if quiet.claims != busy.claims {
		t.Errorf("claims in a busy store = %d, want %d", busy.claims, quiet.claims)
	}
	// The target owns two records, so each verb's keyed reads return exactly
	// those two rows however crowded the store is.
	if busy.listForRows != 2 || busy.claimRows != 2 {
		t.Errorf("rows returned in a busy store: ListFor=%d ClaimDue=%d, want 2 each (quiet: %d, %d)",
			busy.listForRows, busy.claimRows, quiet.listForRows, quiet.claimRows)
	}
}

// TestBeadsNudgeQueueReadKeyIsQueueOnly pins what drives a keyed read on the
// engine's metadata index. The rows a target's read visits are the index
// entries for its key, so the key must be one only queue records carry: nudge
// shadows live in the same store, are closed but never deleted, and would
// otherwise put the target's whole shadow history under every read.
func TestBeadsNudgeQueueReadKeyIsQueueOnly(t *testing.T) {
	store := openNudgeQueueStore(t, t.TempDir())
	now := time.Now().UTC()
	item := testNudgeItem("n-shadowed", "session-live", now)
	item.Reference = &nudgequeue.Reference{Kind: "wait", ID: "w-1"}
	shadowID, _, err := nudgequeue.NewStore(beads.NudgesStore{Store: store}).Save(item)
	if err != nil {
		t.Fatalf("saving the nudge shadow: %v", err)
	}
	shadow, err := store.Get(shadowID)
	if err != nil {
		t.Fatalf("Get shadow: %v", err)
	}
	record := nudgeQueueRecord(item, nudgeQueueStateLive, nil)
	query := nudgeQueueKeyedQuery(item.Agent)
	if len(query.Metadata) == 0 {
		t.Fatalf("keyed query %+v carries no metadata key to drive the lookup", query)
	}
	for key, value := range query.Metadata {
		if _, shared := shadow.Metadata[key]; shared {
			t.Errorf("keyed reads look up metadata %q, which nudge shadows also write; a target's read would visit its whole shadow history", key)
		}
		if record.Metadata[key] != value {
			t.Errorf("queue record metadata %q = %q, want %q so the keyed read finds it", key, record.Metadata[key], value)
		}
	}
}

// TestBeadsNudgeQueueHotVerbsNeverScanTheQueue proves maintenance is off the
// delivery path: no verb but Maintain lists the queue family or its retired
// records, and the reads write nothing.
func TestBeadsNudgeQueueHotVerbsNeverScanTheQueue(t *testing.T) {
	queue, recorder, _ := newRecordedNudgeQueue(t)
	now := time.Now().UTC()
	reference := nudgequeue.Reference{Kind: "wait", ID: "w-1"}

	first := testNudgeItem("n-first", "", now)
	first.Reference = &reference
	second := testNudgeItem("n-second", "", now)
	second.Reference = &reference
	for _, item := range []nudgequeue.Item{first, second, testNudgeItem("n-other", "", now)} {
		if err := queue.Enqueue(item); err != nil {
			t.Fatalf("Enqueue(%s): %v", item.ID, err)
		}
	}
	if err := queue.EnqueueDeferred(testNudgeItem("n-deferred", "", now)); err != nil {
		t.Fatalf("EnqueueDeferred: %v", err)
	}
	claimed := claimAll(t, queue, now)
	if len(claimed) != 3 {
		t.Fatalf("ClaimDue = %v, want the three live nudges", nudgeItemIDs(claimed))
	}
	if err := queue.Ack([]string{"n-second"}, "delivered", "", "commit"); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if err := queue.ReleaseClaims([]string{"n-other"}, time.Time{}); err != nil {
		t.Fatalf("ReleaseClaims: %v", err)
	}
	if _, err := queue.RecordFailure([]string{"n-deferred"}, errors.New("refused"), now); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}
	if err := queue.WithdrawQueuedWaitNudges([]string{"n-other"}); err != nil {
		t.Fatalf("WithdrawQueuedWaitNudges: %v", err)
	}
	if err := queue.Rollback(testNudgeItem("n-ghost", "", now), "never landed"); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	for _, op := range recorder.Ops() {
		if op.Kind != beadstest.OpList {
			continue
		}
		if op.Query.Metadata[nudgeMetaQueueKey] == "" || op.Query.IncludeClosed || op.Query.Status == "closed" {
			t.Errorf("a delivery verb scanned the queue: %+v (caller %v)", op.Query, op.Caller)
		}
	}

	recorder.Reset()
	if _, _, _, err := queue.ListFor(ClaimTarget{QueueKeys: []string{testNudgeAgent}}, now.Add(48*time.Hour)); err != nil {
		t.Fatalf("ListFor: %v", err)
	}
	if _, _, _, err := queue.ListForAgent(testNudgeAgent, now.Add(48*time.Hour)); err != nil {
		t.Fatalf("ListForAgent: %v", err)
	}
	for _, op := range recorder.Ops() {
		if op.Kind != beadstest.OpList && op.Kind != beadstest.OpGet {
			t.Errorf("a read wrote: %s %s (caller %v)", op.Kind, op.ID, op.Caller)
		}
	}
}

// TestBeadsNudgeQueueRecordsCarryTheTargetLabel pins the record shape: a
// session-pinned record is keyed by its session as well as its queue key, an
// unpinned one by its queue key only, and a record written before the target
// label existed still reads.
func TestBeadsNudgeQueueRecordsCarryTheTargetLabel(t *testing.T) {
	store := openNudgeQueueStore(t, t.TempDir())
	queue := newNudgeQueue(t, store)
	now := time.Now().UTC()

	if err := queue.Enqueue(testNudgeItem("n-pinned", "session-live", now)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := queue.Enqueue(testNudgeItem("n-unpinned", "", now)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	pinned, err := store.Get(nudgeQueueBeadID("n-pinned"))
	if err != nil {
		t.Fatalf("Get pinned: %v", err)
	}
	if !nudgeQueueHasLabel(pinned, nudgeQueueTargetLabelPrefix+"session-live") {
		t.Errorf("pinned record labels = %v, want %q", pinned.Labels, nudgeQueueTargetLabelPrefix+"session-live")
	}
	if !nudgeQueueHasLabel(pinned, nudgeQueueLabel) || !nudgeQueueHasLabel(pinned, nudgeQueueAgentLabelPrefix+testNudgeAgent) {
		t.Errorf("pinned record labels = %v, want the family and queue-key labels kept", pinned.Labels)
	}
	unpinned, err := store.Get(nudgeQueueBeadID("n-unpinned"))
	if err != nil {
		t.Fatalf("Get unpinned: %v", err)
	}
	for _, label := range unpinned.Labels {
		if strings.HasPrefix(label, nudgeQueueTargetLabelPrefix) {
			t.Errorf("unpinned record carries %q, want no target label", label)
		}
	}

	legacy := testNudgeItem("n-legacy", "session-live", now)
	record := nudgeQueueRecord(legacy, nudgeQueueStateLive, nil)
	record.Labels = []string{nudgeQueueLabel, nudgeQueueAgentLabelPrefix + legacy.Agent}
	if _, err := store.Create(record); err != nil {
		t.Fatalf("seeding a record without a target label: %v", err)
	}
	claimed, err := queue.ClaimDue(ClaimTarget{QueueKeys: []string{testNudgeAgent}, SessionID: "session-live"}, now)
	if err != nil {
		t.Fatalf("ClaimDue: %v", err)
	}
	if ids := nudgeItemIDs(claimed); !containsNudgeID(ids, "n-legacy") || !containsNudgeID(ids, "n-pinned") {
		t.Fatalf("ClaimDue = %v, want the legacy record served beside the labeled one", ids)
	}
}

// TestBeadsNudgeQueueKeyedReadsIgnoreOtherFamilies pins the family filter: the
// target's identity is shared with session beads and nudge shadows in the same
// store, and a keyed read must neither mistake one for a corrupt queue record
// nor have the store hand it back at all.
func TestBeadsNudgeQueueKeyedReadsIgnoreOtherFamilies(t *testing.T) {
	queue, recorder, backing := newRecordedNudgeQueue(t)
	now := time.Now().UTC()

	seedSameKeyNeighbors(t, backing, 3)
	if err := queue.Enqueue(testNudgeItem("n-real", "", now)); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	recorder.reset()
	pending, _, _, err := queue.ListForAgent(testNudgeAgent, now)
	if err != nil {
		t.Fatalf("ListForAgent: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != "n-real" {
		t.Fatalf("pending = %v, want only the queue record", nudgeItemIDs(pending))
	}
	if got := recorder.rows.Load(); got != 1 {
		t.Fatalf("the keyed read returned %d rows, want only the queue record; the family filter is not pushed to the store", got)
	}
}

// TestBeadsNudgeQueueQuarantinesACorruptRecord is the isolation property: an
// undecodable record fails nobody's delivery, direct access to it still says
// what it is, and Maintain names it after finishing the rest of its pass.
func TestBeadsNudgeQueueQuarantinesACorruptRecord(t *testing.T) {
	store := openNudgeQueueStore(t, t.TempDir())
	queue := newNudgeQueue(t, store)
	now := time.Now().UTC()

	for _, owner := range []string{testNudgeAgent, "beta/broken"} {
		seedCorruptNudgeRecord(t, store, "n-corrupt-"+strings.ReplaceAll(owner, "/", "-"), owner)
	}
	expiring := testNudgeItem("n-expiring", "", now)
	expiring.Agent = "gamma/other"
	expiring.ExpiresAt = now.Add(time.Minute)
	for _, item := range []nudgequeue.Item{testNudgeItem("n-good", "", now), expiring} {
		if err := queue.Enqueue(item); err != nil {
			t.Fatalf("Enqueue(%s): %v", item.ID, err)
		}
	}

	// The skip is reported on the hot path without failing the delivery: the
	// good record is served, and the error says the answer is partial and
	// names the record it left out.
	corruptMine := nudgeQueueBeadID("n-corrupt-alpha-polly")
	assertQuarantined := func(verb string, err error) {
		t.Helper()
		if !beads.IsPartialResult(err) || !errors.Is(err, ErrBeadsNudgeQueueCorrupt) {
			t.Fatalf("%s error = %v, want a partial result wrapping ErrBeadsNudgeQueueCorrupt", verb, err)
		}
		if !strings.Contains(err.Error(), corruptMine) {
			t.Errorf("%s error %q does not name the skipped record %s", verb, err, corruptMine)
		}
		if strings.Contains(err.Error(), nudgeQueueBeadID("n-corrupt-beta-broken")) {
			t.Errorf("%s error %q names another target's record", verb, err)
		}
	}
	pending, _, _, err := queue.ListFor(ClaimTarget{QueueKeys: []string{testNudgeAgent}}, now)
	assertQuarantined("ListFor", err)
	if len(pending) != 1 || pending[0].ID != "n-good" {
		t.Fatalf("pending = %v, want the good nudge served and the corrupt one skipped", nudgeItemIDs(pending))
	}
	_, _, _, err = queue.ListForAgent(testNudgeAgent, now)
	assertQuarantined("ListForAgent", err)
	claimed, err := queue.ClaimDue(ClaimTarget{QueueKeys: []string{testNudgeAgent}}, now)
	assertQuarantined("ClaimDue", err)
	if len(claimed) != 1 || claimed[0].ID != "n-good" {
		t.Fatalf("ClaimDue = %v, want the good nudge claimed past the corrupt one", nudgeItemIDs(claimed))
	}
	// A target with no corrupt record of its own reads clean.
	if _, _, _, err := queue.ListForAgent("gamma/other", now); err != nil {
		t.Fatalf("ListForAgent for an unaffected target: %v", err)
	}
	// Enqueue is not a read: a corrupt record under the same key does not
	// fail a submit, and the reads above are what report it.
	after := testNudgeItem("n-after", "", now)
	after.Reference = &nudgequeue.Reference{Kind: "wait", ID: "w-after"}
	if err := queue.Enqueue(after); err != nil {
		t.Fatalf("Enqueue (with supersession) beside a corrupt record: %v", err)
	}

	if err := queue.Ack([]string{"n-corrupt-alpha-polly"}, "delivered", "", "commit"); !errors.Is(err, ErrBeadsNudgeQueueCorrupt) {
		t.Fatalf("Ack of the corrupt record = %v, want ErrBeadsNudgeQueueCorrupt", err)
	}
	if _, err := queue.Snapshot(); !errors.Is(err, ErrBeadsNudgeQueueCorrupt) {
		t.Fatalf("Snapshot = %v, want the whole-queue diagnostic to surface the corrupt record", err)
	}

	// Finding corrupt records is not a failed pass: the error stays nil and
	// the report names each one, so a caller can tell the two apart.
	afterExpiry := now.Add(2 * time.Minute)
	report, err := queue.Maintain(context.Background(), afterExpiry)
	if err != nil {
		t.Fatalf("Maintain = %v, want a pass that only found corrupt records to succeed", err)
	}
	reported := errors.Join(report.Corrupt...)
	if len(report.Corrupt) != 2 || !errors.Is(reported, ErrBeadsNudgeQueueCorrupt) {
		t.Fatalf("Maintain report Corrupt = %v, want both corrupt records", report.Corrupt)
	}
	for _, nudgeID := range []string{"n-corrupt-alpha-polly", "n-corrupt-beta-broken"} {
		if !strings.Contains(reported.Error(), nudgeQueueBeadID(nudgeID)) {
			t.Errorf("Maintain report %q does not name corrupt record %s", reported, nudgeQueueBeadID(nudgeID))
		}
	}
	record, getErr := store.Get(nudgeQueueBeadID("n-expiring"))
	if getErr != nil {
		t.Fatalf("Get expiring: %v", getErr)
	}
	if record.Metadata[nudgeMetaQueueState] != nudgeQueueStateDead {
		t.Fatalf("expiring record queue_state = %q, want Maintain to finish its pass past the corrupt records", record.Metadata[nudgeMetaQueueState])
	}
}

// seedCorruptNudgeRecord writes a live queue record addressed to owner that no
// reader can decode: its deliver-after time is not a time.
func seedCorruptNudgeRecord(t *testing.T, store beads.Store, nudgeID, owner string) {
	t.Helper()
	if _, err := store.Create(beads.Bead{
		ID:     nudgeQueueBeadID(nudgeID),
		Title:  "nudge-queue:" + nudgeID,
		Type:   nudgeQueueBeadType,
		Labels: []string{nudgeQueueLabel, nudgeQueueAgentLabelPrefix + owner},
		Metadata: map[string]string{
			nudgeMetaNudgeID:      nudgeID,
			nudgeMetaAgent:        owner,
			nudgeMetaQueueKey:     owner,
			nudgeMetaQueueState:   nudgeQueueStateLive,
			nudgeMetaDeliverAfter: "not-a-time",
		},
	}); err != nil {
		t.Fatalf("seeding a corrupt record for %s: %v", owner, err)
	}
}

// TestWrappedClaimDueReportsAQuarantineAsDelivered pins the event a claim past
// a corrupt record emits through the production wrapper. The claim answered
// for every record it could read, so the event is ok, and the partial-result
// error still crosses the wrapper verbatim for the caller to report. Once the
// good item is claimed, the corrupt record alone leaves an idle target, which
// is ok too: reporting either claim as failed would paint the target as an
// outage for as long as the record stays.
func TestWrappedClaimDueReportsAQuarantineAsDelivered(t *testing.T) {
	store := openNudgeQueueStore(t, t.TempDir())
	queue := newNudgeQueue(t, store)
	now := time.Now().UTC()
	seedCorruptNudgeRecord(t, store, "n-corrupt", testNudgeAgent)
	if err := queue.Enqueue(testNudgeItem("n-good", "", now)); err != nil {
		t.Fatalf("Enqueue(n-good): %v", err)
	}
	adapters, err := NewBeadsAdapters(store, testBeadsAdapterIdentity(), queue)
	if err != nil {
		t.Fatalf("projecting the beads binding: %v", err)
	}
	var outcomes []ClassOutcome
	wrapped, err := WrapNudges(adapters.Nudges, ClassWrapping{
		Binding:    "work",
		Capability: ClassCapability{Available: true},
		Observer: ClassObserverFunc(func(event ClassEvent) {
			if event.Kind == ClassEventClaim && event.Operation == "ClaimDue" {
				outcomes = append(outcomes, event.Outcome)
			}
		}),
	})
	if err != nil {
		t.Fatalf("wrapping the Nudges front doors: %v", err)
	}

	target := ClaimTarget{QueueKeys: []string{testNudgeAgent}}
	for _, want := range [][]string{{"n-good"}, {}} {
		claimed, err := wrapped.Queue.ClaimDue(target, now)
		if !beads.IsPartialResult(err) || !errors.Is(err, ErrBeadsNudgeQueueCorrupt) {
			t.Fatalf("ClaimDue error = %v, want the queue's partial result wrapping ErrBeadsNudgeQueueCorrupt", err)
		}
		if got := nudgeItemIDs(claimed); fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("ClaimDue = %v, want %v claimed past the corrupt record", got, want)
		}
	}
	if len(outcomes) != 2 || outcomes[0] != ClassOutcomeOK || outcomes[1] != ClassOutcomeOK {
		t.Fatalf("ClaimDue outcomes = %v, want [ok ok]: a claim past a quarantined record is not a failed claim", outcomes)
	}
}

// TestBeadsNudgeQueueClaimRepairsWhatItTouches pins the targeted repair that
// replaced the per-call maintenance pass: an expired candidate is durably
// dead-lettered instead of claimed, while other targets' records are left for
// Maintain.
func TestBeadsNudgeQueueClaimRepairsWhatItTouches(t *testing.T) {
	store := openNudgeQueueStore(t, t.TempDir())
	queue := newNudgeQueue(t, store)
	now := time.Now().UTC()

	mine := testNudgeItem("n-mine", "", now)
	mine.ExpiresAt = now.Add(time.Minute)
	theirs := testNudgeItem("n-theirs", "", now)
	theirs.Agent = "beta/other"
	theirs.ExpiresAt = now.Add(time.Minute)
	for _, item := range []nudgequeue.Item{mine, theirs} {
		if err := queue.Enqueue(item); err != nil {
			t.Fatalf("Enqueue(%s): %v", item.ID, err)
		}
	}
	afterExpiry := now.Add(2 * time.Minute)
	if claimed := claimAll(t, queue, afterExpiry); len(claimed) != 0 {
		t.Fatalf("ClaimDue = %v, want an expired nudge never claimed", nudgeItemIDs(claimed))
	}
	touched, err := store.Get(nudgeQueueBeadID("n-mine"))
	if err != nil {
		t.Fatalf("Get mine: %v", err)
	}
	if touched.Metadata[nudgeMetaQueueState] != nudgeQueueStateDead || touched.Metadata[nudgeMetaLastError] != "expired" {
		t.Fatalf("touched record metadata = %v, want it dead-lettered as expired", touched.Metadata)
	}
	untouched, err := store.Get(nudgeQueueBeadID("n-theirs"))
	if err != nil {
		t.Fatalf("Get theirs: %v", err)
	}
	if untouched.Metadata[nudgeMetaQueueState] != nudgeQueueStateLive {
		t.Fatalf("another target's record queue_state = %q, want it left for Maintain", untouched.Metadata[nudgeMetaQueueState])
	}
	// The read still answers truthfully before Maintain records it.
	_, _, dead, err := queue.ListForAgent("beta/other", afterExpiry)
	if err != nil {
		t.Fatalf("ListForAgent: %v", err)
	}
	if len(dead) != 1 || dead[0].LastError != "expired" {
		t.Fatalf("dead = %+v, want the expired nudge read as dead before Maintain", dead)
	}
}

// TestBeadsNudgeQueueReleasingAnExpiredClaimRecordsItDead pins the touched
// expiry on ReleaseClaims at the record, where the contract cannot see it: a
// released expired item and a dead-lettered one read the same, because reads
// project an expired live item as dead. Releasing a claim on an item that has
// passed its deliver-by deadline must record it dead as expired, with the
// claim cleared, rather than return it to the live lifecycle.
func TestBeadsNudgeQueueReleasingAnExpiredClaimRecordsItDead(t *testing.T) {
	store := openNudgeQueueStore(t, t.TempDir())
	queue := newNudgeQueue(t, store)
	now := time.Now().UTC()
	claimedAt := now.Add(-time.Minute)

	item := testNudgeItem("n-expired-claim", "", claimedAt)
	item.ExpiresAt = now.Add(-time.Second)
	if err := queue.Enqueue(item); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if claimed := claimAll(t, queue, claimedAt); len(claimed) != 1 {
		t.Fatalf("ClaimDue = %v, want the unexpired nudge", nudgeItemIDs(claimed))
	}
	if err := queue.ReleaseClaims([]string{item.ID}, time.Time{}); err != nil {
		t.Fatalf("ReleaseClaims: %v", err)
	}
	record, err := store.Get(nudgeQueueBeadID(item.ID))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := record.Metadata[nudgeMetaQueueState]; got != nudgeQueueStateDead {
		t.Errorf("queue_state after releasing an expired claim = %q, want %q", got, nudgeQueueStateDead)
	}
	if got := record.Metadata[nudgeMetaTerminalState]; got != "expired" {
		t.Errorf("terminal_state = %q, want expired", got)
	}
	if got := record.Metadata[nudgeMetaDeadAt]; got != formatNudgeTime(item.ExpiresAt) {
		t.Errorf("dead_at = %q, want the deliver-by deadline %q", got, formatNudgeTime(item.ExpiresAt))
	}
	if record.Assignee != "" {
		t.Errorf("assignee = %q, want the claim cleared", record.Assignee)
	}
}

// TestBeadsNudgeQueueSupersedeIsScopedToItsQueueKey pins supersession on the
// keyed read: older live items for the same (agent, source, reference) are
// dead-lettered, and another agent's item with the same reference is not.
func TestBeadsNudgeQueueSupersedeIsScopedToItsQueueKey(t *testing.T) {
	store := openNudgeQueueStore(t, t.TempDir())
	queue := newNudgeQueue(t, store)
	now := time.Now().UTC()
	reference := nudgequeue.Reference{Kind: "wait", ID: "w-1"}

	old := testNudgeItem("n-old", "", now)
	old.Reference = &reference
	neighbor := testNudgeItem("n-neighbor", "", now)
	neighbor.Agent = "beta/other"
	neighbor.Reference = &reference
	replacement := testNudgeItem("n-new", "", now)
	replacement.Reference = &reference
	for _, item := range []nudgequeue.Item{old, neighbor, replacement} {
		if err := queue.Enqueue(item); err != nil {
			t.Fatalf("Enqueue(%s): %v", item.ID, err)
		}
	}
	pending, _, dead, err := queue.ListForAgent(testNudgeAgent, now)
	if err != nil {
		t.Fatalf("ListForAgent: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != "n-new" {
		t.Fatalf("pending = %v, want only the replacement", nudgeItemIDs(pending))
	}
	if len(dead) != 1 || dead[0].ID != "n-old" || dead[0].LastError != "superseded" {
		t.Fatalf("dead = %+v, want the older item superseded", dead)
	}
	pending, _, _, err = queue.ListForAgent("beta/other", now)
	if err != nil {
		t.Fatalf("ListForAgent(neighbor): %v", err)
	}
	if len(pending) != 1 || pending[0].ID != "n-neighbor" {
		t.Fatalf("neighbor pending = %v, want another agent's item untouched", nudgeItemIDs(pending))
	}
}

func nudgeQueueHasLabel(record beads.Bead, label string) bool {
	for _, have := range record.Labels {
		if have == label {
			return true
		}
	}
	return false
}

func containsNudgeID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
